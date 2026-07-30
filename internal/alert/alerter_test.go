package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

var errFailedTransport = errors.New("the server is unreachable")

// recorder is a transport that remembers what it was asked to send, and can be
// told to fail.
type recorder struct {
	name string

	mu   sync.Mutex
	sent []Payload

	fail atomic.Bool
	err  error
}

func newRecorder(name string) *recorder { return &recorder{name: name} }

func (r *recorder) Name() string { return r.name }

func (r *recorder) Send(_ context.Context, p Payload) error {
	if r.fail.Load() {
		return r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, p)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// countKind counts deliveries of one alert kind, so a test can wait on a later
// alert as a barrier and still assert exactly how many of an earlier one arrived.
func (r *recorder) countKind(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.sent {
		if p.Kind == kind {
			n++
		}
	}
	return n
}

func (r *recorder) payloads() []Payload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Payload(nil), r.sent...)
}

type harness struct {
	store *store.Store
	bus   *bus.Bus
	al    *Alerter
	clock *atomic.Int64
}

func newHarness(t *testing.T, routes []Route, mutate func(*Config)) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	cfg := Config{ScanInterval: 5 * time.Millisecond, SendTimeout: time.Second}
	if mutate != nil {
		mutate(&cfg)
	}

	al, err := New(st, b, routes, cfg, nil, func() time.Time {
		return time.Unix(clock.Load(), 0)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &harness{store: st, bus: b, al: al, clock: clock}
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.al.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the alerter did not stop when its context ended")
		}
	})
}

// barrier proves that everything queued before this call has been delivered.
//
// Sleeping and hoping would make these tests both slower and unreliable. The
// sender drains its queue in order, so raising a fresh alert and waiting for it
// establishes that anything enqueued earlier has already been through — which is
// what is needed to assert that something did *not* happen.
func (h *harness) barrier(t *testing.T, rec *recorder) {
	t.Helper()
	ctx := context.Background()
	before := rec.countKind(KindSplitResolving)

	// Acknowledged first so that re-raising it counts as a reopening and is
	// delivered again — otherwise the barrier would work exactly once per test.
	if alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{}); err == nil {
		for _, a := range alerts {
			if a.Kind == KindSplitResolving {
				_, _ = h.store.AckAlert(ctx, a.ID, h.clock.Load())
			}
		}
	}

	h.bus.Publish(bus.SplitStateChanged{Old: "SPLIT", New: "RESOLVING"})
	waitFor(t, "the barrier alert", func() bool {
		return rec.countKind(KindSplitResolving) > before
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// The end-to-end claim: something happens on the chain, and a notification
// arrives at a real HTTP endpoint with the agreed shape.
func TestAnEventReachesAWebhook(t *testing.T) {
	t.Parallel()

	type received struct {
		body        Payload
		contentType string
	}
	got := make(chan received, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p Payload
		_ = json.Unmarshal(body, &p)
		got <- received{body: p, contentType: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	hook, err := NewWebhook("my-server", srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Detail on, so the assertion covers the whole payload rather than the
	// content-free subset.
	h := newHarness(t, []Route{{Transport: hook, MinTier: config.MinTierInfo, IncludeDetail: true}}, nil)
	h.start(t)

	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	select {
	case r := <-got:
		if r.contentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.contentType)
		}
		if r.body.Version != PayloadVersion {
			t.Errorf("forktower = %q, want %q", r.body.Version, PayloadVersion)
		}
		if r.body.Tier != string(store.TierWarning) {
			t.Errorf("tier = %q, want warning", r.body.Tier)
		}
		if r.body.Kind != KindSplitDetected {
			t.Errorf("kind = %q, want %q", r.body.Kind, KindSplitDetected)
		}
		if r.body.Message == "" {
			t.Error("the payload carries no message")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no alert reached the webhook")
	}

	// And the attempt is on record, because a transport that has quietly stopped
	// working is only visible if every attempt is kept.
	waitFor(t, "the delivery to be recorded", func() bool {
		alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
		if err != nil || len(alerts) != 1 {
			return false
		}
		ds, err := h.store.ListDeliveries(context.Background(), alerts[0].ID)
		return err == nil && len(ds) == 1 && ds[0].OK && ds[0].Transport == "my-server"
	})
}

// The same condition twice is one row and one notification. Without this an
// escalating situation buries the user in near-identical messages and the
// important one scrolls away.
func TestTheSameConditionIsRaisedOnce(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}}, nil)
	h.start(t)

	for range 3 {
		h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	}

	waitFor(t, "the first delivery", func() bool { return rec.count() >= 1 })
	h.barrier(t, rec)

	if n := rec.countKind(KindViewDegraded); n != 1 {
		t.Errorf("the same condition was delivered %d times", n)
	}
	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countKindRows(alerts, KindViewDegraded); n != 1 {
		t.Errorf("got %d alert rows for one condition", n)
	}
}

// A condition that returns after the user said they had seen it is news again.
// Without this, a view that flaps is reported once and then never mentioned.
func TestAConditionThatReturnsAfterAcknowledgementIsDeliveredAgain(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}}, nil)
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	waitFor(t, "the first delivery", func() bool { return rec.count() == 1 })

	degraded := h.alertByKind(t, KindViewDegraded)
	if _, err := h.store.AckAlert(context.Background(), degraded.ID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	waitFor(t, "the condition to be reported again", func() bool { return rec.count() == 2 })
}

// An urgent alert nobody has acknowledged is repeated. The user may have been
// asleep, or the phone may have been off.
func TestUnacknowledgedUrgentAlertsAreRepeated(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	// The scan is driven by hand so the assertions are about the repeat interval
	// rather than about when a ticker happened to fire.
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}},
		func(c *Config) {
			c.CriticalRepeat = 30 * time.Minute
			c.ScanInterval = time.Hour
		})
	h.start(t)
	ctx := context.Background()

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "WRONG_BRANCH"})
	waitFor(t, "the first delivery", func() bool { return rec.countKind(KindViewWrongBranch) == 1 })

	// Not yet due.
	h.clock.Add(int64((29 * time.Minute).Seconds()))
	h.al.escalate(ctx)
	h.barrier(t, rec)
	if n := rec.countKind(KindViewWrongBranch); n != 1 {
		t.Fatalf("delivered %d times before the repeat was due", n)
	}

	h.clock.Add(int64((2 * time.Minute).Seconds()))
	h.al.escalate(ctx)
	waitFor(t, "the repeat", func() bool { return rec.countKind(KindViewWrongBranch) == 2 })

	// Acknowledging stops it, or the alert becomes noise the user learns to
	// dismiss without reading.
	urgent := h.alertByKind(t, KindViewWrongBranch)
	if _, err := h.store.AckAlert(ctx, urgent.ID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}

	before := rec.countKind(KindViewWrongBranch)
	h.clock.Add(int64((60 * time.Minute).Seconds()))
	h.al.escalate(ctx)
	h.barrier(t, rec)
	if n := rec.countKind(KindViewWrongBranch); n != before {
		t.Errorf("an acknowledged alert was repeated %d more times", n-before)
	}
}

// Only urgent alerts nag. Repeating everything is how people learn to ignore
// notifications entirely.
func TestOrdinaryAlertsAreNotRepeated(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}},
		func(c *Config) {
			c.CriticalRepeat = time.Minute
			c.ScanInterval = time.Hour
		})
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	waitFor(t, "the first delivery", func() bool { return rec.countKind(KindViewDegraded) == 1 })

	h.clock.Add(int64((3 * time.Hour).Seconds()))
	h.al.escalate(context.Background())
	h.barrier(t, rec)

	if n := rec.countKind(KindViewDegraded); n != 1 {
		t.Errorf("a warning was repeated %d times", n)
	}
}

// Each transport gets what it asked for and nothing else.
func TestEachTransportIsFilteredAndToldSeparately(t *testing.T) {
	t.Parallel()

	quiet := newRecorder("critical-only")
	chatty := newRecorder("everything")
	private := newRecorder("my-own-server")

	h := newHarness(t, []Route{
		{Transport: quiet, MinTier: config.MinTierCritical},
		{Transport: chatty, MinTier: config.MinTierInfo},
		{Transport: private, MinTier: config.MinTierInfo, IncludeDetail: true},
	}, nil)
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	waitFor(t, "the warning to be delivered", func() bool {
		return chatty.count() == 1 && private.count() == 1
	})

	// An urgent alert afterwards proves the quiet transport is reachable at all,
	// and that it had already had its chance at the warning.
	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "DOWN", New: "WRONG_BRANCH"})
	waitFor(t, "the urgent alert", func() bool {
		return quiet.countKind(KindViewWrongBranch) == 1
	})

	if n := quiet.countKind(KindViewDegraded); n != 0 {
		t.Errorf("a critical-only transport received %d warnings", n)
	}

	// Detail off by default: the third-party transport is told the category and
	// nothing about the user's situation.
	if p := chatty.payloads()[0]; p.Subject != "" || p.Message != ContentFreeMessage {
		t.Errorf("a transport without detail received %+v", p)
	}
	if p := private.payloads()[0]; p.Subject == "" || p.Message == ContentFreeMessage {
		t.Errorf("a transport with detail received a content-free payload: %+v", p)
	}
}

// A transport that fails must not stop the others, and the failure must be
// recorded rather than dropped.
func TestOneFailingTransportDoesNotSilenceTheRest(t *testing.T) {
	t.Parallel()

	broken := newRecorder("broken")
	broken.err = errFailedTransport
	broken.fail.Store(true)
	working := newRecorder("working")

	h := newHarness(t, []Route{
		{Transport: broken, MinTier: config.MinTierInfo},
		{Transport: working, MinTier: config.MinTierInfo},
	}, nil)
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	waitFor(t, "the working transport to receive the alert", func() bool {
		return working.count() == 1
	})

	waitFor(t, "both attempts to be recorded", func() bool {
		alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
		if err != nil || len(alerts) != 1 {
			return false
		}
		ds, err := h.store.ListDeliveries(context.Background(), alerts[0].ID)
		return err == nil && len(ds) == 2
	})

	alerts, _ := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	ds, _ := h.store.ListDeliveries(context.Background(), alerts[0].ID)
	var sawFailure bool
	for _, d := range ds {
		if d.Transport == "broken" {
			sawFailure = true
			if d.OK {
				t.Error("a failed delivery was recorded as successful")
			}
			if d.Error == "" {
				t.Error("a failure was recorded with no reason, so nobody can diagnose it")
			}
		}
	}
	if !sawFailure {
		t.Error("the failing transport's attempt was not recorded at all")
	}
}

// An alert raised is announced, so the dashboard updates without polling.
func TestRaisingAnAlertIsAnnounced(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, nil)
	events := h.bus.Subscribe("test", bus.KindAlertRaised)
	h.start(t)

	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	select {
	case e := <-events:
		raised, ok := e.(bus.AlertRaised)
		if !ok {
			t.Fatalf("got %T, want AlertRaised", e)
		}
		if raised.AlertKind != KindSplitDetected {
			t.Errorf("kind = %q, want %q", raised.AlertKind, KindSplitDetected)
		}
		if raised.AlertID == 0 {
			t.Error("the announcement carries no alert id, so the UI cannot look it up")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("raising an alert was never announced")
	}
}

// Alerts are still recorded with no transports configured — a user may be
// watching the dashboard and nothing else.
func TestAlertsAreRecordedWithoutAnyTransports(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, nil)
	h.start(t)

	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	waitFor(t, "the alert to be stored", func() bool {
		alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
		return err == nil && len(alerts) == 1
	})
}

// The window this closes: the daemon is wired up, something happens, and only
// then does the alerter's goroutine get scheduled. Unlike the sentinel this
// engine has no poll to fall back on, so an event missed here is missed forever.
func TestEventsPublishedBeforeRunStartsAreNotLost(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}}, nil)

	// Published before Run is ever called.
	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	h.start(t)

	waitFor(t, "the alert raised before Run started", func() bool { return rec.count() == 1 })
}

// The bus closing is shutdown, not a reason to stop nagging: an urgent alert
// already raised still needs repeating until someone acknowledges it.
func TestEscalationSurvivesTheBusClosing(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}},
		func(c *Config) { c.CriticalRepeat = time.Minute })
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "WRONG_BRANCH"})
	waitFor(t, "the first delivery", func() bool { return rec.count() == 1 })

	h.bus.Close()

	h.clock.Add(int64((5 * time.Minute).Seconds()))
	waitFor(t, "the repeat after the bus closed", func() bool { return rec.count() >= 2 })
}

// An alert nothing could be delivered for is retried rather than abandoned. It is
// measured from when it was raised, so the first scan does not immediately retry
// something that was raised a moment ago.
func TestAnAlertThatWasNeverDeliveredIsRetried(t *testing.T) {
	t.Parallel()

	broken := newRecorder("broken")
	broken.err = errFailedTransport
	broken.fail.Store(true)

	h := newHarness(t, []Route{{Transport: broken, MinTier: config.MinTierInfo}},
		func(c *Config) { c.CriticalRepeat = time.Minute })
	h.start(t)

	h.bus.Publish(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "WRONG_BRANCH"})

	countAttempts := func() int {
		alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
		if err != nil || len(alerts) != 1 {
			return 0
		}
		ds, err := h.store.ListDeliveries(context.Background(), alerts[0].ID)
		if err != nil {
			return 0
		}
		return len(ds)
	}

	waitFor(t, "the first failed attempt", func() bool { return countAttempts() == 1 })

	h.clock.Add(int64((5 * time.Minute).Seconds()))
	waitFor(t, "a retry after the repeat interval", func() bool { return countAttempts() >= 2 })

	// And once it works, the user is told.
	broken.fail.Store(false)
	h.clock.Add(int64((5 * time.Minute).Seconds()))
	waitFor(t, "the alert to finally arrive", func() bool { return broken.count() >= 1 })
}

// alertByKind finds the one alert of a kind, failing the test if it is not there.
func (h *harness) alertByKind(t *testing.T, kind string) store.Alert {
	t.Helper()
	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Kind == kind {
			return a
		}
	}
	t.Fatalf("no %q alert was recorded; got %+v", kind, alerts)
	return store.Alert{}
}

func countKindRows(alerts []store.Alert, kind string) int {
	n := 0
	for _, a := range alerts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}
