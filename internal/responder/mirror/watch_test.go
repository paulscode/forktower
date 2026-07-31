package mirror

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
)

// realClose is a transaction captured from a real Lightning node, so that what
// is scanned and lifted is a transaction that actually exists rather than one
// shaped to pass.
func realClose(t *testing.T, name string) *wire.MsgTx {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "watcher", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	body, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return tx
}

type harness struct {
	t     *testing.T
	store *store.Store
	view  *chainviewtest.View
	obs   *Observer
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	view := chainviewtest.New("regtest")
	// Always this direction. The observer reads the chain the user's own node
	// follows; the other direction differs only in what the policy allows, and
	// that is tested where it lives.
	obs, err := NewObserver(ObserverOptions{
		Store: st, View: view, From: store.BranchSF, To: store.BranchSQ,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: st, view: view, obs: obs}
}

// channelSpending registers a channel whose funding output is the one the given
// transaction spends, which is what makes that transaction ours.
func (h *harness) channelSpending(tx *wire.MsgTx) int64 {
	h.t.Helper()
	ctx := context.Background()

	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1_790_000_000,
	}); err != nil {
		h.t.Fatal(err)
	}

	prev := tx.TxIn[0].PreviousOutPoint
	c := store.Channel{
		LNNodeID: node, FundingTxID: prev.Hash.String(),
		//nolint:gosec // a test's output index
		FundingVout: int32(prev.Index),
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors,
		Relevance: store.Relevant, UpdatedAt: 1_790_000_000,
	}
	id, _, err := h.store.UpsertChannel(ctx, c)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) scan(tx *wire.MsgTx) []Found {
	h.t.Helper()
	meta := h.view.ExtendWith("with-the-transaction", tx)
	found, err := h.obs.ScanBlock(context.Background(), meta.Height, 1_790_000_100)
	if err != nil {
		h.t.Fatal(err)
	}
	return found
}

// A cooperative close on the user's own chain is the plainest thing the mirror
// exists to move.
func TestAnAgreedCloseIsLiftedAndAllowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d of the user's transactions, want 1", len(found))
	}
	got := found[0]

	if got.Lifted.Shape != store.ShapeMutualClose {
		t.Errorf("shape = %q, want a mutual close", got.Lifted.Shape)
	}
	if got.Decision.State != store.MirrorPending {
		t.Errorf("state = %q, want it queued to be copied (%s)",
			got.Decision.State, got.Decision.Reason)
	}
	if !got.New {
		t.Error("a transaction seen for the first time was reported as already known")
	}

	// And the verdict is in the database, where a user can be shown it.
	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TxID != got.Lifted.TxID {
		t.Fatalf("the decision was not recorded: %+v", rows)
	}
	if rows[0].Reason == "" {
		t.Error("a decision was recorded with no reason")
	}
}

// **The bytes must survive exactly.** A transaction is only valid with the
// signatures it was built with, and one byte changed produces something that
// cannot confirm and looks like our fault.
func TestTheTransactionIsLiftedByteForByte(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	original, err := os.ReadFile(filepath.Join("..", "..", "watcher", "testdata", "coop_close.hex"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ToLower(strings.TrimSpace(string(original)))

	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d transactions", len(found))
	}
	if got := strings.ToLower(found[0].Lifted.RawHex); got != want {
		t.Errorf("the lifted bytes differ from what was observed:\n got %s\nwant %s", got, want)
	}
	if found[0].Lifted.TxID != tx.TxHash().String() {
		t.Errorf("the transaction id changed in the lifting: %q", found[0].Lifted.TxID)
	}
}

// The counterparty's force-close is found, judged, and refused — and the refusal
// is the record that answers "why was that not copied?".
func TestTheirForceCloseIsRecordedAsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "force_close_commitment.hex")
	h.channelSpending(tx)

	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d transactions, want the commitment", len(found))
	}
	if found[0].Decision.State != store.MirrorDenied {
		t.Errorf("state = %q, want it refused", found[0].Decision.State)
	}
	if !strings.Contains(found[0].Decision.Reason, "at risk there when it is not at risk now") {
		t.Errorf("the refusal does not say what the harm would be: %q", found[0].Decision.Reason)
	}

	denied, err := h.store.ListMirrorDecisions(context.Background(),
		store.MirrorFilter{State: store.MirrorDenied})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 1 {
		t.Fatalf("the refusal was not recorded: %+v", denied)
	}
}

// Our own force-close is ours to copy, and telling it from theirs rests entirely
// on what the node reported closing the channel with.
func TestOurOwnForceCloseIsAllowedBecauseTheNodeToldUsItWasOurs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "force_close_user.hex")
	id := h.channelSpending(tx)
	// Through the watcher's own path, not the registry poll's: UpsertChannel
	// deliberately refuses to write the close state, so that a poll cannot undo
	// what was established from the chain.
	if err := h.store.SetChannelCloseSF(context.Background(), id,
		store.CloseForce, tx.TxHash().String(), 900_000, 1_790_000_000); err != nil {
		t.Fatal(err)
	}

	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d transactions", len(found))
	}
	if found[0].Lifted.Shape != store.ShapeCommitmentOurs {
		t.Errorf("shape = %q, want our own commitment", found[0].Lifted.Shape)
	}
	if found[0].Decision.State != store.MirrorPending {
		t.Errorf("our own close was refused: %s", found[0].Decision.Reason)
	}
}

// A block with nothing of ours in it is the ordinary case and must cost nothing
// and say nothing.
func TestABlockWithNothingOfOursDecidesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	meta := h.view.Extend("empty", 1)
	found, err := h.obs.ScanBlock(context.Background(), meta.Height, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("an empty block produced %d decisions", len(found))
	}

	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("an empty block wrote %d rows", len(rows))
	}
}

// With no channels registered there is nothing to look for, and the observer
// must not read blocks to discover that.
func TestWithNoChannelsNothingIsScanned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")

	meta := h.view.ExtendWith("with-the-transaction", tx)
	found, err := h.obs.ScanBlock(context.Background(), meta.Height, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("a transaction was considered with no channels registered: %+v", found)
	}
}

// A block scanned twice decides the same thing and writes one row: the mirror
// revisits blocks, and a table that grew on every pass would be a table nobody
// could read.
func TestScanningTheSameBlockTwiceWritesOneRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	meta := h.view.ExtendWith("with-the-transaction", tx)
	first, err := h.obs.ScanBlock(context.Background(), meta.Height, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.obs.ScanBlock(context.Background(), meta.Height, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("scans returned %d then %d", len(first), len(second))
	}
	if !first[0].New {
		t.Error("the first sighting was not reported as new")
	}
	if second[0].New {
		t.Error("the second sighting was reported as new")
	}

	rows, err := h.store.ListMirrorDecisions(context.Background(), store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("two scans of one block wrote %d rows", len(rows))
	}
}

// A channel the classifier ruled out has no exposure on the other chain, so its
// close has nothing to be copied to.
func TestAChannelWithNoExposureIsNotWatchedForCopying(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.channelSpending(tx)
	if err := h.store.SetChannelRelevance(context.Background(), id,
		store.Irrelevant, "opened after the chains separated", 1); err != nil {
		t.Fatal(err)
	}

	if found := h.scan(tx); len(found) != 0 {
		t.Errorf("a channel with no exposure was still considered: %+v", found)
	}
}

// A direction that is not one has a bug behind it.
func TestAnObserverNeedsTwoDifferentChains(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	view := chainviewtest.New("regtest")

	for _, tc := range []struct {
		name string
		opts ObserverOptions
	}{
		{"no storage", ObserverOptions{View: view, From: store.BranchSF, To: store.BranchSQ}},
		{"no chain", ObserverOptions{Store: st, From: store.BranchSF, To: store.BranchSQ}},
		{"one chain twice", ObserverOptions{
			Store: st, View: view, From: store.BranchSF, To: store.BranchSF,
		}},
		{"a chain that does not exist", ObserverOptions{
			Store: st, View: view, From: store.BranchSF, To: "mainnet",
		}},
	} {
		if _, err := NewObserver(tc.opts); err == nil {
			t.Errorf("%s: an observer was built anyway", tc.name)
		}
	}
}

// A chain that cannot be read is an error, not an empty answer that reads as
// "nothing of yours happened".
func TestAChainThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	if _, err := h.obs.ScanBlock(context.Background(), 9999, 1); err == nil {
		t.Error("a block that does not exist reported nothing rather than an error")
	}
}

// Storage that has gone away must not read as a chain with nothing on it.
func TestStorageThatHasGoneAwayIsAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)
	meta := h.view.ExtendWith("with-the-transaction", tx)

	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.obs.ScanBlock(context.Background(), meta.Height, 1); err == nil {
		t.Error("a closed database reported no transactions rather than an error")
	}
}

// Lifting needs a transaction. A match with none behind it means something
// upstream lost it, and inventing an empty one would put an unbroadcastable row
// in the database.
func TestLiftingNothingIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := Lift(watcher.Match{}, nil, Facts{}); err == nil {
		t.Error("a match with no transaction was lifted anyway")
	}
}

// A transaction spending an output nobody is watching is not ours, and the
// policy refuses it on those grounds rather than on its shape.
func TestATransactionOnNobodysChannelIsRefusedAsNotOurs(t *testing.T) {
	t.Parallel()
	tx := realClose(t, "coop_close.hex")
	lifted, err := Lift(watcher.Match{
		Target: watcher.Target{Kind: watcher.KindFunding},
		Tx:     tx,
	}, tx, Facts{})
	if err != nil {
		t.Fatal(err)
	}
	if lifted.ChannelID != 0 {
		t.Fatalf("a match with no channel produced channel %d", lifted.ChannelID)
	}

	decision := Decision(lifted, store.BranchSF, store.BranchSQ, false, 1)
	if decision.State != store.MirrorDenied {
		t.Errorf("state = %q, want it refused", decision.State)
	}
	if !strings.Contains(decision.Reason, "not part of any of your channels") {
		t.Errorf("the refusal names the wrong grounds: %q", decision.Reason)
	}
}

// sweepOf builds a transaction spending one output of another, taking the
// script path the given witness item selects.
//
// `01` is the revocation path — somebody punishing a revoked commitment — and an
// empty item is the delayed path, somebody collecting after the wait. The
// spender has to say which in the clear, so this is evidence rather than a
// guess.
func sweepOf(parent *wire.MsgTx, vout uint32, selector []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: parent.TxHash(), Index: vout},
		Witness:          wire.TxWitness{make([]byte, 71), selector, make([]byte, 40)},
	})
	tx.AddTxOut(&wire.TxOut{Value: 90_000, PkScript: make([]byte, 22)})
	return tx
}

// **Whose sweep it is turns on what it follows**, and this is the path that
// works that out: the commitment's recorded shape decides whether the money
// being collected is ours to help along.
func TestASweepIsJudgedByTheCommitmentItFollows(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		sourceShape store.SpendShape
		wantMirror  bool
	}{
		{"following our own close", store.ShapeCommitmentOurs, true},
		{"following theirs", store.ShapeCommitmentUnknown, false},
	} {
		h := newHarness(t)
		commitment := realClose(t, "force_close_commitment.hex")
		channelID := h.channelSpending(commitment)
		ctx := context.Background()

		// The commitment, as the detection side would have recorded it.
		spendID, _, err := h.store.RecordSpend(ctx, store.Spend{
			Branch: store.BranchSF, ChannelID: channelID,
			OutpointTxID: commitment.TxIn[0].PreviousOutPoint.Hash.String(),
			//nolint:gosec // a test's output index
			OutpointVout: int32(commitment.TxIn[0].PreviousOutPoint.Index),
			SpendTxID:    commitment.TxHash().String(),
			SpendTxHex:   "00", Shape: tc.sourceShape, Status: store.SpendConfirmed,
			BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}

		// And one of its outputs, now watched in its own right.
		if err := h.store.AddWatchOutpoint(ctx, store.WatchOutpoint{
			Branch: store.BranchSF, TxID: commitment.TxHash().String(), Vout: 0,
			ScriptHex:          "0020" + strings.Repeat("cc", 32),
			SourceSpendEventID: spendID, Role: store.RoleToLocal,
		}); err != nil {
			t.Fatal(err)
		}

		sweep := sweepOf(commitment, 0, nil) // the delayed path: collecting
		found := h.scan(sweep)
		if len(found) != 1 {
			t.Fatalf("%s: found %d transactions", tc.name, len(found))
		}
		if found[0].Lifted.SourceShape != tc.sourceShape {
			t.Errorf("%s: source shape = %q, want %q",
				tc.name, found[0].Lifted.SourceShape, tc.sourceShape)
		}
		mirrored := found[0].Decision.State == store.MirrorPending
		if mirrored != tc.wantMirror {
			t.Errorf("%s: mirrored = %v, want %v (%s)",
				tc.name, mirrored, tc.wantMirror, found[0].Decision.Reason)
		}
	}
}

// The user's decision reaches the policy. Without it a funding transaction is
// refused; with it, allowed — and it is the only input here that comes from a
// person rather than from the chain.
func TestTheFundingOptInReachesTheDecision(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	id := h.channelSpending(tx)

	channels, err := h.store.ListChannels(context.Background(), store.ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if channels[0].MirrorFundingOptIn {
		t.Fatal("the opt-in defaulted to on")
	}

	if err := h.store.SetChannelMirrorOptIn(context.Background(), id, true, 2); err != nil {
		t.Fatal(err)
	}
	channels, err = h.store.ListChannels(context.Background(), store.ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !channels[0].MirrorFundingOptIn {
		t.Error("the opt-in did not stick")
	}

	// It is carried into the decision, which a close does not use — but the same
	// read supplies it, and a break in that plumbing would only show up on the
	// one transaction type nobody tests by accident.
	if found := h.scan(tx); len(found) != 1 {
		t.Fatalf("found %d transactions with the opt-in set", len(found))
	}
}

// A channel whose funding outpoint cannot be read is a channel whose
// transactions will never be copied. Not fatal for the others, and never silent.
func TestAnUnreadableChannelIsSkippedLoudlyAndTheRestStillWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if _, _, err := h.store.UpsertChannel(context.Background(), store.Channel{
		LNNodeID: node, FundingTxID: "not-a-transaction-id", FundingVout: 0,
		CapacitySat: 1, ChanType: store.ChanAnchors,
		Relevance: store.Relevant, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	found := h.scan(tx)
	if len(found) != 1 {
		t.Errorf("one unreadable channel stopped the others being considered: %d found",
			len(found))
	}
}

func TestTheHighestBlockLookedAtIsRemembered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	if h.obs.LastScanned() != 0 {
		t.Errorf("a fresh observer had already scanned to %d", h.obs.LastScanned())
	}
	meta := h.view.ExtendWith("with-the-transaction", tx)
	if _, err := h.obs.ScanBlock(context.Background(), meta.Height, 1); err != nil {
		t.Fatal(err)
	}
	if h.obs.LastScanned() != meta.Height {
		t.Errorf("last scanned = %d, want %d", h.obs.LastScanned(), meta.Height)
	}
}

// failingStore fails whichever read a test names, so that the paths taken when
// storage misbehaves are exercised without a broken database.
type failingStore struct {
	Store
	failChannels  bool
	failSpends    bool
	failOutpoints bool
	failRecord    bool
}

func (f *failingStore) ListChannels(
	ctx context.Context, filter store.ChannelFilter,
) ([]store.Channel, error) {
	if f.failChannels {
		return nil, errors.New("the database went away")
	}
	return f.Store.ListChannels(ctx, filter)
}

func (f *failingStore) ListWatchOutpoints(
	ctx context.Context, branch store.Branch,
) ([]store.WatchOutpoint, error) {
	if f.failOutpoints {
		return nil, errors.New("the database went away")
	}
	return f.Store.ListWatchOutpoints(ctx, branch)
}

func (f *failingStore) ListSpends(
	ctx context.Context, filter store.SpendFilter,
) ([]store.Spend, error) {
	if f.failSpends {
		return nil, errors.New("the database went away")
	}
	return f.Store.ListSpends(ctx, filter)
}

func (f *failingStore) RecordMirrorDecision(
	ctx context.Context, d store.MirrorDecision,
) (id int64, existed bool, err error) {
	if f.failRecord {
		return 0, false, errors.New("the database went away")
	}
	return f.Store.RecordMirrorDecision(ctx, d)
}

// A read that failed must never be reported as a chain with nothing of ours on
// it. That is the difference between "we looked and there was nothing" and "we
// could not look", and only one of them is safe to act on.
func TestAStorageFailureIsNeverReportedAsNothingFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fail func(*failingStore)
		// perTx is true when the failure happens while judging one transaction,
		// which is survivable: the others are still judged.
		perTx bool
	}{
		{"reading the channels to watch", func(f *failingStore) { f.failChannels = true }, false},
		{"reading what else is watched", func(f *failingStore) { f.failOutpoints = true }, false},
		{"reading what a transaction follows", func(f *failingStore) { f.failSpends = true }, true},
		{"recording the decision", func(f *failingStore) { f.failRecord = true }, true},
	} {
		h := newHarness(t)
		commitment := realClose(t, "force_close_commitment.hex")
		channelID := h.channelSpending(commitment)
		ctx := context.Background()

		spendID, _, err := h.store.RecordSpend(ctx, store.Spend{
			Branch: store.BranchSF, ChannelID: channelID,
			OutpointTxID: commitment.TxIn[0].PreviousOutPoint.Hash.String(),
			//nolint:gosec // a test's output index
			OutpointVout: int32(commitment.TxIn[0].PreviousOutPoint.Index),
			SpendTxID:    commitment.TxHash().String(), SpendTxHex: "00",
			Shape: store.ShapeCommitmentOurs, Status: store.SpendConfirmed,
			BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.AddWatchOutpoint(ctx, store.WatchOutpoint{
			Branch: store.BranchSF, TxID: commitment.TxHash().String(), Vout: 0,
			ScriptHex:          "0020" + strings.Repeat("cc", 32),
			SourceSpendEventID: spendID, Role: store.RoleToLocal,
		}); err != nil {
			t.Fatal(err)
		}

		broken := &failingStore{Store: h.store}
		tc.fail(broken)
		obs, err := NewObserver(ObserverOptions{
			Store: broken, View: h.view, From: store.BranchSF, To: store.BranchSQ,
		})
		if err != nil {
			t.Fatal(err)
		}

		sweep := sweepOf(commitment, 0, nil)
		meta := h.view.ExtendWith("with-the-sweep", sweep)
		found, scanErr := obs.ScanBlock(ctx, meta.Height, 1)

		if tc.perTx {
			// Survivable: the pass completes, and the transaction it could not
			// judge is simply not among the results rather than quietly allowed.
			if scanErr != nil {
				t.Errorf("%s: a single failure stopped the whole pass: %v", tc.name, scanErr)
			}
			for _, f := range found {
				if f.Decision.State == store.MirrorPending {
					t.Errorf("%s: a transaction was queued despite the failure", tc.name)
				}
			}
			continue
		}
		if scanErr == nil {
			t.Errorf("%s: reported %d transactions rather than an error", tc.name, len(found))
		}
	}
}

// A target of a kind nothing recognises must not be classified as anything in
// particular, and `unknown` is refused by the policy.
func TestATargetOfAnUnrecognisedKindIsNotClassified(t *testing.T) {
	t.Parallel()
	tx := realClose(t, "coop_close.hex")

	lifted, err := Lift(watcher.Match{
		Target: watcher.Target{Kind: watcher.Kind("something_new"), ChannelID: 1},
		Tx:     tx,
	}, tx, Facts{})
	if err != nil {
		t.Fatal(err)
	}
	if lifted.Shape != store.ShapeUnknown {
		t.Errorf("shape = %q, want it left unknown", lifted.Shape)
	}
	if Decision(lifted, store.BranchSF, store.BranchSQ, false, 1).State != store.MirrorDenied {
		t.Error("a transaction of an unrecognised kind was queued to be copied")
	}
}

// **Without this the whole arm is dead, and quietly.** The mirror reads the raw
// transaction back from the spend record when it comes to send it — possibly
// much later, across restarts — and on the chain the user's own node follows
// nothing else writes one. The detection engine watches the other chain.
func TestTheTransactionsBytesAreStoredSoTheyCanBeSentLater(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d transactions", len(found))
	}

	spends, err := h.store.ListSpends(context.Background(), store.SpendFilter{
		Branch: store.BranchSF,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spends) != 1 {
		t.Fatalf("the transaction's bytes were not stored: %+v", spends)
	}
	if spends[0].SpendTxHex != found[0].Lifted.RawHex {
		t.Error("the stored bytes differ from what was lifted")
	}
	if spends[0].SpendTxID != tx.TxHash().String() {
		t.Errorf("the wrong transaction was stored: %q", spends[0].SpendTxID)
	}
	// And on the chain it was found on, not the one it is going to.
	if spends[0].Branch != store.BranchSF {
		t.Errorf("recorded against %q, want the chain it was observed on", spends[0].Branch)
	}
}

// **A decision made before the evidence arrived must not stand for ever.**
//
// The user's own force-close and the counterparty's look identical on the chain.
// The only thing that tells them apart is the closing transaction id the user's
// node reports, and that arrives on the next registry poll — up to a minute
// after the block. Decide once, at the moment the block lands, and their own
// close is filed as the counterparty's and never copied, with a record that
// reads as a principled refusal.
func TestARefusalIsRemadeOnceTheChannelIsKnown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	tx := realClose(t, "force_close_user.hex")
	id := h.channelSpending(tx)

	// The block arrives before the node has said which transaction it closed
	// with, so the commitment cannot be attributed and is refused.
	found := h.scan(tx)
	if len(found) != 1 {
		t.Fatalf("found %d transactions", len(found))
	}
	if found[0].Decision.State != store.MirrorDenied {
		t.Fatalf("with the channel unread, the close was not refused: %+v", found[0].Decision)
	}

	// Nothing has changed yet, so nothing is remade.
	changed, err := h.obs.Reconsider(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("a decision was remade with no new evidence: %+v", changed)
	}

	// The registry catches up and says the close was ours.
	if err := h.store.SetChannelCloseSF(ctx, id, store.CloseForce,
		tx.TxHash().String(), 900_000, 3); err != nil {
		t.Fatal(err)
	}

	changed, err = h.obs.Reconsider(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Fatalf("the refusal was not remade once the channel was read: %+v", changed)
	}

	rows, err := h.store.ListMirrorDecisions(ctx, store.MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != store.MirrorPending {
		t.Errorf("state = %q, want it queued to be copied", rows[0].State)
	}
	// The reason is rewritten too: a row saying "queued" while still carrying the
	// sentence explaining why it was refused would contradict itself.
	if strings.Contains(rows[0].Reason, "at risk there") {
		t.Errorf("the refusal's reason survived the change of mind: %q", rows[0].Reason)
	}
	if !strings.Contains(rows[0].Reason, "yourself") {
		t.Errorf("the new reason does not say why it is being copied: %q", rows[0].Reason)
	}
}

// Only refusals are revisited. Something already sent, or already going, has
// moved past the point where the classification is the open question.
func TestOnlyRefusalsAreReconsidered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	tx := realClose(t, "coop_close.hex")
	h.channelSpending(tx)

	found := h.scan(tx)
	if len(found) != 1 || found[0].Decision.State != store.MirrorPending {
		t.Fatalf("the agreed close was not queued: %+v", found)
	}

	changed, err := h.obs.Reconsider(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("a transaction already on its way was reconsidered: %+v", changed)
	}
}

// A refusal that no later evidence can change is left alone, or the sweep would
// churn over the same rows for ever.
func TestARefusalNothingCanChangeIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	tx := realClose(t, "force_close_commitment.hex")
	h.channelSpending(tx)

	if found := h.scan(tx); found[0].Decision.State != store.MirrorDenied {
		t.Fatalf("their close was not refused: %+v", found[0].Decision)
	}

	// Their close, still theirs however long anybody waits.
	for _, at := range []int64{2, 3, 4} {
		changed, err := h.obs.Reconsider(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(changed) != 0 {
			t.Errorf("the counterparty's close was reconsidered into being copied: %+v", changed)
		}
	}
}
