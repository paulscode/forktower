package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tier is an alert's severity.
type Tier string

// Alert severities. `resolved` and `loss` sit outside the info/warning/critical
// ladder because they describe outcomes rather than urgency; the alerter ranks
// them against that ladder when deciding what to deliver where.
const (
	TierInfo     Tier = "info"
	TierWarning  Tier = "warning"
	TierCritical Tier = "critical"
	TierResolved Tier = "resolved"
	TierLoss     Tier = "loss"
)

// Valid reports whether t is a severity this schema accepts.
func (t Tier) Valid() bool {
	switch t {
	case TierInfo, TierWarning, TierCritical, TierResolved, TierLoss:
		return true
	default:
		return false
	}
}

// Alert is one condition worth telling the user about.
//
// Identity is DedupKey, not ID: raising the same condition again updates the
// existing row rather than adding another, so an escalating situation does not
// bury the user in near-identical notifications.
type Alert struct {
	ID           int64
	Tier         Tier
	Kind         string
	DedupKey     string
	Subject      string
	Message      string
	CreatedAt    int64
	LastRaisedAt int64
	// AckedAt is zero while unacknowledged. Urgent alerts are re-delivered until
	// it is set.
	AckedAt int64
}

// Acked reports whether the user has acknowledged this alert.
func (a Alert) Acked() bool { return a.AckedAt != 0 }

// AlertUpsert is what happened to an alert that was raised.
//
// Three outcomes rather than two, because the caller has to tell them apart to
// decide whether to notify anyone. New and Reopened both mean "tell the user";
// neither means "tell them again about something they are already looking at".
type AlertUpsert struct {
	ID int64
	// New means no alert with this dedup key existed.
	New bool
	// Reopened means one existed and had been acknowledged, so the condition has
	// come back after the user said they had seen it. That is news again, and the
	// acknowledgement is cleared — otherwise a condition that recurs after being
	// dismissed would be silent forever, which is how an alarm becomes decorative.
	Reopened bool
}

// Notify reports whether this raise is worth delivering to a transport. A raise
// that is neither new nor a reopening is the same condition the user has not
// looked at yet, and repeating it is the escalation policy's job, not this one's.
func (u AlertUpsert) Notify() bool { return u.New || u.Reopened }

// ReconcileAlert records an alert only if that condition is not already on the
// user's list, and **never clears an acknowledgement**.
//
// The difference from UpsertAlert is the whole reason this exists. A reconciler
// re-derives standing conditions from stored state on a timer, so it raises the
// same thing over and over for as long as it is true. Going through UpsertAlert
// would clear the acknowledgement each time and notify again — turning "I have
// seen this, I will deal with it tomorrow" into a notification every minute,
// which is its own way of making an alarm decorative.
//
// So an existing row has its last-raised time bumped and nothing else touched.
// The user's acknowledgement is theirs to withdraw.
func (s *Store) ReconcileAlert(ctx context.Context, a Alert) (AlertUpsert, error) {
	if a.DedupKey == "" {
		return AlertUpsert{}, errors.New("store: alert needs a dedup key")
	}
	if !a.Tier.Valid() {
		return AlertUpsert{}, fmt.Errorf("store: alert tier %q is not a known severity", a.Tier)
	}
	if a.Kind == "" {
		return AlertUpsert{}, errors.New("store: alert needs a kind")
	}

	var out AlertUpsert
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var existing int64
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id FROM alerts WHERE dedup_key = ?`, a.DedupKey).Scan(&existing)

		switch {
		case scanErr == nil:
			// Already on the list. Acknowledged or not, it stays as the user left
			// it; only the record of when it was last true moves.
			if _, e := tx.ExecContext(ctx,
				`UPDATE alerts SET last_raised_at = ? WHERE id = ?`,
				a.LastRaisedAt, existing); e != nil {
				return fmt.Errorf("bumping alert %q: %w", a.DedupKey, e)
			}
			out = AlertUpsert{ID: existing}
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO alerts (tier, kind, dedup_key, subject, message,
				                     created_at, last_raised_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				a.Tier, a.Kind, a.DedupKey, nullString(a.Subject), a.Message,
				a.CreatedAt, a.LastRaisedAt)
			if e != nil {
				return fmt.Errorf("recording alert %q: %w", a.DedupKey, e)
			}
			id, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new alert id: %w", e)
			}
			out = AlertUpsert{ID: id, New: true}
			return nil

		default:
			return fmt.Errorf("looking up alert %q: %w", a.DedupKey, scanErr)
		}
	})
	if err != nil {
		return AlertUpsert{}, err
	}
	return out, nil
}

// UpsertAlert records an alert, or bumps the existing one with the same dedup
// key.
//
// Timestamps are parameters rather than read from the clock here, so that
// escalation behaviour can be tested without waiting.
func (s *Store) UpsertAlert(ctx context.Context, a Alert) (AlertUpsert, error) {
	if a.DedupKey == "" {
		return AlertUpsert{}, errors.New("store: alert needs a dedup key")
	}
	if !a.Tier.Valid() {
		return AlertUpsert{}, fmt.Errorf("store: alert tier %q is not a known severity", a.Tier)
	}
	if a.Kind == "" {
		return AlertUpsert{}, errors.New("store: alert needs a kind")
	}

	var out AlertUpsert
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			existing int64
			acked    sql.NullInt64
		)
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id, acked_at FROM alerts WHERE dedup_key = ?`, a.DedupKey).Scan(&existing, &acked)

		switch {
		case scanErr == nil:
			// last_raised_at moves, and an acknowledgement is cleared. The original
			// message and creation time stay: they are the record of when this first
			// happened, and the audit trail is not rewritten in place.
			if _, e := tx.ExecContext(ctx,
				`UPDATE alerts SET last_raised_at = ?, acked_at = NULL WHERE id = ?`,
				a.LastRaisedAt, existing); e != nil {
				return fmt.Errorf("bumping alert %q: %w", a.DedupKey, e)
			}
			out = AlertUpsert{ID: existing, Reopened: acked.Valid}
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO alerts
				   (tier, kind, dedup_key, subject, message, created_at, last_raised_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				a.Tier, a.Kind, a.DedupKey, nullString(a.Subject), a.Message,
				a.CreatedAt, a.LastRaisedAt)
			if e != nil {
				return fmt.Errorf("inserting alert %q: %w", a.DedupKey, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new alert id: %w", e)
			}
			out = AlertUpsert{ID: newID, New: true}
			return nil

		default:
			return fmt.Errorf("looking up alert %q: %w", a.DedupKey, scanErr)
		}
	})
	if err != nil {
		return AlertUpsert{}, err
	}
	return out, nil
}

// GetAlert reads one alert by id, returning ErrNotFound if there is none.
func (s *Store) GetAlert(ctx context.Context, id int64) (Alert, error) {
	var (
		a       Alert
		subject sql.NullString
		acked   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tier, kind, dedup_key, subject, message,
		        created_at, last_raised_at, acked_at
		 FROM alerts WHERE id = ?`, id).
		Scan(&a.ID, &a.Tier, &a.Kind, &a.DedupKey, &subject, &a.Message,
			&a.CreatedAt, &a.LastRaisedAt, &acked)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, fmt.Errorf("alert %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Alert{}, fmt.Errorf("reading alert %d: %w", id, err)
	}
	a.Subject = subject.String
	a.AckedAt = acked.Int64
	return a, nil
}

// ResolveAlert closes a standing alert, because the condition it describes has
// passed.
//
// **This is what a resolution was always supposed to do and never did.** A
// resolved candidate carrying the same dedup key as the warning went through
// UpsertAlert, which found the row, bumped its timestamp and cleared its
// acknowledgement — so a watchtower coming back produced no recovery at all and
// resurrected the "not answering" notice the user had already dismissed. The
// comment promising that recovery is announced was describing something that
// could not happen.
//
// The tier, subject and message move to the resolution's; created_at does not,
// because when the trouble started is the part worth keeping. It is marked
// acknowledged in the same breath: a condition that is over should not go on
// repeating, and nobody needs to dismiss news that the problem went away.
//
// Returns the closed alert's id, or zero when there was nothing standing under
// that key. A resolution for something never raised is not news, and the caller
// drops it rather than announcing relief from a problem the user never had.
func (s *Store) ResolveAlert(ctx context.Context, a Alert) (id int64, err error) {
	if a.DedupKey == "" {
		return 0, errors.New("store: alert needs a dedup key")
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		id = 0
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM alerts WHERE dedup_key = ? AND tier != ?`,
			a.DedupKey, TierResolved)
		switch e := row.Scan(&id); {
		case errors.Is(e, sql.ErrNoRows):
			return nil
		case e != nil:
			return fmt.Errorf("resolving alert %q: %w", a.DedupKey, e)
		}
		if _, e := tx.ExecContext(ctx,
			`UPDATE alerts
			    SET tier = ?, kind = ?, subject = ?, message = ?,
			        last_raised_at = ?, acked_at = ?
			  WHERE id = ?`,
			a.Tier, a.Kind, nullString(a.Subject), a.Message,
			a.LastRaisedAt, a.LastRaisedAt, id); e != nil {
			return fmt.Errorf("resolving alert %q: %w", a.DedupKey, e)
		}
		return nil
	})
	return id, err
}

// AckAlert marks an alert acknowledged, stopping its repeat delivery. It reports
// whether this call was the one that changed it, so acknowledging twice is
// harmless and distinguishable.
func (s *Store) AckAlert(ctx context.Context, id, at int64) (changed bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET acked_at = ? WHERE id = ? AND acked_at IS NULL`, at, id)
	if err != nil {
		return false, fmt.Errorf("acknowledging alert %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acknowledging alert %d: %w", id, err)
	}
	if n > 0 {
		return true, nil
	}

	// Nothing changed: either already acknowledged, or no such alert. Worth
	// telling apart — the second is a bug in the caller.
	var exists int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM alerts WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("alert %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("checking alert %d: %w", id, err)
	}
	return false, nil
}

// AlertFilter narrows ListAlerts.
type AlertFilter struct {
	// UnackedOnly restricts results to alerts still awaiting acknowledgement.
	UnackedOnly bool
	// Limit caps the number returned; zero means the default, and anything above
	// the maximum is clamped rather than refused.
	Limit int
}

// Alert listing bounds.
const (
	DefaultAlertLimit = 100
	MaxAlertLimit     = 500
)

// ListAlerts returns alerts in ascending id order, which is both stable and
// chronological.
func (s *Store) ListAlerts(ctx context.Context, f AlertFilter) ([]Alert, error) {
	query := `SELECT id, tier, kind, dedup_key, subject, message,
	                 created_at, last_raised_at, acked_at
	          FROM alerts`
	if f.UnackedOnly {
		query += ` WHERE acked_at IS NULL`
	}
	query += ` ORDER BY id ASC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, clampLimit(f.Limit, DefaultAlertLimit, MaxAlertLimit))
	if err != nil {
		return nil, fmt.Errorf("listing alerts: %w", err)
	}
	// Nothing new to learn from Close once rows.Err() has been checked below.
	defer func() { _ = rows.Close() }()

	var out []Alert
	for rows.Next() {
		var (
			a       Alert
			subject sql.NullString
			acked   sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &a.Tier, &a.Kind, &a.DedupKey, &subject, &a.Message,
			&a.CreatedAt, &a.LastRaisedAt, &acked); err != nil {
			return nil, fmt.Errorf("scanning alert: %w", err)
		}
		a.Subject = subject.String
		a.AckedAt = acked.Int64
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing alerts: %w", err)
	}
	return out, nil
}

// Delivery is one attempt to send an alert through one transport.
type Delivery struct {
	ID          int64
	AlertID     int64
	Transport   string
	AttemptedAt int64
	OK          bool
	// Error is the failure reason, already scrubbed by the caller. Transport
	// errors routinely echo the request URL, and a notification URL may carry a
	// token in it, so nothing unscrubbed reaches this column: it is read back by
	// the API and included in exported diagnostics.
	Error string
}

// RecordDelivery stores a delivery attempt, successful or not. Failures are
// recorded rather than dropped, because a transport that has quietly stopped
// working is how an alarm becomes decorative.
func (s *Store) RecordDelivery(ctx context.Context, d Delivery) (int64, error) {
	if d.Transport == "" {
		return 0, errors.New("store: delivery needs a transport name")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_deliveries (alert_id, transport, attempted_at, ok, error)
		 VALUES (?, ?, ?, ?, ?)`,
		d.AlertID, d.Transport, d.AttemptedAt, boolToInt(d.OK), nullString(d.Error))
	if err != nil {
		return 0, fmt.Errorf("recording delivery to %q: %w", d.Transport, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading new delivery id: %w", err)
	}
	return id, nil
}

// ListDeliveries returns every attempt for one alert, oldest first.
func (s *Store) ListDeliveries(ctx context.Context, alertID int64) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, alert_id, transport, attempted_at, ok, error
		 FROM alert_deliveries WHERE alert_id = ? ORDER BY id ASC`, alertID)
	if err != nil {
		return nil, fmt.Errorf("listing deliveries for alert %d: %w", alertID, err)
	}
	// Nothing new to learn from Close once rows.Err() has been checked below.
	defer func() { _ = rows.Close() }()

	var out []Delivery
	for rows.Next() {
		var (
			d      Delivery
			ok     int
			errMsg sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.AlertID, &d.Transport, &d.AttemptedAt, &ok, &errMsg); err != nil {
			return nil, fmt.Errorf("scanning delivery: %w", err)
		}
		d.OK = ok != 0
		d.Error = errMsg.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing deliveries for alert %d: %w", alertID, err)
	}
	return out, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func clampLimit(requested, def, maximum int) int {
	switch {
	case requested <= 0:
		return def
	case requested > maximum:
		return maximum
	default:
		return requested
	}
}
