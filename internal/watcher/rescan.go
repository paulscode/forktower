package watcher

import (
	"context"
	"errors"
	"log/slog"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// rescanChunk is how many blocks one turn of the rescan gets through before the
// loop checks for anything more urgent.
//
// Bounded so a sweep of several thousand blocks cannot delay a new tip by its
// whole duration. Not bounded so tightly that the sweep crawls: these are local
// reads and the work per block is a map lookup per transaction input.
const rescanChunk = 25

// sweep is a range of the other chain still to be looked at.
type sweep struct {
	// Next is the height still to scan, and Target the last one in range.
	// Inclusive at both ends. Zero Target means there is nothing to do.
	Next   int32
	Target int32
}

func (s sweep) pending() bool { return s.Target > 0 && s.Next <= s.Target }

// Rescan asks for the other chain to be swept from a height, forwards to
// wherever live scanning has already reached.
//
// Public because the dashboard offers it: a user who has just connected a
// Lightning node, or who has reason to think something was missed, should be
// able to ask rather than wait. Never blocks — the sweep happens on the
// watcher's own goroutine, in slices, between blocks.
func (w *Watcher) Rescan(ctx context.Context, from int32) {
	w.queueSweep(ctx, from, "you asked for it")
}

// rescanFromFork sweeps everything since the chains separated.
//
// This is the case the whole feature exists for. A daemon installed *after* a
// split has a high-water mark at the current tip and knows nothing about the
// blocks between the separation and now — which is exactly the window in which a
// channel would have been attacked on the chain nobody was watching.
func (w *Watcher) rescanFromFork(ctx context.Context, why string) {
	split, err := w.store.GetSplitState(ctx)
	if err != nil {
		w.log.Warn("could not read where the chains separated, so no catch-up scan "+
			"was started", slog.String("error", err.Error()))
		return
	}
	if !split.ForkKnown() || split.ForkHeight <= 0 {
		// Nothing to sweep back to. Before a split there is no separation point,
		// and the live loop has been following the chain all along.
		return
	}
	// The fork block itself is shared by both chains, so the first block that can
	// differ is the one after it.
	w.queueSweep(ctx, split.ForkHeight+1, why)
}

// queueSweep records a range to sweep, widening whatever was already queued.
//
// Widening rather than replacing: two reasons to rescan produce one sweep
// covering both, and the wider of two overlapping ranges is always the safe
// choice. A sweep that has already passed a height does not go back for it —
// that pass recorded what it found, and recording is idempotent.
func (w *Watcher) queueSweep(ctx context.Context, from int32, why string) {
	if from < 1 {
		from = 1
	}

	w.mu.Lock()
	target := w.last.Height
	current := w.sweep
	w.mu.Unlock()

	if target <= 0 {
		// Nothing has been scanned live yet, so there is no "behind" to sweep. The
		// live loop's own gap handling covers everything from here.
		return
	}
	if from > target {
		return
	}

	next := from
	if current.pending() && current.Next < next {
		next = current.Next
	}
	if current.Target > target {
		target = current.Target
	}

	w.setSweep(ctx, sweep{Next: next, Target: target})
	w.wake()
	w.log.Info("catching up on blocks from the other chain",
		slog.Int("from_height", int(next)),
		slog.Int("to_height", int(target)),
		slog.String("reason", why))
}

// setSweep records the sweep in memory and on disk, so a restart resumes rather
// than starting again.
func (w *Watcher) setSweep(ctx context.Context, s sweep) {
	w.mu.Lock()
	w.sweep = s
	w.progress.RescanNext = s.Next
	w.progress.RescanTarget = s.Target
	if s.pending() {
		w.blockedSweep = false
	}
	w.mu.Unlock()

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	if err := w.store.SetMetaInt64(wctx, store.MetaRescanNextSQHeight,
		int64(s.Next)); err != nil {
		w.log.Warn("could not record how far the catch-up scan has got",
			slog.String("error", err.Error()))
		return
	}
	if err := w.store.SetMetaInt64(wctx, store.MetaRescanTargetSQHeight,
		int64(s.Target)); err != nil {
		w.log.Warn("could not record how far the catch-up scan has to go",
			slog.String("error", err.Error()))
	}
}

// loadSweep reads a sweep left unfinished by an earlier run.
func (w *Watcher) loadSweep(ctx context.Context) {
	next, err := w.store.GetMetaInt64(ctx, store.MetaRescanNextSQHeight, 0)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		w.log.Warn("could not read where the catch-up scan had got to",
			slog.String("error", err.Error()))
		return
	}
	target, err := w.store.GetMetaInt64(ctx, store.MetaRescanTargetSQHeight, 0)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		w.log.Warn("could not read where the catch-up scan had got to",
			slog.String("error", err.Error()))
		return
	}

	nextH, nextOK := toInt32(next)
	targetH, targetOK := toInt32(target)
	if !nextOK || !targetOK {
		return
	}

	w.mu.Lock()
	w.sweep = sweep{Next: nextH, Target: targetH}
	w.progress.RescanNext = nextH
	w.progress.RescanTarget = targetH
	pending := w.sweep.pending()
	w.mu.Unlock()

	if pending {
		w.log.Info("resuming a catch-up scan of the other chain",
			slog.Int("from_height", int(nextH)), slog.Int("to_height", int(targetH)))
	}
}

// sweepSlice scans the next few blocks of a pending sweep, and reports whether
// it did any work.
//
// Called from the watcher's own loop between blocks, never on a goroutine of its
// own. One writer of the spend records and one reader of the chain is the whole
// concurrency design here: blocks are ten minutes apart and there is nothing to
// win by racing.
func (w *Watcher) sweepSlice(ctx context.Context) bool {
	w.mu.Lock()
	s := w.sweep
	blocked := w.blockedSweep
	paused := w.guard != nil && w.guard.Paused()
	w.mu.Unlock()

	if !s.pending() || blocked || paused {
		return false
	}

	for range rescanChunk {
		if stopping(ctx) {
			return false
		}
		if !s.pending() {
			break
		}

		hash, err := w.view.BlockHashByHeight(ctx, s.Next)
		if err != nil {
			// A height the backend cannot name is usually a pruned or still-syncing
			// node rather than a fault. It stops this sweep rather than the daemon,
			// and it is retried when the next block arrives.
			w.blockSweep(s.Next, err)
			return false
		}
		meta := chainview.BlockMeta{BlockRef: chainview.BlockRef{Hash: hash, Height: s.Next}}
		if err := w.processWithRetries(ctx, meta); err != nil {
			w.blockSweep(s.Next, err)
			return false
		}

		s.Next++
		w.setSweep(ctx, s)
	}

	if !s.pending() {
		w.log.Info("finished catching up on the other chain",
			slog.Int("through_height", int(s.Target)))
		w.setSweep(ctx, sweep{})
	}
	return true
}

// blockSweep stops a sweep that cannot get past a block, without stopping the
// daemon.
//
// Different from the live loop's stall, and deliberately quieter: the live loop
// failing means nothing new is being checked, which is an emergency. A sweep
// failing means some history has not been re-read yet, which is worth saying and
// worth retrying, but the user is still being watched.
func (w *Watcher) blockSweep(height int32, cause error) {
	w.mu.Lock()
	already := w.blockedSweep
	w.blockedSweep = true
	w.mu.Unlock()

	if already {
		return
	}
	w.log.Warn("could not read a block while catching up on the other chain; will "+
		"try again when the next block arrives",
		slog.Int("height", int(height)), slog.String("error", cause.Error()))
}
