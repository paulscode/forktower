package store

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
)

func startTimeline(t *testing.T, s *Store, clock *atomic.Int64) *bus.Bus {
	t.Helper()

	b := bus.New(nil)
	t.Cleanup(b.Close)

	sub := NewTimelineSubscriber(s, b, nil, func() int64 { return clock.Load() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the timeline subscriber returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the timeline subscriber did not stop when asked")
		}
	})
	return b
}

func waitForTimeline(t *testing.T, s *Store, want int) []TimelineEntry {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		entries, err := s.ListTimeline(context.Background(), 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) >= want {
			return entries
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d timeline entries, have %d", want, len(entries))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// The timeline is what someone reads afterwards to understand what happened, and
// what a support bundle carries. Missing an event defeats both.
func TestTheTimelineRecordsEveryKindOfEvent(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)
	b := startTimeline(t, s, clock)

	events := []bus.Event{
		bus.SplitStateChanged{Old: "UNARMED", New: "ARMED"},
		bus.SplitBranchExtended{Branch: "sq", Block: bus.BlockMetaJSON{Height: 5}},
		bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"},
		bus.AlertRaised{AlertID: 1, Tier: "warning", AlertKind: "x", Message: "something"},
	}
	for _, e := range events {
		b.Publish(e)
	}

	entries := waitForTimeline(t, s, len(events))
	if len(entries) != len(events) {
		t.Fatalf("recorded %d entries for %d events", len(entries), len(events))
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Kind] = true
		if entry.Summary == "" {
			t.Errorf("%s was recorded with nothing to read", entry.Kind)
		}
		if entry.At != clock.Load() {
			t.Errorf("%s recorded at %d, want %d", entry.Kind, entry.At, clock.Load())
		}
		// The payload is the Details view, and it has to survive the round trip.
		if entry.Data != "" && !json.Valid([]byte(entry.Data)) {
			t.Errorf("%s stored an unreadable payload: %s", entry.Kind, entry.Data)
		}
	}
	for _, kind := range bus.AllKinds() {
		if !seen[kind] {
			t.Errorf("%s never reached the timeline", kind)
		}
	}
}

// A goroutine that subscribes when it happens to be scheduled leaves a window
// during startup where events are published to nobody, and the timeline's whole
// job is to have missed nothing.
func TestEventsPublishedBeforeTheSubscriberRunsAreKept(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)
	sub := NewTimelineSubscriber(s, b, nil, func() int64 { return clock.Load() })

	// Published before Run is ever called.
	b.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()

	entries := waitForTimeline(t, s, 1)
	if !strings.Contains(entries[0].Summary, "separated") {
		t.Errorf("got %q, want the split", entries[0].Summary)
	}
}

// The register here is the past tense, read afterwards — not an alert, which
// interrupts someone and has to say what to do. Sharing one string between the
// two would make both worse.
func TestSummariesReadAsPlainSentences(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event bus.Event
		want  string
	}{
		{bus.SplitStateChanged{Old: "UNARMED", New: "ARMED"}, "started watching"},
		{bus.SplitStateChanged{Old: "ARMED", New: "UNARMED"}, "stopped watching"},
		{bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"}, "separated"},
		{bus.SplitStateChanged{Old: "SPLIT", New: "RESOLVING"}, "may be ending"},
		{bus.SplitStateChanged{Old: "RESOLVING", New: "RESOLVED_SF_WON"}, "over"},
		{bus.SplitStateChanged{Old: "RESOLVING", New: "RESOLVED_SQ_WON"}, "over"},
		{bus.SplitStateChanged{New: "SOMETHING_NEW"}, "changed"},
		{bus.SplitBranchExtended{Branch: "sf"}, "your node's chain"},
		{bus.SplitBranchExtended{Branch: "sq"}, "the other chain"},
		{bus.SplitBranchExtended{Branch: "elsewhere"}, "one of the chains"},
		{bus.ViewHealthChanged{View: "sq", New: viewStateOK}, "could see the other chain again"},
		{bus.ViewHealthChanged{View: "sq", New: viewStateWrongBranch}, "paused watching"},
		{bus.ViewHealthChanged{View: "sf", New: "DOWN"}, "trouble seeing your node's chain"},
		{bus.AlertRaised{Message: "the chains separated"}, "raised an alert"},
	}

	// Anything this audience must never be shown, plus the internal names that
	// mean nothing outside this codebase.
	forbidden := []string{
		"UNARMED", "ARMED", "SPLIT", "RESOLVING", "RESOLVED_", viewStateWrongBranch,
		"ECLIPSE_SUSPECT", "SYNCING", "DEGRADED", "outpoint", "reorg", "macaroon",
	}

	for _, tc := range cases {
		got := Summarize(tc.event)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%T(%+v) summarised as %q, want it to mention %q",
				tc.event, tc.event, got, tc.want)
		}
		if !strings.HasSuffix(got, ".") {
			t.Errorf("%q is not a sentence", got)
		}
		for _, word := range forbidden {
			if strings.Contains(got, word) {
				t.Errorf("%q contains %q, which a non-technical reader would not understand",
					got, word)
			}
		}
	}
}

// An event kind a later version adds still belongs in the record: a gap where
// something happened is worse than an unhelpful line.
func TestAnUnknownEventIsStillRecorded(t *testing.T) {
	t.Parallel()

	got := Summarize(unknownEvent{})
	if got == "" {
		t.Fatal("an unrecognised event summarised to nothing")
	}
	if !strings.HasSuffix(got, ".") {
		t.Errorf("%q is not a sentence", got)
	}
}

type unknownEvent struct{}

func (unknownEvent) Kind() string { return "something.new" }

// The event has already happened; losing the record of it because the daemon was
// stopping would leave a gap exactly where someone will later look.
func TestARecordSurvivesShutdown(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)
	sub := NewTimelineSubscriber(s, b, nil, func() int64 { return clock.Load() })

	// A context that is already finished, as it would be mid-shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sub.record(ctx, bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	entries, err := s.ListTimeline(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("recorded %d entries, want the event to have survived shutdown", len(entries))
	}
}
