package alert

import (
	"context"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

func queueMirror(t *testing.T, h *harness, txid string, state store.MirrorState,
	attempts int, lastError string,
) {
	t.Helper()
	ctx := context.Background()

	id, _, err := h.store.RecordMirrorDecision(ctx, store.MirrorDecision{
		TxID: txid, SourceBranch: store.BranchSF, TargetBranch: store.BranchSQ,
		Shape: store.ShapeMutualClose, Reason: "both of you agreed to close it",
		State: store.MirrorPending, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range attempts {
		if err := h.store.UpdateMirrorState(ctx, id, state, lastError, 2); err != nil {
			t.Fatal(err)
		}
	}
}

// **The alert comes from stored state, with no event at all.** A transaction
// that will not go across is a condition rather than a moment, and one that got
// stuck while the daemon was stopped has no event to have missed.
func TestATransactionStuckOnTheWayIsAlertedFromStoredStateAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "aa"+strings.Repeat("0", 62), store.MirrorRejected, 4,
		"min relay fee not met, 200 < 1100")

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Alert
	for i := range alerts {
		if alerts[i].Kind == KindMirrorStuck {
			found = &alerts[i]
		}
	}
	if found == nil {
		t.Fatalf("a transaction stuck after four tries raised nothing: %+v", alerts)
	}
	// The node's own words reach the user, because they are the evidence.
	if !strings.Contains(found.Message, "min relay fee") {
		t.Errorf("what the chain actually said was dropped: %q", found.Message)
	}
	if !strings.Contains(found.Message, "4 times") {
		t.Errorf("the message does not say how hard it has tried: %q", found.Message)
	}
}

// One refusal is usually followed by an acceptance. Alerting on the first would
// train people to ignore the alert that matters.
func TestOneRefusalIsNotWorthAnAlert(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "bb"+strings.Repeat("0", 62), store.MirrorRejected, 1,
		"min relay fee not met")

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if hasKind(alerts, KindMirrorStuck) {
		t.Error("a single refusal raised an alert")
	}
}

// Giving up is worth saying immediately: nothing more is going to happen on its
// own, and the user may need to act.
func TestGivingUpIsSaidStraightAwayAndSaysItMayNeedYou(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "cc"+strings.Repeat("0", 62), store.MirrorAbandoned, 1,
		"non-standard transaction")

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Alert
	for i := range alerts {
		if alerts[i].Kind == KindMirrorGaveUp {
			found = &alerts[i]
		}
	}
	if found == nil {
		t.Fatalf("giving up raised nothing: %+v", alerts)
	}
	if !strings.Contains(found.Message, "may need you to do something") {
		t.Errorf("the message does not say the user may have to act: %q", found.Message)
	}
}

// A transaction that went across says nothing. Anything else would make the
// ordinary case noisy.
func TestATransactionThatWentAcrossSaysNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "dd"+strings.Repeat("0", 62), store.MirrorAccepted, 1, "")
	queueMirror(t, h, "ee"+strings.Repeat("0", 62), store.MirrorDenied, 0, "")

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if hasKind(alerts, KindMirrorStuck) || hasKind(alerts, KindMirrorGaveUp) {
		t.Errorf("an accepted or refused-by-policy transaction raised an alert: %+v", alerts)
	}
}

// The sweep runs on a timer, so a standing condition must not fill the list with
// copies of itself.
func TestASweepRepeatedDoesNotRepeatTheAlert(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "ff"+strings.Repeat("0", 62), store.MirrorRejected, 5,
		"min relay fee not met")

	for range 3 {
		h.al.reconcile(ctx)
	}

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, a := range alerts {
		if a.Kind == KindMirrorStuck {
			count++
		}
	}
	if count != 1 {
		t.Errorf("three sweeps produced %d alerts about one transaction", count)
	}
}

// A refusal with nothing said about it still has to read as a sentence.
func TestARefusalWithNoWordsStillReads(t *testing.T) {
	t.Parallel()

	got := mirrorWhy(store.MirrorDecision{})
	if !strings.Contains(got, "did not say why") {
		t.Errorf("a wordless refusal was described as %q", got)
	}
	if got := attemptCount(1); got != "1 time" {
		t.Errorf("one attempt reads as %q", got)
	}
	if got := attemptCount(4); got != "4 times" {
		t.Errorf("four attempts read as %q", got)
	}
}

// A read that fails must not leave the reconciliation half-done in silence, nor
// take the daemon down with it.
func TestReconcilingAgainstAClosedStoreIsSurvived(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	// Must not panic, and must not pretend it found nothing wrong.
	h.al.reconcile(context.Background())
}

// **The trap that made the reconciler need its own path into the store.**
//
// A standing condition is re-derived on every sweep. Through the ordinary raise
// that would clear the acknowledgement and notify again each time — so a user
// who has seen a problem and decided to deal with it tomorrow would be told
// about it every minute until they did. That is its own way of making an alarm
// decorative, which is the failure this whole project is about.
func TestAnAcknowledgedConditionStaysAcknowledgedAcrossSweeps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	queueMirror(t, h, "1a"+strings.Repeat("0", 62), store.MirrorRejected, 4,
		"min relay fee not met")
	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, a := range alerts {
		if a.Kind == KindMirrorStuck {
			id = a.ID
		}
	}
	if id == 0 {
		t.Fatalf("nothing was raised to acknowledge: %+v", alerts)
	}

	if _, err := h.store.AckAlert(ctx, id, 3); err != nil {
		t.Fatal(err)
	}

	// The condition is still true, so the sweep raises it again — several times.
	for range 5 {
		h.al.reconcile(ctx)
	}

	got, err := h.store.GetAlert(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Acked() {
		t.Error("five sweeps of a standing condition un-acknowledged it, so the " +
			"user would be notified again every time it ran")
	}
	// And it is still the same single alert, with its last-seen time moving.
	if got.LastRaisedAt <= 0 {
		t.Error("the record of when it was last true did not move")
	}
}

// An event-driven raise still reopens an acknowledged alert, because a condition
// that comes back after being dismissed is news again. Only the reconciler is
// quiet.
func TestAnEventStillReopensSomethingAcknowledged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	h.al.raise(ctx, Candidate{
		Tier: store.TierWarning, Kind: KindTowerDown, DedupKey: "tower_down:1",
		Subject: "down", Message: "the tower is not answering.",
	})
	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	id := alerts[0].ID
	if _, err := h.store.AckAlert(ctx, id, 3); err != nil {
		t.Fatal(err)
	}

	h.al.raise(ctx, Candidate{
		Tier: store.TierWarning, Kind: KindTowerDown, DedupKey: "tower_down:1",
		Subject: "down", Message: "the tower is not answering.",
	})

	got, err := h.store.GetAlert(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Acked() {
		t.Error("a condition that recurred after being dismissed stayed dismissed, " +
			"so it would be silent forever")
	}
}

// **The worst condition to miss.** A dropped funding-spend event is a missed
// breach — and with only an event path, an alert that never existed never will.
func TestAConfirmedSpendGetsItsAlertFromStoredStateAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	channelID := h.seedChannel()
	if _, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: "aa" + strings.Repeat("0", 62), OutpointVout: 0,
		SpendTxID: "bb" + strings.Repeat("0", 62), SpendTxHex: "00",
		Shape: store.ShapeCommitmentUnknown, Status: store.SpendConfirmed,
		BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// No event is ever published.
	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(alerts, KindChannelSpent) {
		t.Fatalf("a confirmed commitment on the other chain raised nothing: %+v", alerts)
	}
}

// And a countdown that has already escalated: a dropped `deadline.escalated` is
// a countdown that never gets louder.
func TestAnEscalatedCountdownGetsItsAlertFromStoredStateAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	channelID := h.seedChannel()
	spendID, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: "cc" + strings.Repeat("0", 62), OutpointVout: 0,
		SpendTxID: "dd" + strings.Repeat("0", 62), SpendTxHex: "00",
		Shape: store.ShapeCommitmentUnknown, Status: store.SpendConfirmed,
		BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadlineID, _, err := h.store.UpsertDeadline(ctx, store.Deadline{
		SpendEventID: spendID, Kind: store.DeadlineCSV,
		DeadlineHeight: 900_144, State: store.DeadlineCounting, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The engine wrote the tier before announcing it — persist then publish — so
	// the record of the escalation survives an event nobody received.
	if err := h.store.SetDeadlineEscalation(ctx, deadlineID, 2, 2); err != nil {
		t.Fatal(err)
	}

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(alerts, KindDeadlineWarning) {
		t.Fatalf("a countdown that had already escalated raised nothing: %+v", alerts)
	}
}

// A countdown nothing has announced yet is left to the engine. Guessing the tier
// would mean re-deriving it from a chain tip this sweep does not have.
func TestACountdownNobodyHasAnnouncedIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	channelID := h.seedChannel()
	spendID, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: "ee" + strings.Repeat("0", 62), OutpointVout: 0,
		SpendTxID: "ff" + strings.Repeat("0", 62), SpendTxHex: "00",
		Shape: store.ShapeMutualClose, Status: store.SpendConfirmed,
		BlockHeight: 900_000, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.UpsertDeadline(ctx, store.Deadline{
		SpendEventID: spendID, Kind: store.DeadlineCSV,
		DeadlineHeight: 900_144, State: store.DeadlineCounting, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if hasKind(alerts, KindDeadlineWarning) {
		t.Error("a countdown with nothing announced about it was given a tier the " +
			"sweep could not know")
	}
}
