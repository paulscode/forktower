package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MirrorState is what became of one transaction the mirror considered.
type MirrorState string

// Mirror states.
const (
	// MirrorDenied means the policy refused it. Nothing was broadcast, and this
	// is not a failure — it is the allowlist working. Recorded rather than merely
	// logged because the refusals are the larger set and the ones a user will
	// want explained.
	MirrorDenied MirrorState = "denied"
	// MirrorPending means it was allowed and has not yet been accepted.
	MirrorPending MirrorState = "pending"
	// MirrorAccepted means the target branch took it.
	MirrorAccepted MirrorState = "accepted"
	// MirrorRejected means the target refused it and we are still trying.
	MirrorRejected MirrorState = "rejected"
	// MirrorAbandoned means we stopped trying. The transaction is still the
	// user's problem, so this state exists to be alerted on rather than to close
	// the matter.
	MirrorAbandoned MirrorState = "abandoned"
)

// Valid reports whether s is a state this schema accepts.
func (s MirrorState) Valid() bool {
	switch s {
	case MirrorDenied, MirrorPending, MirrorAccepted, MirrorRejected, MirrorAbandoned:
		return true
	default:
		return false
	}
}

// MirrorDecision is one transaction the mirror considered, and what it decided.
//
// Decisions rather than attempts: the mirror is an allowlist with default deny,
// so most of what it sees it declines, and "why was this not mirrored?" has to
// be answerable from the database rather than from a log line that has since
// rotated away.
type MirrorDecision struct {
	ID           int64
	TxID         string
	SourceBranch Branch
	TargetBranch Branch
	// ChannelID is zero when the transaction could not be tied to a channel,
	// which is itself a reason to deny.
	ChannelID int64
	// Shape is what the classifier made of it, recorded as observed. The policy
	// verdict is only as good as this, and a reader checking one needs the other.
	Shape SpendShape
	// Reason is which rule permitted it, or which rule refused it. Required in
	// both directions.
	Reason      string
	State       MirrorState
	Attempts    int64
	FirstSeenAt int64
	UpdatedAt   int64
	LastError   string
}

// RecordMirrorDecision writes what the policy decided about a transaction, and
// reports whether this was the first time we saw it.
//
// Idempotent on (txid, target branch): the same transaction observed on every
// pass writes one row, not one per pass. A decision that has already been made
// is not remade — in particular a transaction already accepted by the target is
// left alone, because re-deciding it would reset a settled outcome on the
// strength of seeing it again.
func (s *Store) RecordMirrorDecision(
	ctx context.Context, d MirrorDecision,
) (id int64, existed bool, err error) {
	if d.TxID == "" {
		return 0, false, errors.New("store: a mirror decision needs the transaction it is about")
	}
	if d.Reason == "" {
		return 0, false, errors.New("store: a mirror decision needs a reason, in either direction")
	}
	if !d.State.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a mirror state this schema accepts", d.State)
	}
	if !d.SourceBranch.Valid() || !d.TargetBranch.Valid() {
		return 0, false, errors.New("store: a mirror decision needs both of its branches")
	}
	if d.SourceBranch == d.TargetBranch {
		return 0, false, fmt.Errorf(
			"store: mirroring %s to itself is not a direction", d.SourceBranch)
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var existingID int64
		row := tx.QueryRowContext(ctx,
			`SELECT id FROM mirror_decisions WHERE txid = ? AND target_branch = ?`,
			d.TxID, d.TargetBranch)

		switch scanErr := row.Scan(&existingID); {
		case scanErr == nil:
			id, existed = existingID, true
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO mirror_decisions
				   (txid, source_branch, target_branch, channel_id, shape, reason,
				    state, attempts, first_seen_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				d.TxID, d.SourceBranch, d.TargetBranch, nullID(d.ChannelID),
				d.Shape, d.Reason, d.State, d.Attempts, d.FirstSeenAt, d.UpdatedAt)
			if e != nil {
				return fmt.Errorf("recording the mirror decision for %s: %w", d.TxID, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new mirror decision id: %w", e)
			}
			id, existed = newID, false
			return nil

		default:
			return fmt.Errorf("looking up the mirror decision for %s: %w", d.TxID, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, existed, nil
}

// UpdateMirrorState records the outcome of a broadcast attempt.
//
// The attempt counter is incremented here rather than passed in, so a caller
// cannot lose count by reading, deciding and writing while another pass does the
// same. `lastError` is cleared on success rather than left to linger, because a
// stale error beside an accepted transaction reads as a live problem.
func (s *Store) UpdateMirrorState(
	ctx context.Context, id int64, state MirrorState, lastError string, at int64,
) error {
	if !state.Valid() {
		return fmt.Errorf("store: %q is not a mirror state this schema accepts", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE mirror_decisions
		    SET state = ?, last_error = ?, attempts = attempts + 1, updated_at = ?
		  WHERE id = ?`,
		state, nullString(lastError), at, id)
	if err != nil {
		return fmt.Errorf("recording a mirror attempt: %w", err)
	}
	return requireOneRow(res, "mirror decision", id)
}

// MirrorFilter narrows ListMirrorDecisions.
type MirrorFilter struct {
	// State, when set, restricts to decisions in that state.
	State MirrorState
	// ChannelID, when set, restricts to one channel.
	ChannelID int64
	// TargetBranch, when set, restricts to one direction.
	TargetBranch Branch
	// Limit bounds the result. Zero means the default, because an unbounded read
	// of a table that grows with chain activity is a memory problem waiting for a
	// busy day.
	Limit int
}

// DefaultMirrorLimit bounds a mirror listing that did not ask for one.
const DefaultMirrorLimit = 500

// ListMirrorDecisions returns decisions newest first, because the interesting
// ones are the recent ones.
func (s *Store) ListMirrorDecisions(ctx context.Context, f MirrorFilter) ([]MirrorDecision, error) {
	// Fixed query, filters as parameters — see ListChannels for why.
	const query = `
		SELECT id, txid, source_branch, target_branch, channel_id, shape, reason,
		       state, attempts, first_seen_at, updated_at, last_error
		  FROM mirror_decisions
		 WHERE (? = '' OR state = ?)
		   AND (? = 0  OR channel_id = ?)
		   AND (? = '' OR target_branch = ?)
		 ORDER BY id DESC LIMIT ?`

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultMirrorLimit
	}

	rows, err := s.db.QueryContext(ctx, query,
		f.State, f.State,
		f.ChannelID, f.ChannelID,
		f.TargetBranch, f.TargetBranch,
		limit)
	if err != nil {
		return nil, fmt.Errorf("listing mirror decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MirrorDecision
	for rows.Next() {
		var (
			d         MirrorDecision
			channelID sql.NullInt64
			lastErr   sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.TxID, &d.SourceBranch, &d.TargetBranch,
			&channelID, &d.Shape, &d.Reason, &d.State, &d.Attempts,
			&d.FirstSeenAt, &d.UpdatedAt, &lastErr); err != nil {
			return nil, fmt.Errorf("reading a mirror decision: %w", err)
		}
		d.ChannelID = channelID.Int64
		d.LastError = lastErr.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing mirror decisions: %w", err)
	}
	return out, nil
}
