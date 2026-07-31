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
