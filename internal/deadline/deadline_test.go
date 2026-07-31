package deadline

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

const (
	fundingA = "1111111111111111111111111111111111111111111111111111111111111111"
	commitTX = "2222222222222222222222222222222222222222222222222222222222222222"
	justiceT = "3333333333333333333333333333333333333333333333333333333333333333"
)

type harness struct {
	t     *testing.T
	store *store.Store
	bus   *bus.Bus
	eng   *Engine
	clock *atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	eng, err := New(st, b, store.BranchSQ, nil,
		func() time.Time { return time.Unix(clock.Load(), 0) })
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	return &harness{t: t, store: st, bus: b, eng: eng, clock: clock}
}

func (h *harness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.eng.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			h.t.Error("the engine did not stop")
		}
	})
}

func (h *harness) waitFor(what string, cond func() bool) {
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

func (h *harness) deadlines(state store.DeadlineState) []store.Deadline {
	h.t.Helper()
	got, err := h.store.ListDeadlines(context.Background(), state)
	if err != nil {
		h.t.Fatalf("reading deadlines: %v", err)
	}
	return got
}

// channelWithDelays sets up a channel and returns its id.
func (h *harness) channelWithDelays(local, remote *int32) int64 {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: "02node", Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		h.t.Fatal(err)
	}
	id, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: fundingA, FundingVout: 0,
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors,
		CSVDelayLocal: local, CSVDelayRemote: remote, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// confirmHeight is the block every staged commitment confirms in. A constant
// because every deadline in these tests is measured from it, and a number that
// moved between cases would make the expected heights unreadable.
const confirmHeight int32 = 500

// commitmentAt records a confirmed commitment and returns its spend id.
func (h *harness) commitmentAt(channelID int64, shape store.SpendShape) int64 {
	h.t.Helper()
	id, _, err := h.store.RecordSpend(context.Background(), store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: fundingA, OutpointVout: 0,
		SpendTxID: commitTX, SpendTxHex: "00",
		BlockHash: "aa", BlockHeight: confirmHeight,
		Shape: shape, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) announceSpend(spendID, channelID int64, shape store.SpendShape) {
	h.bus.Publish(bus.FundingSpent{
		SpendEventID: spendID, ChannelID: channelID, Branch: string(store.BranchSQ),
		SpendTxid: commitTX, Shape: string(shape),
		Status: string(store.SpendConfirmed), Height: confirmHeight,
	})
}

func (h *harness) extendSQ(height int32, intervalSecs float64) {
	h.bus.Publish(bus.SplitBranchExtended{
		Branch:          string(store.BranchSQ),
		Block:           bus.BlockMetaJSON{Height: height},
		AvgIntervalSecs: intervalSecs,
	})
}

func ptr(v int32) *int32 { return &v }

// A confirmed commitment starts a countdown, and says so at once. The moment it
// confirms is when the user has the most time and therefore the most options;
// waiting for a threshold before saying anything would spend the part of the
// window that was worth the most.
func TestAConfirmedCommitmentStartsACountdownAndSaysSoAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	events := h.bus.Subscribe("test", bus.KindDeadlineEscalated)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	select {
	case e := <-events:
		got, ok := e.(bus.DeadlineEscalated)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.Level != int(LevelDetected) {
			t.Errorf("first warning was level %d, want %d", got.Level, LevelDetected)
		}
		if got.ChannelID != channelID {
			t.Errorf("named channel %d", got.ChannelID)
		}
		if got.RemainingBlocks != 1000 {
			t.Errorf("said %d blocks remain, want 1000", got.RemainingBlocks)
		}
		// Never a block count on its own: a minority chain can take half an hour a
		// block, and letting somebody assume ten minutes is the sort of help that
		// costs money.
		if got.EstWallClock == "" {
			t.Error("the warning gave a block count with no idea what it means in time")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a confirmed commitment raised no warning at all")
	}

	h.waitFor("the countdown to be recorded", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})
	d := h.deadlines(store.DeadlineCounting)[0]
	if d.DeadlineHeight != 1500 {
		t.Errorf("deadline at height %d, want 1500", d.DeadlineHeight)
	}
	if d.Assumed {
		t.Error("a countdown built on a real delay was flagged as assumed")
	}
}

// A cooperative close puts nobody on a timer: it pays both sides directly and
// there is nothing to contest.
func TestACooperativeCloseStartsNoCountdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeMutualClose)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeMutualClose)
	h.extendSQ(501, 600)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if got := h.deadlines(store.DeadlineCounting); len(got) != 0 {
		t.Errorf("a cooperative close started %d countdowns", len(got))
	}
}

// The user's own chain is their node's business; this engine counts the chain
// nobody else is watching.
func TestASpendOnTheUsersOwnChainIsNotCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.bus.Publish(bus.FundingSpent{
		SpendEventID: spendID, ChannelID: channelID, Branch: string(store.BranchSF),
		Shape: string(store.ShapeCommitmentUnknown), Status: string(store.SpendConfirmed),
	})
	h.extendSQ(501, 600)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if got := h.deadlines(store.DeadlineCounting); len(got) != 0 {
		t.Errorf("a spend on the user's own chain started %d countdowns", len(got))
	}
}

// An unconfirmed sighting is early warning, not a clock: the delay it would
// start is measured from a block it is not in yet.
func TestAnUnconfirmedSightingStartsNoCountdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.bus.Publish(bus.FundingSpent{
		SpendEventID: spendID, ChannelID: channelID, Branch: string(store.BranchSQ),
		Shape: string(store.ShapeCommitmentUnknown), Status: string(store.SpendMempool),
	})
	h.extendSQ(501, 600)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if got := h.deadlines(store.DeadlineCounting); len(got) != 0 {
		t.Errorf("an unconfirmed sighting started %d countdowns", len(got))
	}
}

// Each tier is reached once and stays reached. Re-announcing the same tier on
// every block would bury the moment it actually changed.
func TestEachTierIsReachedExactlyOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(100))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	events := h.bus.Subscribe("test", bus.KindDeadlineEscalated)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	// Walk the chain forward through the whole window, a block at a time.
	for height := int32(501); height <= 600; height++ {
		h.extendSQ(height, 600)
	}
	h.waitFor("the countdown to run out", func() bool {
		return len(h.deadlines(store.DeadlineExpired)) == 1
	})

	levels := map[int]int{}
	for {
		select {
		case e := <-events:
			if got, ok := e.(bus.DeadlineEscalated); ok {
				levels[got.Level]++
			}
			continue
		default:
		}
		break
	}

	for _, level := range []int{int(LevelDetected), int(LevelHalf), int(LevelUrgent)} {
		if levels[level] != 1 {
			t.Errorf("level %d was raised %d times, want exactly once", level, levels[level])
		}
	}
}

// The countdown running out with nobody having answered it is the loss this
// software exists to notice.
func TestACountdownRunningOutIsALoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(10))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	losses := h.bus.Subscribe("test", bus.KindDeadlineExpiredLoss)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	h.extendSQ(510, 600)

	select {
	case e := <-losses:
		got, ok := e.(bus.DeadlineExpiredLoss)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.ChannelID != channelID {
			t.Errorf("named channel %d", got.ChannelID)
		}
		if got.AmountSat != 1_000_000 {
			t.Errorf("said %d satoshis were at stake, want the channel's capacity",
				got.AmountSat)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a countdown ran out with no loss recorded")
	}

	h.waitFor("the countdown to be marked as run out", func() bool {
		return len(h.deadlines(store.DeadlineExpired)) == 1
	})
}

// The distinction the specification did not draw, and the one that would tell
// somebody they had been robbed at the moment their own money became spendable.
//
// A commitment the peer published leaves *their* output waiting, and the end of
// that wait is when they take the money. Our own commitment leaves *our* output
// waiting, and the end of that wait is when we can claim it.
func TestOurOwnCommitmentRunningOutIsNotALoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(10), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentOurs)

	losses := h.bus.Subscribe("test", bus.KindDeadlineExpiredLoss)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentOurs)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})
	// Our own delay, not theirs.
	if got := h.deadlines(store.DeadlineCounting)[0].DeadlineHeight; got != 510 {
		t.Errorf("deadline at %d, want our own delay of ten blocks", got)
	}

	h.extendSQ(510, 600)
	h.waitFor("the wait to finish", func() bool {
		return len(h.deadlines(store.DeadlineExpired)) == 1
	})

	select {
	case e := <-losses:
		t.Errorf("waiting out our own delay was announced as a loss: %+v", e)
	default:
	}
}

// A justice transaction answering the commitment stops the clock.
func TestAJusticeTransactionStopsTheClock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	resolved := h.bus.Subscribe("test", bus.KindDeadlineResolved)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	justiceID, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, OutpointTxID: commitTX, OutpointVout: 0,
		SpendTxID: justiceT, SpendTxHex: "00", BlockHash: "bb", BlockHeight: 600,
		Shape: store.ShapeJustice, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: justiceID, SourceSpendEventID: spendID,
		Role: string(store.RoleToLocal), Shape: string(store.ShapeJustice),
	})

	select {
	case e := <-resolved:
		got, ok := e.(bus.DeadlineResolved)
		if !ok {
			t.Fatalf("got %T", e)
		}
		// Named, so the record says what answered it.
		if got.ByTxid != justiceT {
			t.Errorf("resolved by %q, want the justice transaction", got.ByTxid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a countdown that was answered never stopped")
	}

	h.waitFor("the countdown to be marked as answered", func() bool {
		return len(h.deadlines(store.DeadlineResolved)) == 1 &&
			len(h.deadlines(store.DeadlineCounting)) == 0
	})
}

// Watching the contested output leave is better information than counting to a
// number that may have been a guess. A deadline built on the assumed floor can
// sit later than the real one, so without this the money goes and the countdown
// carries on saying there is time.
func TestASweepOfTheContestedOutputEndsTheCountdownAsALoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	losses := h.bus.Subscribe("test", bus.KindDeadlineExpiredLoss)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	// Far short of the deadline at 5500, which is the point.
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 99, SourceSpendEventID: spendID,
		Role: string(store.RoleToLocal), Shape: string(store.ShapeDelayedSweep),
	})

	select {
	case <-losses:
	case <-time.After(5 * time.Second):
		t.Fatal("the contested output was swept and nothing was said")
	}
	h.waitFor("the countdown to end", func() bool {
		return len(h.deadlines(store.DeadlineExpired)) == 1
	})
	// And never as though it had been answered.
	if len(h.deadlines(store.DeadlineResolved)) != 0 {
		t.Error("somebody taking the money was read as the countdown being answered")
	}
}

// The same event on our own commitment is the opposite: the wait ended and we
// collected. Announcing a loss there would be the same false alarm arriving by
// another route.
func TestASweepOfOurOwnOutputIsNotALoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentOurs)

	losses := h.bus.Subscribe("test", bus.KindDeadlineExpiredLoss)
	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentOurs)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 99, SourceSpendEventID: spendID,
		Role: string(store.RoleToLocal), Shape: string(store.ShapeDelayedSweep),
	})

	h.waitFor("the countdown to be settled", func() bool {
		return len(h.deadlines(store.DeadlineResolved)) == 1
	})
	select {
	case e := <-losses:
		t.Errorf("collecting our own funds was announced as a loss: %+v", e)
	default:
	}
}

// Payments in flight have their own clocks, and a sweep of the commitment's
// contested output says nothing about them.
func TestASweepDoesNotSettleThePaymentsInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	if err := h.store.ReplaceHTLCSnapshot(ctx, channelID, 1, []store.HTLCSnapshot{
		{Direction: "incoming", AmountMsat: 1000, CLTVExpiry: 900},
	}); err != nil {
		t.Fatal(err)
	}
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("both countdowns", func() bool { return h.eng.Status().Counting == 2 })

	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 99, SourceSpendEventID: spendID,
		Shape: string(store.ShapeDelayedSweep),
	})

	h.waitFor("the commitment's clock to end", func() bool {
		return len(h.deadlines(store.DeadlineExpired)) == 1
	})
	running := h.deadlines(store.DeadlineCounting)
	if len(running) != 1 || running[0].Kind != store.DeadlineHTLCIncoming {
		t.Errorf("the payment's own clock was settled too: %+v", running)
	}
}

// A countdown started from a floor must not stay on that floor for ever. The
// rule that a missing delay produces a conservative deadline rather than none is
// only half an answer; the other half is replacing the guess when the truth
// arrives.
func TestACountdownIsBroughtUpToDateWhenTheDelayBecomesKnown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// A channel first seen while it was already closing carries almost nothing.
	channelID := h.channelWithDelays(nil, nil)
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	h.waitFor("a countdown on the floor", func() bool { return h.eng.Status().Assumed == 1 })
	if got := h.deadlines(store.DeadlineCounting)[0].DeadlineHeight; got != 500+144 {
		t.Fatalf("the floor put the deadline at %d", got)
	}

	// The next full poll fills the delay in.
	if _, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: fundingA, FundingVout: 0,
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors,
		CSVDelayLocal: ptr(144), CSVDelayRemote: ptr(2016), UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(bus.ChannelUpserted{Channel: bus.ChannelJSON{ID: channelID}})

	h.waitFor("the countdown to be brought up to date", func() bool {
		got := h.deadlines(store.DeadlineCounting)
		return len(got) == 1 && got[0].DeadlineHeight == 500+2016
	})
	got := h.deadlines(store.DeadlineCounting)[0]
	if got.Assumed {
		t.Error("the countdown is still flagged as a guess")
	}
	// The tier already reached is not walked back: a warning the user has
	// already had must not be un-said.
	if got.Escalation < LevelDetected {
		t.Errorf("the escalation tier was reset to %d", got.Escalation)
	}
	h.waitFor("the dashboard to stop reporting an assumption", func() bool {
		return h.eng.Status().InputsKnown()
	})
}

// A close that leaves the chain does not stop the clock at once. It may confirm
// again within a block or two — a counterparty replacing a breach with a higher
// fee looks exactly like this — and a countdown dropped at the first sign of a
// reorganisation would stop precisely when it mattered.
func TestACloseThatLeavesTheChainIsGivenTimeToComeBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})

	if err := h.store.UpdateSpendStatus(ctx, spendID, store.SpendReorgedOut,
		"aa", 500, 2); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(bus.SpendReorgedOut{SpendEventID: spendID, Branch: string(store.BranchSQ)})

	// A few blocks later it is still counting.
	h.extendSQ(520, 600)
	time.Sleep(30 * time.Millisecond) //nolint:forbidigo // proving an absence
	if len(h.deadlines(store.DeadlineCounting)) != 1 {
		t.Fatal("a countdown was dropped the moment its close left the chain")
	}

	// Far enough past it that it is not coming back.
	h.extendSQ(500+ReorgPatienceBlocks+1, 600)
	h.waitFor("the countdown to be retired", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 0
	})

	// Retired as resolved, not expired: the commitment that started this clock is
	// no longer on the chain, so there is nothing left to lose. Calling it expired
	// would put the word for the bad outcome on a harmless event.
	if len(h.deadlines(store.DeadlineResolved)) != 1 {
		t.Errorf("retired as %v, want it recorded as nothing left to lose",
			h.deadlines(store.DeadlineExpired))
	}
}

// A missing delay must be visible before anything goes wrong, which is the only
// time it can still be fixed.
func TestAnAssumedDelayIsVisibleBeforeItMatters(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(nil, nil)
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	h.waitFor("the assumption to be visible", func() bool {
		return h.eng.Status().Assumed == 1
	})
	status := h.eng.Status()
	if status.InputsKnown() {
		t.Error("a countdown on a floor was reported as fully known")
	}
	if len(status.AssumedChannels) != 1 || status.AssumedChannels[0] != channelID {
		t.Errorf("the assumption was not attributed to a channel: %+v", status.AssumedChannels)
	}
	if status.Counting != 1 || status.EarliestHeight != 500+store.AssumedDeadlineFloor {
		t.Errorf("status = %+v", status)
	}
}

func TestStatusIsCleanWhenEveryInputIsKnown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })
	if !h.eng.Status().InputsKnown() {
		t.Error("a countdown built on real numbers was reported as assumed")
	}
}

// Payments in flight are counted too, and the soonest of everything is what the
// status reports.
func TestPaymentsInFlightAreCountedAndTheSoonestLeads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	if err := h.store.ReplaceHTLCSnapshot(ctx, channelID, 1, []store.HTLCSnapshot{
		{Direction: "incoming", AmountMsat: 1000, CLTVExpiry: 700},
		{Direction: "outgoing", AmountMsat: 2000, CLTVExpiry: 900},
	}); err != nil {
		t.Fatal(err)
	}
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	// Waiting on the engine's own view, not on the rows: the third row lands a
	// moment before the status that summarises it, and reading the store first
	// would be reading between the two writes.
	h.waitFor("all three countdowns", func() bool {
		return h.eng.Status().Counting == 3
	})
	if got := h.eng.Status().EarliestHeight; got != 700 {
		t.Errorf("the soonest deadline is at %d, want the incoming payment's 700", got)
	}
}

// Repeating the same event must not start a second clock beside the first. Two
// countdowns for one commitment would disagree on the same screen.
func TestRepeatingTheEventDoesNotStartASecondClock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	for range 4 {
		h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	}

	h.waitFor("the countdown", func() bool {
		return len(h.deadlines(store.DeadlineCounting)) == 1
	})
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting the repeats land
	if got := h.deadlines(store.DeadlineCounting); len(got) != 1 {
		t.Errorf("four announcements produced %d countdowns", len(got))
	}
}

// With no cadence known, nothing is said about time — rather than letting
// somebody assume ten minutes a block on a chain that is taking forty.
func TestNoCadenceMeansNoClaimAboutTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	events := h.bus.Subscribe("test", bus.KindDeadlineEscalated)
	h.run()
	h.extendSQ(500, 0)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)

	select {
	case e := <-events:
		got, _ := e.(bus.DeadlineEscalated)
		if got.EstWallClock != "" {
			t.Errorf("claimed %q about time with no cadence to go on", got.EstWallClock)
		}
		if got.RemainingBlocks != 1000 {
			t.Errorf("remaining = %d", got.RemainingBlocks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no warning was raised")
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := bus.New(nil)
	t.Cleanup(b.Close)

	if _, err := New(nil, b, store.BranchSQ, nil, nil); err == nil {
		t.Error("an engine with no store was accepted")
	}
	if _, err := New(st, nil, store.BranchSQ, nil, nil); err == nil {
		t.Error("an engine with no bus was accepted")
	}
	if _, err := New(st, b, "elsewhere", nil, nil); err == nil {
		t.Error("an engine on an unknown branch was accepted")
	}
	if _, err := New(st, b, store.BranchSQ, nil, nil); err != nil {
		t.Errorf("a usable engine was refused: %v", err)
	}
}

// Shutdown closes the store while the engine is running: none of these paths may
// panic, and the daemon must still stop.
func TestTheStoreClosingUnderneathIsSurvived(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.Run(ctx) }()

	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.extendSQ(600, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 1, SourceSpendEventID: spendID, Shape: string(store.ShapeJustice),
	})
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting it meet a dead store

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutting down reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine did not stop after its store went away")
	}
}

// A commitment on a channel the registry never recorded still gets a countdown.
// The delay is unknown, so it runs from the floor — but a channel we know less
// about is not a channel to stop watching.
func TestACommitmentWithNoChannelBehindItStillCounts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spendID := h.commitmentAt(0, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, 0, store.ShapeCommitmentUnknown)

	h.waitFor("a countdown on the floor", func() bool {
		return h.eng.Status().Counting == 1
	})
	d := h.deadlines(store.DeadlineCounting)[0]
	if !d.Assumed || d.DeadlineHeight != confirmHeight+store.AssumedDeadlineFloor {
		t.Errorf("got %+v, want a flagged floor", d)
	}
}

// A spend the engine is told about but cannot find is a reason to say so, not to
// invent a countdown from nothing.
func TestASpendThatCannotBeFoundStartsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(9999, channelID, store.ShapeCommitmentUnknown)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if got := h.deadlines(store.DeadlineCounting); len(got) != 0 {
		t.Errorf("a spend that does not exist started %d countdowns", len(got))
	}
}

// Blocks on the user's own chain do not move a countdown that is measuring the
// other one.
func TestBlocksOnTheOtherChainDoNotMoveTheCountdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(10))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	// Far past the deadline, but on the wrong chain.
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSF),
		Block:  bus.BlockMetaJSON{Height: 9999},
	})
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if len(h.deadlines(store.DeadlineExpired)) != 0 {
		t.Error("a block on the user's own chain ran out a countdown on the other one")
	}
}

// A justice transaction answering something no countdown is measuring is not an
// error, and must not stop an unrelated one.
func TestJusticeForSomethingElseStopsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 42, SourceSpendEventID: 4242, Shape: string(store.ShapeJustice),
	})
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 42, SourceSpendEventID: 0, Shape: string(store.ShapeJustice),
	})

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.eng.Status().Counting != 1 {
		t.Error("a countdown was stopped by a justice transaction for something else")
	}
}

// A reorganisation on the user's own chain says nothing about a countdown
// measuring the other one.
func TestAReorgOnTheOtherChainIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	h.bus.Publish(bus.SpendReorgedOut{SpendEventID: spendID, Branch: string(store.BranchSF)})
	h.extendSQ(501, 600)

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.eng.Status().Counting != 1 {
		t.Error("a reorganisation on the user's own chain disturbed the other chain's clock")
	}
}

// The event bus closing stops the engine cleanly rather than spinning.
func TestTheBusClosingStopsTheEngine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.Run(ctx) }()

	h.bus.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopping reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine kept running after the bus closed")
	}
}

// A day is a day, whichever side of the boundary it falls.
func TestTheWordingForADayAndForHours(t *testing.T) {
	t.Parallel()

	if got := HumanDuration(36*time.Hour + time.Minute); got != "about 2 days" {
		t.Errorf("just over 36 hours reads as %q", got)
	}
	if got := HumanDuration(25 * time.Hour); got != "about 25 hours" {
		t.Errorf("25 hours reads as %q", got)
	}
	if got := HumanDuration(59 * time.Second); got != aboutAMinute {
		t.Errorf("under a minute reads as %q", got)
	}
}

// A channel nobody is counting anything for is not worth recomputing, and a
// channel that does not exist is not a reason to fall over.
func TestRecomputingWhatIsNotThereChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })
	before := h.deadlines(store.DeadlineCounting)[0]

	// A channel with no id at all, one that does not exist, and one that has no
	// countdowns of its own.
	h.bus.Publish(bus.ChannelUpserted{Channel: bus.ChannelJSON{ID: 0}})
	h.bus.Publish(bus.ChannelUpserted{Channel: bus.ChannelJSON{ID: 9999}})

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	after := h.deadlines(store.DeadlineCounting)
	if len(after) != 1 || after[0].DeadlineHeight != before.DeadlineHeight {
		t.Errorf("a recompute for an unrelated channel moved a countdown: %+v", after)
	}
}

// A sweep collecting from something no countdown is measuring is not an error,
// and must not disturb an unrelated one.
func TestASweepOfSomethingElseSettlesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 99, SourceSpendEventID: 4242,
		Shape: string(store.ShapeDelayedSweep),
	})
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 99, SourceSpendEventID: 0,
		Shape: string(store.ShapeDelayedSweep),
	})

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.eng.Status().Counting != 1 {
		t.Error("a sweep of something else ended an unrelated countdown")
	}
}

// A shape that settles nothing settles nothing. An HTLC being claimed says
// nothing about the commitment's own window.
func TestAnUnrelatedSecondOrderShapeIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(5000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	h.run()
	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	for _, shape := range []store.SpendShape{
		store.ShapeHTLCClaim, store.ShapeUnknown, store.ShapeMutualClose,
	} {
		h.bus.Publish(bus.SecondOrderSpent{
			SpendEventID: 99, SourceSpendEventID: spendID, Shape: string(shape),
		})
	}

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if h.eng.Status().Counting != 1 {
		t.Error("a shape that settles nothing ended the countdown")
	}
}

// The recompute path against a store that has gone must not panic.
func TestRecomputingAgainstAClosedStoreIsSurvived(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	channelID := h.channelWithDelays(ptr(144), ptr(1000))
	spendID := h.commitmentAt(channelID, store.ShapeCommitmentUnknown)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.eng.Run(ctx) }()

	h.extendSQ(500, 600)
	h.announceSpend(spendID, channelID, store.ShapeCommitmentUnknown)
	h.waitFor("the countdown", func() bool { return h.eng.Status().Counting == 1 })

	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(bus.ChannelUpserted{Channel: bus.ChannelJSON{ID: channelID}})
	h.bus.Publish(bus.SecondOrderSpent{
		SpendEventID: 1, SourceSpendEventID: spendID,
		Shape: string(store.ShapeDelayedSweep),
	})
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting it meet a dead store

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutting down reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the engine did not stop after its store went away")
	}
}
