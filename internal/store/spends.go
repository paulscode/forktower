package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SpendShape is what a transaction spending a watched outpoint turns out to be.
type SpendShape string

// Spend shapes, from doc 05 §2's classification.
//
// `commitment_unknown` is the one that matters most: a commitment we cannot
// prove is either ours or revoked. It is treated as hostile until shown
// otherwise, because the alternative is treating a breach as routine.
const (
	ShapeMutualClose       SpendShape = "mutual_close"
	ShapeCommitmentOurs    SpendShape = "commitment_ours"
	ShapeCommitmentUnknown SpendShape = "commitment_unknown"
	ShapeCommitmentRevoked SpendShape = "commitment_revoked"
	ShapeJustice           SpendShape = "justice"
	ShapeDelayedSweep      SpendShape = "delayed_sweep"
	ShapeHTLCClaim         SpendShape = "htlc_claim"
	ShapeUnknown           SpendShape = "unknown"
)

// Valid reports whether sh is a shape this schema accepts.
func (sh SpendShape) Valid() bool {
	switch sh {
	case ShapeMutualClose, ShapeCommitmentOurs, ShapeCommitmentUnknown,
		ShapeCommitmentRevoked, ShapeJustice, ShapeDelayedSweep,
		ShapeHTLCClaim, ShapeUnknown:
		return true
	default:
		return false
	}
}

// SpendStatus is how firmly a spend has happened.
type SpendStatus string

// Spend statuses.
const (
	// SpendMempool is an unconfirmed sighting. It buys the user time; it is not
	// yet a fact about the chain.
	SpendMempool SpendStatus = "mempool"
	// SpendConfirmed is in a block.
	SpendConfirmed SpendStatus = "confirmed"
	// SpendReorgedOut was in a block that is no longer on the chain. Kept rather
	// than deleted: it happened, and the record of it is the audit trail.
	SpendReorgedOut SpendStatus = "reorged_out"
)

// Valid reports whether st is a status this schema accepts.
func (st SpendStatus) Valid() bool {
	switch st {
	case SpendMempool, SpendConfirmed, SpendReorgedOut:
		return true
	default:
		return false
	}
}

// OutpointRole is what an output of a confirmed commitment is for.
type OutpointRole string

// Outpoint roles.
const (
	RoleToLocal  OutpointRole = "to_local"
	RoleToRemote OutpointRole = "to_remote"
	RoleHTLC     OutpointRole = "htlc"
	RoleAnchor   OutpointRole = "anchor"
	RoleUnknown  OutpointRole = "unknown"
)

// Valid reports whether r is a role this schema accepts.
func (r OutpointRole) Valid() bool {
	switch r {
	case RoleToLocal, RoleToRemote, RoleHTLC, RoleAnchor, RoleUnknown:
		return true
	default:
		return false
	}
}

// Spend is a transaction spending an outpoint Forktower was watching.
type Spend struct {
	ID     int64
	Branch Branch
	// ChannelID is zero for a second-order watch: an output of a commitment
	// already seen, which belongs to no channel of ours directly.
	ChannelID    int64
	OutpointTxID string
	OutpointVout int32
	SpendTxID    string
	// SpendTxHex is the whole transaction. Kept because the mirror needs to
	// rebroadcast it later, and because a spend seen once on a chain nobody else
	// is watching may not be fetchable again.
	SpendTxHex  string
	BlockHash   string
	BlockHeight int32
	Shape       SpendShape
	Status      SpendStatus
	FirstSeenAt int64
	UpdatedAt   int64
}

// RecordSpend stores a spend, reporting whether it was already known.
//
// Idempotent on (branch, outpoint, spending txid), which is what makes replaying
// a block safe: the watcher re-scans from the fork point after a reorg, and
// every one of those blocks must produce zero new rows the second time.
//
// An existing row is *not* overwritten. What is already there was written when
// the spend was first seen, and later passes know no more about it than the
// first did — status and shape have their own methods, called when there is
// actually something new to say.
func (s *Store) RecordSpend(ctx context.Context, sp Spend) (id int64, existed bool, err error) {
	if !sp.Branch.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a branch", sp.Branch)
	}
	if sp.OutpointTxID == "" || sp.SpendTxID == "" {
		return 0, false, errors.New("store: a spend needs both an outpoint and a spending transaction")
	}
	if sp.SpendTxHex == "" {
		return 0, false, errors.New("store: a spend needs the raw transaction, which the mirror will need later")
	}
	if !sp.Status.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a spend status", sp.Status)
	}
	if sp.Shape == "" {
		sp.Shape = ShapeUnknown
	}
	if !sp.Shape.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a spend shape", sp.Shape)
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var existingID int64
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id FROM spend_events
			  WHERE branch = ? AND outpoint_txid = ? AND outpoint_vout = ? AND spend_txid = ?`,
			sp.Branch, sp.OutpointTxID, sp.OutpointVout, sp.SpendTxID).Scan(&existingID)

		switch {
		case scanErr == nil:
			id, existed = existingID, true
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO spend_events
				   (branch, channel_id, outpoint_txid, outpoint_vout, spend_txid,
				    spend_tx_hex, block_hash, block_height, shape, status,
				    first_seen_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sp.Branch, nullID(sp.ChannelID), sp.OutpointTxID, sp.OutpointVout,
				sp.SpendTxID, sp.SpendTxHex, nullString(sp.BlockHash),
				nullInt32(sp.BlockHeight), sp.Shape, sp.Status,
				sp.FirstSeenAt, sp.UpdatedAt)
			if e != nil {
				return fmt.Errorf("recording the spend of %s:%d: %w",
					sp.OutpointTxID, sp.OutpointVout, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new spend id: %w", e)
			}
			id, existed = newID, false
			return nil

		default:
			return fmt.Errorf("looking up the spend of %s:%d: %w",
				sp.OutpointTxID, sp.OutpointVout, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, existed, nil
}

// UpdateSpendStatus moves a spend between mempool, confirmed and reorged-out.
func (s *Store) UpdateSpendStatus(
	ctx context.Context, id int64, status SpendStatus, blockHash string, height int32, at int64,
) error {
	if !status.Valid() {
		return fmt.Errorf("store: %q is not a spend status", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE spend_events SET status = ?, block_hash = ?, block_height = ?, updated_at = ?
		  WHERE id = ?`,
		status, nullString(blockHash), nullInt32(height), at, id)
	if err != nil {
		return fmt.Errorf("updating spend %d: %w", id, err)
	}
	return requireOneRow(res, "spend", id)
}

// UpdateSpendShape records what a spend turned out to be.
//
// Separate from recording the spend because classification needs the channel's
// own data and sometimes a second transaction, and neither is available at the
// moment the spend is first seen. Watching must not wait for classification.
func (s *Store) UpdateSpendShape(ctx context.Context, id int64, shape SpendShape, at int64) error {
	if !shape.Valid() {
		return fmt.Errorf("store: %q is not a spend shape", shape)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE spend_events SET shape = ?, updated_at = ? WHERE id = ?`, shape, at, id)
	if err != nil {
		return fmt.Errorf("classifying spend %d: %w", id, err)
	}
	return requireOneRow(res, "spend", id)
}

// GetSpend reads one spend by id.
func (s *Store) GetSpend(ctx context.Context, id int64) (Spend, error) {
	var (
		sp          Spend
		channelID   sql.NullInt64
		blockHash   sql.NullString
		blockHeight sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, branch, channel_id, outpoint_txid, outpoint_vout, spend_txid,
		        spend_tx_hex, block_hash, block_height, shape, status,
		        first_seen_at, updated_at
		   FROM spend_events WHERE id = ?`, id).
		Scan(&sp.ID, &sp.Branch, &channelID, &sp.OutpointTxID, &sp.OutpointVout,
			&sp.SpendTxID, &sp.SpendTxHex, &blockHash, &blockHeight, &sp.Shape,
			&sp.Status, &sp.FirstSeenAt, &sp.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Spend{}, fmt.Errorf("spend %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Spend{}, fmt.Errorf("reading spend %d: %w", id, err)
	}
	sp.ChannelID = channelID.Int64
	sp.BlockHash = blockHash.String
	sp.BlockHeight = heightFrom(blockHeight)
	return sp, nil
}

// SpendFilter narrows ListSpends.
type SpendFilter struct {
	Branch    Branch
	ChannelID int64
	Status    SpendStatus
	Shape     SpendShape
	Limit     int
}

// Spend listing bounds.
const (
	DefaultSpendLimit = 200
	MaxSpendLimit     = 1000
)

// ListSpends returns spends in ascending id order, which is both stable and the
// order they were first seen.
func (s *Store) ListSpends(ctx context.Context, f SpendFilter) ([]Spend, error) {
	// Fixed query, filters as parameters — see ListChannels for why.
	const query = `
		SELECT id, branch, channel_id, outpoint_txid, outpoint_vout, spend_txid,
		       spend_tx_hex, block_hash, block_height, shape, status,
		       first_seen_at, updated_at
		  FROM spend_events
		 WHERE (? = '' OR branch = ?)
		   AND (? = 0  OR channel_id = ?)
		   AND (? = '' OR status = ?)
		   AND (? = '' OR shape = ?)
		 ORDER BY id ASC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query,
		f.Branch, f.Branch,
		f.ChannelID, f.ChannelID,
		f.Status, f.Status,
		f.Shape, f.Shape,
		clampLimit(f.Limit, DefaultSpendLimit, MaxSpendLimit))
	if err != nil {
		return nil, fmt.Errorf("listing spends: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Spend
	for rows.Next() {
		var (
			sp        Spend
			channelID sql.NullInt64
			blockHash sql.NullString
			height    sql.NullInt64
		)
		if err := rows.Scan(&sp.ID, &sp.Branch, &channelID, &sp.OutpointTxID,
			&sp.OutpointVout, &sp.SpendTxID, &sp.SpendTxHex, &blockHash, &height,
			&sp.Shape, &sp.Status, &sp.FirstSeenAt, &sp.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning a spend: %w", err)
		}
		sp.ChannelID = channelID.Int64
		sp.BlockHash = blockHash.String
		sp.BlockHeight = heightFrom(height)
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing spends: %w", err)
	}
	return out, nil
}

// WatchOutpoint is an output of a confirmed commitment that now needs watching
// in its own right.
type WatchOutpoint struct {
	Branch    Branch
	TxID      string
	Vout      int32
	ScriptHex string
	// SourceSpendEventID is the spend that produced this output, which is how a
	// reorg that removes the commitment also removes what it created.
	SourceSpendEventID int64
	Role               OutpointRole
}

// AddWatchOutpoint records an output to watch. Idempotent: re-scanning a block
// after a reorg must add nothing.
func (s *Store) AddWatchOutpoint(ctx context.Context, w WatchOutpoint) error {
	if !w.Branch.Valid() {
		return fmt.Errorf("store: %q is not a branch", w.Branch)
	}
	if w.ScriptHex == "" {
		return errors.New("store: an outpoint to watch needs its script, which is what the scan matches on")
	}
	if !w.Role.Valid() {
		return fmt.Errorf("store: %q is not an outpoint role", w.Role)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO watch_outpoints (branch, txid, vout, script_hex, source_spend_event_id, role)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(branch, txid, vout) DO NOTHING`,
		w.Branch, w.TxID, w.Vout, w.ScriptHex, w.SourceSpendEventID, w.Role)
	if err != nil {
		return fmt.Errorf("recording the outpoint %s:%d to watch: %w", w.TxID, w.Vout, err)
	}
	return nil
}

// ListWatchOutpoints returns everything being watched on a branch beyond the
// channels themselves.
func (s *Store) ListWatchOutpoints(ctx context.Context, branch Branch) ([]WatchOutpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT branch, txid, vout, script_hex, source_spend_event_id, role
		   FROM watch_outpoints WHERE branch = ? ORDER BY txid ASC, vout ASC`, branch)
	if err != nil {
		return nil, fmt.Errorf("listing watched outpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WatchOutpoint
	for rows.Next() {
		var w WatchOutpoint
		if err := rows.Scan(&w.Branch, &w.TxID, &w.Vout, &w.ScriptHex,
			&w.SourceSpendEventID, &w.Role); err != nil {
			return nil, fmt.Errorf("scanning a watched outpoint: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing watched outpoints: %w", err)
	}
	return out, nil
}

// nullID treats zero as absent, for the optional channel reference.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
