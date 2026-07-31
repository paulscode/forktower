package store

import (
	"context"
	"strings"
	"testing"
)

func sampleDecision(channelID int64) MirrorDecision {
	return MirrorDecision{
		TxID:         "cc" + strings.Repeat("0", 62),
		SourceBranch: BranchSF,
		TargetBranch: BranchSQ,
		ChannelID:    channelID,
		Shape:        ShapeMutualClose,
		Reason:       "cooperative close of a registered channel",
		State:        MirrorPending,
		FirstSeenAt:  1_790_000_000,
		UpdatedAt:    1_790_000_000,
	}
}

// The refusals are the larger half and the ones a user will ask about. A log
// line cannot be shown, counted, or read after it has rotated away.
func TestARefusalIsRecordedAndNotMerelyLogged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	d := sampleDecision(channelID)
	d.Shape = ShapeCommitmentUnknown
	d.State = MirrorDenied
	d.Reason = "the counterparty's commitment: mirroring it would create exposure " +
		"on the other branch that did not exist"
	if _, _, err := s.RecordMirrorDecision(ctx, d); err != nil {
		t.Fatal(err)
	}

	denied, err := s.ListMirrorDecisions(ctx, MirrorFilter{State: MirrorDenied})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 1 {
		t.Fatalf("got %d denials, want 1 — a refusal that is not stored cannot be shown", len(denied))
	}
	if !strings.Contains(denied[0].Reason, "would create exposure") {
		t.Errorf("the refusal does not say why: %q", denied[0].Reason)
	}
	if denied[0].Shape != ShapeCommitmentUnknown {
		t.Errorf("the observed shape was lost: %q", denied[0].Shape)
	}
}

// A decision with no reason is indistinguishable from a bug, in either
// direction.
func TestADecisionNeedsAReasonWhicheverWayItWent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for _, state := range []MirrorState{MirrorDenied, MirrorPending} {
		d := sampleDecision(0)
		d.State, d.Reason = state, ""
		if _, _, err := s.RecordMirrorDecision(ctx, d); err == nil {
			t.Errorf("a %q decision was accepted with no reason", state)
		}
	}
}

// The mirror sees the same transaction on every pass. One row, not one per pass.
func TestSeeingATransactionAgainDoesNotWriteAnotherRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	d := sampleDecision(0)
	id, existed, err := s.RecordMirrorDecision(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Error("a transaction seen for the first time was reported as already known")
	}

	again, existed, err := s.RecordMirrorDecision(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("the same transaction got a second row: %d then %d", id, again)
	}
	if !existed {
		t.Error("a transaction seen twice was reported as new")
	}
}

// Re-observing a transaction the target already accepted must not reset it.
// Seeing it again is not new information about its fate.
func TestSeeingAnAcceptedTransactionAgainDoesNotUndoTheOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	d := sampleDecision(0)
	id, _, err := s.RecordMirrorDecision(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMirrorState(ctx, id, MirrorAccepted, "", 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.RecordMirrorDecision(ctx, d); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListMirrorDecisions(ctx, MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != MirrorAccepted {
		t.Errorf("state = %q after re-observing an accepted transaction, want %q",
			rows[0].State, MirrorAccepted)
	}
}

// The same transaction can legitimately be considered in both directions.
func TestTheSameTransactionCanBeDecidedForEachBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	toSQ := sampleDecision(0)
	toSF := sampleDecision(0)
	toSF.SourceBranch, toSF.TargetBranch = BranchSQ, BranchSF

	if _, _, err := s.RecordMirrorDecision(ctx, toSQ); err != nil {
		t.Fatal(err)
	}
	if _, existed, err := s.RecordMirrorDecision(ctx, toSF); err != nil {
		t.Fatal(err)
	} else if existed {
		t.Error("the other direction was treated as the same decision")
	}

	rows, err := s.ListMirrorDecisions(ctx, MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows for two directions, want 2", len(rows))
	}
}

// Mirroring a branch to itself is not a direction, and a decision that got there
// has a bug behind it.
func TestMirroringABranchToItselfIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	d := sampleDecision(0)
	d.SourceBranch, d.TargetBranch = BranchSQ, BranchSQ
	if _, _, err := s.RecordMirrorDecision(ctx, d); err == nil {
		t.Error("a decision to mirror a branch to itself was accepted")
	}

	d = sampleDecision(0)
	d.TargetBranch = "mainnet"
	if _, _, err := s.RecordMirrorDecision(ctx, d); err == nil {
		t.Error("a decision naming a branch that does not exist was accepted")
	}
}

// The count must not be lost by two passes reading, deciding and writing at
// once; and a stale error beside an accepted transaction reads as a live
// problem.
func TestAttemptsAccumulateAndASuccessClearsTheError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	id, _, err := s.RecordMirrorDecision(ctx, sampleDecision(0))
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := s.UpdateMirrorState(ctx, id, MirrorRejected,
			"min relay fee not met", 1_790_000_100); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListMirrorDecisions(ctx, MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Attempts != 3 {
		t.Errorf("attempts = %d after three tries, want 3", rows[0].Attempts)
	}
	if rows[0].LastError != "min relay fee not met" {
		t.Errorf("the rejection reason was lost: %q", rows[0].LastError)
	}

	if err := s.UpdateMirrorState(ctx, id, MirrorAccepted, "", 1_790_000_200); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListMirrorDecisions(ctx, MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].LastError != "" {
		t.Errorf("an accepted transaction still carries an error: %q", rows[0].LastError)
	}
	if rows[0].Attempts != 4 {
		t.Errorf("attempts = %d, want 4 — the successful one counts too", rows[0].Attempts)
	}
}

// A table that grows with chain activity must not be read unbounded.
func TestAListingIsBoundedEvenWhenNobodyAsked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range DefaultMirrorLimit + 10 {
		d := sampleDecision(0)
		d.TxID = strings.Repeat("0", 60) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + "f"
		d.State = MirrorDenied
		if _, _, err := s.RecordMirrorDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ListMirrorDecisions(ctx, MirrorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > DefaultMirrorLimit {
		t.Errorf("got %d rows with no limit asked for, want at most %d",
			len(rows), DefaultMirrorLimit)
	}
	// Newest first: the recent ones are the interesting ones.
	if len(rows) > 1 && rows[0].ID < rows[1].ID {
		t.Error("decisions came back oldest first")
	}
}

func TestUpdatingAMissingDecisionIsAnError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if err := s.UpdateMirrorState(context.Background(), 4242,
		MirrorAccepted, "", 1); err == nil {
		t.Error("recording an attempt against a decision that does not exist was accepted")
	}
	if err := s.UpdateMirrorState(context.Background(), 1,
		"teleported", "", 1); err == nil {
		t.Error("an invented mirror state was accepted")
	}
}

// The one control in the schema that increases exposure. Off by default, and a
// registry poll must never write it — the same rule that protects the close
// state and the relevance, for a stronger reason.
func TestAPollCannotTurnOnFundingMirroring(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	c := sampleChannel(node)
	id, _, err := s.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	channels, err := s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if channels[0].MirrorFundingOptIn {
		t.Error("funding mirroring defaulted to on, which would create exposure nobody asked for")
	}

	if err := s.SetChannelMirrorOptIn(ctx, id, true, 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	// A poll comes round again, carrying no opinion about this.
	c.CapacitySat = 2_200_000
	if _, _, err := s.UpsertChannel(ctx, c); err != nil {
		t.Fatal(err)
	}

	channels, err = s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !channels[0].MirrorFundingOptIn {
		t.Error("a poll turned the user's funding opt-in back off")
	}

	// And it can be turned off again, by the same deliberate route.
	if err := s.SetChannelMirrorOptIn(ctx, id, false, 1_790_000_200); err != nil {
		t.Fatal(err)
	}
	channels, err = s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if channels[0].MirrorFundingOptIn {
		t.Error("the opt-in could not be withdrawn")
	}
}

// The Details view asks by state, by channel and by direction.
func TestMirrorDecisionsCanBeNarrowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	denied := sampleDecision(channelID)
	denied.State, denied.Reason = MirrorDenied, "not on the allowlist"
	denied.TxID = "01" + strings.Repeat("0", 62)
	pending := sampleDecision(channelID)
	pending.TxID = "02" + strings.Repeat("0", 62)
	otherWay := sampleDecision(0)
	otherWay.TxID = "03" + strings.Repeat("0", 62)
	otherWay.SourceBranch, otherWay.TargetBranch = BranchSQ, BranchSF

	for _, d := range []MirrorDecision{denied, pending, otherWay} {
		if _, _, recErr := s.RecordMirrorDecision(ctx, d); recErr != nil {
			t.Fatal(recErr)
		}
	}

	for _, tc := range []struct {
		name string
		f    MirrorFilter
		want int
	}{
		{"everything", MirrorFilter{}, 3},
		{"refusals", MirrorFilter{State: MirrorDenied}, 1},
		{"one channel", MirrorFilter{ChannelID: channelID}, 2},
		{"towards sq", MirrorFilter{TargetBranch: BranchSQ}, 2},
		{"towards sf", MirrorFilter{TargetBranch: BranchSF}, 1},
		{"a refusal for one channel", MirrorFilter{State: MirrorDenied, ChannelID: channelID}, 1},
		{"an explicit limit", MirrorFilter{Limit: 1}, 1},
	} {
		got, listErr := s.ListMirrorDecisions(ctx, tc.f)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(got) != tc.want {
			t.Errorf("%s: got %d decisions, want %d", tc.name, len(got), tc.want)
		}
	}
}

func TestReadingMirrorDecisionsFromAClosedStoreFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	if _, _, err := s.RecordMirrorDecision(ctx, sampleDecision(0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListMirrorDecisions(ctx, MirrorFilter{}); err == nil {
		t.Error("listing from a closed store reported no decisions rather than an error")
	}
	if _, _, err := s.RecordMirrorDecision(ctx, sampleDecision(0)); err == nil {
		t.Error("recording a decision in a closed store was accepted")
	}
	if err := s.UpdateMirrorState(ctx, 1, MirrorAccepted, "", 1); err == nil {
		t.Error("recording an attempt in a closed store was accepted")
	}
}

func TestTurningOnMirroringForAMissingChannelIsAnError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if err := s.SetChannelMirrorOptIn(context.Background(), 4242, true, 1); err == nil {
		t.Error("an opt-in against a channel that does not exist was accepted")
	}
}
