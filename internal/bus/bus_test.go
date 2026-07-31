package bus

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureHandler records what was logged, so tests can assert that a dropped
// event is reported rather than swallowed.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// at returns the messages logged at a level, with their attributes flattened for
// easy substring assertions.
func (h *captureHandler) at(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []string
	for _, r := range h.records {
		if r.Level != level {
			continue
		}
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" ")
			b.WriteString(a.Key)
			b.WriteString("=")
			b.WriteString(a.Value.String())
			return true
		})
		out = append(out, b.String())
	}
	return out
}

// testEvent is a minimal Event so the bus can be exercised without depending on
// the shape of any real one.
type testEvent struct {
	kind string
	seq  int
}

func (e testEvent) Kind() string { return e.kind }

// recvTimeout is generous: these are in-process channel handoffs, so exceeding it
// means something is genuinely stuck rather than merely slow.
const recvTimeout = 2 * time.Second

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while an event was expected")
		}
		return e
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

func TestFanOutToEverySubscriber(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()

	first := b.Subscribe("first")
	second := b.Subscribe("second")

	b.Publish(testEvent{kind: KindSplitStateChanged})

	// Both must see it: a subscriber that happened to be created second is not
	// less entitled to the event.
	for name, ch := range map[string]<-chan Event{"first": first, "second": second} {
		got := recv(t, ch)
		if got.Kind() != KindSplitStateChanged {
			t.Errorf("%s received %q", name, got.Kind())
		}
	}
}

func TestSubscribeWithNoKindsReceivesEverything(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()

	ch := b.Subscribe("everything")
	for _, k := range AllKinds() {
		b.Publish(testEvent{kind: k})
	}

	for _, want := range AllKinds() {
		got := recv(t, ch)
		if got.Kind() != want {
			t.Errorf("got %q, want %q", got.Kind(), want)
		}
	}
}

func TestKindFilteringExcludesTheRest(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()

	ch := b.Subscribe("picky", KindAlertRaised, KindViewHealthChanged)

	b.Publish(testEvent{kind: KindSplitStateChanged}) // not wanted
	b.Publish(testEvent{kind: KindAlertRaised})       // wanted
	b.Publish(testEvent{kind: KindSplitBranchExtended})
	b.Publish(testEvent{kind: KindViewHealthChanged}) // wanted

	if got := recv(t, ch).Kind(); got != KindAlertRaised {
		t.Errorf("first delivered event was %q, want %q", got, KindAlertRaised)
	}
	if got := recv(t, ch).Kind(); got != KindViewHealthChanged {
		t.Errorf("second delivered event was %q, want %q", got, KindViewHealthChanged)
	}

	select {
	case e := <-ch:
		t.Errorf("received an unwanted event: %q", e.Kind())
	default:
	}
}

func TestOrderIsPreservedPerSubscriber(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()

	ch := b.Subscribe("ordered")
	const n = 50
	for i := range n {
		b.Publish(testEvent{kind: KindSplitBranchExtended, seq: i})
	}
	for i := range n {
		got := recv(t, ch).(testEvent)
		if got.seq != i {
			t.Fatalf("event %d arrived out of order: got seq %d", i, got.seq)
		}
	}
}

// A full buffer means the consumer is stuck, not merely busy. The newest
// description of the world is the useful one, so the oldest pending event gives
// way — and the drop is logged, because a stuck engine is worth knowing about
// even though a missed notification is recoverable from the store.
func TestOverflowDropsTheOldestAndLogsIt(t *testing.T) {
	t.Parallel()

	h := &captureHandler{}
	b := New(slog.New(h))
	defer b.Close()

	ch := b.Subscribe("stuck")

	// Fill the buffer, then overflow it by a known amount without reading.
	const overflow = 5
	for i := range SubscriberBuffer + overflow {
		b.Publish(testEvent{kind: KindSplitBranchExtended, seq: i})
	}

	if got := b.Dropped("stuck"); got != overflow {
		t.Errorf("Dropped = %d, want %d", got, overflow)
	}

	errs := h.at(slog.LevelError)
	if len(errs) != overflow {
		t.Errorf("logged %d errors, want one per drop (%d)", len(errs), overflow)
	}
	if len(errs) > 0 {
		if !strings.Contains(errs[0], "subscriber=stuck") {
			t.Errorf("log does not name the subscriber, so the stuck consumer is unidentifiable: %q",
				errs[0])
		}
		if !strings.Contains(errs[0], "kind="+KindSplitBranchExtended) {
			t.Errorf("log does not name the event kind: %q", errs[0])
		}
	}

	// The survivors are the newest: the oldest `overflow` events were displaced.
	first := recv(t, ch).(testEvent)
	if first.seq != overflow {
		t.Errorf("oldest surviving event has seq %d, want %d — the wrong end was dropped",
			first.seq, overflow)
	}

	drained := 1
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			last := e.(testEvent)
			drained++
			if last.seq == SubscriberBuffer+overflow-1 {
				if drained != SubscriberBuffer {
					t.Errorf("buffer held %d events, want %d", drained, SubscriberBuffer)
				}
				return
			}
		case <-time.After(recvTimeout):
			t.Fatalf("only drained %d events before stalling", drained)
		}
	}
}

// A slow subscriber must not hold up a healthy one, or one stuck engine would
// silence the whole daemon.
func TestOneStuckSubscriberDoesNotBlockAnother(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()

	stuck := b.Subscribe("stuck")
	healthy := b.Subscribe("healthy")
	_ = stuck // deliberately never read

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range SubscriberBuffer * 2 {
			b.Publish(testEvent{kind: KindAlertRaised, seq: i})
		}
	}()

	// The healthy subscriber keeps receiving throughout.
	for range SubscriberBuffer {
		recv(t, healthy)
	}

	select {
	case <-done:
	case <-time.After(recvTimeout):
		t.Fatal("Publish blocked on the stuck subscriber")
	}
}

func TestCloseUnblocksSubscribers(t *testing.T) {
	t.Parallel()

	b := New(nil)
	ch := b.Subscribe("waiting")

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		// Ranging over the channel is how an engine consumes; it must end when the
		// bus closes rather than waiting forever.
		for range ch { //nolint:revive // draining until closed is the point
		}
	}()

	b.Close()

	select {
	case <-blocked:
	case <-time.After(recvTimeout):
		t.Fatal("a subscriber ranging over its channel did not finish after Close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	b := New(nil)
	b.Subscribe("one")
	b.Close()
	b.Close() // must not panic by closing a channel twice
}

// Shutdown is not perfectly ordered. An engine finishing its last write as the
// bus closes should not bring the process down.
func TestPublishAfterCloseIsANoOp(t *testing.T) {
	t.Parallel()

	b := New(nil)
	b.Subscribe("gone")
	b.Close()

	b.Publish(testEvent{kind: KindAlertRaised})
}

func TestSubscribeAfterCloseReturnsAClosedChannel(t *testing.T) {
	t.Parallel()

	b := New(nil)
	b.Close()

	ch := b.Subscribe("late")
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("a late subscriber received an event from a closed bus")
		}
	case <-time.After(recvTimeout):
		t.Error("a late subscriber would wait forever instead of seeing a closed channel")
	}
}

func TestPublishNilIsIgnored(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()
	ch := b.Subscribe("any")

	b.Publish(nil)

	select {
	case e := <-ch:
		t.Errorf("a nil event was delivered as %v", e)
	default:
	}
}

func TestDroppedIsReportedAtCloseForASlowSubscriber(t *testing.T) {
	t.Parallel()

	h := &captureHandler{}
	b := New(slog.New(h))

	b.Subscribe("never-reads")
	for i := range SubscriberBuffer + 3 {
		b.Publish(testEvent{kind: KindAlertRaised, seq: i})
	}
	b.Close()

	// Recorded at close as well as at the time, so a slow consumer leaves a trace
	// even if nobody was watching the logs when it happened.
	var found bool
	for _, w := range h.at(slog.LevelWarn) {
		if strings.Contains(w, "never-reads") && strings.Contains(w, "dropped_total=3") {
			found = true
		}
	}
	if !found {
		t.Errorf("closing did not summarise the losses; warnings were %v", h.at(slog.LevelWarn))
	}
}

func TestDroppedForUnknownSubscriber(t *testing.T) {
	t.Parallel()

	b := New(nil)
	defer b.Close()
	if got := b.Dropped("nobody"); got != 0 {
		t.Errorf("Dropped for an unknown subscriber = %d, want 0", got)
	}
}

// Concurrent publishers, subscribers and a close, to be run under the race
// detector. It asserts only that nothing panics or deadlocks: with this much
// concurrency, which events arrive is not a property worth pinning down.
func TestConcurrentUseIsSafe(t *testing.T) {
	t.Parallel()

	b := New(nil)

	// Signalled once publishing is genuinely under way, so the close below overlaps
	// with it deterministically rather than by sleeping and hoping.
	started := make(chan struct{})
	var once sync.Once

	var wg sync.WaitGroup
	for p := range 4 {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := range 200 {
				b.Publish(testEvent{kind: AllKinds()[i%len(AllKinds())], seq: p*1000 + i})
				once.Do(func() { close(started) })
			}
		}(p)
	}
	for s := range 4 {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			ch := b.Subscribe("consumer")
			_ = s
			for range ch { //nolint:revive // draining until closed is the point
			}
		}(s)
	}

	<-started
	b.Close()
	wg.Wait()
}

func TestEventKindsMatchTheirConstants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		event Event
		want  string
	}{
		{SplitStateChanged{}, KindSplitStateChanged},
		{SplitBranchExtended{}, KindSplitBranchExtended},
		{ViewHealthChanged{}, KindViewHealthChanged},
		{AlertRaised{}, KindAlertRaised},
		{ChannelUpserted{}, KindChannelUpserted},
		{ChannelClosedSF{}, KindChannelClosedSF},
		{FundingSpent{}, KindFundingSpent},
		{SecondOrderSpent{}, KindSecondOrderSpent},
		{SpendReorgedOut{}, KindSpendReorgedOut},
		{MempoolSighting{}, KindMempoolSighting},
		{DeadlineEscalated{}, KindDeadlineEscalated},
		{DeadlineResolved{}, KindDeadlineResolved},
		{DeadlineExpiredLoss{}, KindDeadlineExpiredLoss},
	}
	for _, tc := range cases {
		if got := tc.event.Kind(); got != tc.want {
			t.Errorf("%T.Kind() = %q, want %q", tc.event, got, tc.want)
		}
	}

	// Every kind with an event type must be listed, or a subscriber filtering on
	// AllKinds would silently miss one.
	listed := make(map[string]bool, len(AllKinds()))
	for _, k := range AllKinds() {
		if listed[k] {
			t.Errorf("%q is listed twice in AllKinds", k)
		}
		listed[k] = true
	}
	for _, tc := range cases {
		if !listed[tc.want] {
			t.Errorf("%q has an event type but is missing from AllKinds", tc.want)
		}
	}

	// And the other direction, which was not checked before: a kind listed in
	// AllKinds with no event type behind it means every subscriber filtering on
	// it waits forever for something nothing publishes.
	if len(cases) != len(AllKinds()) {
		t.Errorf("AllKinds lists %d kinds but only %d have an event type here",
			len(AllKinds()), len(cases))
	}
}
