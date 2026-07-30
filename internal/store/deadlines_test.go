package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func seedSpend(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.RecordSpend(ctx, sampleSpend(channelID))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Recomputing a deadline must update the clock already running, not start a
// second one beside it. Two countdowns for one spend would disagree on the same
// screen, and the user has no way to tell which to believe.
func TestRecomputingADeadlineUpdatesTheSameClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	// First pass: the CSV delay was not known, so a conservative floor was used.
	id, changed, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV,
		DeadlineHeight: 850_100 + AssumedDeadlineFloor,
		Assumed:        true, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a new deadline was reported as unchanged")
	}

	// Same figures again: nothing to do.
	if _, changed, err = s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV,
		DeadlineHeight: 850_100 + AssumedDeadlineFloor,
		Assumed:        true, UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	} else if changed {
		t.Error("an unchanged deadline was reported as changed")
	}

	// The real delay arrives.
	again, changed, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV,
		DeadlineHeight: 850_100 + 288, Assumed: false, UpdatedAt: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a corrected deadline was reported as unchanged")
	}
	if again != id {
		t.Errorf("recomputing started a second clock: %d then %d", id, again)
	}

	got, err := s.ListDeadlines(ctx, DeadlineCounting)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d deadlines for one spend", len(got))
	}
	if got[0].Assumed {
		t.Error("the deadline still reads as assumed after the real delay arrived")
	}
	if got[0].DeadlineHeight != 850_100+288 {
		t.Errorf("height = %d", got[0].DeadlineHeight)
	}
}

// The three kinds run against the same spend at once: an HTLC can expire before
// the commitment's own window closes, and the user needs the earliest.
func TestOneSpendCanRunSeveralClocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	for kind, height := range map[DeadlineKind]int32{
		DeadlineCSV:          850_388,
		DeadlineHTLCIncoming: 850_200,
		DeadlineHTLCOutgoing: 850_250,
	} {
		if _, _, err := s.UpsertDeadline(ctx, Deadline{
			SpendEventID: spendID, Kind: kind, DeadlineHeight: height, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListDeadlines(ctx, DeadlineCounting)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d deadlines, want one per kind", len(got))
	}
	// Soonest first: that is the order they matter in and the order a screen
	// should show them.
	for i := 1; i < len(got); i++ {
		if got[i].DeadlineHeight < got[i-1].DeadlineHeight {
			t.Errorf("deadlines are not soonest-first: %d before %d",
				got[i-1].DeadlineHeight, got[i].DeadlineHeight)
		}
	}
	if got[0].Kind != DeadlineHTLCIncoming {
		t.Errorf("the earliest deadline is %q, want the incoming HTLC", got[0].Kind)
	}
}

// An escalation only ever moves forward. A restart, or two paths noticing the
// same stage, must not walk the user back through alarms they have already had —
// an alarm that repeats an old stage looks like a new event.
func TestEscalationOnlyMovesForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	id, _, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV, DeadlineHeight: 850_388, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetDeadlineEscalation(ctx, id, 2, 2); err != nil {
		t.Fatal(err)
	}
	// A second path notices the same stage, or an earlier one.
	if err := s.SetDeadlineEscalation(ctx, id, 1, 3); err != nil {
		t.Fatalf("going backwards should be a no-op, not an error: %v", err)
	}

	got, err := s.ListDeadlines(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Escalation != 2 {
		t.Errorf("escalation = %d, want it to have stayed at 2", got[0].Escalation)
	}

	if err := s.SetDeadlineEscalation(ctx, id, 3, 4); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListDeadlines(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Escalation != 3 {
		t.Errorf("escalation = %d, want 3", got[0].Escalation)
	}
}

// Recomputing the height must not reset how far the user has already been
// escalated, or a corrected deadline replays every alarm.
func TestRecomputingDoesNotResetEscalation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	id, _, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV,
		DeadlineHeight: 850_244, Assumed: true, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeadlineEscalation(ctx, id, 2, 2); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV,
		DeadlineHeight: 850_388, Assumed: false, UpdatedAt: 3,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListDeadlines(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Escalation != 2 {
		t.Errorf("escalation = %d after recomputing, want it kept at 2", got[0].Escalation)
	}
	if got[0].State != DeadlineCounting {
		t.Errorf("state = %q after recomputing", got[0].State)
	}
}

func TestDeadlineStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	id, _, err := s.UpsertDeadline(ctx, Deadline{
		SpendEventID: spendID, Kind: DeadlineCSV, DeadlineHeight: 850_388, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	counting, err := s.ListDeadlines(ctx, DeadlineCounting)
	if err != nil {
		t.Fatal(err)
	}
	if len(counting) != 1 {
		t.Fatalf("got %d counting deadlines", len(counting))
	}

	if err := s.SetDeadlineState(ctx, id, DeadlineResolved,
		"ee"+strings.Repeat("0", 62), 5); err != nil {
		t.Fatal(err)
	}

	counting, err = s.ListDeadlines(ctx, DeadlineCounting)
	if err != nil {
		t.Fatal(err)
	}
	if len(counting) != 0 {
		t.Errorf("got %d counting deadlines after one resolved", len(counting))
	}

	resolved, err := s.ListDeadlines(ctx, DeadlineResolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].ResolvedByTxID == "" {
		t.Errorf("got %+v, want the resolving transaction recorded", resolved)
	}
}

func TestDeadlineWritesRejectWhatTheSchemaWouldNotAccept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	spendID := seedSpend(t, s)

	bad := map[string]Deadline{
		"no spend":     {Kind: DeadlineCSV, DeadlineHeight: 1},
		"unknown kind": {SpendEventID: spendID, Kind: "someday", DeadlineHeight: 1},
		"unknown state": {SpendEventID: spendID, Kind: DeadlineCSV,
			DeadlineHeight: 1, State: "pending"},
	}
	for name, d := range bad {
		if _, _, err := s.UpsertDeadline(ctx, d); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	if err := s.SetDeadlineState(ctx, 9999, DeadlineResolved, "", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	if err := s.SetDeadlineState(ctx, 1, "somewhere", "", 1); err == nil {
		t.Error("an unknown state was accepted")
	}
}

// The floor exists so that a missing input never means a missing countdown. An
// alarm that fires early is a bug report; one that never fires is a loss.
func TestTheAssumedFloorIsConservative(t *testing.T) {
	t.Parallel()

	if AssumedDeadlineFloor <= 0 {
		t.Fatal("the floor must be a real number of blocks")
	}
	// Short enough to fire before any realistic channel's window closes. 144
	// blocks is about a day; channels are opened with delays from roughly there
	// upwards, so a shorter floor is safe and a longer one might not be.
	if AssumedDeadlineFloor > 144 {
		t.Errorf("floor = %d, which may fall after a real window has closed",
			AssumedDeadlineFloor)
	}
}
