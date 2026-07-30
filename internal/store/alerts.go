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

// UpsertAlert records an alert, or bumps the existing one with the same dedup
// key. It reports the row's id and whether it was newly created — the caller
// delivers a notification for a new alert, and applies its repeat policy to one
// that already existed.
//
// Timestamps are parameters rather than read from the clock here, so that
// escalation behaviour can be tested without waiting.
func (s *Store) UpsertAlert(ctx context.Context, a Alert) (id int64, wasNew bool, err error) {
	if a.DedupKey == "" {
		return 0, false, errors.New("store: alert needs a dedup key")
	}
	if !a.Tier.Valid() {
		return 0, false, fmt.Errorf("store: alert tier %q is not a known severity", a.Tier)
	}
	if a.Kind == "" {
		return 0, false, errors.New("store: alert needs a kind")
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var existing int64
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id FROM alerts WHERE dedup_key = ?`, a.DedupKey).Scan(&existing)

		switch {
		case scanErr == nil:
			// Only last_raised_at moves. The original message and creation time are
			// the record of when this first happened, and the audit trail is not
			// rewritten in place.
			if _, e := tx.ExecContext(ctx,
				`UPDATE alerts SET last_raised_at = ? WHERE id = ?`,
				a.LastRaisedAt, existing); e != nil {
				return fmt.Errorf("bumping alert %q: %w", a.DedupKey, e)
			}
			id, wasNew = existing, false
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
			id, wasNew = newID, true
			return nil

		default:
			return fmt.Errorf("looking up alert %q: %w", a.DedupKey, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, wasNew, nil
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
