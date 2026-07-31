package watcher

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// catchUp handles a tip that does not follow the last block processed.
//
// Two situations arrive here and they are answered the same way, because from a
// standing start they are indistinguishable: the chain grew while nobody was
// looking, or part of it was replaced. Walking back from the new tip until
// something recognisable turns up settles which — and the answer is the same
// walk either way.
func (w *Watcher) catchUp(ctx context.Context, tip chainview.BlockMeta, last blockRef) {
	path, attach, err := w.walkBack(ctx, tip, last)
	if err != nil {
		w.stall(ctx, tip.Height, err)
		return
	}

	if attach.Height < last.Height {
		// The chain was replaced. Everything recorded above the point the two
		// branches part company is no longer on the chain, and saying so is not
		// optional: a breach that disappears has not necessarily gone away.
		w.rollBackAbove(ctx, attach.Height)
		w.log.Warn("part of the other chain was replaced",
			slog.Int("from_height", int(last.Height)),
			slog.Int("back_to_height", int(attach.Height)))
	}

	w.processTo(ctx, path)
}

// walkBack finds where the new tip's chain rejoins the one already scanned, and
// returns the blocks between — oldest first, ending at the tip.
//
// Bounded by MaxReorgDepth. Beyond that this stops being a reorganisation and
// starts being a reason to doubt that the backend is on the chain we think it
// is, which is a different problem with a different answer.
func (w *Watcher) walkBack(
	ctx context.Context, tip chainview.BlockMeta, last blockRef,
) (path []chainview.BlockMeta, attach blockRef, err error) {
	known, err := w.knownHashes(ctx, last)
	if err != nil {
		return nil, blockRef{}, err
	}

	path = []chainview.BlockMeta{tip}
	current := tip

	for range w.cfg.MaxReorgDepth {
		if height, recognised := known[current.PrevHash]; recognised {
			// Oldest first, which is the order they must be processed in: a block's
			// spends may create outputs a later block in the same walk spends.
			reverse(path)
			return path, blockRef{Height: height, Hash: current.PrevHash}, nil
		}
		if current.Height <= 1 {
			return nil, blockRef{}, errors.New(
				"walked back to the start of the chain without recognising anything")
		}

		parent, headerErr := w.view.BlockHeaderByHash(ctx, current.PrevHash)
		if headerErr != nil {
			return nil, blockRef{}, fmt.Errorf("reading the other chain's history: %w", headerErr)
		}
		path = append(path, parent)
		current = parent
	}

	w.deepReorg(ctx, tip, last)
	return nil, blockRef{}, fmt.Errorf(
		"the other chain was replaced further back than %d blocks", w.cfg.MaxReorgDepth)
}

// knownHashes is everything the walk may stop at: the last block processed, plus
// the recent history the detection engine recorded for this chain.
//
// The engine's history is included because it is the only record of blocks that
// were on this chain before a reorganisation took them off it, and stopping at
// one of those is what makes the difference between "we know where these two
// branches part company" and "we have no idea".
func (w *Watcher) knownHashes(ctx context.Context, last blockRef) (map[chainhash.Hash]int32, error) {
	out := make(map[chainhash.Hash]int32, w.cfg.MaxReorgDepth+1)
	if last.known() {
		out[last.Hash] = last.Height
	}

	recent, err := w.store.RecentBranchHashes(ctx, w.branch, int(w.cfg.MaxReorgDepth))
	if err != nil {
		return nil, fmt.Errorf("reading the other chain's recent history: %w", err)
	}
	for _, hexHash := range recent {
		h, parseErr := chainhash.NewHashFromStr(hexHash)
		if parseErr != nil {
			continue
		}
		if _, already := out[*h]; already {
			continue
		}
		// Height is unknown for these, and a wrong one would be worse than none:
		// it decides which spends get rolled back. Zero means "older than the last
		// block processed", which is all the caller needs to know.
		out[*h] = 0
	}
	return out, nil
}

// processTo processes blocks in order, stopping if one cannot be processed.
func (w *Watcher) processTo(ctx context.Context, path []chainview.BlockMeta) {
	for _, meta := range path {
		if ctx.Err() != nil {
			return
		}
		if err := w.processWithRetries(ctx, meta); err != nil {
			w.stall(ctx, meta.Height, err)
			return
		}
		w.commitMark(ctx, blockRef{Height: meta.Height, Hash: meta.Hash})
	}
	w.clearStall()
}

// processWithRetries gives one block a bounded number of attempts.
//
// Bounded rather than infinite, because the high-water mark only advances after
// a block commits: a block that fails forever freezes scanning while the daemon
// stays up and the backend looks healthy. Nobody would notice, which is why
// running out of attempts is loud.
func (w *Watcher) processWithRetries(ctx context.Context, meta chainview.BlockMeta) error {
	var lastErr error
	for attempt := 1; attempt <= w.cfg.BlockAttempts; attempt++ {
		err := w.processBlock(ctx, meta)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return err
		}
		w.log.Warn("could not read a block on the other chain, will try again",
			slog.Int("height", int(meta.Height)),
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()))

		if attempt < w.cfg.BlockAttempts {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(w.cfg.RetryDelay):
			}
		}
	}
	return lastErr
}

// processBlock scans one block and records what it finds.
//
// Scanned more than once when a pass recorded something new, because a later
// stage may have added outpoints in response: a sweep of a commitment that
// confirmed in this very block cannot be found on the first pass, since the
// output it spends did not exist until the commitment was read. The scan is
// cheap and idempotent, so looking again is the whole answer.
func (w *Watcher) processBlock(ctx context.Context, meta chainview.BlockMeta) error {
	set := w.WatchSet()
	if set.Empty() {
		// Nothing to look for. Still advances the mark: these blocks genuinely
		// contained nothing of interest, and re-reading them later would find the
		// same nothing more slowly.
		return nil
	}

	// The gate. On a full node this is always a possible match and costs one
	// call; on a light client it is what stops every block being downloaded.
	possible, err := w.view.MatchBlock(ctx, meta.Hash, set.ChainViewSet())
	if err != nil {
		return fmt.Errorf("checking block %d for anything of ours: %w", meta.Height, err)
	}
	if !possible {
		return nil
	}

	blk, err := w.view.Block(ctx, meta.Hash)
	if err != nil {
		return fmt.Errorf("reading block %d: %w", meta.Height, err)
	}

	for pass := range maxSamePasses {
		if pass > 0 {
			w.rebuild(ctx)
			set = w.WatchSet()
		}
		recorded, recErr := w.recordMatches(ctx, meta, ScanBlock(blk, set))
		if recErr != nil {
			return recErr
		}
		if recorded == 0 {
			return nil
		}
	}
	// Every pass kept finding something new, which should be impossible: each
	// recorded spend is idempotent, so a second sighting of the same one records
	// nothing. Said out loud rather than looped on.
	w.log.Error("a block kept producing new spends after being scanned repeatedly",
		slog.Int("height", int(meta.Height)))
	return nil
}

// recordMatches stores what a scan found, and returns how much of it was news.
func (w *Watcher) recordMatches(
	ctx context.Context, meta chainview.BlockMeta, matches []Match,
) (int, error) {
	var fresh int
	for _, m := range matches {
		vout, inRange := toInt32(int64(m.Target.Outpoint.Index))
		if !inRange {
			// Cannot have come from the watchset builder, which reads a 32-bit
			// column. Refused rather than wrapped, because a wrapped index is
			// negative and would name an output that does not exist.
			w.log.Error("a watched outpoint has an output index the database cannot hold",
				slog.String("outpoint", m.Target.Outpoint.String()))
			continue
		}

		wctx, cancel := writeCtx(ctx)
		id, existed, err := w.store.RecordSpend(wctx, store.Spend{
			Branch:       w.branch,
			ChannelID:    m.Target.ChannelID,
			OutpointTxID: m.Target.Outpoint.Hash.String(),
			OutpointVout: vout,
			SpendTxID:    m.TxID.String(),
			SpendTxHex:   rawTx(m.Tx),
			BlockHash:    meta.Hash.String(),
			BlockHeight:  meta.Height,
			Shape:        store.ShapeUnknown,
			Status:       store.SpendConfirmed,
			FirstSeenAt:  w.now().Unix(),
			UpdatedAt:    w.now().Unix(),
		})
		cancel()
		if err != nil {
			return fresh, fmt.Errorf("recording a spend in block %d: %w", meta.Height, err)
		}

		if existed {
			// Seen before. Its status may still need moving on — a spend first seen
			// unconfirmed, now in a block — but it is not news.
			if err := w.confirmIfPending(ctx, id, m, meta); err != nil {
				return fresh, err
			}
			continue
		}
		fresh++
		w.announce(m, id, meta)
	}
	return fresh, nil
}

// confirmIfPending moves a spend already recorded into this block.
//
// The case that matters is a spend that was reorganised out and has landed
// again on the new branch. That is the same event happening a second time, not a
// second event, so the record is updated rather than duplicated — and it is
// announced again, because a subscriber told the spend had gone needs telling it
// is back.
func (w *Watcher) confirmIfPending(
	ctx context.Context, id int64, m Match, meta chainview.BlockMeta,
) error {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	sp, err := w.store.GetSpend(wctx, id)
	if err != nil {
		return fmt.Errorf("reading a spend already recorded: %w", err)
	}
	if sp.Status == store.SpendConfirmed && sp.BlockHash == meta.Hash.String() {
		return nil
	}

	if err := w.store.UpdateSpendStatus(wctx, id, store.SpendConfirmed,
		meta.Hash.String(), meta.Height, w.now().Unix()); err != nil {
		return fmt.Errorf("confirming a spend: %w", err)
	}
	if sp.Status == store.SpendReorgedOut {
		w.log.Info("a spend that had been dropped from the other chain is back in a block",
			slog.Int64("spend_id", id), slog.Int("height", int(meta.Height)))
	}
	w.announce(m, id, meta)
	return nil
}

// announce publishes what was found, in the shape the rest of the daemon reads.
func (w *Watcher) announce(m Match, id int64, meta chainview.BlockMeta) {
	if m.Target.Kind == KindFunding {
		w.bus.Publish(bus.FundingSpent{
			SpendEventID: id,
			ChannelID:    m.Target.ChannelID,
			Branch:       string(w.branch),
			SpendTxid:    m.TxID.String(),
			Shape:        string(store.ShapeUnknown),
			Status:       string(store.SpendConfirmed),
			Height:       meta.Height,
		})
		return
	}
	w.bus.Publish(bus.SecondOrderSpent{
		SpendEventID:       id,
		SourceSpendEventID: m.Target.SourceSpendEventID,
		Role:               string(m.Target.Role),
		Shape:              string(store.ShapeUnknown),
	})
}

// rollBackAbove marks everything recorded above a height as no longer on the
// chain.
//
// Marked rather than deleted: it happened, and the record of it is the audit
// trail. And announced, because a spend disappearing is itself worth knowing —
// a counterparty replacing a breach with a higher fee looks exactly like this,
// and reading it as good news would be the wrong instinct.
func (w *Watcher) rollBackAbove(ctx context.Context, height int32) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	spends, err := w.store.ListSpends(wctx, store.SpendFilter{
		Branch: w.branch, Status: store.SpendConfirmed, Limit: store.MaxSpendLimit,
	})
	if err != nil {
		w.log.Error("could not read what had been recorded on the replaced part of "+
			"the other chain", slog.String("error", err.Error()))
		return
	}

	for _, sp := range spends {
		if sp.BlockHeight <= height {
			continue
		}
		if err := w.store.UpdateSpendStatus(wctx, sp.ID, store.SpendReorgedOut,
			sp.BlockHash, sp.BlockHeight, w.now().Unix()); err != nil {
			w.log.Error("could not record that a spend is no longer on the other chain",
				slog.Int64("spend_id", sp.ID), slog.String("error", err.Error()))
			continue
		}
		w.bus.Publish(bus.SpendReorgedOut{SpendEventID: sp.ID, Branch: string(w.branch)})
	}
}

// deepReorg handles a chain that was replaced further back than any
// reorganisation should reach.
//
// Not treated as a very large reorganisation, because at this depth the more
// likely explanation is that the backend is not on the chain we believe it is —
// and scanning the wrong chain produces a clean report about a chain nobody
// needs watched. Watching stops until the detection engine has re-verified,
// which it does on its own schedule.
func (w *Watcher) deepReorg(ctx context.Context, tip chainview.BlockMeta, last blockRef) {
	w.log.Error("the other chain was replaced further back than any reorganisation "+
		"should reach, so scanning has stopped until the daemon can confirm it is "+
		"looking at the right chain",
		slog.Int("depth_limit", int(w.cfg.MaxReorgDepth)),
		slog.Int("was_at_height", int(last.Height)),
		slog.Int("now_at_height", int(tip.Height)))

	w.raise(ctx, store.TierCritical, DeepReorgAlertKind,
		fmt.Sprintf("%s:%d", DeepReorgAlertKind, tip.Height),
		"Forktower stopped watching the other chain",
		"The other chain changed further back than a normal reorganisation reaches. "+
			"Forktower has stopped scanning it rather than risk reporting on the wrong "+
			"chain, and will start again once it has confirmed which chain that backend "+
			"is following.")

	w.mu.Lock()
	w.progress.Stalled = true
	w.progress.StalledAt = tip.Height
	w.progress.Why = "the other chain changed further back than a normal " +
		"reorganisation reaches, so scanning stopped"
	w.mu.Unlock()
}

// stall records that scanning has stopped getting anywhere, and says so.
func (w *Watcher) stall(ctx context.Context, height int32, cause error) {
	w.mu.Lock()
	already := w.progress.Stalled && w.progress.StalledAt == height
	w.progress.Stalled = true
	w.progress.StalledAt = height
	w.progress.Why = "a block on the other chain could not be read after several tries"
	w.mu.Unlock()

	if already {
		return
	}
	w.log.Error("scanning the other chain has stopped making progress",
		slog.Int("height", int(height)), slog.String("error", cause.Error()))

	w.raise(ctx, store.TierCritical, StalledAlertKind,
		fmt.Sprintf("%s:%d", StalledAlertKind, height),
		"Forktower has stopped scanning the other chain",
		"A block on the other chain could not be read after several tries, so nothing "+
			"new is being checked. Forktower is still running, which is exactly why this "+
			"needs saying: everything else will look normal.")
}

func (w *Watcher) clearStall() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.progress.Stalled {
		w.log.Info("scanning the other chain is making progress again")
	}
	w.progress.Stalled = false
	w.progress.StalledAt = 0
	w.progress.Why = ""
}

// raise records an alert. Best effort: an alert that cannot be stored is still
// worth logging, and the log line above it has already done that.
func (w *Watcher) raise(ctx context.Context, tier store.Tier, kind, dedup, subject, msg string) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	now := w.now().Unix()
	up, err := w.store.UpsertAlert(wctx, store.Alert{
		Tier: tier, Kind: kind, DedupKey: dedup, Subject: subject, Message: msg,
		CreatedAt: now, LastRaisedAt: now,
	})
	if err != nil {
		w.log.Error("could not record an alert", slog.String("error", err.Error()))
		return
	}
	if !up.Notify() {
		return
	}
	w.bus.Publish(bus.AlertRaised{
		AlertID: up.ID, Tier: string(tier), AlertKind: kind, DedupKey: dedup, Message: msg,
	})
}

// commitMark advances the high-water mark, after the block it names is done.
//
// Written after processing rather than before, which is what makes a crash
// mid-block safe: the block is scanned again on the next start, and every write
// the scan makes is idempotent.
func (w *Watcher) commitMark(ctx context.Context, ref blockRef) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	if err := w.store.SetMetaInt64(wctx, store.MetaLastScannedSQHeight,
		int64(ref.Height)); err != nil {
		w.log.Error("could not record how far the other chain has been scanned",
			slog.String("error", err.Error()))
		return
	}
	if err := w.store.SetMeta(wctx, store.MetaLastScannedSQHash, ref.Hash.String()); err != nil {
		w.log.Error("could not record how far the other chain has been scanned",
			slog.String("error", err.Error()))
		return
	}

	w.mu.Lock()
	w.last = ref
	w.progress.Height = ref.Height
	w.progress.Hash = ref.Hash.String()
	w.progress.At = w.now().Unix()
	w.mu.Unlock()
}

// rawTx serialises a transaction for storage. The whole thing is kept because
// the mirror needs to rebroadcast it later, and a spend seen once on a chain
// nobody else is watching may not be fetchable again.
func rawTx(tx *wire.MsgTx) string {
	if tx == nil {
		return ""
	}
	var buf writerTo
	if err := tx.Serialize(&buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf.b)
}

// writerTo is a minimal sink, so serialising does not need bytes.Buffer's whole
// surface.
type writerTo struct{ b []byte }

func (w *writerTo) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func reverse(metas []chainview.BlockMeta) {
	for i, j := 0, len(metas)-1; i < j; i, j = i+1, j-1 {
		metas[i], metas[j] = metas[j], metas[i]
	}
}

// writeCtx detaches a storage write from the shutdown that may be cancelling its
// trigger. Five seconds, then it really does give up.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}
