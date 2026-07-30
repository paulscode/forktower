package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Branch identifies which of the two chains a record belongs to.
//
// Named by role, not by merit: "sf" is the chain the user's own node follows and
// "sq" is the other one. Nothing here ranks them, and the daemon takes no
// position on which is legitimate.
type Branch string

// The two chains.
const (
	// BranchSF is the chain the user's own Bitcoin node follows.
	BranchSF Branch = "sf"
	// BranchSQ is the chain it does not — the one we watch on their behalf.
	BranchSQ Branch = "sq"
)

// Valid reports whether b is one of the two chains.
func (b Branch) Valid() bool {
	switch b {
	case BranchSF, BranchSQ:
		return true
	default:
		return false
	}
}

// SplitState is how far apart the two chains are known to be.
type SplitState string

// Split states. Reaching a resolved state is an operator's judgement, never the
// daemon's: it observes divergence, it does not adjudicate outcomes.
const (
	// StateUnarmed means the daemon does not yet have two healthy views to
	// compare.
	StateUnarmed SplitState = "UNARMED"
	// StateArmed means both views are healthy and agree.
	StateArmed SplitState = "ARMED"
	// StateSplit means they have persistently diverged.
	StateSplit SplitState = "SPLIT"
	// StateResolving means one chain appears to have stopped advancing.
	StateResolving SplitState = "RESOLVING"
	// StateResolvedSFWon and StateResolvedSQWon are set only by an operator
	// confirming what happened.
	StateResolvedSFWon SplitState = "RESOLVED_SF_WON"
	StateResolvedSQWon SplitState = "RESOLVED_SQ_WON"
)

// Valid reports whether s is a known state.
func (s SplitState) Valid() bool {
	switch s {
	case StateUnarmed, StateArmed, StateSplit, StateResolving,
		StateResolvedSFWon, StateResolvedSQWon:
		return true
	default:
		return false
	}
}

// Split is the persisted record of the divergence.
type Split struct {
	State SplitState
	// ForkHeight and ForkHash locate where the chains separated. Zero and empty
	// until a split is recorded. Once set they are the anchor for rescans and for
	// deciding which channels are exposed, so they are not revised casually.
	ForkHeight int32
	ForkHash   string
	DetectedAt int64
	UpdatedAt  int64
}

// ForkKnown reports whether a separation point has been recorded.
func (s Split) ForkKnown() bool { return s.ForkHash != "" }

// GetSplitState reads the singleton split record. The row is created by the
// initial migration, so this never reports ErrNotFound on a migrated database.
func (s *Store) GetSplitState(ctx context.Context) (Split, error) {
	var (
		out        Split
		forkHeight sql.NullInt64
		forkHash   sql.NullString
		detectedAt sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT state, fork_height, fork_hash, detected_at, updated_at
		 FROM split_state WHERE id = 1`).
		Scan(&out.State, &forkHeight, &forkHash, &detectedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Split{}, fmt.Errorf("split state row: %w", ErrNotFound)
	}
	if err != nil {
		return Split{}, fmt.Errorf("reading split state: %w", err)
	}
	if forkHeight.Valid {
		h, err := int32FromDB("split_state.fork_height", forkHeight.Int64)
		if err != nil {
			return Split{}, err
		}
		out.ForkHeight = h
	}
	out.ForkHash = forkHash.String
	out.DetectedAt = detectedAt.Int64
	return out, nil
}

// SaveSplitState replaces the singleton split record.
func (s *Store) SaveSplitState(ctx context.Context, sp Split) error {
	if !sp.State.Valid() {
		return fmt.Errorf("store: split state %q is not a known state", sp.State)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE split_state
		 SET state = ?, fork_height = ?, fork_hash = ?, detected_at = ?, updated_at = ?
		 WHERE id = 1`,
		sp.State, nullInt32(sp.ForkHeight), nullString(sp.ForkHash),
		nullInt64(sp.DetectedAt), sp.UpdatedAt)
	if err != nil {
		return fmt.Errorf("saving split state: %w", err)
	}
	return nil
}

// BranchBlock is one observed block on one chain.
type BranchBlock struct {
	Branch     Branch
	Height     int32
	Hash       string
	PrevHash   string
	BlockTime  int64
	ReceivedAt int64
}

// BranchBlockRetention is how many recent blocks are kept per chain.
//
// This table is a cache of recent tips used for cadence and for recognising a
// reorganisation, not part of the audit trail, so old rows are pruned. The window
// is comfortably larger than the deepest reorganisation the watcher will follow
// before treating it as a branch-identity problem instead.
const BranchBlockRetention = 500

// RecordBranchBlock stores a block and prunes the chain's history to the
// retention window.
//
// Recording the same block twice is a no-op: the daemon re-reads recent blocks
// after a restart, and that must not multiply rows.
func (s *Store) RecordBranchBlock(ctx context.Context, b BranchBlock) error {
	if !b.Branch.Valid() {
		return fmt.Errorf("store: branch %q is not \"sf\" or \"sq\"", b.Branch)
	}
	if b.Hash == "" {
		return errors.New("store: branch block needs a hash")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO branch_blocks (branch, height, hash, prev_hash, block_time, received_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (branch, hash) DO NOTHING`,
			b.Branch, b.Height, b.Hash, b.PrevHash, b.BlockTime, b.ReceivedAt); err != nil {
			return fmt.Errorf("recording %s block %s: %w", b.Branch, b.Hash, err)
		}

		// Keep the newest by height. Ties on height (competing blocks at the same
		// point) are broken by hash so the choice is deterministic rather than
		// dependent on insertion order.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM branch_blocks
			 WHERE branch = ?
			   AND hash NOT IN (
			     SELECT hash FROM branch_blocks
			     WHERE branch = ?
			     ORDER BY height DESC, hash DESC
			     LIMIT ?
			   )`, b.Branch, b.Branch, BranchBlockRetention); err != nil {
			return fmt.Errorf("pruning %s blocks: %w", b.Branch, err)
		}
		return nil
	})
}

// RecentBranchHashes returns up to n of the most recently seen block hashes on a
// chain, newest first.
//
// Used to recognise where a reorganisation reattaches: the watcher walks a new
// chain backwards until it reaches a hash it has already processed.
func (s *Store) RecentBranchHashes(ctx context.Context, branch Branch, n int) ([]string, error) {
	if !branch.Valid() {
		return nil, fmt.Errorf("store: branch %q is not \"sf\" or \"sq\"", branch)
	}
	if n <= 0 {
		n = BranchBlockRetention
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM branch_blocks WHERE branch = ?
		 ORDER BY height DESC, hash DESC LIMIT ?`, branch, n)
	if err != nil {
		return nil, fmt.Errorf("reading recent %s hashes: %w", branch, err)
	}
	// Nothing new to learn from Close once rows.Err() has been checked below.
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scanning %s hash: %w", branch, err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recent %s hashes: %w", branch, err)
	}
	return out, nil
}

// CountBranchBlocks reports how many blocks are retained for a chain.
func (s *Store) CountBranchBlocks(ctx context.Context, branch Branch) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_blocks WHERE branch = ?`, branch).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting %s blocks: %w", branch, err)
	}
	return n, nil
}

func nullInt32(v int32) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
