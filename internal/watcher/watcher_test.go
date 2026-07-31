package watcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
)

// pausable is the detection engine, as far as the watcher is concerned.
type pausable struct{ paused atomic.Bool }

func (p *pausable) Paused() bool { return p.paused.Load() }

type liveHarness struct {
	t     *testing.T
	store *store.Store
	bus   *bus.Bus
	view  *chainviewtest.View
	guard *pausable
	w     *Watcher
	clock *atomic.Int64
	done  chan error
}

func newLiveHarness(t *testing.T, mutate func(*Config)) *liveHarness {
	t.Helper()

	st := openStore(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)

	view := chainviewtest.New("sq")
	view.Extend("start", 10)
	t.Cleanup(view.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)
	guard := &pausable{}

	cfg := Config{BlockAttempts: 2, RetryDelay: time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}

	w, err := New(st, b, view, store.BranchSQ, guard, cfg, nil,
		func() time.Time { return time.Unix(clock.Load(), 0) })
	if err != nil {
		t.Fatalf("building the watcher: %v", err)
	}
	return &liveHarness{t: t, store: st, bus: b, view: view, guard: guard, w: w, clock: clock}
}

func (h *liveHarness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.done = make(chan error, 1)
	go func() { h.done <- h.w.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			h.t.Error("the watcher did not stop")
		}
	})
}

func (h *liveHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // waiting on a real goroutine
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *liveHarness) spends() []store.Spend {
	h.t.Helper()
	got, err := h.store.ListSpends(context.Background(), store.SpendFilter{Branch: store.BranchSQ})
	if err != nil {
		h.t.Fatalf("reading spends: %v", err)
	}
	return got
}

// plant puts a transaction spending the given outpoint into the next block, and
// returns that block.
func (h *liveHarness) plant(label string, prevout wire.OutPoint) (chainview.BlockMeta, *wire.MsgTx) {
	h.t.Helper()
	tx := spend(prevout)
	meta := h.view.Extend(label, 1)
	h.view.PutTransactions(meta.Hash, coinbase(), tx)
	return meta, tx
}

func fundingOutpoint(t *testing.T, txid string, vout uint32) wire.OutPoint {
	t.Helper()
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		t.Fatal(err)
	}
	return wire.OutPoint{Hash: *h, Index: vout}
}

// Nothing is scanned before the daemon has been told where it is. The first tip
// sets the mark and scanning starts from there; the historical sweep is the
// rescan's job, which knows where to start from because it is anchored on the
// fork point rather than on whenever the daemon happened to be installed.
func TestTheFirstTipOnlySetsTheStartingPoint(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point to be recorded", func() bool {
		return h.w.Progress().Height == h.view.Tip().Height
	})
	if len(h.spends()) != 0 {
		t.Error("blocks from before the daemon started were scanned")
	}
}

func TestAFundingSpendIsRecordedAndAnnounced(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	id := addChannel(t, h.store, fundingA, store.Relevant)
	events := h.bus.Subscribe("test", bus.KindFundingSpent)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	_, tx := h.plant("spent", fundingOutpoint(t, fundingA, 1))

	select {
	case e := <-events:
		got, ok := e.(bus.FundingSpent)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.ChannelID != id {
			t.Errorf("announced channel %d, want %d", got.ChannelID, id)
		}
		if got.SpendTxid != tx.TxHash().String() {
			t.Error("announced the wrong transaction")
		}
		if got.Branch != string(store.BranchSQ) {
			t.Errorf("announced branch %q", got.Branch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a spend of a channel's funding output was never announced")
	}

	h.waitFor("the spend to be stored", func() bool { return len(h.spends()) == 1 })
	sp := h.spends()[0]
	if sp.Status != store.SpendConfirmed || sp.ChannelID != id {
		t.Errorf("stored %+v", sp)
	}
	// The whole transaction is kept: the mirror needs to rebroadcast it later,
	// and a spend seen once on a chain nobody else watches may not be fetchable
	// again.
	if sp.SpendTxHex == "" {
		t.Error("the raw transaction was not kept")
	}
}

// Blocks with nothing of ours in them still advance the mark, and re-reading
// them later would find the same nothing more slowly.
func TestOrdinaryBlocksAdvanceTheMark(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	start := h.w.Progress().Height

	h.view.Extend("quiet", 5)
	h.waitFor("five quiet blocks to be scanned", func() bool {
		return h.w.Progress().Height == start+5
	})
	if len(h.spends()) != 0 {
		t.Error("a spend was invented from blocks containing nothing")
	}
	if !h.w.Progress().Progressing() {
		t.Error("scanning is not reported as progressing")
	}
}

// The chain replaces the block a spend was in. The spend is marked as no longer
// on the chain and announced — a breach that disappears has not necessarily gone
// away, and reading it as good news would be the wrong instinct.
func TestAReorgRemovesASpendAndSaysSo(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	gone := h.bus.Subscribe("test", bus.KindSpendReorgedOut)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	meta, _ := h.plant("breach", fundingOutpoint(t, fundingA, 1))
	h.waitFor("the spend to be recorded", func() bool { return len(h.spends()) == 1 })

	// The engine's own record of this chain, which is what the walk stops at.
	recordBranchBlocks(t, h.store, h.view, meta.Height)

	// A different branch replaces it, one block longer so the tip moves.
	h.view.Reorg(meta.Height-1, "replacement", 2)

	select {
	case e := <-gone:
		got, ok := e.(bus.SpendReorgedOut)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.Branch != string(store.BranchSQ) {
			t.Errorf("announced branch %q", got.Branch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a spend that left the chain was never announced")
	}

	h.waitFor("the spend to be marked as gone", func() bool {
		got := h.spends()
		return len(got) == 1 && got[0].Status == store.SpendReorgedOut
	})
	// Marked, not deleted: it happened, and the record of it is the audit trail.
	if h.spends()[0].SpendTxID == "" {
		t.Error("the record of what happened was destroyed rather than marked")
	}
}

// The same spend landing again on the new branch is the same event happening a
// second time, not a second event. One record, updated, and announced again so a
// subscriber told it had gone is told it is back.
func TestASpendThatComesBackIsTheSameRecord(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	spent := h.bus.Subscribe("test", bus.KindFundingSpent)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	prevout := fundingOutpoint(t, fundingA, 1)
	tx := spend(prevout)
	meta := h.view.Extend("breach", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), tx)

	h.waitFor("the spend to be recorded", func() bool { return len(h.spends()) == 1 })
	recordBranchBlocks(t, h.store, h.view, meta.Height)

	// The replacement branch contains the very same transaction.
	replaced := h.view.Reorg(meta.Height-1, "replacement", 2)
	h.view.PutTransactions(hashAtHeight(t, h.view, meta.Height), coinbase(), tx)
	_ = replaced

	h.waitFor("the spend to be confirmed again", func() bool {
		got := h.spends()
		return len(got) == 1 && got[0].Status == store.SpendConfirmed
	})
	if len(h.spends()) != 1 {
		t.Errorf("one event became %d records", len(h.spends()))
	}

	// Announced at least twice: once when first seen, once when it came back.
	var announced int
	deadline := time.After(2 * time.Second)
	for announced < 2 {
		select {
		case <-spent:
			announced++
		case <-deadline:
			t.Fatalf("the spend was announced %d times, want it re-announced after "+
				"coming back", announced)
		}
	}
}

// After downtime the chain has moved on by several blocks. Everything in the gap
// must be scanned, not skipped: a spend in one of those blocks is exactly what
// this exists to find.
func TestAGapAfterDowntimeIsScannedNotSkipped(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	start := h.w.Progress().Height

	// Blocks appear without the watcher being told about each one, which is what
	// a restart or a dropped subscription looks like. The spend is in the middle
	// of the gap, so finding it proves the gap was walked rather than jumped.
	h.view.Extend("gap", 3)
	tx := spend(fundingOutpoint(t, fundingA, 1))
	middle := h.view.Extend("gap-spend", 1)
	h.view.PutTransactions(middle.Hash, coinbase(), tx)
	h.view.Extend("gap", 3)

	h.waitFor("the gap to be scanned", func() bool {
		return h.w.Progress().Height == start+7
	})
	got := h.spends()
	if len(got) != 1 {
		t.Fatalf("found %d spends in the gap, want the planted one", len(got))
	}
	if got[0].BlockHeight != middle.Height {
		t.Errorf("the spend was recorded at height %d, want %d",
			got[0].BlockHeight, middle.Height)
	}
}

// A chain replaced further back than any reorganisation should reach is more
// likely a backend on the wrong chain than a very deep reorganisation, and
// scanning the wrong chain produces a clean report about a chain nobody needs
// watched.
func TestADeepReorgStopsScanningAndSaysWhy(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, func(c *Config) { c.MaxReorgDepth = 3 })
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	// Replace far more than the limit allows.
	h.view.Reorg(2, "elsewhere", 12)

	h.waitFor("scanning to stop", func() bool { return h.w.Progress().Stalled })

	got := h.w.Progress()
	if got.Progressing() {
		t.Error("a watcher that stopped scanning reports itself as progressing")
	}
	if got.Why == "" {
		t.Error("scanning stopped without an explanation a user could read")
	}

	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var raised bool
	for _, a := range alerts {
		if a.Kind == DeepReorgAlertKind {
			raised = true
			if a.Tier != store.TierCritical {
				t.Errorf("the alert is tier %q", a.Tier)
			}
		}
	}
	if !raised {
		t.Error("scanning stopped without raising an alert")
	}
}

// The mark only advances after a block commits, so a block that fails every
// attempt freezes scanning while the daemon stays up and the backend looks
// healthy. Nobody would notice, which is why it is loud.
func TestABlockThatCannotBeReadStallsLoudly(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	frozen := h.w.Progress().Height

	h.view.Fail("Block", errors.New("the node is not answering"))
	h.view.Extend("unreadable", 1)

	h.waitFor("the watcher to report itself stuck", func() bool {
		return h.w.Progress().Stalled
	})
	if got := h.w.Progress().Height; got != frozen {
		t.Errorf("the mark advanced past a block that was never read: %d", got)
	}
	if h.w.Progress().Progressing() {
		t.Error("a stuck watcher reports itself as progressing")
	}

	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var raised bool
	for _, a := range alerts {
		if a.Kind == StalledAlertKind {
			raised = true
		}
	}
	if !raised {
		t.Error("a watcher that stopped scanning raised no alert")
	}

	// And it recovers when the node does.
	h.view.Fail("Block", nil)
	h.view.Extend("readable", 1)
	h.waitFor("scanning to resume", func() bool { return h.w.Progress().Progressing() })
}

// Scanning a chain the daemon is not sure about produces a clean report on a
// chain nobody needs watched, which is worse than producing nothing: the user is
// told they are covered while the exposure goes unseen.
func TestNothingIsScannedWhileTheDaemonIsUnsureOfTheChain(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	frozen := h.w.Progress().Height

	h.guard.paused.Store(true)
	h.plant("would-be-breach", fundingOutpoint(t, fundingA, 1))

	// Nothing happens for a while, which is the assertion.
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if got := h.w.Progress().Height; got != frozen {
		t.Errorf("scanning continued while paused: %d", got)
	}
	if len(h.spends()) != 0 {
		t.Error("a spend was recorded from a chain the daemon was unsure of")
	}

	// And it picks up where it left off once the doubt is resolved.
	h.guard.paused.Store(false)
	h.view.Extend("after", 1)
	h.waitFor("the missed spend to be found", func() bool { return len(h.spends()) == 1 })
}

// A channel that arrives after scanning has started must be watched from then
// on, without restarting anything.
func TestANewChannelJoinsTheWatchset(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	if !h.w.WatchSet().Empty() {
		t.Fatal("something was being watched before any channel existed")
	}

	addChannel(t, h.store, fundingA, store.Relevant)
	h.bus.Publish(bus.ChannelUpserted{New: true, Channel: bus.ChannelJSON{ID: 1}})

	h.waitFor("the new channel to be watched", func() bool { return h.w.WatchSet().Len() == 1 })

	h.plant("spent", fundingOutpoint(t, fundingA, 1))
	h.waitFor("its spend to be found", func() bool { return len(h.spends()) == 1 })
}

// Restarting must not re-report what was already found, and must not skip what
// was not.
func TestScanningResumesWhereItLeftOff(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	meta, _ := h.plant("spent", fundingOutpoint(t, fundingA, 1))

	// Waiting for the block to be *committed*, not merely for the spend row to
	// appear. The row is written first and the mark advances afterwards — which is
	// what makes a crash mid-block safe — so reading the mark as soon as the row
	// exists reads the height from before this block.
	h.waitFor("the block to be committed", func() bool {
		return h.w.Progress().Height == meta.Height
	})
	if len(h.spends()) != 1 {
		t.Fatalf("recorded %d spends, want 1", len(h.spends()))
	}
	before := h.w.Progress().Height

	// A second watcher over the same database and the same chain, as a restart is.
	second, err := New(h.store, h.bus, h.view, store.BranchSQ, h.guard,
		Config{BlockAttempts: 2, RetryDelay: time.Millisecond}, nil,
		func() time.Time { return time.Unix(h.clock.Load(), 0) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- second.Run(ctx) }()

	h.waitFor("the restarted watcher to pick up the mark", func() bool {
		return second.Progress().Height == before
	})
	if len(h.spends()) != 1 {
		t.Errorf("restarting turned one spend into %d records", len(h.spends()))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the second watcher did not stop")
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	st := openStore(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)
	view := chainviewtest.New("sq")
	t.Cleanup(view.Close)

	if _, err := New(nil, b, view, store.BranchSQ, nil, Config{}, nil, nil); err == nil {
		t.Error("a watcher with no store was accepted")
	}
	if _, err := New(st, nil, view, store.BranchSQ, nil, Config{}, nil, nil); err == nil {
		t.Error("a watcher with no bus was accepted")
	}
	if _, err := New(st, b, nil, store.BranchSQ, nil, Config{}, nil, nil); err == nil {
		t.Error("a watcher with no chain view was accepted")
	}
	if _, err := New(st, b, view, "elsewhere", nil, Config{}, nil, nil); err == nil {
		t.Error("a watcher on an unknown branch was accepted")
	}
	if _, err := New(st, b, view, store.BranchSQ, nil, Config{}, nil, nil); err != nil {
		t.Errorf("a usable watcher was refused: %v", err)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	got := Config{}.withDefaults()
	if got.MaxReorgDepth != DefaultMaxReorgDepth ||
		got.BlockAttempts != DefaultBlockAttempts ||
		got.RetryDelay != DefaultRetryDelay {
		t.Errorf("got %+v", got)
	}
	set := Config{MaxReorgDepth: 5, BlockAttempts: 2, RetryDelay: time.Second}
	if set.withDefaults() != set {
		t.Error("explicit settings were overwritten")
	}
}

// A height that cannot be narrowed is refused rather than wrapped: a wrapped
// height is negative, and a negative height reads as an ordinary value
// everywhere downstream.
func TestOversizedHeightsAreRefusedNotWrapped(t *testing.T) {
	t.Parallel()

	for _, v := range []int64{1 << 40, -(1 << 40)} {
		if _, ok := toInt32(v); ok {
			t.Errorf("%d was narrowed rather than refused", v)
		}
	}
	if got, ok := toInt32(961_632); !ok || got != 961_632 {
		t.Errorf("an ordinary height was refused: %d %v", got, ok)
	}
}

// recordBranchBlocks tells the detection engine's history about a chain, which
// is what the reorganisation walk stops at when a block is no longer on the
// active chain and its header can no longer be fetched by height.
func recordBranchBlocks(t *testing.T, st *store.Store, v *chainviewtest.View, upTo int32) {
	t.Helper()
	for height := int32(1); height <= upTo; height++ {
		hash, err := v.BlockHashByHeight(context.Background(), height)
		if err != nil {
			continue
		}
		if err := st.RecordBranchBlock(context.Background(), store.BranchBlock{
			Branch: store.BranchSQ, Hash: hash.String(), Height: height, ReceivedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func hashAtHeight(t *testing.T, v *chainviewtest.View, height int32) chainhash.Hash {
	t.Helper()
	h, err := v.BlockHashByHeight(context.Background(), height)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Watching what a confirmed commitment created is how outcomes get reported
// rather than only threats: a sweep is somebody taking the money, and which
// output it came from is what says whose.
func TestASecondOrderSpendIsAnnouncedAsSuch(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	sourceID := addSpend(t, h.store, store.BranchSQ)
	if err := h.store.AddWatchOutpoint(ctx, store.WatchOutpoint{
		Branch: store.BranchSQ, TxID: commitTX, Vout: 0, ScriptHex: "0020aabb",
		SourceSpendEventID: sourceID, Role: store.RoleToLocal,
	}); err != nil {
		t.Fatal(err)
	}

	events := h.bus.Subscribe("test", bus.KindSecondOrderSpent)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.plant("sweep", fundingOutpoint(t, commitTX, 0))

	select {
	case e := <-events:
		got, ok := e.(bus.SecondOrderSpent)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.SourceSpendEventID != sourceID {
			t.Errorf("pointed back at spend %d, want %d", got.SourceSpendEventID, sourceID)
		}
		if got.Role != string(store.RoleToLocal) {
			t.Errorf("announced role %q", got.Role)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a spend of a commitment's output was never announced")
	}
}

// A reorganisation takes away a commitment and the sweep of its output together.
// Both must be marked as gone, not just the one the watcher happened to be
// looking at.
func TestAReorgRemovesEverythingAboveTheAttachPoint(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	addChannel(t, h.store, fundingA, store.Relevant)
	sourceID := addSpend(t, h.store, store.BranchSQ)
	if err := h.store.AddWatchOutpoint(ctx, store.WatchOutpoint{
		Branch: store.BranchSQ, TxID: commitTX, Vout: 0, ScriptHex: "51",
		SourceSpendEventID: sourceID, Role: store.RoleToLocal,
	}); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	first, _ := h.plant("funding-spend", fundingOutpoint(t, fundingA, 1))
	h.waitFor("the first spend", func() bool { return len(h.spends()) >= 2 })
	second, _ := h.plant("sweep", fundingOutpoint(t, commitTX, 0))
	h.waitFor("both spends", func() bool { return len(h.spends()) >= 3 })

	recordBranchBlocks(t, h.store, h.view, second.Height)
	h.view.Reorg(first.Height-1, "replacement", 4)

	h.waitFor("both spends to be marked as gone", func() bool {
		var gone int
		for _, sp := range h.spends() {
			if sp.Status == store.SpendReorgedOut {
				gone++
			}
		}
		return gone == 2
	})
}

// A high-water mark that cannot be read is treated as no mark at all. Starting
// again from the tip loses a little history that the rescan will pick up;
// scanning forward from a block that may not be on this chain would be wrong in
// a way nothing downstream could detect.
func TestAnUnreadableMarkStartsAgainFromTheTip(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	if err := h.store.SetMetaInt64(ctx, store.MetaLastScannedSQHeight, 5); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetMeta(ctx, store.MetaLastScannedSQHash, "not a hash"); err != nil {
		t.Fatal(err)
	}

	h.run()
	h.waitFor("scanning to start from the tip", func() bool {
		return h.w.Progress().Height == h.view.Tip().Height
	})
}

// The chain view refusing to say what it is following is a stall, not a crash:
// the daemon stays up and says it cannot see.
func TestAChainThatWillNotAnswerItsHistoryStalls(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	// A tip that does not follow what was last processed, and no way to walk back.
	h.view.Fail("BlockHeaderByHash", errors.New("not answering"))
	h.view.Reorg(4, "elsewhere", 3)

	h.waitFor("the watcher to report itself stuck", func() bool {
		return h.w.Progress().Stalled
	})
}

// A view that cannot even be subscribed to is a startup failure worth reporting,
// not something to loop on quietly.
func TestAViewThatCannotBeFollowedIsAnError(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	h.view.Fail("SubscribeTip", errors.New("no"))

	err := h.w.Run(context.Background())
	if err == nil {
		t.Fatal("a view that could not be followed was accepted")
	}
}

// Shutdown closes the store while the watcher is running. Every path here meets
// a database that is no longer there, and none of them may panic.
func TestTheStoreClosingUnderneathIsSurvived(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	h.plant("spent", fundingOutpoint(t, fundingA, 1))
	h.bus.Publish(bus.ChannelUpserted{New: true, Channel: bus.ChannelJSON{ID: 1}})
	h.view.Extend("more", 2)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting the loop meet a dead store

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutting down reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not stop after its store went away")
	}
}

// A script that will not decode is treated as absent rather than as an error.
// It costs nothing on a full node, which matches outpoints, and the readiness
// check says so on the tier where it matters.
func TestAnUnreadableScriptIsTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	if got := decodeScript("not hex"); got != nil {
		t.Errorf("decoded %q from nonsense", got)
	}
	if got := decodeScript(""); got != nil {
		t.Errorf("decoded %q from nothing", got)
	}
	if got := decodeScript("51"); len(got) != 1 || got[0] != 0x51 {
		t.Errorf("a usable script decoded to %x", got)
	}
}

func TestSerialisingATransactionThatIsNotThere(t *testing.T) {
	t.Parallel()

	if got := rawTx(nil); got != "" {
		t.Errorf("serialised %q from no transaction", got)
	}
	if got := rawTx(spend(outpoint(t, 0x11, 0))); got == "" {
		t.Error("a real transaction serialised to nothing")
	}
}

// The gate is what makes the light-client tier possible: it says a block cannot
// contain anything of interest, and the block is then never downloaded. On a
// full node it always says maybe, which costs one call.
func TestABlockTheGateRulesOutIsNeverFetched(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.view.SetMatches(false)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	start := h.w.Progress().Height

	// The block does contain a spend of a watched outpoint. The gate says it does
	// not, and the watcher believes it — which is correct: a filter that lies is a
	// backend problem, not something to second-guess per block.
	h.view.Fail("Block", errors.New("this block must not be fetched"))
	h.plant("ruled-out", fundingOutpoint(t, fundingA, 1))

	h.waitFor("the block to be passed over", func() bool {
		return h.w.Progress().Height == start+1
	})
	if len(h.spends()) != 0 {
		t.Error("a block the gate ruled out was fetched anyway")
	}
	if h.w.Progress().Stalled {
		t.Error("passing over a block was treated as a failure")
	}
}

// A gate that will not answer is a failure like any other, and stalls rather
// than being taken as "nothing here".
func TestAGateThatWillNotAnswerStalls(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	h.view.Fail("MatchBlock", errors.New("not answering"))
	h.view.Extend("unanswerable", 1)

	h.waitFor("the watcher to report itself stuck", func() bool {
		return h.w.Progress().Stalled
	})
}

// With no channels there is nothing to look for, and blocks still go past. The
// mark advances: those blocks genuinely contained nothing of ours, and reading
// them again later would find the same nothing more slowly.
func TestWithNothingToWatchBlocksStillGoPast(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	start := h.w.Progress().Height

	h.view.Extend("quiet", 3)
	h.waitFor("the blocks to be passed over", func() bool {
		return h.w.Progress().Height == start+3
	})
	if h.w.WatchSet().Len() != 0 {
		t.Error("something was being watched with no channels configured")
	}
}

// commitmentSpending builds a transaction with the marks a commitment cannot
// avoid leaving, spending the given outpoint.
func commitmentSpending(prevout wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prevout,
		Witness:          fundingSpendWitness(),
		Sequence:         0x80_ab_cd_ef,
	})
	tx.AddTxOut(wire.NewTxOut(500_000, p2wsh))
	tx.AddTxOut(wire.NewTxOut(AnchorValueSat, p2wsh))
	tx.LockTime = 0x20_12_34_56
	return tx
}

// justiceSpending builds a transaction taking the revocation branch of a
// contested output, which is what answering a breach looks like on the chain.
func justiceSpending(prevout wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: prevout,
		Witness:          wire.TxWitness{{0x30, 0x44}, {0x01}, {0x63, 0x21}},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(wire.NewTxOut(490_000, p2wpkh))
	return tx
}

// The whole point of the second-order half: a commitment appears, and Forktower
// then reports how it ended rather than only that it happened.
func TestACommitmentIsFollowedByWhatBecomesOfIt(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	first := h.view.Extend("commitment", 1)
	h.view.PutTransactions(first.Hash, coinbase(), commitment)

	h.waitFor("the commitment to be recorded", func() bool { return len(h.spends()) == 1 })
	if got := h.spends()[0].Shape; got != store.ShapeCommitmentUnknown {
		t.Errorf("the commitment was recorded as %q", got)
	}

	// Its outputs are now watched in their own right, which is what makes the
	// outcome observable at all.
	h.waitFor("the commitment's outputs to be watched", func() bool {
		return h.w.WatchSet().Len() == 3 // the funding output plus two commitment outputs
	})
	watched, err := h.store.ListWatchOutpoints(context.Background(), store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != 2 {
		t.Fatalf("recorded %d outputs to watch, want 2", len(watched))
	}
	for _, o := range watched {
		if o.SourceSpendEventID != h.spends()[0].ID {
			t.Error("a watched output does not point back at the commitment that made it")
		}
	}

	// And the answer to it, in a later block.
	justice := justiceSpending(wire.OutPoint{Hash: commitment.TxHash(), Index: 0})
	second := h.view.Extend("justice", 1)
	h.view.PutTransactions(second.Hash, coinbase(), justice)

	h.waitFor("the justice transaction", func() bool { return len(h.spends()) == 2 })
	var found bool
	for _, sp := range h.spends() {
		if sp.SpendTxID == justice.TxHash().String() {
			found = true
			if sp.Shape != store.ShapeJustice {
				t.Errorf("the justice transaction was recorded as %q", sp.Shape)
			}
		}
	}
	if !found {
		t.Error("the spend of the commitment's output was not recorded")
	}
}

// The case a single scan cannot answer on its own: a sweep in the
// very same block as the commitment it sweeps. The first pass cannot find it,
// because the output did not exist until the commitment was read. The second
// pass can, and this is what proves the loop makes one.
func TestASweepInTheSameBlockAsItsCommitmentIsFound(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	justice := justiceSpending(wire.OutPoint{Hash: commitment.TxHash(), Index: 0})

	meta := h.view.Extend("both-at-once", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), commitment, justice)

	h.waitFor("both the commitment and its answer", func() bool { return len(h.spends()) == 2 })

	shapes := map[store.SpendShape]bool{}
	for _, sp := range h.spends() {
		shapes[sp.Shape] = true
		if sp.BlockHeight != meta.Height {
			t.Errorf("a spend was recorded at height %d, want %d", sp.BlockHeight, meta.Height)
		}
	}
	if !shapes[store.ShapeCommitmentUnknown] || !shapes[store.ShapeJustice] {
		t.Errorf("found %v, want a commitment and the justice answering it", shapes)
	}
}

// A commitment the user's own node says it broadcast is theirs, and must not be
// reported as a stranger force-closing their channel.
func TestOurOwnForceCloseIsNotReportedAsAnAttack(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	id := addChannel(t, h.store, fundingA, store.Relevant)
	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	if err := h.store.SetChannelCloseSF(ctx, id, store.ClosePending,
		commitment.TxHash().String(), 0, 1); err != nil {
		t.Fatal(err)
	}

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	meta := h.view.Extend("our-force-close", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), commitment)

	h.waitFor("the commitment", func() bool { return len(h.spends()) == 1 })
	if got := h.spends()[0].Shape; got != store.ShapeCommitmentOurs {
		t.Errorf("our own force close was recorded as %q", got)
	}
}

// A cooperative close is recorded as one, so the user is not told a channel
// they closed on purpose was attacked.
func TestACooperativeCloseIsRecognised(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: fundingOutpoint(t, fundingA, 1),
		Witness:          fundingSpendWitness(),
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(wire.NewTxOut(500_000, p2wpkh))
	tx.AddTxOut(wire.NewTxOut(400_000, p2wpkh))

	meta := h.view.Extend("coop", 1)
	h.view.PutTransactions(meta.Hash, coinbase(), tx)

	h.waitFor("the close", func() bool { return len(h.spends()) == 1 })
	if got := h.spends()[0].Shape; got != store.ShapeMutualClose {
		t.Errorf("a cooperative close was recorded as %q", got)
	}
	// Nothing to follow: a cooperative close pays people directly and leaves no
	// contested output behind.
	watched, err := h.store.ListWatchOutpoints(context.Background(), store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != 0 {
		t.Errorf("a cooperative close left %d outputs being watched", len(watched))
	}
}

// A justice transaction is proof of what the commitment it answered was. That
// is the one time "whose commitment was that" gets a real answer after the fact,
// and it is the difference between telling a user something worrying happened
// and telling them what it was.
func TestAJusticeTransactionSettlesWhatTheCommitmentWas(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	first := h.view.Extend("commitment", 1)
	h.view.PutTransactions(first.Hash, coinbase(), commitment)
	h.waitFor("the commitment", func() bool { return len(h.spends()) == 1 })

	if got := h.spends()[0].Shape; got != store.ShapeCommitmentUnknown {
		t.Fatalf("the commitment was recorded as %q", got)
	}

	justice := justiceSpending(wire.OutPoint{Hash: commitment.TxHash(), Index: 0})
	second := h.view.Extend("justice", 1)
	h.view.PutTransactions(second.Hash, coinbase(), justice)

	h.waitFor("the commitment to be settled as a revoked one", func() bool {
		for _, sp := range h.spends() {
			if sp.SpendTxID == commitment.TxHash().String() {
				return sp.Shape == store.ShapeCommitmentRevoked
			}
		}
		return false
	})
}

// A commitment the user's own node said it broadcast keeps that label even when
// it turns out to have been revoked: "we published a revoked commitment" is a
// different and more useful thing to know than "it was revoked".
func TestOurOwnCommitmentKeepsItsLabelEvenWhenAnswered(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	ctx := context.Background()

	id := addChannel(t, h.store, fundingA, store.Relevant)
	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	if err := h.store.SetChannelCloseSF(ctx, id, store.ClosePending,
		commitment.TxHash().String(), 0, 1); err != nil {
		t.Fatal(err)
	}

	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	first := h.view.Extend("our-commitment", 1)
	h.view.PutTransactions(first.Hash, coinbase(), commitment)
	h.waitFor("the commitment", func() bool { return len(h.spends()) == 1 })

	justice := justiceSpending(wire.OutPoint{Hash: commitment.TxHash(), Index: 0})
	second := h.view.Extend("justice", 1)
	h.view.PutTransactions(second.Hash, coinbase(), justice)
	h.waitFor("the justice transaction", func() bool { return len(h.spends()) == 2 })

	for _, sp := range h.spends() {
		if sp.SpendTxID == commitment.TxHash().String() &&
			sp.Shape != store.ShapeCommitmentOurs {
			t.Errorf("our own commitment was relabelled %q", sp.Shape)
		}
	}
}
