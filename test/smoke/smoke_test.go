//go:build integration

// Package smoke runs the whole product end to end: real binaries, real Bitcoin
// nodes, a real chain split, and a notification arriving at a real HTTP endpoint.
//
// Everything else in this repository tests a part. This tests that the parts are
// connected — which is a different question, and the one that has caught the most
// defects on this project.
package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	// detectionBudget is how long the daemon gets to notice a split. Generous:
	// it takes about two seconds in practice, and a smoke test that fails on a
	// loaded machine teaches people to re-run it rather than read it.
	detectionBudget = 120 * time.Second
	// startupBudget covers building the world, which pulls an image the first time.
	startupBudget = 5 * time.Minute
	// shutdownBudget is what the daemon promises a supervisor.
	shutdownBudget = 10 * time.Second
)

// notifications is the endpoint the daemon is configured to alert.
type notifications struct {
	server *httptest.Server

	mu       sync.Mutex
	received []map[string]any
}

func newNotifications(t *testing.T) *notifications {
	t.Helper()
	n := &notifications{}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			n.mu.Lock()
			n.received = append(n.received, payload)
			n.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(n.server.Close)
	return n
}

func (n *notifications) all() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]map[string]any(nil), n.received...)
}

func (n *notifications) sawKind(kind string) bool {
	for _, payload := range n.all() {
		if payload["kind"] == kind {
			return true
		}
	}
	return false
}

// The whole product, from two Bitcoin nodes disagreeing to a notification
// arriving somewhere a person would see it.
func TestASplitReachesTheUser(t *testing.T) {
	repo := repoRoot(t)
	bin := buildBinaries(t, repo)

	world := newWorld(t, repo, bin)
	world.up(t)

	alerts := newNotifications(t)
	daemon := startDaemon(t, repo, bin, alerts.server.URL)

	// Before the split, the daemon is watching and calm about the chain itself.
	waitFor(t, "the daemon to start watching", func() bool {
		return daemon.status(t)["split"].(map[string]any)["state"] == "ARMED"
	})

	// It proves its own alarm works before anything has gone wrong, which is the
	// only reason to believe the notification below means anything.
	waitFor(t, "the daemon to test its own notifications", func() bool {
		return alerts.sawKind("self_test")
	})

	world.split(t)

	waitFor(t, "the split to be reported", func() bool {
		return daemon.status(t)["split"].(map[string]any)["state"] == "SPLIT"
	})

	// The three places it has to arrive.
	status := daemon.status(t)
	headline := status["headline"].(map[string]any)
	if !strings.Contains(headline["detail"].(string), "separated") {
		t.Errorf("the dashboard does not say the chains separated: %v", headline["detail"])
	}
	if headline["state"] == "protected" || headline["state"] == "getting_ready" {
		t.Errorf("the dashboard is calm during a split: %v", headline["state"])
	}

	waitFor(t, "the alert to be delivered", func() bool {
		return alerts.sawKind("split_detected")
	})

	waitFor(t, "the split to reach the timeline", func() bool {
		for _, entry := range daemon.list(t, "/api/v1/timeline") {
			if summary, _ := entry["summary"].(string); strings.Contains(summary, "separated") {
				return true
			}
		}
		return false
	})

	// The separation point, which is what bounds everything downstream.
	split := status["split"].(map[string]any)
	fork, _ := split["fork"].(map[string]any)
	if fork == nil {
		t.Fatal("no separation point was recorded")
	}
	if height, _ := fork["height"].(float64); height <= 0 {
		t.Errorf("separation point at height %v", fork["height"])
	}

	// And the dashboard itself is served from the same listener.
	if page := daemon.page(t, "/"); !strings.Contains(page, "<!DOCTYPE html>") {
		t.Errorf("the dashboard was not served: %.80s", page)
	}

	// Nothing delivered to a third party may carry the user's situation. The
	// operator of a notification service is an actor in the threat model, and this
	// is the one place the real payload can be inspected as it leaves.
	for _, payload := range alerts.all() {
		if subject, ok := payload["subject"].(string); ok && subject != "" {
			t.Errorf("a payload carried a subject to a third party: %v", payload)
		}
		if payload["forktower"] != "v1" {
			t.Errorf("a payload is not identifiable as ours: %v", payload)
		}
	}

	// The dashboard's own script, against this daemon's real responses. Not a
	// browser, but it is the half that breaks: a field renamed on one side shows
	// up here as an exception rather than as an empty page nobody notices.
	mustRender(t, repo, daemon.base)

	daemon.stop(t)
}

func renderWithTheRealScript(t *testing.T, repo, base string, session ...string) (string, bool) {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so the dashboard script was not exercised " +
			"against the live daemon")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	args := append([]string{filepath.Join(repo, "test", "smoke", "render.js"), base}, session...)
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

func mustRender(t *testing.T, repo, base string, session ...string) {
	t.Helper()
	out, ok := renderWithTheRealScript(t, repo, base, session...)
	if !ok {
		t.Errorf("the dashboard script failed against a live daemon:\n%s", out)
		return
	}
	t.Logf("dashboard: %s", out)
}

// --- the world ----------------------------------------------------------------

type world struct {
	repo string
	bin  string
}

func newWorld(t *testing.T, repo, bin string) *world {
	t.Helper()
	w := &world{repo: repo, bin: bin}
	t.Cleanup(func() { w.run(t, startupBudget, "down") })
	return w
}

func (w *world) up(t *testing.T)    { t.Helper(); w.run(t, startupBudget, "up") }
func (w *world) split(t *testing.T) { t.Helper(); w.run(t, startupBudget, "split") }

func (w *world) run(t *testing.T, timeout time.Duration, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(w.bin, "forkbench"), args...)
	cmd.Dir = w.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forkbench %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// --- the daemon ---------------------------------------------------------------

type daemonProcess struct {
	cmd    *exec.Cmd
	base   string
	logs   *os.File
	logDir string
	exited chan struct{}
}

// startDaemon runs the real binary, not the package it is built from. Flag
// parsing, signal handling and shutdown are part of what this test is for, and
// none of them exist below main.
func startDaemon(t *testing.T, repo, bin, webhookURL string) *daemonProcess {
	t.Helper()
	return startDaemonWithAuth(t, repo, bin, webhookURL, `auth = "none"`, "127.0.0.1")
}

// startDaemonWithAuth runs the daemon in one of its authentication modes.
//
// The bind address is a parameter because it is not free to choose: `none`
// refuses anything but loopback, since that would serve the dashboard
// unauthenticated to the network, and `platform` refuses loopback, since no proxy
// could reach it. Both refusals are the point, so the test has to honour them.
func startDaemonWithAuth(t *testing.T, repo, bin, webhookURL, authBlock, host string) *daemonProcess {
	t.Helper()

	dir := t.TempDir()
	port := freePort(t)
	configPath := filepath.Join(dir, "forktower.toml")

	config := fmt.Sprintf(`
[sf]
rpc_url = "http://127.0.0.1:18443"
rpc_user = "forkbench"
rpc_pass = "forkbench"
zmq_rawblock = "tcp://127.0.0.1:28332"

[sq.bitcoind]
rpc_url = "http://127.0.0.1:18444"
rpc_user = "forkbench"
rpc_pass = "forkbench"
zmq_rawblock = "tcp://127.0.0.1:28342"

[sentinel]
poll_interval_secs = 1
split_confirm_depth = 2

[[alerts.transport]]
name = "smoke-test"
type = "webhook"
min_tier = "info"
url = %q

[store]
path = %q

[ui]
listen = "%s:%d"
%s

[log]
level = "debug"
`, webhookURL, filepath.Join(dir, "forktower.db"), host, port, authBlock)

	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "forktowerd.log")
	logs, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(bin, "forktowerd"), "--config", configPath)
	cmd.Dir = repo
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting forktowerd: %v", err)
	}

	// Reaped by one goroutine, and only one: Wait may not be called twice, and
	// both the readiness poll and the cleanup need to know when the process is
	// gone.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// Always 127.0.0.1 regardless of what it bound to: a proxy reaching a
	// non-loopback bind still arrives over this machine's own network.
	d := &daemonProcess{cmd: cmd, base: fmt.Sprintf("http://127.0.0.1:%d", port),
		logs: logs, logDir: logPath, exited: exited}

	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			<-exited
		}
		_ = logs.Close()
		if t.Failed() {
			if body, readErr := os.ReadFile(logPath); readErr == nil {
				t.Logf("forktowerd log:\n%s", body)
			}
		}
	})

	// Waiting out the whole budget for a process that has already died wastes two
	// minutes and reports a timeout where the real answer is in the log.
	waitFor(t, "the daemon to answer", func() bool {
		select {
		case <-exited:
			body, _ := os.ReadFile(logPath)
			t.Fatalf("the daemon exited during startup:\n%s", body)
		default:
		}
		resp, getErr := http.Get(d.base + "/api/v1/healthz")
		if getErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return d
}

// stop asks the daemon to shut down the way a supervisor would, and holds it to
// the budget it promises.
func (d *daemonProcess) stop(t *testing.T) {
	t.Helper()

	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("asking the daemon to stop: %v", err)
	}

	select {
	case <-d.exited:
		if state := d.cmd.ProcessState; state != nil && !state.Success() {
			t.Errorf("the daemon exited with %v, want a clean stop", state)
		}
	case <-time.After(shutdownBudget):
		_ = d.cmd.Process.Kill()
		t.Errorf("the daemon did not stop within %s of being asked", shutdownBudget)
	}
}

func (d *daemonProcess) status(t *testing.T) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	d.get(t, "/api/v1/status", &envelope)
	return envelope.Data
}

func (d *daemonProcess) list(t *testing.T, path string) []map[string]any {
	t.Helper()
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	d.get(t, path, &envelope)
	return envelope.Data
}

func (d *daemonProcess) get(t *testing.T, path string, into any) {
	t.Helper()
	resp, err := http.Get(d.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

func (d *daemonProcess) page(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(d.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// --- plumbing -----------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("cannot find the repository from %s: %v", dir, err)
	}
	return dir
}

// buildBinaries builds what is being tested, rather than trusting whatever
// happens to be in bin/ from an earlier run.
func buildBinaries(t *testing.T, repo string) string {
	t.Helper()
	dir := t.TempDir()

	for _, name := range []string{"forktowerd", "forkbench"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build",
			"-o", filepath.Join(dir, name), "./cmd/"+name)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("building %s: %v\n%s", name, err, out)
		}
	}
	return dir
}

// freePort asks the operating system for one nobody is using. A fixed port makes
// a test that passes only until something else on the machine wants it.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(detectionBudget)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s", detectionBudget, what)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// bcryptFor is a hash of the password below, generated once and pinned here. The
// cost is deliberately the minimum: this is a test, and a real hash would add a
// second to every run for no more assurance.
const (
	smokePassword = "correct horse battery staple"
	smokeHash     = "$2a$04$09p6P..vE9vMRjYwPuHg9uAsXWXzLes9KnjFIk.NYUSC41tQJcHe2"
)

// The password path, end to end, against a real daemon — which no browser and no
// unit test had ever exercised together. The handlers are covered elsewhere; what
// is new here is that the page itself does the right thing on both sides of a
// sign-in.
func TestTheDashboardBehindAPassword(t *testing.T) {
	repo := repoRoot(t)
	bin := buildBinaries(t, repo)

	world := newWorld(t, repo, bin)
	world.up(t)

	alerts := newNotifications(t)
	daemon := startDaemonWithAuth(t, repo, bin, alerts.server.URL,
		"auth = \"password\"\npassword_hash = \""+smokeHash+"\"", "127.0.0.1")

	// Nothing is readable without signing in.
	resp, err := http.Get(daemon.base + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d without a session, want 401", resp.StatusCode)
	}

	// But the page is, or there is nowhere to sign in from.
	if page := daemon.page(t, "/"); !strings.Contains(page, "<!DOCTYPE html>") {
		t.Error("the sign-in page is not served")
	}

	// And it shows the sign-in form rather than an error, which is the difference
	// between "you need to sign in" and "something is broken".
	out, ok := renderWithTheRealScript(t, repo, daemon.base)
	if !ok {
		t.Errorf("the dashboard script failed with no session:\n%s", out)
	}
	if !strings.Contains(out, "sign-in shown") {
		t.Errorf("the page did not offer a sign-in form:\n%s", out)
	}

	session := signIn(t, daemon.base)
	mustRender(t, repo, daemon.base, session)

	daemon.stop(t)
}

// signIn performs a real sign-in and returns the session cookie.
func signIn(t *testing.T, base string) string {
	t.Helper()

	// A request from somewhere else is refused even with the right password: an
	// attacker who can make the browser sign in has changed what the user sees.
	if code := postLogin(t, base, smokePassword, "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("a cross-site sign-in returned %d, want 403", code)
	}
	if code := postLogin(t, base, "wrong", base); code != http.StatusUnauthorized {
		t.Errorf("a wrong password returned %d, want 401", code)
	}

	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/login",
		strings.NewReader(`{"password":"`+smokePassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", base)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signing in returned %d", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name != "forktower_session" {
			continue
		}
		if !c.HttpOnly {
			t.Error("the session cookie is readable by scripts")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("SameSite = %v, want Strict", c.SameSite)
		}
		return c.Value
	}
	t.Fatal("signing in set no session cookie")
	return ""
}

func postLogin(t *testing.T, base, password, origin string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/login",
		strings.NewReader(`{"password":"`+password+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Platform mode delegates to the proxy that already authenticated the user, and
// is the only mode that may bind somewhere other than this machine.
func TestTheDashboardBehindAPlatformProxy(t *testing.T) {
	repo := repoRoot(t)
	bin := buildBinaries(t, repo)

	world := newWorld(t, repo, bin)
	world.up(t)

	alerts := newNotifications(t)
	// Bound off loopback, because that is the arrangement platform mode exists
	// for: a proxy in front, reaching it over the machine's own network.
	daemon := startDaemonWithAuth(t, repo, bin, alerts.server.URL, `auth = "platform"`, "0.0.0.0")

	if status := daemon.status(t); status == nil {
		t.Fatal("nothing was readable in platform mode")
	}
	mustRender(t, repo, daemon.base)

	// There is no password to offer, and saying so is clearer than a refusal.
	if code := postLogin(t, daemon.base, "anything", daemon.base); code != http.StatusNotFound {
		t.Errorf("a sign-in attempt in platform mode returned %d, want 404", code)
	}

	daemon.stop(t)
}
