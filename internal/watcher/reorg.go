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
// **The chain growing and the chain being replaced are not indistinguishable,
// and treating them as though they were was a bug with teeth.** This used to
// walk back from the new tip until something recognisable turned up, on the
// reasoning that the walk answers both questions. It does — but only within
// MaxReorgDepth, which is a hundred blocks, and a node catching up advances
// thousands between polls. Every poll during an initial sync therefore walked a
// hundred blocks, found nothing, and reported the other chain as replaced
// further back than any reorganisation reaches: ninety-six critical alerts on a
// freshly installed appliance, about a node that was working perfectly.
//
// One question separates them and it costs one call. If the block we last
// processed is still on this chain at the height we processed it, the chain grew
// and nothing was replaced. Only if that block is gone has anything been
// rewritten.
func (w *Watcher) catchUp(ctx context.Context, tip chainview.BlockMeta, last blockRef) {
	if tip.Height > last.Height {
		still, err := w.view.BlockHashByHeight(ctx, last.Height)
		switch {
		case err != nil:
			// Cannot tell. Fall through to the walk, which is the cautious
			// answer: it may conclude the chain was replaced, and stopping on a
			// chain we cannot ask about is the right way to be wrong.
			w.log.Debug("could not confirm the last block is still on the other chain",
				slog.Int("height", int(last.Height)), slog.String("error", err.Error()))
		case still == last.Hash:
			w.grewForward(ctx, tip, last)
			return
		}
	}

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

// grewForward advances the mark over blocks that were simply appended.
//
// A short gap is read block by block, because those blocks may contain spends of
// the user's channels and that is the whole job.
//
// A long one is not. **Re-anchoring at the tip is the same decision this watcher
// already makes the first time it ever sees the chain**, and for the same
// reason: sweeping history is the rescan's job, and the rescan knows where to
// start because it is anchored on the fork point rather than on whenever the
// daemon happened to be looking. Reading a quarter of a million blocks one at a
// time to reach the present would delay watching the present indefinitely.
func (w *Watcher) grewForward(ctx context.Context, tip chainview.BlockMeta, last blockRef) {
	gap := tip.Height - last.Height

	if gap > w.cfg.MaxForwardGap {
		w.commitMark(ctx, blockRef{Height: tip.Height, Hash: tip.Hash})
		w.log.Info("the other chain moved on faster than it was being read, so "+
			"reading has resumed at its tip — earlier blocks are the rescan's, and "+
			"it starts from where the chains part rather than from here",
			slog.Int("was_at_height", int(last.Height)),
			slog.Int("now_at_height", int(tip.Height)))
		return
	}

	path := make([]chainview.BlockMeta, 0, gap)
	for height := last.Height + 1; height < tip.Height; height++ {
		hash, err := w.view.BlockHashByHeight(ctx, height)
		if err != nil {
			w.stall(ctx, height, fmt.Errorf("reading the other chain forward: %w", err))
			return
		}
		header, err := w.view.BlockHeaderByHash(ctx, hash)
		if err != nil {
			w.stall(ctx, height, fmt.Errorf("reading the other chain forward: %w", err))
			return
		}
		path = append(path, header)
	}
	path = append(path, tip)

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

		// Classified before it is written, not after. A row that exists for even a
		// moment with a placeholder shape is a row a reader can see, and the
		// difference between "unknown" and "somebody's commitment" is the whole
		// difference between routine and urgent.
		shape := w.shapeOf(ctx, m, meta)

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
			Shape:        shape,
			Status:       store.SpendConfirmed,
			FirstSeenAt:  w.now().Unix(),
			UpdatedAt:    w.now().Unix(),
		})
		cancel()
		if err != nil {
			return fresh, fmt.Errorf("recording a spend in block %d: %w", meta.Height, err)
		}

		if existed {
			// Seen before, but its status may still need moving on: a spend first
			// noticed in the memory pool, or one that was reorganised out and has
			// landed again. That counts as something happening, so the pass loop
			// looks again — the confirmation may have made outputs real that the
			// same block already spends.
			changed, err := w.confirmIfPending(ctx, id, m, meta, shape)
			if err != nil {
				return fresh, err
			}
			if changed {
				fresh++
			}
			continue
		}

		w.followCommitment(ctx, id, m, shape)
		w.resolveSource(ctx, m, shape)
		fresh++
		w.announce(m, id, meta, shape)
	}
	return fresh, nil
}

// shapeOf works out what a spend appears to be.
//
// Two different questions with two different answers. A spend of a funding
// output is a channel closing, and the only thing worth deciding is what kind of
// close. A spend of one of a commitment's own outputs is the *outcome*, and
// there the spending witness says plainly which branch of the script was taken.
func (w *Watcher) shapeOf(ctx context.Context, m Match, meta chainview.BlockMeta) store.SpendShape {
	if m.Target.Kind == KindFunding {
		return ClassifyShape(ShapeFacts{
			Tx:           m.Tx,
			TxID:         m.TxID.String(),
			OurCloseTxID: w.ourCloseTxID(ctx, m.Target.ChannelID),
		})
	}
	return ClassifySweep(SweepFacts{
		Tx:          m.Tx,
		InputIndex:  m.InputIndex,
		Role:        m.Target.Role,
		SpendHeight: meta.Height,
		// The deadline is the deadline engine's to compute and does not exist yet
		// at this point in the block. Left unset on purpose: the witness is the
		// stronger evidence anyway, and timing is only its fallback.
		DeadlineHeight: 0,
	})
}

// ourCloseTxID is what the user's own Lightning node said it broadcast, if
// anything. The one piece of information that can tell our commitment from
// theirs, since a commitment reveals nothing about which side holds it.
func (w *Watcher) ourCloseTxID(ctx context.Context, channelID int64) string {
	if channelID == 0 {
		return ""
	}
	channels, err := w.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		w.log.Warn("could not check whether a channel close was one of yours",
			slog.String("error", err.Error()))
		return ""
	}
	for _, c := range channels {
		if c.ID == channelID {
			return c.CloseTxID
		}
	}
	return ""
}

// followCommitment starts watching what a commitment created.
//
// A commitment on the chain nobody is watching is the beginning of the story,
// not the end: its outputs are what say how it finished. Anything else — a
// cooperative close, a sweep, something unrecognised — creates nothing worth
// following, because it pays people directly and leaves no contested output.
func (w *Watcher) followCommitment(
	ctx context.Context, id int64, m Match, shape store.SpendShape,
) {
	switch shape {
	case store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked:
		w.watchCommitmentOutputs(ctx, m, id)
	case store.ShapeMutualClose, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown:
	}
}

// confirmIfPending moves a spend already recorded into this block.
//
// The case that matters is a spend that was reorganised out and has landed
// again on the new branch. That is the same event happening a second time, not a
// second event, so the record is updated rather than duplicated — and it is
// announced again, because a subscriber told the spend had gone needs telling it
// is back.
func (w *Watcher) confirmIfPending(
	ctx context.Context, id int64, m Match, meta chainview.BlockMeta, shape store.SpendShape,
) (bool, error) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	sp, err := w.store.GetSpend(wctx, id)
	if err != nil {
		return false, fmt.Errorf("reading a spend already recorded: %w", err)
	}
	if sp.Status == store.SpendConfirmed && sp.BlockHash == meta.Hash.String() {
		return false, nil
	}

	if err := w.store.UpdateSpendStatus(wctx, id, store.SpendConfirmed,
		meta.Hash.String(), meta.Height, w.now().Unix()); err != nil {
		return false, fmt.Errorf("confirming a spend: %w", err)
	}
	if sp.Status == store.SpendReorgedOut {
		w.log.Info("a spend that had been dropped from the other chain is back in a block",
			slog.Int64("spend_id", id), slog.Int("height", int(meta.Height)))
	}

	// The outputs become real here, and only here. A commitment first seen
	// unconfirmed reached storage through the memory pool, which deliberately
	// starts nothing watching — the outputs did not exist yet. This is the block
	// saying they do, so it is the moment to start; and on a full node, which
	// sees the memory pool, this is the *ordinary* path rather than the unusual
	// one. Missing it meant a commitment noticed early was never followed up,
	// which is to say the outcome was never reported at all.
	w.followCommitment(ctx, id, m, sp.Shape)

	w.announce(m, id, meta, shape)
	return true, nil
}

// resolveSource settles what a commitment was, after the fact.
//
// A justice transaction is proof: only the holder of a revocation secret can
// take that branch, and revocation secrets exist only for commitments that were
// revoked. So a commitment we could only call "somebody's" becomes definitely a
// revoked one — the single case where "whose commitment was that" gets a real
// answer later, and the difference between telling a user something worrying
// happened and telling them what it was.
//
// Only ever from `commitment_unknown`. A commitment the user's own node said it
// broadcast keeps that label even if it turns out to have been revoked, because
// "we published a revoked commitment" is a different and more useful thing to
// know than "it was revoked".
func (w *Watcher) resolveSource(ctx context.Context, m Match, shape store.SpendShape) {
	if shape != store.ShapeJustice || m.Target.SourceSpendEventID == 0 {
		return
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	source, err := w.store.GetSpend(wctx, m.Target.SourceSpendEventID)
	if err != nil {
		w.log.Warn("could not look up the close a justice transaction answered",
			slog.String("error", err.Error()))
		return
	}
	if source.Shape != store.ShapeCommitmentUnknown {
		return
	}
	if err := w.store.UpdateSpendShape(wctx, source.ID,
		store.ShapeCommitmentRevoked, w.now().Unix()); err != nil {
		w.log.Error("could not record that a close was of a revoked commitment",
			slog.Int64("spend_id", source.ID), slog.String("error", err.Error()))
		return
	}
	w.log.Info("a close on the other chain is now known to have been a revoked one, "+
		"because it was answered", slog.Int64("spend_id", source.ID))
}

// announce publishes what was found, in the shape the rest of the daemon reads.
func (w *Watcher) announce(
	m Match, id int64, meta chainview.BlockMeta, shape store.SpendShape,
) {
	if m.Target.Kind == KindFunding {
		w.bus.Publish(bus.FundingSpent{
			SpendEventID: id,
			ChannelID:    m.Target.ChannelID,
			Branch:       string(w.branch),
			SpendTxid:    m.TxID.String(),
			Shape:        string(shape),
			Status:       string(store.SpendConfirmed),
			Height:       meta.Height,
		})
		return
	}
	w.bus.Publish(bus.SecondOrderSpent{
		SpendEventID:       id,
		SourceSpendEventID: m.Target.SourceSpendEventID,
		Role:               string(m.Target.Role),
		Shape:              string(shape),
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

	// **A stable key, not one per height.** This carried the tip's height, so a
	// condition that recurred at a moving height minted a fresh critical alert
	// every time instead of collapsing into one. What the user needs to know is
	// that scanning has stopped — once, loudly — not the height it was at on
	// each of ninety-six occasions.
	w.raise(ctx, store.TierCritical, DeepReorgAlertKind, DeepReorgAlertKind,
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

	// **A stable key, not one per height** — the same fix deepReorg carries
	// twenty lines above, and this is what it warns about. Keyed by height, a
	// condition that recurred as the height moved minted a fresh *critical*
	// alert every time: eleven of them in ten minutes on one install, all
	// describing the same thing. What a user needs to know is that scanning has
	// stopped, once, not the height it was at on each occasion.
	w.raise(ctx, store.TierCritical, StalledAlertKind, StalledAlertKind,
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
