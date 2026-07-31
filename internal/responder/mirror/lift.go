package mirror

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
)

// Lifted is one transaction taken off a chain, with everything the policy needs
// to decide about it.
//
// **The bytes are the transaction as it was found.** Nothing here re-serialises
// from a parsed form, and nothing anywhere in this package modifies a
// transaction: what is offered to the other chain is byte-for-byte what was
// observed on this one. That is not a nicety — a transaction is only valid with
// the signatures it was built with, and anything that changed a single byte
// would produce something that could not confirm and would look like our fault.
type Lifted struct {
	TxID string
	// RawHex is the transaction exactly as it appeared, ready to be offered
	// elsewhere unchanged.
	RawHex string
	// ChannelID is the channel it belongs to, and Shape what it appears to be.
	ChannelID int64
	Shape     store.SpendShape
	// SourceShape is what the commitment this transaction spends was classified
	// as, for a second-order transaction. Empty when it spends a funding output.
	// It is what decides whose sweep or justice transaction this is.
	SourceShape store.SpendShape
	// Outpoint identifies what was spent, which is how a caller ties this back to
	// what it was watching.
	OutpointTxID string
	OutpointVout int32
}

// Facts are the pieces a caller has to supply that cannot be read off the
// transaction itself.
type Facts struct {
	// OurCloseTxID is the closing transaction the user's own node reported for
	// this channel, if it reported one. The only thing that can tell our
	// commitment from theirs.
	OurCloseTxID string
	// SourceShape is what the spent output's commitment was classified as, for a
	// second-order spend.
	SourceShape store.SpendShape
	// Role is what the spent output was thought to be.
	Role store.OutpointRole
	// SpendHeight is where this confirmed, and DeadlineHeight where a contested
	// delay expires. Both zero when not known.
	SpendHeight, DeadlineHeight int32
	// ChannelID is which channel this belongs to, for a transaction whose target
	// does not carry one.
	//
	// A second-order target is a commitment's *output*, and the watchset records
	// it against the spend that produced it rather than against a channel — so
	// for those the channel has to be resolved by the caller and handed in.
	ChannelID int64
}

// Lift turns a match against the watchset into something the policy can judge.
//
// The classification is the same code the detection side uses, deliberately: a
// second implementation would be a second place for "whose commitment is this"
// to be answered differently, and the two answers would disagree exactly when it
// mattered.
func Lift(m watcher.Match, tx *wire.MsgTx, f Facts) (Lifted, error) {
	if tx == nil {
		return Lifted{}, fmt.Errorf("mirror: no transaction to lift for %s", m.Target.Outpoint)
	}

	raw, err := rawHex(tx)
	if err != nil {
		return Lifted{}, err
	}

	channelID := m.Target.ChannelID
	if channelID == 0 {
		channelID = f.ChannelID
	}
	out := Lifted{
		TxID:         tx.TxHash().String(),
		RawHex:       raw,
		ChannelID:    channelID,
		OutpointTxID: m.Target.Outpoint.Hash.String(),
		//nolint:gosec // an output index is nowhere near a 32-bit bound
		OutpointVout: int32(m.Target.Outpoint.Index),
	}

	switch m.Target.Kind {
	case watcher.KindFunding:
		out.Shape = watcher.ClassifyShape(watcher.ShapeFacts{
			Tx: tx, TxID: out.TxID, OurCloseTxID: f.OurCloseTxID,
		})
	case watcher.KindCommitmentOutput:
		out.Shape = watcher.ClassifySweep(watcher.SweepFacts{
			Tx: tx, InputIndex: m.InputIndex, Role: f.Role,
			SpendHeight: f.SpendHeight, DeadlineHeight: f.DeadlineHeight,
		})
		out.SourceShape = f.SourceShape
	default:
		out.Shape = store.ShapeUnknown
	}

	return out, nil
}

// rawHex serialises a transaction back to the form a node accepts.
func rawHex(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", fmt.Errorf("mirror: reading the transaction's bytes: %w", err)
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// Decision turns a lifted transaction into the record of what was decided about
// it.
//
// Kept next to the lifting so that the whole journey from "a transaction was
// found" to "here is the row that will be written" reads in one place, and so
// that no caller can produce a decision without a reason attached.
func Decision(l Lifted, from, to store.Branch, fundingOptIn bool, at int64) store.MirrorDecision {
	verdict := Decide(Inputs{
		Shape:        l.Shape,
		From:         from,
		To:           to,
		ChannelKnown: l.ChannelID != 0,
		SourceShape:  l.SourceShape,
		FundingOptIn: fundingOptIn,
	})

	state := store.MirrorDenied
	if verdict.Mirror {
		state = store.MirrorPending
	}

	return store.MirrorDecision{
		TxID:         l.TxID,
		SourceBranch: from,
		TargetBranch: to,
		ChannelID:    l.ChannelID,
		Shape:        l.Shape,
		Reason:       verdict.Reason,
		State:        state,
		FirstSeenAt:  at,
		UpdatedAt:    at,
	}
}
