package mirror

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// fakeChain takes transactions, or refuses them however a test needs.
//
// Guarded because the runner tests drive it from its own goroutine while the
// test reads what arrived. Without the lock the race detector is right to
// complain, and the complaint would be about the test rather than the code.
type fakeChain struct {
	mu   sync.Mutex
	err  error
	sent []string
}

func (f *fakeChain) Broadcast(_ context.Context, tx *wire.MsgTx) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tx != nil {
		f.sent = append(f.sent, tx.TxHash().String())
	}
	return f.err
}

// refuse sets what the chain says next.
func (f *fakeChain) refuse(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// took is how many transactions it has been given.
func (f *fakeChain) took() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// lastTook is the id of the most recent one, or empty.
func (f *fakeChain) lastTook() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

type mirrorHarness struct {
	t     *testing.T
	store *store.Store
	chain *fakeChain
	m     *Mirror
	clock *atomic.Int64
}

func newMirrorHarness(t *testing.T) *mirrorHarness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)
	chain := &fakeChain{}

	m, err := New(Options{
		Store: st, Target: chain, Branch: store.BranchSQ,
		Now: func() time.Time { return time.Unix(clock.Load(), 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &mirrorHarness{t: t, store: st, chain: chain, m: m, clock: clock}
}

// queue puts a real transaction into the store the way the observer would, and
// records the decision to copy it.
func (h *mirrorHarness) queue(tx *wire.MsgTx) int64 {
	h.t.Helper()
	ctx := context.Background()

	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		h.t.Fatal(err)
	}
	prev := tx.TxIn[0].PreviousOutPoint
	channelID, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: node, FundingTxID: prev.Hash.String(),
		//nolint:gosec // a test's output index
		FundingVout: int32(prev.Index),
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}

	raw, err := rawHex(tx)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSF, ChannelID: channelID,
		OutpointTxID: prev.Hash.String(),
		//nolint:gosec // a test's output index
		OutpointVout: int32(prev.Index),
		SpendTxID:    tx.TxHash().String(), SpendTxHex: raw,
		Shape: store.ShapeMutualClose, Status: store.SpendConfirmed,
		BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		h.t.Fatal(err)
	}

	id, _, err := h.store.RecordMirrorDecision(ctx, store.MirrorDecision{
		TxID: tx.TxHash().String(), SourceBranch: store.BranchSF,
		TargetBranch: store.BranchSQ, ChannelID: channelID,
		Shape: store.ShapeMutualClose, Reason: "both of you agreed to close it",
		State: store.MirrorPending, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *mirrorHarness) decision(id int64) store.MirrorDecision {
	h.t.Helper()
	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	h.t.Fatalf("decision %d is gone", id)
	return store.MirrorDecision{}
}

func TestATransactionTheOtherChainAcceptsIsRecordedAsAccepted(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.queue(tx)

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].State != store.MirrorAccepted {
		t.Fatalf("outcomes = %+v", out)
	}
	if h.chain.took() != 1 || h.chain.lastTook() != tx.TxHash().String() {
		t.Errorf("the wrong bytes were sent: %v", h.chain.sent)
	}
	if got := h.decision(id); got.State != store.MirrorAccepted || got.Attempts != 1 {
		t.Errorf("recorded as %q after %d attempts", got.State, got.Attempts)
	}
}

// **The case Forktower cannot fix, and must not pretend it can.** No fee bump,
// no child-pays-for-parent, no re-signing: all three need keys this daemon
// refuses to hold.
func TestAFeeTooLowForTheOtherChainIsExplainedRatherThanFixed(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.queue(tx)
	h.chain.refuse(errors.New("min relay fee not met, 200 < 1100"))

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("outcomes = %+v", out)
	}
	if out[0].Rejection != RejectFeeTooLow {
		t.Errorf("rejection = %q, want %q", out[0].Rejection, RejectFeeTooLow)
	}
	if !strings.Contains(out[0].Note, "cannot raise it") {
		t.Errorf("the note does not admit what cannot be done: %q", out[0].Note)
	}
	if !strings.Contains(out[0].Note, "keep trying") {
		t.Errorf("the note does not say it will keep trying: %q", out[0].Note)
	}
	// Still waiting, not given up on: fees change.
	if got := h.decision(id); got.State != store.MirrorRejected {
		t.Errorf("state = %q, want it still being retried", got.State)
	}
	// And the node's own words are kept, for somebody who wants them.
	if got := h.decision(id); !strings.Contains(got.LastError, "min relay fee") {
		t.Errorf("the node's own words were lost: %q", got.LastError)
	}
}

// Retrying the same bytes cannot help some refusals, and a transaction retried
// forever is one nobody is ever told about.
func TestARefusalThatCannotChangeIsGivenUpOnAndSaidSo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		nodeSaid  string
		want      Rejection
		mustSay   string
		giveUpNow bool
	}{
		{"already spent over there", "txn-mempool-conflict", RejectConflict,
			"closed a different way", true},
		{"a shape that chain will not relay", "non-standard transaction",
			RejectNonStandard, "different rules", true},
		{"the parent is not there yet", "bad-txns-inputs-missingorspent",
			RejectMissingParent, "has not seen the transaction this one spends", false},
	} {
		h := newMirrorHarness(t)
		tx := realClose(t, "coop_close.hex")
		id := h.queue(tx)
		h.chain.refuse(errors.New(tc.nodeSaid))

		out, err := h.m.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Rejection != tc.want {
			t.Fatalf("%s: outcomes = %+v", tc.name, out)
		}
		if !strings.Contains(out[0].Note, tc.mustSay) {
			t.Errorf("%s: note %q does not say %q", tc.name, out[0].Note, tc.mustSay)
		}

		got := h.decision(id)
		if tc.giveUpNow {
			if got.State != store.MirrorAbandoned {
				t.Errorf("%s: state = %q, want it given up on", tc.name, got.State)
			}
			if !strings.Contains(out[0].Note, "stopped trying") {
				t.Errorf("%s: giving up was not said: %q", tc.name, out[0].Note)
			}
		} else if got.State != store.MirrorRejected {
			t.Errorf("%s: state = %q, want it still being retried", tc.name, got.State)
		}
	}
}

// Backing off is what stops a refused transaction being hammered at a node the
// user depends on.
func TestARefusedTransactionIsNotRetriedImmediately(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.queue(tx)
	h.chain.refuse(errors.New("min relay fee not met"))

	if _, err := h.m.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sentAfterFirst := h.chain.took()

	// Straight away: nothing.
	if _, err := h.m.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.chain.took() != sentAfterFirst {
		t.Error("a refused transaction was retried immediately")
	}

	// Once the wait has passed: tried again.
	h.clock.Add(int64(FirstDelay.Seconds()) + 1)
	if _, err := h.m.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.chain.took() != sentAfterFirst+1 {
		t.Errorf("after the wait, sent %d times, want %d",
			h.chain.took(), sentAfterFirst+1)
	}
}

// Enough tries and it is worth more to tell somebody than to try again.
func TestEventuallyItStopsTryingAndSaysWhy(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.queue(tx)
	h.chain.refuse(errors.New("min relay fee not met"))

	for range MaxAttempts + 2 {
		if _, err := h.m.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.clock.Add(int64(MaxDelay.Seconds()) + 1)
		if h.decision(id).State == store.MirrorAbandoned {
			break
		}
	}

	got := h.decision(id)
	if got.State != store.MirrorAbandoned {
		t.Fatalf("state = %q after %d attempts, want it given up on",
			got.State, got.Attempts)
	}
	if got.Attempts > MaxAttempts {
		t.Errorf("tried %d times, want no more than %d", got.Attempts, MaxAttempts)
	}
	// And it stays given up on rather than starting over.
	before := h.chain.took()
	h.clock.Add(int64(MaxDelay.Seconds()) + 1)
	if _, err := h.m.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.chain.took() != before {
		t.Error("a transaction that had been given up on was tried again")
	}
}

// A transaction refused is not a transaction lost: the record has to survive a
// restart, or the attempt count resets and it is retried forever in silence.
func TestAnAttemptSurvivesARestart(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.queue(tx)
	h.chain.refuse(errors.New("min relay fee not met"))

	if _, err := h.m.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A second engine over the same database, with no memory of the first.
	restarted, err := New(Options{
		Store: h.store, Target: h.chain, Branch: store.BranchSQ,
		Now: func() time.Time { return time.Unix(h.clock.Load(), 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.Add(int64(FirstDelay.Seconds()) + 1)
	if _, err := restarted.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := h.decision(id); got.Attempts != 2 {
		t.Errorf("attempts = %d across a restart, want 2", got.Attempts)
	}
}

// A refusal only the user's own node can see must be recorded verbatim, because
// the classification is our reading of it and the words are the evidence.
func TestAnUnrecognisedRefusalIsPassedOnInTheNodesOwnWords(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.queue(tx)
	h.chain.refuse(errors.New("something nobody has seen before"))

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Rejection != RejectOther {
		t.Errorf("rejection = %q", out[0].Rejection)
	}
	if !strings.Contains(out[0].Note, "something nobody has seen before") {
		t.Errorf("the node's own words were not passed on: %q", out[0].Note)
	}
}

// Refusals are not offered at all: the queue is what the policy allowed.
func TestARefusedDecisionIsNeverBroadcast(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "force_close_commitment.hex")
	h.queue(tx)

	// Overwrite the queued decision with the refusal a real policy run produces.
	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateMirrorState(context.Background(), rows[0].ID,
		store.MirrorDenied, "", 1); err != nil {
		t.Fatal(err)
	}

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || h.chain.took() != 0 {
		t.Errorf("a refused transaction was offered to the other chain: %+v %v",
			out, h.chain.sent)
	}
}

// Only the direction this engine serves. Two mirrors run, one each way, and each
// must leave the other's queue alone.
func TestOnlyTheTransactionsForThisChainAreOffered(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.queue(tx)

	// The same transaction, decided about in the other direction.
	if _, _, err := h.store.RecordMirrorDecision(context.Background(), store.MirrorDecision{
		TxID: tx.TxHash().String(), SourceBranch: store.BranchSQ,
		TargetBranch: store.BranchSF, Shape: store.ShapeMutualClose,
		Reason: "the other way", State: store.MirrorPending,
		FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("offered %d transactions, want only the one for this chain", len(out))
	}
}

// Bytes that are gone cannot be sent, and retrying that forever helps nobody.
func TestATransactionWhoseBytesAreGoneIsGivenUpOn(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)

	id, _, err := h.store.RecordMirrorDecision(context.Background(), store.MirrorDecision{
		TxID: strings.Repeat("ab", 32), SourceBranch: store.BranchSF,
		TargetBranch: store.BranchSQ, Shape: store.ShapeMutualClose,
		Reason: "agreed close", State: store.MirrorPending,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.m.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].State != store.MirrorAbandoned {
		t.Fatalf("outcomes = %+v", out)
	}
	if !strings.Contains(out[0].Note, "no longer has the transaction") {
		t.Errorf("the note does not say what is missing: %q", out[0].Note)
	}
	if h.decision(id).State != store.MirrorAbandoned {
		t.Error("it was left waiting to be retried forever")
	}
	if h.chain.took() != 0 {
		t.Error("something was sent despite having no bytes")
	}
}

func TestAMirrorNeedsItsParts(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no storage", Options{Target: &fakeChain{}, Branch: store.BranchSQ}},
		{"no chain", Options{Store: st, Branch: store.BranchSQ}},
		{"no chain named", Options{Store: st, Target: &fakeChain{}}},
		{"a chain that does not exist", Options{
			Store: st, Target: &fakeChain{}, Branch: "mainnet",
		}},
	} {
		if _, err := New(tc.opts); err == nil {
			t.Errorf("%s: a mirror was built anyway", tc.name)
		}
	}
}

func TestStorageThatHasGoneAwayStopsThePass(t *testing.T) {
	t.Parallel()
	h := newMirrorHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.queue(tx)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := h.m.Pass(context.Background()); err == nil {
		t.Error("a closed database reported nothing waiting rather than an error")
	}
}

// --- The classification itself ---

func TestRefusalsAreReadFromWhatANodeActuallySays(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		said string
		want Rejection
	}{
		{"min relay fee not met, 250 < 1100", RejectFeeTooLow},
		{"mempool min fee not met", RejectFeeTooLow},
		{"insufficient fee, rejecting replacement", RejectFeeTooLow},
		{"bad-txns-inputs-missingorspent", RejectMissingParent},
		{"Missing inputs", RejectMissingParent},
		{"txn-mempool-conflict", RejectConflict},
		{"non-standard transaction: dust", RejectNonStandard},
		{"scriptpubkey", RejectNonStandard},
		{"the node caught fire", RejectOther},
		{"", RejectOther},
	} {
		if got := Classify(errors.New(tc.said)); got != tc.want {
			t.Errorf("%q read as %q, want %q", tc.said, got, tc.want)
		}
	}

	if got := Classify(nil); got != "" {
		t.Errorf("no error classified as %q", got)
	}
}

// Whether to keep trying turns on whether the same bytes could ever be accepted.
func TestWhetherToKeepTryingTurnsOnWhetherAnythingCouldChange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		r    Rejection
		want bool
	}{
		{RejectFeeTooLow, true},     // fees fall
		{RejectMissingParent, true}, // the parent may arrive
		{RejectOther, true},         // unknown, so err towards trying
		{RejectConflict, false},     // something else spent it
		{RejectNonStandard, false},  // that chain will never relay it
		{Rejection("invented"), true},
	} {
		if got := tc.r.Retriable(); got != tc.want {
			t.Errorf("%q retriable = %v, want %v", tc.r, got, tc.want)
		}
	}
}

// Every explanation says whether anything can be done, because "we could not
// copy it" without that leaves somebody waiting for a fix that is not coming.
func TestEveryExplanationSaysWhetherAnythingCanBeDone(t *testing.T) {
	t.Parallel()

	for _, r := range []Rejection{
		RejectFeeTooLow, RejectMissingParent, RejectConflict,
		RejectNonStandard, RejectOther, Rejection("invented"),
	} {
		got := r.Explain("the node said something")
		const shortestUsefulSentence = 40
		if len(got) < shortestUsefulSentence {
			t.Errorf("%q explained as a label rather than a sentence: %q", r, got)
		}
		if strings.Contains(strings.ToLower(got), " sq ") ||
			strings.Contains(strings.ToLower(got), " sf ") {
			t.Errorf("%q used an internal chain name: %q", r, got)
		}
	}

	if got := RejectOther.Explain(""); !strings.Contains(got, "did not say why") {
		t.Errorf("a refusal with no words was explained as %q", got)
	}
}

// The wait doubles and then stops doubling, so a mirror never backs off so far
// that it would miss a fee market opening up.
func TestTheWaitGrowsAndThenStops(t *testing.T) {
	t.Parallel()

	if got := Backoff(0); got != FirstDelay {
		t.Errorf("before any attempt the wait is %v, want %v", got, FirstDelay)
	}
	if got := Backoff(1); got != FirstDelay {
		t.Errorf("after one attempt the wait is %v, want %v", got, FirstDelay)
	}
	if Backoff(2) <= Backoff(1) {
		t.Error("the wait did not grow")
	}
	for _, attempt := range []int64{20, 100, 10_000} {
		if got := Backoff(attempt); got != MaxDelay {
			t.Errorf("after %d attempts the wait is %v, want it capped at %v",
				attempt, got, MaxDelay)
		}
	}
}
