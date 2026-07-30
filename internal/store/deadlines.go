package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DeadlineKind is which clock is running.
type DeadlineKind string

// Deadline kinds.
const (
	// DeadlineCSV is the window to answer a commitment that has confirmed —
	// after which the peer can sweep and the money is gone.
	DeadlineCSV DeadlineKind = "csv"
	// DeadlineHTLCIncoming is an incoming HTLC we know the preimage for and must
	// claim before its expiry.
	DeadlineHTLCIncoming DeadlineKind = "htlc_incoming"
	// DeadlineHTLCOutgoing is an outgoing HTLC that times out at its expiry.
	DeadlineHTLCOutgoing DeadlineKind = "htlc_outgoing"
)

// Valid reports whether k is a kind this schema accepts.
func (k DeadlineKind) Valid() bool {
	switch k {
	case DeadlineCSV, DeadlineHTLCIncoming, DeadlineHTLCOutgoing:
		return true
	default:
		return false
	}
}

// DeadlineState is what became of a deadline.
type DeadlineState string

// Deadline states.
const (
	DeadlineCounting DeadlineState = "counting"
	DeadlineResolved DeadlineState = "resolved"
	DeadlineExpired  DeadlineState = "expired"
)

// Valid reports whether st is a state this schema accepts.
func (st DeadlineState) Valid() bool {
	switch st {
	case DeadlineCounting, DeadlineResolved, DeadlineExpired:
		return true
	default:
		return false
	}
}

// AssumedDeadlineFloor is the delay used when the real one is not known.
//
// The low end of what channels actually use. Deliberately conservative: too
// short means an alarm that fires earlier than it needed to, which is a bug
// report. Too long means one that fires after the money has gone, which is not.
const AssumedDeadlineFloor = 144

// Deadline is a clock running against the user.
type Deadline struct {
	ID             int64
	SpendEventID   int64
	Kind           DeadlineKind
	DeadlineHeight int32
	State          DeadlineState
	// Escalation is the highest tier already alerted, so a restart does not
	// replay every escalation the user has already been through.
	Escalation int32
	// Assumed means an input was missing and a conservative floor was used.
	// Surfaced distinctly, because a countdown the user cannot fully trust is
	// still worth more than no countdown, but they should know which it is.
	Assumed        bool
	ResolvedByTxID string
	UpdatedAt      int64
}

// UpsertDeadline records a deadline, or updates the one already there.
//
// Keyed on (spend, kind), so recomputing after better information arrives
// updates the existing clock rather than starting a second one beside it. Two
// deadlines for one spend would mean two countdowns disagreeing on the same
// screen.
//
// **A deadline is never skipped because an input was missing.** The caller
// supplies `Assumed` and a height computed from AssumedDeadlineFloor instead;
// the natural implementation — skip the row when the delay is unknown — produces
// no countdown, no escalation and no loss event, so the breach alerts once and
// then goes quiet exactly as the window closes.
func (s *Store) UpsertDeadline(ctx context.Context, d Deadline) (id int64, changed bool, err error) {
	if d.SpendEventID == 0 {
		return 0, false, fmt.Errorf("store: a deadline needs the spend that started it")
	}
	if !d.Kind.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a deadline kind", d.Kind)
	}
	if d.State == "" {
		d.State = DeadlineCounting
	}
	if !d.State.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a deadline state", d.State)
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			existingID      int64
			existingHeight  int32
			existingAssumed bool
		)
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id, deadline_height, assumed FROM deadlines
			  WHERE spend_event_id = ? AND kind = ?`,
			d.SpendEventID, d.Kind).Scan(&existingID, &existingHeight, &existingAssumed)

		switch {
		case scanErr == nil:
			id = existingID
			if existingHeight == d.DeadlineHeight && existingAssumed == d.Assumed {
				changed = false
				return nil
			}
			// The height and whether it is assumed may both improve — an unknown
			// CSV delay that later becomes known replaces the floor with the real
			// figure. State and escalation are not touched: they belong to the
			// engine that runs the clock, not to whoever recomputed the height.
			if _, e := tx.ExecContext(ctx,
				`UPDATE deadlines SET deadline_height = ?, assumed = ?, updated_at = ?
				  WHERE id = ?`,
				d.DeadlineHeight, boolToInt(d.Assumed), d.UpdatedAt, existingID); e != nil {
				return fmt.Errorf("updating deadline %d: %w", existingID, e)
			}
			changed = true
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO deadlines
				   (spend_event_id, kind, deadline_height, state, escalation, assumed, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				d.SpendEventID, d.Kind, d.DeadlineHeight, d.State, d.Escalation,
				boolToInt(d.Assumed), d.UpdatedAt)
			if e != nil {
				return fmt.Errorf("recording a deadline for spend %d: %w", d.SpendEventID, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new deadline id: %w", e)
			}
			id, changed = newID, true
			return nil

		default:
			return fmt.Errorf("looking up the deadline for spend %d: %w", d.SpendEventID, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, changed, nil
}

// ListDeadlines returns deadlines in a state, soonest first.
//
// Soonest first because that is the order they matter in, and the one the user
// needs at the top of a screen.
func (s *Store) ListDeadlines(ctx context.Context, state DeadlineState) ([]Deadline, error) {
	query := `SELECT id, spend_event_id, kind, deadline_height, state, escalation,
	                 assumed, resolved_by_txid, updated_at
	            FROM deadlines`
	var args []any
	if state != "" {
		query += " WHERE state = ?"
		args = append(args, state)
	}
	query += " ORDER BY deadline_height ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing deadlines: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Deadline
	for rows.Next() {
		var (
			d       Deadline
			assumed int
			txid    sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.SpendEventID, &d.Kind, &d.DeadlineHeight,
			&d.State, &d.Escalation, &assumed, &txid, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning a deadline: %w", err)
		}
		d.Assumed = assumed != 0
		d.ResolvedByTxID = txid.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing deadlines: %w", err)
	}
	return out, nil
}

// SetDeadlineState records what became of a deadline.
func (s *Store) SetDeadlineState(
	ctx context.Context, id int64, state DeadlineState, resolvedBy string, at int64,
) error {
	if !state.Valid() {
		return fmt.Errorf("store: %q is not a deadline state", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE deadlines SET state = ?, resolved_by_txid = ?, updated_at = ? WHERE id = ?`,
		state, nullString(resolvedBy), at, id)
	if err != nil {
		return fmt.Errorf("updating deadline %d: %w", id, err)
	}
	return requireOneRow(res, "deadline", id)
}

// SetDeadlineEscalation records the highest tier already alerted.
//
// Only ever moves forward. A restart, or a recomputation, must not walk the user
// back through escalations they have already had — an alarm that repeats an old
// stage looks like a new event.
func (s *Store) SetDeadlineEscalation(ctx context.Context, id int64, tier int32, at int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deadlines SET escalation = ?, updated_at = ?
		  WHERE id = ? AND escalation < ?`,
		tier, at, id, tier)
	if err != nil {
		return fmt.Errorf("recording escalation for deadline %d: %w", id, err)
	}
	// No rows means the deadline is already at or past this tier, which is not an
	// error: two paths noticing the same escalation must not fight.
	if _, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("checking escalation for deadline %d: %w", id, err)
	}
	return nil
}
