package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sampleSpend(channelID int64) Spend {
	return Spend{
		Branch:       BranchSQ,
		ChannelID:    channelID,
		OutpointTxID: "aa" + strings.Repeat("0", 62),
		OutpointVout: 0,
		SpendTxID:    "bb" + strings.Repeat("0", 62),
		SpendTxHex:   "0200000001",
		Status:       SpendMempool,
		FirstSeenAt:  1_790_000_000,
		UpdatedAt:    1_790_000_000,
	}
}

// The watcher re-scans from the fork point after a reorganisation, so every
// block it has already processed gets processed again. Each of those must
// produce zero new rows the second time.
func TestRecordingTheSameSpendTwiceIsOneRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	sp := sampleSpend(channelID)
	id, existed, err := s.RecordSpend(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Error("a spend seen for the first time was reported as already known")
	}

	again, existed, err := s.RecordSpend(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Error("a spend seen twice was reported as new")
	}
	if again != id {
		t.Errorf("the same spend got a second row: %d then %d", id, again)
	}

	all, err := s.ListSpends(ctx, SpendFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("got %d rows for one spend", len(all))
	}
}

// The same outpoint spent by a *different* transaction is a different event —
// that is what a competing sweep looks like, and both need recording.
func TestTwoTransactionsSpendingTheSameOutpointAreTwoEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	sp := sampleSpend(channelID)
	if _, _, err := s.RecordSpend(ctx, sp); err != nil {
		t.Fatal(err)
	}
	sp.SpendTxID = "cc" + strings.Repeat("0", 62)
	if _, existed, err := s.RecordSpend(ctx, sp); err != nil {
		t.Fatal(err)
	} else if existed {
		t.Error("a competing spend of the same outpoint was treated as a duplicate")
	}

	all, err := s.ListSpends(ctx, SpendFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d rows, want both competing spends", len(all))
	}
}

// The same transaction seen on both chains is two facts, not one: it may confirm
// on one and not the other, and that difference is the whole product.
func TestTheSameSpendOnBothChainsIsTwoEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	sp := sampleSpend(channelID)
	if _, _, err := s.RecordSpend(ctx, sp); err != nil {
		t.Fatal(err)
	}
	sp.Branch = BranchSF
	if _, existed, err := s.RecordSpend(ctx, sp); err != nil {
		t.Fatal(err)
	} else if existed {
		t.Error("the same spend on the other chain was treated as a duplicate")
	}

	sq, err := s.ListSpends(ctx, SpendFilter{Branch: BranchSQ})
	if err != nil {
		t.Fatal(err)
	}
	if len(sq) != 1 {
		t.Errorf("got %d spends on the other chain, want 1", len(sq))
	}
}

// Recording is not classifying. The shape needs the channel's own data and
// sometimes a second transaction, neither of which is available when the spend
// is first seen — and watching must not wait for classification.
func TestASpendIsRecordedBeforeItIsUnderstood(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	sp := sampleSpend(channelID)
	sp.Shape = "" // the caller does not know yet
	id, _, err := s.RecordSpend(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSpends(ctx, SpendFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Shape != ShapeUnknown {
		t.Errorf("shape = %q, want %q", got[0].Shape, ShapeUnknown)
	}

	// Later, once it is understood.
	if err := s.UpdateSpendShape(ctx, id, ShapeCommitmentUnknown, 1_790_000_100); err != nil {
		t.Fatal(err)
	}
	// And once it confirms.
	if err := s.UpdateSpendStatus(ctx, id, SpendConfirmed,
		"dd"+strings.Repeat("0", 62), 850_100, 1_790_000_200); err != nil {
		t.Fatal(err)
	}

	got, err = s.ListSpends(ctx, SpendFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Shape != ShapeCommitmentUnknown || got[0].Status != SpendConfirmed {
		t.Errorf("got shape %q status %q", got[0].Shape, got[0].Status)
	}
	if got[0].BlockHeight != 850_100 {
		t.Errorf("height = %d", got[0].BlockHeight)
	}
	// The raw transaction survives, because the mirror will need it and the
	// chain it came from may not still be servable.
	if got[0].SpendTxHex == "" {
		t.Error("the raw transaction was lost")
	}
}

// A re-scan after a reorganisation records the same spend again. It must not
// overwrite what the first pass wrote, because the later pass knows no more
// than the first did.
func TestRerecordingDoesNotOverwriteWhatIsKnown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	sp := sampleSpend(channelID)
	id, _, err := s.RecordSpend(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSpendShape(ctx, id, ShapeCommitmentRevoked, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSpendStatus(ctx, id, SpendConfirmed, "dd", 850_100, 2); err != nil {
		t.Fatal(err)
	}

	// The re-scan, which knows only what the block says.
	if _, existed, err := s.RecordSpend(ctx, sp); err != nil {
		t.Fatal(err)
	} else if !existed {
		t.Fatal("the re-scan did not recognise the spend")
	}

	got, err := s.ListSpends(ctx, SpendFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Shape != ShapeCommitmentRevoked {
		t.Errorf("shape = %q after a re-scan, want the classification kept", got[0].Shape)
	}
	if got[0].Status != SpendConfirmed {
		t.Errorf("status = %q after a re-scan, want it kept", got[0].Status)
	}
}

// A spend the chain has taken back is kept, not deleted. It happened, and the
// record of it is the audit trail.
func TestAReorgedOutSpendIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	id, _, err := s.RecordSpend(ctx, sampleSpend(channelID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSpendStatus(ctx, id, SpendReorgedOut, "", 0, 3); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSpends(ctx, SpendFilter{Status: SpendReorgedOut})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reorged-out spends, want it kept", len(got))
	}
	if got[0].BlockHash != "" || got[0].BlockHeight != 0 {
		t.Error("a reorged-out spend still claims a block")
	}
}

// Second-order watching: the outputs of a confirmed commitment, which is where
// the race for the money actually happens.
func TestWatchedOutpointsAreIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}
	spendID, _, err := s.RecordSpend(ctx, sampleSpend(channelID))
	if err != nil {
		t.Fatal(err)
	}

	w := WatchOutpoint{
		Branch: BranchSQ, TxID: "bb" + strings.Repeat("0", 62), Vout: 0,
		ScriptHex:          "0020" + strings.Repeat("ee", 32),
		SourceSpendEventID: spendID, Role: RoleToLocal,
	}
	for range 3 {
		if err := s.AddWatchOutpoint(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListWatchOutpoints(ctx, BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d watched outpoints after adding one three times", len(got))
	}
	if got[0].Role != RoleToLocal || got[0].ScriptHex == "" {
		t.Errorf("got %+v", got[0])
	}

	// The other chain is watched separately.
	other, err := s.ListWatchOutpoints(ctx, BranchSF)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("got %d outpoints on the other chain", len(other))
	}
}

func TestSpendWritesRejectWhatTheSchemaWouldNotAccept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	bad := map[string]Spend{
		"no branch":   {OutpointTxID: "aa", SpendTxID: "bb", SpendTxHex: "00", Status: SpendMempool},
		"no outpoint": {Branch: BranchSQ, SpendTxID: "bb", SpendTxHex: "00", Status: SpendMempool},
		"no raw tx":   {Branch: BranchSQ, OutpointTxID: "aa", SpendTxID: "bb", Status: SpendMempool},
		"unknown status": {Branch: BranchSQ, OutpointTxID: "aa", SpendTxID: "bb",
			SpendTxHex: "00", Status: "maybe"},
		"unknown shape": {Branch: BranchSQ, OutpointTxID: "aa", SpendTxID: "bb",
			SpendTxHex: "00", Status: SpendMempool, Shape: "something-new"},
	}
	for name, sp := range bad {
		if _, _, err := s.RecordSpend(ctx, sp); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// The raw transaction is required because the mirror needs it later, and a
	// spend on a chain nobody else watches may not be fetchable again.
	if _, _, err := s.RecordSpend(ctx, Spend{
		Branch: BranchSQ, OutpointTxID: "aa", SpendTxID: "bb", Status: SpendMempool,
	}); err == nil || !strings.Contains(err.Error(), "raw transaction") {
		t.Errorf("got %v, want it to say why the raw transaction is needed", err)
	}

	if err := s.UpdateSpendShape(ctx, 9999, ShapeJustice, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	if err := s.UpdateSpendStatus(ctx, 9999, SpendConfirmed, "", 0, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	if err := s.AddWatchOutpoint(ctx, WatchOutpoint{Branch: BranchSQ, TxID: "a",
		ScriptHex: "", Role: RoleToLocal}); err == nil {
		t.Error("an outpoint with no script was accepted; the scan matches on scripts")
	}
}
