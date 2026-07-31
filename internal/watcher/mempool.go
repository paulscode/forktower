package watcher

import (
	"context"
	"errors"
	"log/slog"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// subscribeMempool starts listening for unconfirmed transactions, if the backend
// has any to offer.
//
// A backend with no view of the memory pool is an absence, not a failure: a
// light client genuinely cannot see one, and a daemon that refused to start
// because of it would be refusing to do the ninety per cent of its job that does
// not need it. The channel comes back nil in that case, and a nil channel in a
// select simply never fires.
func (w *Watcher) subscribeMempool(ctx context.Context) <-chan *wire.MsgTx {
	txs, err := w.view.SubscribeMempoolTx(ctx)
	switch {
	case err == nil:
		w.log.Info("watching for channel closes before they are confirmed")
		return txs

	case errors.Is(err, chainview.ErrUnsupported):
		w.log.Info("this backend cannot see unconfirmed transactions, so closes on " +
			"the other chain will be noticed when they are mined rather than before")
		return nil

	default:
		if stopping(ctx) {
			return nil
		}
		// Worth saying, and worth continuing without. Losing the early warning
		// costs the user a block of notice; losing the block scanning would cost
		// them the protection.
		w.log.Warn("could not watch for unconfirmed transactions on the other chain, "+
			"so closes will be noticed when they are mined",
			slog.String("error", err.Error()))
		return nil
	}
}

// handleMempoolTx records a spend of something watched, seen before any block
// contains it.
//
// Recorded as a sighting rather than as a fact. An unconfirmed transaction may
// be replaced, may never confirm, and may not be what the rest of the network is
// seeing — so it goes in with a status that says so, and the block that confirms
// it later updates that same record rather than creating a second one.
func (w *Watcher) handleMempoolTx(ctx context.Context, tx *wire.MsgTx) {
	if w.guard != nil && w.guard.Paused() {
		// Same reason nothing is scanned while the daemon is unsure which chain it
		// is looking at: a sighting from the wrong chain is worse than none.
		return
	}

	matches := ScanTx(tx, w.WatchSet())
	if len(matches) == 0 {
		return
	}

	for _, m := range matches {
		vout, inRange := toInt32(int64(m.Target.Outpoint.Index))
		if !inRange {
			continue
		}
		shape := w.shapeOf(ctx, m, chainview.BlockMeta{})

		wctx, cancel := writeCtx(ctx)
		id, existed, err := w.store.RecordSpend(wctx, store.Spend{
			Branch:       w.branch,
			ChannelID:    m.Target.ChannelID,
			OutpointTxID: m.Target.Outpoint.Hash.String(),
			OutpointVout: vout,
			SpendTxID:    m.TxID.String(),
			SpendTxHex:   rawTx(m.Tx),
			Shape:        shape,
			Status:       store.SpendMempool,
			FirstSeenAt:  w.now().Unix(),
			UpdatedAt:    w.now().Unix(),
		})
		cancel()

		if err != nil {
			w.log.Error("could not record an unconfirmed spend",
				slog.String("error", err.Error()))
			continue
		}
		if existed {
			// Already known, either from an earlier sighting of the same
			// transaction or from a block. Neither is news.
			continue
		}

		// Deliberately not followed up with second-order watching. The outputs of
		// an unconfirmed commitment do not exist yet, and might never; watching
		// them starts when a block says they are real.
		w.log.Warn("a transaction closing one of your channels was seen on the other "+
			"chain before any block accepted it",
			slog.Int64("channel_id", m.Target.ChannelID),
			slog.String("shape", string(shape)))

		w.bus.Publish(bus.MempoolSighting{
			SpendEventID: id,
			ChannelID:    m.Target.ChannelID,
			Branch:       string(w.branch),
			SpendTxid:    m.TxID.String(),
			Shape:        string(shape),
		})
	}
}
