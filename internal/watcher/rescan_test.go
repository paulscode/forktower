package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// splitAt records a separation point, as the detection engine does when it finds
// one.
func splitAt(t *testing.T, st *store.Store, height int32) {
	t.Helper()
	if err := st.SaveSplitState(context.Background(), store.Split{
		State: store.StateSplit, ForkHeight: height, ForkHash: "aa", DetectedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// The case the whole feature exists for. A daemon installed after a split has a
// high-water mark at the current tip and knows nothing about the blocks between
// the separation and now — which is exactly the window in which a channel would
// have been attacked on the chain nobody was watching.
func TestASweepFindsSpendsFromBeforeTheDaemonStarted(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)

	// Fifty blocks of history the watcher was never told about, with a spend of a
	// different watched channel planted in three of them. Three channels rather
	// than three spends of one, because an outpoint can only be spent once and
	// three identical transactions would be one record, correctly.
	for _, txid := range []string{fundingA, fundingB, fundingC} {
		addChannel(t, h.store, txid, store.Relevant)
	}
	at := map[int]string{7: fundingA, 23: fundingB, 44: fundingC}

	forkHeight := h.view.Tip().Height
	var planted []int32
	for i := range 50 {
		meta := h.view.Extend("history", 1)
		if txid, ok := at[i]; ok {
			h.view.PutTransactions(meta.Hash, coinbase(),
				spend(fundingOutpoint(t, txid, 1)))
			planted = append(planted, meta.Height)
		}
	}
	splitAt(t, h.store, forkHeight)

	h.run()
	h.waitFor("the starting point at the tip", func() bool {
		return h.w.Progress().Height == h.view.Tip().Height
	})
	// Nothing yet: the live loop starts at the tip and looks forward.
	if len(h.spends()) != 0 {
		t.Fatal("history was scanned before anyone asked for it")
	}

	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: string(store.StateSplit)})

	h.waitFor("all three planted spends", func() bool { return len(h.spends()) == 3 })
	h.waitFor("the sweep to finish", func() bool { return !h.w.Progress().Rescanning() })

	seen := map[int32]bool{}
	for _, sp := range h.spends() {
		seen[sp.BlockHeight] = true
	}
	for _, height := range planted {
		if !seen[height] {
			t.Errorf("the spend at height %d was not found", height)
		}
	}
}

// Interrupting a sweep and starting again must find the rest, and must not find
// anything twice.
func TestASweepResumesWhereItStoppedAndFindsEachSpendOnce(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	for _, txid := range []string{fundingA, fundingB, fundingC} {
		addChannel(t, h.store, txid, store.Relevant)
	}
	at := map[int]string{7: fundingA, 23: fundingB, 44: fundingC}

	forkHeight := h.view.Tip().Height
	for i := range 50 {
		meta := h.view.Extend("history", 1)
		if txid, ok := at[i]; ok {
			h.view.PutTransactions(meta.Hash, coinbase(),
				spend(fundingOutpoint(t, txid, 1)))
		}
	}
	tip := h.view.Tip().Height
	splitAt(t, h.store, forkHeight)

	// A sweep already half done, as a restart mid-sweep leaves behind.
	half := forkHeight + 25
	if err := h.store.SetMetaInt64(ctx, store.MetaLastScannedSQHeight, int64(tip)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetMeta(ctx, store.MetaLastScannedSQHash,
		hashAtHeight(t, h.view, tip).String()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetMetaInt64(ctx, store.MetaRescanNextSQHeight, int64(half)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetMetaInt64(ctx, store.MetaRescanTargetSQHeight, int64(tip)); err != nil {
		t.Fatal(err)
	}

	h.run()

	// Picked up first, then finished. Waiting only for "not rescanning" would be
	// satisfied by the instant before the unfinished sweep was even read back.
	h.waitFor("the unfinished sweep to be picked up", func() bool {
		return h.w.Progress().RescanTarget == tip
	})
	h.waitFor("the rest of the sweep", func() bool { return !h.w.Progress().Rescanning() })

	// Only the spend above the resume point: the two below it were the earlier
	// run's to find, and going back for them would be re-reading history the mark
	// says was already read.
	got := h.spends()
	if len(got) != 1 {
		t.Fatalf("found %d spends, want only the one above where the sweep resumed: %+v",
			len(got), got)
	}
	if got[0].BlockHeight <= half {
		t.Errorf("the sweep went back below where it had got to: height %d",
			got[0].BlockHeight)
	}
}

// Running the same sweep twice must not turn one spend into two records.
func TestSweepingTwiceRecordsEachSpendOnce(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	forkHeight := h.view.Tip().Height
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("after", 3)
	splitAt(t, h.store, forkHeight)

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	for range 3 {
		h.w.Rescan(ctx, forkHeight+1)
		h.waitFor("the sweep to finish", func() bool { return !h.w.Progress().Rescanning() })
	}

	if got := h.spends(); len(got) != 1 {
		t.Errorf("sweeping three times produced %d records for one spend", len(got))
	}
}

// A channel seen for the first time has a history nobody has checked.
func TestANewChannelTriggersASweepOfItsHistory(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)

	forkHeight := h.view.Tip().Height
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("after", 2)
	splitAt(t, h.store, forkHeight)

	h.run()
	h.waitFor("the starting point at the tip", func() bool {
		return h.w.Progress().Height == h.view.Tip().Height
	})

	// The channel arrives after the spend has already happened.
	addChannel(t, h.store, fundingA, store.Relevant)
	h.bus.Publish(bus.ChannelUpserted{New: true, Channel: bus.ChannelJSON{ID: 1}})

	h.waitFor("the spend in the channel's history", func() bool { return len(h.spends()) == 1 })
}

// A channel that merely changed is not a reason to re-read the chain. Sweeping
// on every alias a counterparty picks would be a lot of reading for no new
// information.
func TestAChangedChannelDoesNotTriggerASweep(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)

	forkHeight := h.view.Tip().Height
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("after", 2)
	splitAt(t, h.store, forkHeight)
	addChannel(t, h.store, fundingA, store.Relevant)

	h.run()
	h.waitFor("the starting point at the tip", func() bool {
		return h.w.Progress().Height == h.view.Tip().Height
	})

	h.bus.Publish(bus.ChannelUpserted{New: false, Channel: bus.ChannelJSON{ID: 1}})

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.w.Progress().Rescanning() {
		t.Error("a channel that merely changed started a sweep of the chain")
	}
	if len(h.spends()) != 0 {
		t.Error("history was re-read for a channel that had not changed materially")
	}
}

// Before a split there is no separation point to sweep back to, and the live
// loop has been following the chain all along.
func TestNoSweepIsStartedWithoutASeparationPoint(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: string(store.StateSplit)})

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.w.Progress().Rescanning() {
		t.Error("a sweep started with nowhere to sweep back to")
	}
}

// Two reasons to sweep produce one sweep covering both, and the wider of two
// overlapping ranges is always the safe choice.
func TestOverlappingSweepsAreWidenedNotReplaced(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	forkHeight := h.view.Tip().Height
	early := h.view.Extend("early-breach", 1)
	h.view.PutTransactions(early.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("later", 20)
	splitAt(t, h.store, forkHeight)

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	// The guard goes on, so the sweeps are queued but do not start — which is
	// what lets two requests be queued before either runs.
	h.guard.paused.Store(true)

	h.w.Rescan(ctx, forkHeight+15)
	h.w.Rescan(ctx, forkHeight+1)

	if got := h.w.Progress().RescanNext; got != forkHeight+1 {
		t.Errorf("the sweep starts at %d, want the earlier %d", got, forkHeight+1)
	}

	h.guard.paused.Store(false)
	h.view.Extend("nudge", 1)
	h.waitFor("the earlier spend to be found", func() bool { return len(h.spends()) == 1 })
}

// A sweep asked to start above where scanning has already reached has nothing to
// do, and must not record a range that can never complete.
func TestASweepAboveTheMarkIsNotStarted(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.w.Rescan(ctx, h.w.Progress().Height+100)
	if h.w.Progress().Rescanning() {
		t.Error("a sweep was started for blocks that have not happened")
	}
}

// A block the backend cannot name stops the sweep, not the daemon — a pruned or
// still-syncing node is the common case, not a fault — and it is tried again
// when the next block arrives.
func TestASweepBlockedByThePastRetriesOnTheNextBlock(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	forkHeight := h.view.Tip().Height
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("after", 3)
	splitAt(t, h.store, forkHeight)

	h.view.Fail("BlockHashByHeight", errors.New("that block was pruned"))
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.w.Rescan(ctx, forkHeight+1)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting the sweep fail
	if len(h.spends()) != 0 {
		t.Error("a spend was found through a backend that could not answer")
	}
	// The daemon is fine: only the catch-up is held up, and the live loop is
	// still following the chain.
	if h.w.Progress().Stalled {
		t.Error("a catch-up that could not read history stopped the live scan")
	}

	// The node starts answering, and the next block is the cue to try again.
	h.view.Fail("BlockHashByHeight", nil)
	h.view.Extend("recovered", 1)
	h.waitFor("the sweep to get through", func() bool { return len(h.spends()) == 1 })
}

// Nothing is swept while the daemon is unsure which chain it is looking at, for
// the same reason nothing is scanned live.
func TestNoSweepingWhileTheDaemonIsUnsureOfTheChain(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	forkHeight := h.view.Tip().Height
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), spend(fundingOutpoint(t, fundingA, 1)))
	h.view.Extend("after", 2)
	splitAt(t, h.store, forkHeight)

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.guard.paused.Store(true)
	h.w.Rescan(ctx, forkHeight+1)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if len(h.spends()) != 0 {
		t.Error("history was swept from a chain the daemon was unsure of")
	}
	if !h.w.Progress().Rescanning() {
		t.Error("the sweep was forgotten rather than held")
	}

	// Un-pausing is not itself an event the watcher hears, so it picks up again
	// at the next block — which on this chain is never more than one block away.
	h.guard.paused.Store(false)
	h.view.Extend("resumed", 1)
	h.waitFor("the sweep to resume", func() bool { return len(h.spends()) == 1 })
}

// A sweep is bounded and finishes, rather than running to the end of the chain.
func TestASweepStopsAtWhereLiveScanningHasReached(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	mark := h.w.Progress().Height

	h.w.Rescan(ctx, 1)
	if got := h.w.Progress().RescanTarget; got != mark {
		t.Errorf("the sweep runs to %d, want it to stop at %d", got, mark)
	}
	h.waitFor("the sweep to finish", func() bool { return !h.w.Progress().Rescanning() })
}

func TestSweepBoundsAreReadBack(t *testing.T) {
	t.Parallel()

	if (sweep{}).pending() {
		t.Error("an empty sweep is pending")
	}
	if (sweep{Next: 5, Target: 4}).pending() {
		t.Error("a sweep past its target is pending")
	}
	if !(sweep{Next: 4, Target: 4}).pending() {
		t.Error("a sweep with one block left is not pending")
	}
	if (Progress{}).Rescanning() {
		t.Error("nothing to catch up on reads as catching up")
	}
	if !(Progress{RescanNext: 2, RescanTarget: 9}).Rescanning() {
		t.Error("a sweep in progress does not read as catching up")
	}
}

// Asking for a sweep before anything has been scanned has nothing to sweep back
// to: the live loop's own gap handling covers everything from here.
func TestASweepAskedForBeforeAnythingIsScannedDoesNothing(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)

	h.w.Rescan(context.Background(), 1)
	if h.w.Progress().Rescanning() {
		t.Error("a sweep was queued before there was anything behind us")
	}
}

// A stored sweep whose numbers cannot be held in the column they came from is
// ignored rather than narrowed: a wrapped height would name a real block, and
// sweeping the wrong range silently is worse than not sweeping.
func TestAnUnreadableSweepMarkIsIgnored(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	if err := h.store.SetMetaInt64(ctx, store.MetaRescanNextSQHeight, 1<<40); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetMetaInt64(ctx, store.MetaRescanTargetSQHeight, 1<<41); err != nil {
		t.Fatal(err)
	}

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	if h.w.Progress().Rescanning() {
		t.Error("an unreadable sweep was acted on")
	}
}

// The event bus closing must not stop the watcher: it still has a chain to
// follow, and the events it listens for are hints about what to watch rather
// than the reason it is watching.
func TestTheBusClosingDoesNotStopTheWatcher(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	start := h.w.Progress().Height

	h.bus.Close()

	h.view.Extend("after", 2)
	h.waitFor("scanning to continue without the bus", func() bool {
		return h.w.Progress().Height == start+2
	})
}
