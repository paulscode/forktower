package alert

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

// clearSelfTestHistory undoes the harness's suppression, so the loop behaves as
// it would on a machine where Forktower has just been installed.
func (h *harness) clearSelfTestHistory(t *testing.T) {
	t.Helper()
	if err := h.store.SetMetaInt64(
		context.Background(), store.MetaLastSelfTestAt, 0); err != nil {
		t.Fatal(err)
	}
}

// The whole point of the feature: a notification path nobody has exercised is an
// assumption, and the moment it is needed is the worst time to find out the URL
// was wrong.
func TestTheSelfTestReachesEveryTransport(t *testing.T) {
	t.Parallel()

	// Deliberately set above what the self-test carries. A transport a user
	// restricted to critical-only still has to be *proven* to work, and proving it
	// is the point.
	quiet := newRecorder("critical-only")
	chatty := newRecorder("everything")

	h := newHarness(t, []Route{
		{Transport: quiet, MinTier: config.MinTierCritical},
		{Transport: chatty, MinTier: config.MinTierInfo},
	}, nil)

	results := h.al.SelfTest(context.Background())

	if len(results) != 2 {
		t.Fatalf("got %d results, want one per transport", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("transport %q reported %q", r.Transport, r.Error)
		}
	}
	if quiet.countKind(KindSelfTest) != 1 {
		t.Error("a critical-only transport was never tested, so it is still an assumption")
	}
	if chatty.countKind(KindSelfTest) != 1 {
		t.Error("the test message did not arrive")
	}

	// The message says what it is, so a user who receives it at 3am is not
	// alarmed by their own alarm.
	msg := chatty.payloads()[0].Message
	if !strings.Contains(strings.ToLower(msg), "test") {
		t.Errorf("the test message does not identify itself: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "nothing is wrong") {
		t.Errorf("the test message does not reassure: %q", msg)
	}
}

// A broken alarm has to be alarming. Silently failing the test would leave the
// user believing they are covered.
func TestAFailingTransportRaisesAWarning(t *testing.T) {
	t.Parallel()

	broken := newRecorder("my-webhook")
	broken.err = errFailedTransport
	broken.fail.Store(true)
	working := newRecorder("my-phone")

	h := newHarness(t, []Route{
		{Transport: broken, MinTier: config.MinTierInfo},
		{Transport: working, MinTier: config.MinTierInfo},
	}, nil)
	h.start(t)
	ctx := context.Background()

	results := h.al.SelfTest(ctx)

	var sawFailure bool
	for _, r := range results {
		if r.Transport == "my-webhook" {
			sawFailure = true
			if r.OK {
				t.Error("a transport that returned an error was reported as working")
			}
			if r.Error == "" {
				t.Error("the failure has no reason, so nobody can fix it")
			}
		}
	}
	if !sawFailure {
		t.Fatal("the failing transport produced no result at all")
	}

	// And the warning is raised, so it shows on the dashboard rather than only in
	// a log nobody reads.
	warning := h.alertByKind(t, KindTransportFailing)
	if warning.Tier != store.TierWarning {
		t.Errorf("tier = %q, want warning", warning.Tier)
	}
	if warning.DedupKey != KindTransportFailing+":my-webhook" {
		t.Errorf("dedup key = %q, want it to name the transport", warning.DedupKey)
	}
	if !strings.Contains(warning.Message, "my-webhook") {
		t.Errorf("the warning does not say which transport failed: %q", warning.Message)
	}

	// The warning goes out through the transports that do work, which is the only
	// way the user hears about the one that does not.
	waitFor(t, "the warning to reach a working transport", func() bool {
		return working.countKind(KindTransportFailing) == 1
	})
}

// One failing transport must not be reported as all of them failing.
func TestOnlyTheFailingTransportIsBlamed(t *testing.T) {
	t.Parallel()

	broken := newRecorder("broken")
	broken.err = errFailedTransport
	broken.fail.Store(true)
	working := newRecorder("working")

	h := newHarness(t, []Route{
		{Transport: broken, MinTier: config.MinTierInfo},
		{Transport: working, MinTier: config.MinTierInfo},
	}, nil)

	h.al.SelfTest(context.Background())

	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if n := countKindRows(alerts, KindTransportFailing); n != 1 {
		t.Errorf("got %d transport-failure alerts for one broken transport", n)
	}
}

// A warning about a transport that has since started working again must not keep
// asking for attention: the condition is over, and there is nothing to do.
func TestAFixedTransportStopsBeingReportedAsBroken(t *testing.T) {
	t.Parallel()

	broken := newRecorder("my-webhook")
	broken.err = errFailedTransport
	broken.fail.Store(true)

	h := newHarness(t, []Route{{Transport: broken, MinTier: config.MinTierInfo}}, nil)
	ctx := context.Background()

	h.al.SelfTest(ctx)
	failing := h.alertByKind(t, KindTransportFailing)
	if failing.Acked() {
		t.Fatal("a live failure was already marked as handled")
	}

	broken.fail.Store(false)
	h.clock.Add(int64(time.Hour.Seconds()))
	h.al.SelfTest(ctx)

	got, err := h.store.GetAlert(ctx, failing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Acked() {
		t.Error("the transport works again but is still reported as broken")
	}

	// And if it breaks again, the user hears about it: the raise reopens the same
	// alert rather than being swallowed as a duplicate.
	broken.fail.Store(true)
	h.clock.Add(int64(time.Hour.Seconds()))
	h.al.SelfTest(ctx)

	got, err = h.store.GetAlert(ctx, failing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Acked() {
		t.Error("a transport that broke again is still marked as handled")
	}
}

// A user who has just set notifications up wants to know now that they work, not
// in a week.
func TestTheSelfTestRunsOnceAtFirstStart(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}},
		func(c *Config) { c.SelfTestInterval = 168 * time.Hour })
	h.clearSelfTestHistory(t)
	h.start(t)

	waitFor(t, "the first-run notification test", func() bool {
		return rec.countKind(KindSelfTest) == 1
	})

	// And then it waits: a weekly test that ran every minute would be worse than
	// none, because people would silence it.
	h.clock.Add(int64((24 * time.Hour).Seconds()))
	h.barrier(t, rec)
	if n := rec.countKind(KindSelfTest); n != 1 {
		t.Errorf("the test ran %d times within a day of a weekly schedule", n)
	}

	h.clock.Add(int64((7 * 24 * time.Hour).Seconds()))
	waitFor(t, "the scheduled test a week later", func() bool {
		return rec.countKind(KindSelfTest) == 2
	})
}

// Nothing to test, and nothing to say about it.
func TestTheSelfTestDoesNothingWithoutTransports(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, nil)
	h.clearSelfTestHistory(t)

	if results := h.al.SelfTest(context.Background()); results != nil {
		t.Errorf("got %v, want no results at all", results)
	}
	alerts, err := h.store.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Errorf("a test with nothing to test recorded %d alerts", len(alerts))
	}
}

// The self-test is on record, so the dashboard can say when the alarm was last
// proven to work rather than only that it is configured.
func TestTheSelfTestRecordsWhenItRan(t *testing.T) {
	t.Parallel()

	rec := newRecorder("phone")
	h := newHarness(t, []Route{{Transport: rec, MinTier: config.MinTierInfo}}, nil)
	ctx := context.Background()

	h.al.SelfTest(ctx)

	at, err := h.store.GetMetaInt64(ctx, store.MetaLastSelfTestAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if at != h.clock.Load() {
		t.Errorf("recorded %d, want %d", at, h.clock.Load())
	}

	// Every attempt is on record too, so a transport that quietly stopped working
	// is visible in the history rather than only in the moment.
	alert := h.alertByKind(t, KindSelfTest)
	ds, err := h.store.ListDeliveries(ctx, alert.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || !ds[0].OK || ds[0].Transport != "phone" {
		t.Errorf("delivery history is %+v", ds)
	}
}

// clockAdvancer stands in for a transport that takes real time to answer.
type clockAdvancer struct {
	*recorder
	clock *atomic.Int64
	by    int64
}

func (c *clockAdvancer) Send(ctx context.Context, p Payload) error {
	c.clock.Add(c.by)
	return c.recorder.Send(ctx, p)
}

// The interval is measured from when the test started, not when it finished.
// Measuring from completion lets a slow transport push the schedule later on
// every run — half an hour of timeouts a week is a fortnight of drift a year,
// and the whole point is that this happens on a known cadence.
func TestTheSelfTestIsStampedWhenItBeganNotWhenItFinished(t *testing.T) {
	t.Parallel()

	rec := newRecorder("slow")
	h := newHarness(t, nil, nil)

	slow := &clockAdvancer{recorder: rec, clock: h.clock, by: 90}
	h.al.routes = []Route{{Transport: slow, MinTier: config.MinTierInfo}}

	started := h.clock.Load()
	h.al.SelfTest(context.Background())

	if h.clock.Load() == started {
		t.Fatal("the fake transport did not advance the clock, so this proves nothing")
	}

	at, err := h.store.GetMetaInt64(context.Background(), store.MetaLastSelfTestAt, 0)
	if err != nil {
		t.Fatal(err)
	}
	if at != started {
		t.Errorf("recorded %d, want %d — the schedule drifts by however long delivery took",
			at, started)
	}
}
