package mirror

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
)

type runnerHarness struct {
	t     *testing.T
	store *store.Store
	view  *chainviewtest.View
	chain *fakeChain
	bus   *bus.Bus
	run   *Runner
}

// testDeadline is how long a test waits on work happening in another goroutine.
//
// **Generous on purpose**, as in the watcher and registry packages for the same
// reason: on an idle machine these are met in milliseconds, so the limit
// measures the build host rather than the code. Five seconds failed a full-suite
// run under the race detector while passing every time in isolation, and a gate
// that fails on load is one people re-run rather than read.
const testDeadline = 30 * time.Second

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)

	view := chainviewtest.New("regtest")
	chain := &fakeChain{}

	observer, err := NewObserver(ObserverOptions{
		Store: st, View: view, From: store.BranchSF, To: store.BranchSQ,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := New(Options{Store: st, Target: chain, Branch: store.BranchSQ})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerOptions{
		Observer: observer, Mirror: sender, Bus: b, From: store.BranchSF,
		Interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &runnerHarness{t: t, store: st, view: view, chain: chain, bus: b, run: runner}
}

func (h *runnerHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.run.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("the runner stopped with an error: %v", err)
			}
		case <-time.After(testDeadline):
			h.t.Error("the runner did not stop when asked")
		}
	})
}

// channelSpending mirrors the observer harness's helper.
func (h *runnerHarness) channelSpending(tx *wire.MsgTx) int64 {
	h.t.Helper()
	ctx := context.Background()

	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		h.t.Fatal(err)
	}
	prev := tx.TxIn[0].PreviousOutPoint
	id, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: node, FundingTxID: prev.Hash.String(),
		//nolint:gosec // a test's output index
		FundingVout: int32(prev.Index),
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors,
		Relevance: store.Relevant, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *runnerHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.After(testDeadline)
	for !ok() {
		select {
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond): //nolint:forbidigo // polling for an effect
		}
	}
}

// A new block on the watched chain is read as it arrives, because a close the
// user is waiting on should not sit in a queue.
func TestABlockOnTheWatchedChainIsReadAsItArrives(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)
	h.start()

	meta := h.view.ExtendWith("with-the-close", tx)
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSF),
		Block:  bus.BlockMetaJSON{Hash: meta.Hash.String(), Height: meta.Height},
	})

	h.waitFor("the transaction to be decided about and sent", func() bool {
		return h.chain.took() > 0
	})

	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != store.MirrorAccepted {
		t.Errorf("decisions = %+v", rows)
	}
}

// Blocks on the chain this runner is not watching are the other runner's
// business. Acting on both would decide every transaction twice.
func TestABlockOnTheOtherChainIsIgnored(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)
	h.start()

	meta := h.view.ExtendWith("with-the-close", tx)
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSQ),
		Block:  bus.BlockMetaJSON{Hash: meta.Hash.String(), Height: meta.Height},
	})

	//nolint:forbidigo // proving an absence
	time.Sleep(100 * time.Millisecond)
	if h.chain.took() != 0 {
		t.Errorf("a block on the other chain was scanned anyway: %d sent", h.chain.took())
	}
}

// A refusal is recorded and nothing is sent. The database is where the user
// reads it; the log would drown in them.
func TestARefusedTransactionIsRecordedAndNotSent(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	tx := realClose(t, "force_close_commitment.hex")
	h.channelSpending(tx)
	h.start()

	meta := h.view.ExtendWith("with-their-close", tx)
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSF),
		Block:  bus.BlockMetaJSON{Hash: meta.Hash.String(), Height: meta.Height},
	})

	h.waitFor("the refusal to be recorded", func() bool {
		rows, err := h.store.ListMirrorDecisions(context.Background(),
			store.MirrorFilter{State: store.MirrorDenied})
		return err == nil && len(rows) == 1
	})
	if h.chain.took() != 0 {
		t.Errorf("a refused transaction was sent anyway: %d sent", h.chain.took())
	}
}

// The queue is worked through on a timer as well, because whether the other
// chain will now accept something depends on that chain rather than on this one.
func TestTheQueueIsRetriedOnItsOwn(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	tx := realClose(t, "coop_close.hex")
	channelID := h.channelSpending(tx)
	ctx := context.Background()

	raw, err := rawHex(tx)
	if err != nil {
		t.Fatal(err)
	}
	prev := tx.TxIn[0].PreviousOutPoint
	if _, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSF, ChannelID: channelID,
		OutpointTxID: prev.Hash.String(),
		//nolint:gosec // a test's output index
		OutpointVout: int32(prev.Index),
		SpendTxID:    tx.TxHash().String(), SpendTxHex: raw,
		Shape: store.ShapeMutualClose, Status: store.SpendConfirmed,
		BlockHeight: 100, FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.RecordMirrorDecision(ctx, store.MirrorDecision{
		TxID: tx.TxHash().String(), SourceBranch: store.BranchSF,
		TargetBranch: store.BranchSQ, ChannelID: channelID,
		Shape: store.ShapeMutualClose, Reason: "agreed close",
		State: store.MirrorPending, FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// No block is published at all: only the timer.
	h.start()
	h.waitFor("the queue to be worked through", func() bool {
		return h.chain.took() > 0
	})
}

// A chain that cannot be read stops nothing: the runner keeps going and the next
// block is tried.
func TestABlockThatCannotBeReadDoesNotStopTheRunner(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)
	h.start()

	// A height that does not exist.
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSF),
		Block:  bus.BlockMetaJSON{Height: 9999},
	})
	//nolint:forbidigo // letting the bad block be processed
	time.Sleep(50 * time.Millisecond)

	// And then a real one, which must still work.
	meta := h.view.ExtendWith("with-the-close", tx)
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSF),
		Block:  bus.BlockMetaJSON{Hash: meta.Hash.String(), Height: meta.Height},
	})
	h.waitFor("the good block to be handled", func() bool {
		return h.chain.took() > 0
	})
}

func TestARunnerNeedsItsParts(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := bus.New(nil)
	t.Cleanup(b.Close)

	var view chainview.ChainView = chainviewtest.New("regtest")
	observer, err := NewObserver(ObserverOptions{
		Store: st, View: view, From: store.BranchSF, To: store.BranchSQ,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := New(Options{Store: st, Target: &fakeChain{}, Branch: store.BranchSQ})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts RunnerOptions
	}{
		{"nothing to watch with", RunnerOptions{Mirror: sender, Bus: b, From: store.BranchSF}},
		{"nothing to send with", RunnerOptions{Observer: observer, Bus: b, From: store.BranchSF}},
		{"no bus", RunnerOptions{Observer: observer, Mirror: sender, From: store.BranchSF}},
		{"no chain named", RunnerOptions{Observer: observer, Mirror: sender, Bus: b}},
	} {
		if _, err := NewRunner(tc.opts); err == nil {
			t.Errorf("%s: a runner was built anyway", tc.name)
		}
	}
}

// A closed bus stops the runner cleanly rather than spinning on a dead channel.
func TestAClosedBusStopsTheRunner(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run.Run(ctx) }()

	h.bus.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stopping reported an error: %v", err)
		}
	case <-time.After(testDeadline):
		t.Error("the runner did not stop when its bus closed")
	}
}
