package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	return newHarnessWith(t, mutate, nil)
}

// newHarnessWith also lets a test substitute the daemon's dependencies, which is
// how a Lightning node is stood in for without one existing.
func newHarnessWith(
	t *testing.T,
	mutate func(*config.Config, *chainviewtest.View, *chainviewtest.View),
	adjust func(*app.Deps),
) *harness {
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

	deps := app.Deps{SF: sf, SQ: sq, Listener: listener}
	if adjust != nil {
		adjust(&deps)
	}

	daemon, err := app.New(ctx, cfg, nil, deps)
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

	// A port of its own. Taking the configured one would make this test pass or
	// fail depending on what else happens to be running on the machine.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	daemon, err := app.New(ctx, cfg, nil,
		app.Deps{SF: sf, SQ: elsewhere, Listener: listener})
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

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	sf, sq := chainviewtest.NewSharedHistory(1)
	_, err = app.New(ctx, cfg, nil, app.Deps{SF: sf, SQ: sq, Listener: listener})
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

// Someone who installs Forktower *because* they heard the chains had split is
// the likely user during a real fork. Before this worked, the daemon sat at
// "getting set up, nothing to do yet" forever — the calmest message it has, at
// the worst possible moment — because arming requires the chains to agree and
// there was no path from there to a split.
func TestADaemonStartedAfterASplitStillFindsIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *config.Config, sf, sq *chainviewtest.View) {
		c.Sentinel.SplitConfirmDepth = 2
		// The chains have already parted ways before anything starts.
		sf.Extend("ours", 5)
		sq.Extend("theirs", 5)
	})
	h.start(t)

	waitFor(t, "the split to be found from a standing start", func() bool {
		status := h.status(t)
		split, _ := status["split"].(map[string]any)
		return split != nil && split["state"] == string(store.StateSplit)
	})

	status := h.status(t)
	headline := status["headline"].(map[string]any)
	if headline["state"] == "getting_ready" {
		t.Fatal("a live split is being reported as ordinary start-up")
	}
	if !strings.Contains(headline["detail"].(string), "separated") {
		t.Errorf("the headline does not mention the split: %v", headline["detail"])
	}

	// And the user is told, rather than only shown.
	waitFor(t, "an alert about the split", func() bool {
		for _, raw := range listOf(t, h, "/api/v1/alerts") {
			if entry, ok := raw.(map[string]any); ok && entry["kind"] == "split_detected" {
				return true
			}
		}
		return false
	})

	// The separation point is recorded, which is what bounds how far the user's
	// own chain is trusted — the reason a late install needed care in the first
	// place.
	split := status["split"].(map[string]any)
	fork, _ := split["fork"].(map[string]any)
	if fork == nil {
		t.Fatal("no separation point was recorded")
	}
	if height, _ := fork["height"].(float64); int32(height) != sharedHistory {
		t.Errorf("separation point at height %v, want %d", fork["height"], sharedHistory)
	}
}

// A companion watchtower is switched on, and the daemon builds a warden for it
// without needing the tower to be running.
//
// The tower's own reachability is the warden's business and is tested there.
// What matters here is that a configured tower is wired into the lifecycle at
// all, and that a daemon starts and stops cleanly with one attached — an engine
// that stops the daemon shutting down is a worse failure than a tower nobody is
// watching.
func TestADaemonWithAWatchtowerConfiguredStartsAndStops(t *testing.T) {
	t.Parallel()

	credentials := t.TempDir()
	macaroon := filepath.Join(credentials, "readonly.macaroon")
	if err := os.WriteFile(macaroon, []byte{0x02, 0x01, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(cfg *config.Config, _, _ *chainviewtest.View) {
		cfg.Tower.LND = config.TowerInstance{
			Enabled:      true,
			Listen:       "abcdef.onion:9911",
			APIURL:       "http://127.0.0.1:1",
			MacaroonPath: macaroon,
			DataDir:      credentials,
		}
	})

	h.start(t)

	// Serving normally, with a tower configured that is not answering. A
	// watchtower that cannot be reached is a thing to report, never a thing that
	// stops the rest of the daemon working.
	resp, err := http.Get(h.base + "/api/v1/healthz")
	if err != nil {
		t.Fatalf("the daemon stopped serving with a tower configured: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health = %d with a tower configured", resp.StatusCode)
	}
}

// A tower whose credential cannot be read is a configuration error worth
// refusing over, because the alternative is a daemon that appears to be running
// a watchtower and is not.
func TestATowerWithAnUnreadableCredentialIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sf := chainviewtest.New("regtest")
	sq := chainviewtest.New("regtest")

	cfg := config.Default()
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
	cfg.UI.Auth = config.AuthNone
	cfg.Tower.LND = config.TowerInstance{
		Enabled:      true,
		Listen:       "abcdef.onion:9911",
		APIURL:       "http://127.0.0.1:1",
		MacaroonPath: filepath.Join(t.TempDir(), "does-not-exist.macaroon"),
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	daemon, err := app.New(ctx, cfg, nil, app.Deps{SF: sf, SQ: sq, Listener: listener})
	if err == nil {
		_ = daemon.Close()
		t.Fatal("a tower whose credential cannot be read was accepted")
	}
	if !strings.Contains(err.Error(), "watchtower") {
		t.Errorf("the error does not say what could not be set up: %v", err)
	}
}

// `platform` authentication on an unrecognised platform is a claim nobody
// checked, and it is said out loud.
//
// It serves the dashboard unauthenticated on a non-loopback address, trusting a
// proxy is in front of it. On StartOS and Umbrel one is, and warning every time
// would be noise nobody reads. The likeliest way to arrive here otherwise is
// copying a configuration from a packaged install — a sensible thing to do that
// quietly removes the only thing between the dashboard and the network.
func TestPlatformAuthWarnsWhenNothingCanConfirmTheProxy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform config.Platform
		wantWarn bool
	}{
		{"a self-hosted install", config.PlatformUnknown, true},
		{"StartOS, where the proxy really is there", config.PlatformStartOS04, false},
		{"Umbrel, likewise", config.PlatformUmbrel, false},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

		sf, sq := chainviewtest.NewSharedHistory(sharedHistory)
		cfg := config.Default()
		cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
		cfg.UI.Auth = config.AuthPlatform
		cfg.UI.Listen = "0.0.0.0:0"
		cfg.Platform = tc.platform

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		daemon, err := app.New(context.Background(), cfg, log,
			app.Deps{SF: sf, SQ: sq, Listener: listener})
		if err != nil {
			_ = listener.Close()
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = daemon.Close()

		warned := strings.Contains(buf.String(), "nothing here can confirm that proxy")
		if warned != tc.wantWarn {
			t.Errorf("%s: warned = %v, want %v\n%s", tc.name, warned, tc.wantWarn, buf.String())
		}
	}
}
