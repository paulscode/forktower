package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// fakeSentinel is a scriptable stand-in for the detection engine, so a handler
// can be driven through states a real daemon would take days to reach.
type fakeSentinel struct {
	mu         sync.Mutex
	state      sentinel.State
	checks     sentinel.Checks
	paused     bool
	sfView     chainview.BackendHealth
	sqView     chainview.BackendHealth
	sfIdentity chainview.Identity
	sqIdentity chainview.Identity
}

func newFakeSentinel() *fakeSentinel {
	return &fakeSentinel{
		state: sentinel.State{
			Phase:    sentinel.PhaseArmed,
			SFHealth: chainview.HealthOK,
			SQHealth: chainview.HealthOK,
		},
		checks: sentinel.Checks{
			SameNetwork: true, DistinctNodes: true, DistinctVerified: true,
			OnExpectedBranch: true, BranchVerifiedAt: 1_790_000_000,
		},
		sfView: chainview.BackendHealth{State: chainview.HealthOK, PeerCount: 10, SyncProgress: 1},
		sqView: chainview.BackendHealth{State: chainview.HealthOK, PeerCount: 4, SyncProgress: 1},
		sfIdentity: chainview.Identity{
			Endpoint: "http://own:8332", Subversion: "/Satoshi:29.3.0/Knots:20260508/",
		},
		sqIdentity: chainview.Identity{Endpoint: "http://other:8432", Subversion: "/Satoshi:29.0.0/"},
	}
}

func (f *fakeSentinel) State() sentinel.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeSentinel) Checks() sentinel.Checks {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func (f *fakeSentinel) Paused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeSentinel) Views() (sf, sq chainview.BackendHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sfView, f.sqView
}

func (f *fakeSentinel) Identities() (sf, sq chainview.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sfIdentity, f.sqIdentity
}

func (f *fakeSentinel) set(mutate func(*fakeSentinel)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

// fakeAlerter records what the API asked of the notification subsystem.
type fakeAlerter struct {
	mu         sync.Mutex
	names      []string
	tested     [][]string
	raised     []alert.Candidate
	failing    map[string]bool
	testErr    error
	testCalled int
}

func newFakeAlerter(names ...string) *fakeAlerter {
	return &fakeAlerter{names: names, failing: map[string]bool{}}
}

func (f *fakeAlerter) TransportNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

func (f *fakeAlerter) TestTransports(_ context.Context, names ...string) ([]alert.SelfTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testCalled++
	f.tested = append(f.tested, append([]string(nil), names...))
	if f.testErr != nil {
		return nil, f.testErr
	}
	targets := names
	if len(targets) == 0 {
		targets = f.names
	}
	out := make([]alert.SelfTestResult, 0, len(targets))
	for _, n := range targets {
		out = append(out, alert.SelfTestResult{Transport: n, OK: !f.failing[n]})
	}
	return out, nil
}

func (f *fakeAlerter) Raise(_ context.Context, c alert.Candidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raised = append(f.raised, c)
}

func (f *fakeAlerter) raisedKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.raised))
	for _, c := range f.raised {
		out = append(out, c.Kind)
	}
	return out
}

type harness struct {
	srv     *Server
	store   *store.Store
	sen     *fakeSentinel
	alerter *fakeAlerter
	ln      *fakeLightning
	clock   *atomic.Int64
	ts      *httptest.Server
}

// fakeLightning stands in for the channel registry. Nil health means no
// Lightning node is configured, which is a different thing from one that cannot
// be read.
type fakeLightning struct {
	mu     sync.Mutex
	health []registry.SourceHealth
}

func (f *fakeLightning) Health() []registry.SourceHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health
}

func (f *fakeLightning) set(h []registry.SourceHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = h
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	sen := newFakeSentinel()
	al := newFakeAlerter("my-phone")
	ln := &fakeLightning{}

	// A self-test on record by default, so the transport check reports a real
	// outcome rather than "not tested yet" in every unrelated test.
	if err := st.SetMetaInt64(ctx, store.MetaLastSelfTestAt, clock.Load()); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Auth: config.AuthNone}
	if mutate != nil {
		mutate(&cfg)
	}

	srv, err := New(st, sen, al, ln, cfg, nil, func() time.Time {
		return time.Unix(clock.Load(), 0)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	return &harness{srv: srv, store: st, sen: sen, alerter: al, ln: ln, clock: clock, ts: ts}
}

// do sends a request with an Origin matching the server, which is what a browser
// on the dashboard sends.
func (h *harness) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	return h.doCtx(t, context.Background(), method, path, body, func(r *http.Request) {
		r.Header.Set("Origin", h.origin())
	})
}

func (h *harness) doWith(t *testing.T, method, path, body string, prepare func(*http.Request)) *http.Response {
	t.Helper()
	return h.doCtx(t, context.Background(), method, path, body, prepare)
}

func (h *harness) doCtx(
	t *testing.T, ctx context.Context, method, path, body string, prepare func(*http.Request),
) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, h.ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	} else {
		// A body-less POST must be indistinguishable from one a browser sends.
		req.ContentLength = 0
	}
	if prepare != nil {
		prepare(req)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) origin() string { return h.ts.URL }

// decode reads an envelope and fails the test if the request did not succeed.
func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var env struct {
		Data  T         `json:"data"`
		Error *apiError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if env.Error != nil {
		t.Fatalf("request failed: %s — %s", env.Error.Code, env.Error.Message)
	}
	return env.Data
}

// errorCode reads the stable code from a failure, which is what a caller
// branches on.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if env.Error == nil {
		t.Fatalf("expected an error, got data %v", env.Data)
	}
	if env.Error.Message == "" {
		t.Error("an error was returned with no message for the user")
	}
	return env.Error.Code
}
