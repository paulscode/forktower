package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/app"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

const sharedHistory = 200

type harness struct {
	app    *app.App
	sf, sq *chainviewtest.View
	cfg    config.Config
	base   string
	done   chan error
	// delivered counts notifications that actually arrived, so a test can tell a
	// configured transport from a working one.
	delivered *atomic.Int64
}

// newHarness builds the whole daemon against two scriptable chain views, which is
// the only way to assert startup ordering without two real Bitcoin nodes — and a
// test that needs those is a test nobody runs.
func newHarness(t *testing.T, mutate func(*config.Config, *chainviewtest.View, *chainviewtest.View)) *harness {
	t.Helper()
	ctx := context.Background()

	sf, sq := chainviewtest.NewSharedHistory(sharedHistory)
	sf.SetIdentity(chainview.Identity{
		Endpoint: "http://own-node:8332", LocalAddresses: []string{"own:8333"},
	})
	sq.SetIdentity(chainview.Identity{
		Endpoint: "http://other-node:8432", LocalAddresses: []string{"other:8433"},
	})

	// A notification channel that genuinely works. Without one the daemon is
	// correct to report that it has no way to reach the user — see
	// TestADaemonWithNowhereToSendAlertsSaysSo — and every other assertion here
	// would be about that instead.
	delivered := &atomic.Int64{}
	notifications := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(notifications.Close)

	cfg := config.Default()
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
	cfg.Sentinel.PollIntervalSecs = 1
	cfg.UI.Auth = config.AuthNone
	cfg.Alerts.Transport = []config.TransportConfig{{
		Name: "my-server", Type: config.TransportWebhook,
		URL: notifications.URL, MinTier: config.MinTierInfo,
	}}
	if mutate != nil {
		mutate(&cfg, sf, sq)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	daemon, err := app.New(ctx, cfg, nil, app.Deps{SF: sf, SQ: sq, Listener: listener})
	if err != nil {
		_ = listener.Close()
		t.Fatalf("building the daemon: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Close() })

	return &harness{
		app: daemon, sf: sf, sq: sq, cfg: cfg,
		base:      "http://" + daemon.Addr(),
		delivered: delivered,
	}
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.done = make(chan error, 1)
	go func() { h.done <- h.app.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(app.ShutdownTimeout + 5*time.Second):
			t.Error("the daemon did not stop when asked")
		}
	})

	// The dashboard answering is the signal that everything behind it started.
	waitFor(t, "the daemon to come up", func() bool {
		resp, err := http.Get(h.base + "/api/v1/healthz") //nolint:noctx // a liveness poll in a test
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// status reads the one endpoint that answers everything.
func (h *harness) status(t *testing.T) map[string]any {
	t.Helper()
	const path = "/api/v1/status"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.base+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  map[string]any `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decoding %s: %v — %s", path, err, body)
	}
	if envelope.Error != nil {
		t.Fatalf("GET %s failed: %s — %s", path, envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Data
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The first time any of this runs as a daemon: configuration in, a dashboard
// answering, and a clean stop.
func TestTheDaemonStartsServesAndStops(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.start(t)

	status := h.status(t)
	headline, ok := status["headline"].(map[string]any)
	if !ok {
		t.Fatalf("no headline in %v", status)
	}
	if headline["title"] == "" {
		t.Error("the dashboard has nothing to say")
	}
	if len(status["readiness"].([]any)) == 0 {
		t.Error("no readiness checks were reported")
	}

	// And the page itself, which shares the listener.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.base+"/", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "<!DOCTYPE html>") {
		t.Errorf("the dashboard was not served: %q", page[:min(80, len(page))])
	}
}

// The engines reach agreement and the daemon settles into watching, which is what
// "it works" means before anything has gone wrong.
func TestTheDaemonReachesTheWatchingState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.start(t)

	waitFor(t, "the daemon to start watching", func() bool {
		status := h.status(t)
		split, _ := status["split"].(map[string]any)
		return split != nil && split["state"] == string(store.StateArmed)
	})

	// And it says so in words a person can read, without an internal name.
	status := h.status(t)
	headline := status["headline"].(map[string]any)
	if headline["state"] != "protected" {
		t.Errorf("headline state = %v, want protected", headline["state"])
	}
	for _, leak := range []string{"ARMED", "UNARMED", "sq_", "sf_"} {
		text, _ := headline["title"].(string)
		detail, _ := headline["detail"].(string)
		if strings.Contains(text+detail, leak) {
			t.Errorf("%q reached the headline", leak)
		}
	}
}

// A split detected by the daemon has to reach all three places it belongs: the
// dashboard, the alert list, and the timeline. This is the whole product in one
// assertion.
func TestASplitReachesTheDashboardTheAlertsAndTheTimeline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Config, _, _ *chainviewtest.View) {
		c.Sentinel.SplitConfirmDepth = 2
	})
	h.start(t)

	waitFor(t, "the daemon to start watching", func() bool {
		status := h.status(t)
		split, _ := status["split"].(map[string]any)
		return split != nil && split["state"] == string(store.StateArmed)
	})

	// The chains part ways.
	h.sf.Extend("ours", 5)
	h.sq.Extend("theirs", 5)

	waitFor(t, "the split to be reported", func() bool {
		status := h.status(t)
		split, _ := status["split"].(map[string]any)
		return split != nil && split["state"] == string(store.StateSplit)
	})

	status := h.status(t)
	headline := status["headline"].(map[string]any)
	if headline["state"] != "attention" {
		t.Errorf("headline state = %v, want attention", headline["state"])
	}
	if !strings.Contains(headline["detail"].(string), "separated") {
		t.Errorf("the headline does not say what happened: %v", headline["detail"])
	}

	// The alert list.
	waitFor(t, "an alert about the split", func() bool {
		for _, raw := range listOf(t, h, "/api/v1/alerts") {
			if entry, ok := raw.(map[string]any); ok && entry["kind"] == "split_detected" {
				return true
			}
		}
		return false
	})

	// And the timeline, in plain language.
	waitFor(t, "the split in the timeline", func() bool {
		for _, raw := range listOf(t, h, "/api/v1/timeline") {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if summary, _ := entry["summary"].(string); strings.Contains(summary, "separated") {
				return true
			}
		}
		return false
	})
}

// A view on another network answers every request correctly and diverges
// permanently, so nothing downstream could tell it apart from a real split.
// Refusing to start beats spending a week confidently watching the wrong chain.
func TestTheDaemonRefusesTwoDifferentNetworks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")

	// A different first block, which is what actually makes it another network.
	// Rewriting later history would not: the genesis block survives a
	// reorganisation, which is the whole reason it is the thing compared.
	sf, _ := chainviewtest.NewSharedHistory(10)
	elsewhere := chainviewtest.New("another-network")

	daemon, err := app.New(ctx, cfg, nil, app.Deps{SF: sf, SQ: elsewhere})
	if err != nil {
		t.Fatalf("building the daemon: %v", err)
	}
	t.Cleanup(func() { _ = daemon.Close() })

	err = daemon.Run(runContext(t))
	if err == nil {
		t.Fatal("the daemon started against a view on another chain")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

// The default experience with nothing configured: the daemon works, and says
// plainly that it cannot reach anyone. That is accurate rather than alarmist —
// an alarm nobody can hear is incomplete setup — and it resolves itself once a
// platform's own notifications are wired in.
func TestADaemonWithNowhereToSendAlertsSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config.Config, _, _ *chainviewtest.View) {
		c.Alerts.Transport = nil
	})
	h.start(t)

	status := h.status(t)
	headline := status["headline"].(map[string]any)
	if headline["state"] != "action_needed" {
		t.Errorf("headline state = %v, want action_needed", headline["state"])
	}
	action, _ := headline["action"].(map[string]any)
	if action == nil || action["label"] == "" {
		t.Error("the user is told something is wrong with nothing to do about it")
	}
}

// The alarm proves itself on first run rather than waiting a week, so a broken
// notification channel is found while someone is still looking at the screen.
func TestTheDaemonTestsItsOwnAlarmOnFirstRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.start(t)

	waitFor(t, "the first-run notification test", func() bool {
		return h.delivered.Load() > 0
	})
}

// One node behind both configurations produces views that agree by construction,
// so divergence becomes unrepresentable and every indicator stays green forever.
// A single mis-wired setting does it.
func TestTheDaemonRefusesTwoViewsOfOneNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(_ *config.Config, sf, sq *chainviewtest.View) {
		same := chainview.Identity{
			Endpoint: "http://node:8332", LocalAddresses: []string{"node:8333"},
		}
		sf.SetIdentity(same)
		sq.SetIdentity(same)
	})

	err := h.app.Run(runContext(t))
	if err == nil {
		t.Fatal("the daemon started with both views pointed at one node")
	}
	if !strings.Contains(err.Error(), "same") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

// A configuration that could never work must fail while someone is watching the
// terminal, not hours later.
func TestTheDaemonRefusesAnUnusableConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
	cfg.Alerts.Transport = []config.TransportConfig{
		{Name: "typo", Type: config.TransportWebhook, URL: "not a url"},
	}

	sf, sq := chainviewtest.NewSharedHistory(1)
	_, err := app.New(ctx, cfg, nil, app.Deps{SF: sf, SQ: sq})
	if err == nil {
		t.Fatal("a notification channel that could never deliver was accepted")
	}
	if !strings.Contains(err.Error(), "notification") {
		t.Errorf("the error does not say which part is wrong: %v", err)
	}
}

// Stopping twice, and stopping something that never ran, must both be harmless:
// shutdown paths race, and one of them failing loudly is noise an operator has to
// interpret at the worst moment.
func TestClosingIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	if err := h.app.Close(); err != nil {
		t.Fatalf("closing a daemon that never ran: %v", err)
	}
	if err := h.app.Close(); err != nil {
		t.Errorf("closing twice: %v", err)
	}
}

func listOf(t *testing.T, h *harness, path string) []any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.base+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Data []any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func runContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
