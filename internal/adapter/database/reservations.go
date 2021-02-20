package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/nglogic/go-example-project/internal/app"
	"github.com/sirupsen/logrus"
)

// Customer type enums for customer table.
const (
	customerTypeBusiness   = "business"
	customerTypeIndividual = "individual"
)

// ReservationsRepository manages reservation data in db.
type ReservationsRepository struct {
	parent *Adapter
	db     *sqlx.DB
	log    logrus.FieldLogger
}

// GetBikeAvailability returns true if bike with given id is available for rent in given time range.
func (r *ReservationsRepository) GetBikeAvailability(ctx context.Context, bikeID string, startTime, endTime time.Time) (bool, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("creating postgresql transaction: %w", err)
	}
	defer func() {
		_ = tx.Commit()
	}()

	return r.checkAvailability(ctx, tx, bikeID, startTime, endTime)
}

// ListReservations returns list of reservations matching request criteria.
func (r *ReservationsRepository) ListReservations(ctx context.Context, req app.ListReservationsRequest) ([]app.Reservation, error) {
	var rs []reservationModel
	err := r.db.SelectContext(
		ctx,
		&rs,
		`select 
			r.*, 
			c.first_name, c.surname, c.email, c.type,
			b.model_name, b.weight, b.price_per_h
		from reservations r
		inner join customers c on r.customer_id = c.id
		inner join bikes b on r.bike_id = b.id
		where 
			r.start_time < $2
			and r.end_time > $1
			and r.bike_id = $3`,
		req.StartTime,
		req.EndTime,
		req.BikeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying for reservations in postgresql: %w", err)
	}

	result := make([]app.Reservation, 0, len(rs))
	for _, v := range rs {
		result = append(result, v.ToAppReservation())
	}
	return result, nil
}

// CreateReservation creates new reservation in db.
// Bike id must be provided.
// If customer doesn't exists, it is created with reservation.
func (r *ReservationsRepository) CreateReservation(ctx context.Context, reservation app.Reservation) (*app.Reservation, error) {
	if err := r.checkReservationData(reservation); err != nil {
		return nil, err
	}

	tx, err := r.db.BeginTxx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
	})
	if err != nil {
		return nil, fmt.Errorf("creating postgresql transaction: %w", err)
	}
	defer rollbackTx(ctx, tx, r.log) // This will be noop after successful commit.

	// Constraint checks have to be deffered to the end of the sql tx,
	// because we might have to create new customer in current transaction.
	if _, err := tx.ExecContext(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
		return nil, fmt.Errorf("setting postgresql transaction constraints: %w", err)
	}

	bike, err := r.parent.Bikes().Get(ctx, reservation.Bike.ID)
	if err != nil {
		return nil, fmt.Errorf("invalid bike: %w", err)
	}
	reservation.Bike = *bike

	available, err := r.checkAvailability(ctx, tx, reservation.Bike.ID, reservation.StartTime, reservation.EndTime)
	if err != nil {
		return nil, fmt.Errorf("checking bike availability: %w", err)
	}
	if !available {
		return nil, app.NewConflictError("bike not available")
	}

	if reservation.Customer.ID != "" {
		customer, err := r.parent.Customers().GetInTx(ctx, tx, reservation.Customer.ID)
		if err != nil {
			return nil, fmt.Errorf("invalid customer: %w", err)
		}
		reservation.Customer = *customer
	} else {
		id, err := r.parent.Customers().AddInTx(ctx, tx, reservation.Customer)
		if err != nil {
			return nil, fmt.Errorf("creating customer: %w", err)
		}
		reservation.Customer.ID = id
	}

	if err := r.createReservation(ctx, tx, reservation); err != nil {
		return nil, fmt.Errorf("creating reservation: %w", err)
	}

	if err := commitTx(ctx, tx, r.log); err != nil {
		return nil, fmt.Errorf("commiting postgres transaction: %w", err)
	}

	return &reservation, nil
}

// CancelReservation sets reservation status to canceled.
// Returns app.ErrNotFound if reservation doesn't exists.
func (r *ReservationsRepository) CancelReservation(ctx context.Context, bikeID string, id string) error {
	res, err := r.db.ExecContext(
		ctx,
		`update reservations 
		set status='canceled'
		where bike_id=$1 and id=$2`,
		bikeID,
		id,
	)
	if err != nil {
		return fmt.Errorf("updating reservation status in postgres: %w", err)
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		return app.ErrNotFound
	}
	return nil
}

func (r *ReservationsRepository) checkAvailability(ctx context.Context, tx *sqlx.Tx, bikeID string, startTime, endTime time.Time) (bool, error) {
	var count int
	rows, err := tx.NamedQuery(
		`select count(*) from reservations WHERE 
			bike_id = :bike_id
			and start_time < :end_time
			and end_time > :start_time
			and status != 'canceled'
		`,
		map[string]interface{}{
			"bike_id":    bikeID,
			"start_time": startTime,
			"end_time":   endTime,
		},
	)
	if err != nil {
		return false, fmt.Errorf("querying for conflicting reservations in postgresql: %w", err)
	}
	for rows.Next() {
		if err := rows.Scan(&count); err != nil {
			return false, fmt.Errorf("scanning postgresql query result: %w", err)
		}
	}

	if count > 0 {
		return false, nil
	}
	return true, nil
}

func (r *ReservationsRepository) checkReservationData(reservation app.Reservation) error {
	if reservation.ID == "" {
		return errors.New("reservation id is empty")
	}
	if reservation.Customer.ID == "" && reservation.Customer.Email == "" {
		return errors.New("customer id or email must be set")
	}
	if reservation.Bike.ID == "" {
		return errors.New("bike id is empty")
	}
	return nil
}

func (r *ReservationsRepository) createReservation(ctx context.Context, tx *sqlx.Tx, reservation app.Reservation) error {
	m := newReservationModel(reservation)
	_, err := tx.NamedExec(
		`insert into reservations (id, status, bike_id, customer_id, start_time, end_time, total_value, applied_discount)
		values (:id, :status, :bike_id, :customer_id, :start_time, :end_time, :total_value, :applied_discount)`,
		m,
	)
	if err != nil {
		return fmt.Errorf("inserting reservation row into postgres: %w", err)
	}

	app.AugmentLogFromCtx(ctx, r.log).
		WithField("id", m.ID).
		WithField("bikeId", m.BikeID).
		WithField("customerId", m.CustomerID).
		Info("reservation created in db")

	return nil
}

type reservationModel struct {
	ID              string    `db:"id"`
	Status          string    `db:"status"`
	BikeID          string    `db:"bike_id"`
	CustomerID      string    `db:"customer_id"`
	StartTime       time.Time `db:"start_time"`
	EndTime         time.Time `db:"end_time"`
	TotalValue      float64   `db:"total_value"`
	AppliedDiscount float64   `db:"applied_discount"`

	// Join on customers
	FirstName string `db:"first_name"`
	Surname   string `db:"surname"`
	Email     string `db:"email"`
	Type      string `db:"type"`

	// Join on bikes
	ModelName    string  `db:"model_name"`
	Weight       float64 `db:"weight"`
	PricePerHour float64 `db:"price_per_h"`
}

func newReservationModel(ar app.Reservation) reservationModel {
	return reservationModel{
		ID:              ar.ID,
		Status:          string(ar.Status),
		BikeID:          ar.Bike.ID,
		CustomerID:      ar.Customer.ID,
		StartTime:       ar.StartTime,
		EndTime:         ar.EndTime,
		TotalValue:      ar.TotalValue,
		AppliedDiscount: ar.AppliedDiscount,
	}
}

func (m *reservationModel) ToAppReservation() app.Reservation {
	cm := customerModel{
		ID:        m.CustomerID,
		Type:      m.Type,
		FirstName: m.FirstName,
		Surname:   m.Surname,
		Email:     m.Email,
	}
	bm := bikeModel{
		ID:           m.BikeID,
		ModelName:    m.ModelName,
		Weight:       m.Weight,
		PricePerHour: m.PricePerHour,
	}
	return app.Reservation{
		ID:              m.ID,
		Status:          app.ReservationStatus(m.Status),
		Customer:        cm.ToAppCustomer(),
		Bike:            bm.ToAppBike(),
		StartTime:       m.StartTime,
		EndTime:         m.EndTime,
		TotalValue:      m.TotalValue,
		AppliedDiscount: m.AppliedDiscount,
	}
}
