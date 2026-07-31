package watcher

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strconv"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// CommitmentOutputs lists what a confirmed commitment created, so that each can
// be watched in its own right.
//
// This is what lets Forktower report an *outcome* rather than only a threat. A
// commitment appearing on the other chain says somebody may be taking money; the
// spends of its outputs say whether they succeeded, whether a justice
// transaction landed, and whether a delay ran out unanswered. Without this half
// the user is told something is happening and never told how it ended.
//
// Roles are best-effort by design. An anchor is unmistakable — nothing else in a
// commitment is 330 satoshis — but the contested output, the counterparty's
// output and the payments in flight are all the same kind of script, and telling
// them apart from the outside needs keys we do not have. Rather than guess, the
// rest are recorded as unknown and settled later by what spends them: the
// spending witness says which branch of the script was taken, which is a fact
// rather than an inference.
func CommitmentOutputs(
	txid chainhash.Hash, tx *wire.MsgTx, branch store.Branch, sourceSpendID int64,
) []store.WatchOutpoint {
	if tx == nil {
		return nil
	}

	out := make([]store.WatchOutpoint, 0, len(tx.TxOut))
	for i, o := range tx.TxOut {
		if len(o.PkScript) == 0 {
			// Nothing to watch and nothing to match on. A commitment does not
			// produce these, but a transaction we misread as one might.
			continue
		}
		out = append(out, store.WatchOutpoint{
			Branch:             branch,
			TxID:               txid.String(),
			Vout:               int32(i),
			ScriptHex:          hex.EncodeToString(o.PkScript),
			SourceSpendEventID: sourceSpendID,
			Role:               roleOf(o),
		})
	}
	return out
}

// roleOf names an output as far as it honestly can.
func roleOf(out *wire.TxOut) store.OutpointRole {
	if out.Value == AnchorValueSat {
		return store.RoleAnchor
	}
	return store.RoleUnknown
}

// watchCommitmentOutputs records what a commitment created.
//
// Idempotent, because it runs again on every re-scan of the same block: after a
// reorganisation, after a restart mid-block, and on the second pass the scanner
// makes when it found something new. Adding an outpoint that is already watched
// changes nothing.
func (w *Watcher) watchCommitmentOutputs(
	ctx context.Context, m Match, spendID int64,
) {
	outputs := CommitmentOutputs(m.TxID, m.Tx, w.branch, spendID)
	if len(outputs) == 0 {
		return
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	var added int
	for _, o := range outputs {
		if err := w.store.AddWatchOutpoint(wctx, o); err != nil {
			w.log.Error("could not start watching what a channel close created",
				slog.String("outpoint", o.TxID+":"+strconv.Itoa(int(o.Vout))),
				slog.String("error", err.Error()))
			continue
		}
		added++
	}
	if added > 0 {
		w.log.Info("now watching what a channel close created on the other chain",
			slog.Int("outputs", added), slog.Int64("spend_id", spendID))
	}
}
