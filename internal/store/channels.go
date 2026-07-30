package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// LNImpl is which Lightning implementation a node runs.
type LNImpl string

// The implementations Forktower can read from.
const (
	ImplLND LNImpl = "lnd"
	ImplCLN LNImpl = "cln"
)

// Valid reports whether i is an implementation this schema accepts.
func (i LNImpl) Valid() bool {
	switch i {
	case ImplLND, ImplCLN:
		return true
	default:
		return false
	}
}

// ChanType is how a channel's commitment is constructed, which decides what its
// close transactions look like.
type ChanType string

// Channel commitment types.
const (
	ChanLegacy       ChanType = "legacy"
	ChanStaticRemote ChanType = "static_remote"
	ChanAnchors      ChanType = "anchors"
	ChanTaproot      ChanType = "taproot"
	ChanTypeUnknown  ChanType = "unknown"
)

// Valid reports whether t is a type this schema accepts.
func (t ChanType) Valid() bool {
	switch t {
	case ChanLegacy, ChanStaticRemote, ChanAnchors, ChanTaproot, ChanTypeUnknown:
		return true
	default:
		return false
	}
}

// CloseState is how far a channel has got towards being closed on the chain the
// user's own node follows.
type CloseState string

// Channel close states.
const (
	CloseOpen    CloseState = "open"
	ClosePending CloseState = "pending_close"
	CloseCoop    CloseState = "coop_closed"
	CloseForce   CloseState = "force_closed"
	CloseBreach  CloseState = "breach_closed"
)

// Valid reports whether c is a state this schema accepts.
func (c CloseState) Valid() bool {
	switch c {
	case CloseOpen, ClosePending, CloseCoop, CloseForce, CloseBreach:
		return true
	default:
		return false
	}
}

// Relevance is whether a channel is exposed on the chain the user's node does
// not follow.
type Relevance string

// Relevance values. `unknown` is a watching instruction rather than a shrug: the
// watchset is never narrowed to `relevant`, because a channel we could not
// classify is exactly the one an attacker would choose.
const (
	Relevant         Relevance = "relevant"
	Irrelevant       Relevance = "irrelevant"
	RelevanceUnknown Relevance = "unknown"
)

// Valid reports whether r is a value this schema accepts.
func (r Relevance) Valid() bool {
	switch r {
	case Relevant, Irrelevant, RelevanceUnknown:
		return true
	default:
		return false
	}
}

// LNNode is a Lightning node Forktower reads from.
type LNNode struct {
	ID         string // node pubkey, lowercase hex
	Impl       LNImpl
	Alias      string
	LastSeenAt int64
}

// Channel is one of the user's channels.
//
// Identified by its funding outpoint, because that is the only identifier that
// survives everything else about it changing.
type Channel struct {
	ID               int64
	LNNodeID         string
	FundingTxID      string
	FundingVout      int32
	FundingScriptHex string
	CapacitySat      int64
	ChanType         ChanType

	// CSVDelayLocal is what *we* wait after our own force-close.
	// CSVDelayRemote is what the *peer* waits after theirs — which makes it the
	// window we have to answer a breach against us. Nil means the source did not
	// say; a deadline is still created, from a conservative floor.
	CSVDelayLocal  *int32
	CSVDelayRemote *int32

	PeerPubkey string
	// PeerAlias is chosen by the counterparty, who is the adversary here. Clamped
	// on the way in by UpsertChannel.
	PeerAlias  string
	OpenHeight int32
	SCID       string

	CloseState  CloseState
	CloseTxID   string
	CloseHeight int32

	Relevance       Relevance
	RelevanceReason string

	UpdatedAt int64
}

// MaxPeerAliasBytes bounds a counterparty-chosen name.
const MaxPeerAliasBytes = 32

// cleanAlias makes a counterparty's chosen name safe to store and to show.
//
// Clamped here, on ingest, rather than at each of the places that display it:
// there is one way in and several ways out, and the one that gets forgotten is
// always an output. Control characters go, and the result is cut to 32 bytes on
// a rune boundary — a half-written rune in the database is a second problem to
// debug on top of whatever the first one was.
func cleanAlias(alias string) string {
	var b strings.Builder
	for _, r := range alias {
		if r == unicode.ReplacementChar || !unicode.IsGraphic(r) {
			continue
		}
		if b.Len()+len(string(r)) > MaxPeerAliasBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// UpsertLNNode records a Lightning node, or updates what is known about it.
func (s *Store) UpsertLNNode(ctx context.Context, n LNNode) error {
	if n.ID == "" {
		return errors.New("store: a Lightning node needs its pubkey")
	}
	if !n.Impl.Valid() {
		return fmt.Errorf("store: %q is not a Lightning implementation this build reads", n.Impl)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ln_nodes (id, impl, alias, last_seen_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   impl = excluded.impl,
		   alias = excluded.alias,
		   last_seen_at = excluded.last_seen_at`,
		n.ID, n.Impl, nullString(cleanAlias(n.Alias)), n.LastSeenAt)
	if err != nil {
		return fmt.Errorf("recording Lightning node %s: %w", n.ID, err)
	}
	return nil
}

// UpsertChannel records a channel, and reports whether anything about it
// actually changed.
//
// The `changed` flag is what stops the registry announcing a channel on every
// poll: with a sixty-second cycle and a handful of channels, emitting
// unconditionally would bury a real change in a stream of identical events, and
// the timeline is meant to be readable afterwards.
//
// Deliberately does not touch the close state or the relevance: those are
// decided by the watcher and the classifier respectively, from evidence the
// Lightning source does not have, and a poll must not overwrite them.
func (s *Store) UpsertChannel(ctx context.Context, c Channel) (id int64, changed bool, err error) {
	if c.FundingTxID == "" {
		return 0, false, errors.New("store: a channel needs its funding transaction")
	}
	if c.LNNodeID == "" {
		return 0, false, errors.New("store: a channel needs the node it belongs to")
	}
	if !c.ChanType.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a channel type this schema accepts", c.ChanType)
	}
	c.PeerAlias = cleanAlias(c.PeerAlias)

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			existingID int64
			same       bool
		)
		row := tx.QueryRowContext(ctx,
			`SELECT id,
			        funding_script_hex IS ? AND capacity_sat = ? AND chan_type = ?
			        AND csv_delay_local IS ? AND csv_delay_remote IS ?
			        AND peer_pubkey IS ? AND peer_alias IS ?
			        AND open_height IS ? AND scid IS ?
			   FROM channels WHERE funding_txid = ? AND funding_vout = ?`,
			nullString(c.FundingScriptHex), c.CapacitySat, c.ChanType,
			nullOptionalInt32(c.CSVDelayLocal), nullOptionalInt32(c.CSVDelayRemote),
			nullString(c.PeerPubkey), nullString(c.PeerAlias),
			nullInt32(c.OpenHeight), nullString(c.SCID),
			c.FundingTxID, c.FundingVout)

		switch scanErr := row.Scan(&existingID, &same); {
		case scanErr == nil:
			id = existingID
			if same {
				// Nothing to write and nothing to announce. Still not an error:
				// polling an unchanged channel is the ordinary case.
				changed = false
				return nil
			}
			if _, e := tx.ExecContext(ctx,
				`UPDATE channels SET
				   ln_node_id = ?, funding_script_hex = ?, capacity_sat = ?,
				   chan_type = ?, csv_delay_local = ?, csv_delay_remote = ?,
				   peer_pubkey = ?, peer_alias = ?, open_height = ?, scid = ?,
				   updated_at = ?
				 WHERE id = ?`,
				c.LNNodeID, nullString(c.FundingScriptHex), c.CapacitySat,
				c.ChanType, nullOptionalInt32(c.CSVDelayLocal), nullOptionalInt32(c.CSVDelayRemote),
				nullString(c.PeerPubkey), nullString(c.PeerAlias),
				nullInt32(c.OpenHeight), nullString(c.SCID),
				c.UpdatedAt, existingID); e != nil {
				return fmt.Errorf("updating channel %s: %w", c.FundingTxID, e)
			}
			changed = true
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO channels
				   (ln_node_id, funding_txid, funding_vout, funding_script_hex,
				    capacity_sat, chan_type, csv_delay_local, csv_delay_remote,
				    peer_pubkey, peer_alias, open_height, scid, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.LNNodeID, c.FundingTxID, c.FundingVout, nullString(c.FundingScriptHex),
				c.CapacitySat, c.ChanType, nullOptionalInt32(c.CSVDelayLocal),
				nullOptionalInt32(c.CSVDelayRemote), nullString(c.PeerPubkey),
				nullString(c.PeerAlias), nullInt32(c.OpenHeight), nullString(c.SCID),
				c.UpdatedAt)
			if e != nil {
				return fmt.Errorf("recording channel %s: %w", c.FundingTxID, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new channel id: %w", e)
			}
			id, changed = newID, true
			return nil

		default:
			return fmt.Errorf("looking up channel %s: %w", c.FundingTxID, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, changed, nil
}

// ChannelFilter narrows ListChannels.
type ChannelFilter struct {
	LNNodeID string
	// Relevance, when set, restricts to channels with that classification.
	Relevance Relevance
	// OpenOnly restricts to channels not yet closed on the user's own chain.
	OpenOnly bool
}

// ListChannels returns channels in ascending id order, which is stable and is
// the order they were first seen.
func (s *Store) ListChannels(ctx context.Context, f ChannelFilter) ([]Channel, error) {
	// One fixed query with every filter expressed as "unset, or matching", rather
	// than a WHERE clause assembled at runtime. The fragments would have been our
	// own constants either way, but a store with no dynamic SQL anywhere is a
	// store where that question never has to be asked again.
	const query = `
		SELECT id, ln_node_id, funding_txid, funding_vout, funding_script_hex,
		       capacity_sat, chan_type, csv_delay_local, csv_delay_remote,
		       peer_pubkey, peer_alias, open_height, scid,
		       sf_close_state, sf_close_txid, sf_close_height,
		       sq_relevance, sq_relevance_reason, updated_at
		  FROM channels
		 WHERE (? = '' OR ln_node_id = ?)
		   AND (? = '' OR sq_relevance = ?)
		   AND (? = 0  OR sf_close_state = ?)
		 ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query,
		f.LNNodeID, f.LNNodeID,
		f.Relevance, f.Relevance,
		boolToInt(f.OpenOnly), CloseOpen)
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Channel
	for rows.Next() {
		c, scanErr := scanChannel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	return out, nil
}

func scanChannel(rows *sql.Rows) (Channel, error) {
	var (
		c           Channel
		script      sql.NullString
		local       sql.NullInt64
		remote      sql.NullInt64
		peerPubkey  sql.NullString
		peerAlias   sql.NullString
		openHeight  sql.NullInt64
		scid        sql.NullString
		closeTxID   sql.NullString
		closeHeight sql.NullInt64
		reason      sql.NullString
	)
	if err := rows.Scan(&c.ID, &c.LNNodeID, &c.FundingTxID, &c.FundingVout, &script,
		&c.CapacitySat, &c.ChanType, &local, &remote,
		&peerPubkey, &peerAlias, &openHeight, &scid,
		&c.CloseState, &closeTxID, &closeHeight,
		&c.Relevance, &reason, &c.UpdatedAt); err != nil {
		return Channel{}, fmt.Errorf("scanning channel: %w", err)
	}
	c.FundingScriptHex = script.String
	c.PeerPubkey = peerPubkey.String
	c.PeerAlias = peerAlias.String
	c.SCID = scid.String
	c.CloseTxID = closeTxID.String
	c.RelevanceReason = reason.String
	c.OpenHeight = heightFrom(openHeight)
	c.CloseHeight = heightFrom(closeHeight)
	if local.Valid {
		v := heightFrom(local)
		c.CSVDelayLocal = &v
	}
	if remote.Valid {
		v := heightFrom(remote)
		c.CSVDelayRemote = &v
	}
	return c, nil
}

// SetChannelCloseSF records that a channel has closed on the user's own chain.
//
// Separate from UpsertChannel because it comes from a different place — the
// watcher, from the chain — and a Lightning poll must not be able to undo it.
func (s *Store) SetChannelCloseSF(
	ctx context.Context, id int64, state CloseState, txid string, height int32, at int64,
) error {
	if !state.Valid() {
		return fmt.Errorf("store: %q is not a close state this schema accepts", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels
		    SET sf_close_state = ?, sf_close_txid = ?, sf_close_height = ?, updated_at = ?
		  WHERE id = ?`,
		state, nullString(txid), nullInt32(height), at, id)
	if err != nil {
		return fmt.Errorf("recording the close of channel %d: %w", id, err)
	}
	return requireOneRow(res, "channel", id)
}

// SetChannelRelevance records whether a channel is exposed on the other chain.
func (s *Store) SetChannelRelevance(
	ctx context.Context, id int64, r Relevance, reason string, at int64,
) error {
	if !r.Valid() {
		return fmt.Errorf("store: %q is not a relevance this schema accepts", r)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET sq_relevance = ?, sq_relevance_reason = ?, updated_at = ?
		  WHERE id = ?`,
		r, nullString(reason), at, id)
	if err != nil {
		return fmt.Errorf("recording the relevance of channel %d: %w", id, err)
	}
	return requireOneRow(res, "channel", id)
}

// HTLCSnapshot is one in-flight HTLC at a moment in time.
type HTLCSnapshot struct {
	Direction   string // 'incoming' or 'outgoing'
	AmountMsat  int64
	CLTVExpiry  int32
	PaymentHash string
}

// ReplaceHTLCSnapshot swaps a channel's in-flight HTLCs for the current set.
//
// Replaced rather than accumulated: these are a live picture, not a history, and
// an HTLC that has settled must stop producing a deadline. Done in one
// transaction so a reader never sees a channel with none.
func (s *Store) ReplaceHTLCSnapshot(
	ctx context.Context, channelID int64, takenAt int64, htlcs []HTLCSnapshot,
) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM htlc_snapshots WHERE channel_id = ?`, channelID); err != nil {
			return fmt.Errorf("clearing the HTLC snapshot for channel %d: %w", channelID, err)
		}
		for _, h := range htlcs {
			if h.Direction != "incoming" && h.Direction != "outgoing" {
				return fmt.Errorf("store: %q is not an HTLC direction", h.Direction)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO htlc_snapshots
				   (channel_id, taken_at, direction, amount_msat, cltv_expiry, payment_hash)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				channelID, takenAt, h.Direction, h.AmountMsat, h.CLTVExpiry,
				nullString(h.PaymentHash)); err != nil {
				return fmt.Errorf("recording an HTLC for channel %d: %w", channelID, err)
			}
		}
		return nil
	})
}

// ListHTLCs returns a channel's most recent in-flight HTLCs.
func (s *Store) ListHTLCs(ctx context.Context, channelID int64) ([]HTLCSnapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT direction, amount_msat, cltv_expiry, payment_hash
		   FROM htlc_snapshots WHERE channel_id = ? ORDER BY cltv_expiry ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("listing HTLCs for channel %d: %w", channelID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []HTLCSnapshot
	for rows.Next() {
		var (
			h    HTLCSnapshot
			hash sql.NullString
		)
		if err := rows.Scan(&h.Direction, &h.AmountMsat, &h.CLTVExpiry, &hash); err != nil {
			return nil, fmt.Errorf("scanning an HTLC: %w", err)
		}
		h.PaymentHash = hash.String
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing HTLCs for channel %d: %w", channelID, err)
	}
	return out, nil
}

// requireOneRow turns "updated nothing" into an error, because a silent no-op on
// an id that does not exist is how a caller comes to believe it recorded
// something it did not.
func requireOneRow(res sql.Result, what string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking the update to %s %d: %w", what, id, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, id, ErrNotFound)
	}
	return nil
}

// heightFrom converts a value read back from the database.
//
// Clamped rather than truncated. These are heights we wrote as int32, so a value
// outside the range means the row is corrupt — and a plain conversion would wrap
// it to a negative number, which reads as "long ago" and would make a deadline
// look already passed. Clamping to the maximum errs the other way: a deadline
// impossibly far off is noticed by a person, one that silently expired is not.
func heightFrom(v sql.NullInt64) int32 {
	switch {
	case !v.Valid:
		return 0
	case v.Int64 > math.MaxInt32:
		return math.MaxInt32
	case v.Int64 < math.MinInt32:
		return math.MinInt32
	default:
		return int32(v.Int64)
	}
}

// nullOptionalInt32 distinguishes "the source did not say" from "the source said
// zero". For a CSV delay those are different facts: an unknown delay produces a
// deadline from a conservative floor, and a delay of zero would produce one that
// has already passed.
func nullOptionalInt32(v *int32) any {
	if v == nil {
		return nil
	}
	return *v
}
