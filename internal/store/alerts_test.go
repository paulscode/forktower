package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sampleAlert() Alert {
	return Alert{
		Tier:         TierCritical,
		Kind:         "funding_spent",
		DedupKey:     "funding_spent:abc:0",
		Subject:      "abc:0",
		Message:      "A channel was closed on the other chain.",
		CreatedAt:    1000,
		LastRaisedAt: 1000,
	}
}

// The dedup key is the point of this table: raising the same condition again must
// not produce a second row, or an escalating situation buries the user in
// near-identical notifications and the important one scrolls away.
func TestUpsertAlertDeduplicatesByKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	a := sampleAlert()
	first, err := s.UpsertAlert(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !first.New {
		t.Error("first raise reported as pre-existing")
	}
	if first.Reopened {
		t.Error("a brand new alert was reported as a reopening")
	}

	a.LastRaisedAt = 2000
	a.Message = "this message is ignored on a repeat"
	second, err := s.UpsertAlert(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if second.New {
		t.Error("second raise of the same key reported as new")
	}
	if second.Reopened {
		t.Error("a repeat of an unacknowledged alert was reported as a reopening, " +
			"which would notify the user again about something already in front of them")
	}
	if second.Notify() {
		t.Error("a repeat of an unacknowledged alert asked to be delivered again")
	}
	if second.ID != first.ID {
		t.Errorf("second raise created a new row: id %d then %d", first.ID, second.ID)
	}

	got, err := s.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	if got[0].LastRaisedAt != 2000 {
		t.Errorf("last_raised_at = %d, want it bumped to 2000", got[0].LastRaisedAt)
	}
	// The first message and creation time are the record of when this began; a
	// repeat updates when it last happened, not the history of it.
	if got[0].CreatedAt != 1000 {
		t.Errorf("created_at = %d, want the original 1000", got[0].CreatedAt)
	}
	if got[0].Message != "A channel was closed on the other chain." {
		t.Errorf("message was rewritten on a repeat: %q", got[0].Message)
	}
}

func TestUpsertAlertRejectsIncompleteAlerts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	cases := []struct {
		name    string
		mutate  func(*Alert)
		wantSub string
	}{
		{"no dedup key", func(a *Alert) { a.DedupKey = "" }, "dedup key"},
		{"no kind", func(a *Alert) { a.Kind = "" }, "kind"},
		{"unknown tier", func(a *Alert) { a.Tier = "screaming" }, "severity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := sampleAlert()
			tc.mutate(&a)
			_, err := s.UpsertAlert(ctx, a)
			if err == nil {
				t.Fatalf("accepted an alert with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error does not mention %q: %v", tc.wantSub, err)
			}
		})
	}
}

func TestAckAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	up, err := s.UpsertAlert(ctx, sampleAlert())
	if err != nil {
		t.Fatal(err)
	}
	id := up.ID

	changed, err := s.AckAlert(ctx, id, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("first acknowledgement reported no change")
	}

	// Acknowledging twice is harmless, and distinguishable, so a duplicated click
	// is not an error the user has to think about.
	changed, err = s.AckAlert(ctx, id, 4000)
	if err != nil {
		t.Fatalf("second acknowledgement failed: %v", err)
	}
	if changed {
		t.Error("second acknowledgement reported a change")
	}

	got, err := s.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].AckedAt != 3000 {
		t.Errorf("acked_at = %d, want the first acknowledgement's 3000", got[0].AckedAt)
	}
	if !got[0].Acked() {
		t.Error("Acked() is false after acknowledgement")
	}
}

func TestAckUnknownAlertIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	// Distinguished from "already acknowledged" because this one is a caller bug.
	_, err := s.AckAlert(ctx, 4242, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("AckAlert on a missing alert returned %v, want ErrNotFound", err)
	}
}

func TestListAlertsUnackedFilterAndOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	var ids []int64
	for i, key := range []string{"a", "b", "c"} {
		a := sampleAlert()
		a.DedupKey = key
		a.CreatedAt = int64(100 + i)
		a.LastRaisedAt = a.CreatedAt
		up, err := s.UpsertAlert(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, up.ID)
	}

	if _, err := s.AckAlert(ctx, ids[1], 500); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d alerts, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Errorf("results are not in ascending id order: %v then %v", all[i-1].ID, all[i].ID)
		}
	}

	unacked, err := s.ListAlerts(ctx, AlertFilter{UnackedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unacked) != 2 {
		t.Fatalf("got %d unacknowledged alerts, want 2", len(unacked))
	}
	for _, a := range unacked {
		if a.Acked() {
			t.Errorf("alert %d is acknowledged but appeared in the unacknowledged list", a.ID)
		}
	}
}

func TestListAlertsClampsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range 5 {
		a := sampleAlert()
		a.DedupKey = string(rune('a' + i))
		if _, err := s.UpsertAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListAlerts(ctx, AlertFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("with Limit 2 got %d alerts", len(got))
	}

	// An absurd limit is clamped rather than refused: the caller is a UI, and
	// failing its request is less useful than capping it.
	got, err = s.ListAlerts(ctx, AlertFilter{Limit: 1_000_000})
	if err != nil {
		t.Fatalf("an over-large limit was refused rather than clamped: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d alerts, want all 5", len(got))
	}
}

func TestRecordDeliveryKeepsFailuresToo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	up, err := s.UpsertAlert(ctx, sampleAlert())
	if err != nil {
		t.Fatal(err)
	}
	id := up.ID

	// A failure is recorded, not dropped: a transport that has quietly stopped
	// working is how an alarm becomes decorative, and that is only visible if the
	// failures are kept.
	if _, err := s.RecordDelivery(ctx, Delivery{
		AlertID: id, Transport: "my-phone", AttemptedAt: 10, OK: false,
		Error: "connection refused",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDelivery(ctx, Delivery{
		AlertID: id, Transport: "my-phone", AttemptedAt: 20, OK: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListDeliveries(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(got))
	}
	if got[0].OK || got[0].Error != "connection refused" {
		t.Errorf("first attempt recorded wrongly: %+v", got[0])
	}
	if !got[1].OK || got[1].Error != "" {
		t.Errorf("second attempt recorded wrongly: %+v", got[1])
	}
}

func TestRecordDeliveryNeedsATransportName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if _, err := s.RecordDelivery(ctx, Delivery{AlertID: 1, AttemptedAt: 1}); err == nil {
		t.Error("accepted a delivery with no transport name")
	}
}

func TestTierValid(t *testing.T) {
	t.Parallel()
	for _, ok := range []Tier{TierInfo, TierWarning, TierCritical, TierResolved, TierLoss} {
		if !ok.Valid() {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []Tier{"", "urgent", "INFO"} {
		if bad.Valid() {
			t.Errorf("%q should not be valid", bad)
		}
	}
}

// A condition that comes back after the user said they had seen it is news
// again. Without this, a view that degrades, is acknowledged, recovers and
// degrades again is silent forever — which is exactly how an alarm becomes
// decorative while still appearing to work.
func TestRaisingAnAcknowledgedAlertReopensIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	a := sampleAlert()
	first, err := s.UpsertAlert(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AckAlert(ctx, first.ID, 3000); err != nil {
		t.Fatal(err)
	}

	a.LastRaisedAt = 4000
	again, err := s.UpsertAlert(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if again.New {
		t.Error("a reopening was reported as a new alert, which would split the history in two")
	}
	if !again.Reopened {
		t.Fatal("the condition returned after acknowledgement and nobody would be told")
	}
	if !again.Notify() {
		t.Error("a reopened alert did not ask to be delivered")
	}

	// The acknowledgement is cleared, so escalation resumes and the dashboard
	// shows it as needing attention rather than as already handled.
	got, err := s.GetAlert(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Acked() {
		t.Errorf("the alert is still acknowledged at %d after being reopened", got.AckedAt)
	}
	if got.LastRaisedAt != 4000 {
		t.Errorf("last_raised_at = %d, want 4000", got.LastRaisedAt)
	}
}

func TestGetAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	want := sampleAlert()
	up, err := s.UpsertAlert(ctx, want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAlert(ctx, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DedupKey != want.DedupKey || got.Tier != want.Tier || got.Message != want.Message {
		t.Errorf("read back %+v, want the alert that was stored", got)
	}
	if got.Acked() {
		t.Error("a fresh alert reads back as acknowledged")
	}

	if _, err := s.GetAlert(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v for a missing alert, want ErrNotFound", err)
	}
}

// Closing an alert retires the row rather than opening a second one.
//
// **The bug this was written for shipped.** A resolution carrying the warning's
// dedup key went through UpsertAlert, which found the row, bumped it and cleared
// the acknowledgement — so the good news was recorded as a fresh copy of the bad
// news, and a warning the user had already dismissed came back.
func TestResolvingAnAlertRetiresTheRowItNames(t *testing.T) {
	t.Parallel()
	st := openTemp(t)
	ctx := t.Context()

	up, err := st.UpsertAlert(ctx, Alert{
		Tier: TierWarning, Kind: "tower_down", DedupKey: "tower_down:1",
		Subject: "Your watchtower is not answering", Message: "It stopped replying.",
		CreatedAt: 100, LastRaisedAt: 100,
	})
	if err != nil {
		t.Fatalf("raising the warning: %v", err)
	}
	if _, err := st.AckAlert(ctx, up.ID, 110); err != nil {
		t.Fatalf("dismissing the warning: %v", err)
	}

	id, err := st.ResolveAlert(ctx, Alert{
		Tier: TierResolved, Kind: "tower_recovered", DedupKey: "tower_down:1",
		Subject: "Your watchtower is answering again", Message: "It is back.",
		LastRaisedAt: 200,
	})
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if id != up.ID {
		t.Fatalf("resolve reported id %d, want the existing row %d", id, up.ID)
	}

	rows, err := st.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows after one thing broke and was fixed, want one", len(rows))
	}
	got := rows[0]
	if got.Tier != TierResolved || got.Kind != "tower_recovered" {
		t.Errorf("the row still reads %s/%s", got.Tier, got.Kind)
	}
	if got.Message != "It is back." {
		t.Errorf("message = %q, still the failure's words", got.Message)
	}
	// When the trouble started is the part worth keeping; a closed alert whose
	// history begins at the moment it ended says nothing about how long it ran.
	if got.CreatedAt != 100 {
		t.Errorf("created_at = %d, want the moment the trouble started", got.CreatedAt)
	}
	if got.AckedAt == 0 {
		t.Error("the resolution needs dismissing, so a condition that is over " +
			"goes on asking for attention")
	}
}

// Resolving something that was never raised must not invent a row.
//
// The caller relies on this to tell "I closed a thread" from "I have news of my
// own", and the difference decides whether a split ending is announced or
// swallowed.
func TestResolvingSomethingNeverRaisedChangesNothing(t *testing.T) {
	t.Parallel()
	st := openTemp(t)
	ctx := t.Context()

	id, err := st.ResolveAlert(ctx, Alert{
		Tier: TierResolved, Kind: "split_resolved", DedupKey: "split_resolved",
		Message: "The split has ended.", LastRaisedAt: 100,
	})
	if err != nil {
		t.Fatalf("resolving nothing: %v", err)
	}
	if id != 0 {
		t.Errorf("reported closing row %d, but nothing was standing", id)
	}
	rows, err := st.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows exist after resolving something never raised", len(rows))
	}
}
