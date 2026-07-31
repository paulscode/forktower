//go:build integration

// Package scenarios drives the whole daemon against real containers, through the
// situations doc 01 sets out.
//
// Everything else in this repository tests a piece: a classifier against crafted
// transactions, an engine against a fake chain, a handler against a fake engine.
// Those prove the pieces do what they were written to do. These prove the pieces
// add up — that a counterparty publishing an old commitment on a chain nobody is
// watching ends with a person being told, in time, with a countdown they can
// act on.
//
// Slow, and unapologetically so. Each one builds two Bitcoin nodes, two
// Lightning nodes, a channel with payments through it, a chain split, and a
// running daemon. That is what the situation is; a faster test would be a test
// of something else.
package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// dashboard is where the daemon under test serves its API.
	dashboard = "http://127.0.0.1:8330"
	// settle is how long anything driven by a chain event is given to arrive.
	// Generous: a block has to be mined, seen over ZMQ, scanned, recorded and
	// published before an assertion can pass.
	settle = 90 * time.Second
	// worldTimeout bounds building the containers.
	worldTimeout = 8 * time.Minute
)

// world is a running forkbench, with a daemon watching it.
type world struct {
	t       *testing.T
	repoDir string
	daemon  *exec.Cmd
	dataDir string
	logPath string
}

// freshWorld tears down whatever was there and builds a new one, with a channel
// open and Forktower watching.
//
// Rebuilt per scenario rather than shared. A channel can only be closed once, so
// scenarios that close one cannot follow each other — and a suite whose cases
// depend on the order they run in is a suite that fails mysteriously the day
// somebody adds one.
func freshWorld(t *testing.T) *world {
	t.Helper()

	repo := repoRoot(t)
	w := &world{t: t, repoDir: repo}

	w.forkbench(t, "down")
	// And the daemon's own database, which `forkbench down` knows nothing about.
	// Leaving it behind means the next scenario starts with the last one's
	// channels, spends and high-water mark — against a chain that no longer
	// exists, which the watcher correctly reports as the chain having been
	// replaced further back than any reorganisation should reach. Correct, and
	// nothing to do with the scenario being run.
	w.wipeDaemonState(t)

	t.Cleanup(func() {
		w.stopDaemon()
		w.forkbench(t, "down")
		w.wipeDaemonState(t)
	})

	w.forkbench(t, "up")
	w.forkbench(t, "ln-up")
	return w
}

// wipeDaemonState removes everything the daemon remembers between runs.
func (w *world) wipeDaemonState(t *testing.T) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(w.repoDir, ".dev"),
		filepath.Join(w.repoDir, "deploy", "forkbench", "creds"),
	} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("clearing %s: %v", path, err)
		}
	}
}

// repoRoot finds the checkout, so the helpers can run forkbench and read the
// dev configuration from wherever `go test` was started.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("cannot find the repository from the working directory")
	return ""
}

// forkbench runs one of the tool's commands and fails the test if it does not
// work. The output is kept and only shown when something goes wrong.
func (w *world) forkbench(t *testing.T, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), worldTimeout)
	defer cancel()

	full := append([]string{"run", "./cmd/forkbench"}, args...)
	cmd := exec.CommandContext(ctx, "go", full...)
	cmd.Dir = w.repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forkbench %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// startDaemon builds and runs forktowerd against the world, with its Lightning
// credentials copied out.
func (w *world) startDaemon(t *testing.T) {
	t.Helper()
	w.startDaemonWith(t, nil)
}

// startDaemonWith starts the daemon with extra settings from the environment.
//
// The environment rather than a second config file, because an override always
// beats the file and a scenario that had to keep its own copy of the whole
// configuration would drift from the one people actually run.
func (w *world) startDaemonWith(t *testing.T, env []string) {
	t.Helper()

	w.forkbench(t, "ln-credentials", "-ln-node", "user",
		"-out", filepath.Join("deploy", "forkbench", "creds"))

	w.dataDir = filepath.Join(w.repoDir, ".dev")
	binary := filepath.Join(t.TempDir(), "forktowerd")

	build := exec.Command("go", "build", "-o", binary, "./cmd/forktowerd") //nolint:gosec // this repo's own
	build.Dir = w.repoDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the daemon: %v\n%s", err, out)
	}

	logFile := filepath.Join(t.TempDir(), "forktowerd.log")
	handle, err := os.Create(logFile) //nolint:gosec // a path this test just made
	if err != nil {
		t.Fatal(err)
	}
	w.logPath = logFile

	//nolint:gosec // the binary is this repository's own, just built
	cmd := exec.Command(binary, "--config", filepath.Join("deploy", "forkbench", "forktower.dev.toml"))
	cmd.Dir = w.repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = handle
	cmd.Stderr = handle
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the daemon: %v", err)
	}
	w.daemon = cmd

	waitFor(t, "the dashboard to answer", func() bool {
		resp, getErr := http.Get(dashboard + "/api/v1/healthz") //nolint:noctx // a liveness poll
		if getErr != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// stopDaemon stops it, if one is running.
func (w *world) stopDaemon() {
	if w.daemon == nil || w.daemon.Process == nil {
		return
	}
	_ = w.daemon.Process.Kill()
	_, _ = w.daemon.Process.Wait()
	w.daemon = nil
}

// restartDaemon stops and starts it, which is what a package upgrade or a reboot
// looks like from the daemon's point of view.
func (w *world) restartDaemon(t *testing.T) {
	t.Helper()
	w.stopDaemon()
	w.startDaemon(t)
}

// logs returns what the daemon has said, for a failure message worth reading.
func (w *world) logs() string {
	if w.logPath == "" {
		return ""
	}
	body, err := os.ReadFile(w.logPath) //nolint:gosec // a path this test made
	if err != nil {
		return ""
	}
	return string(body)
}

// --- reading the dashboard -------------------------------------------------

func get[T any](t *testing.T, path string) T {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		dashboard+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  T `json:"data"`
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

// alert is what the dashboard says about one condition.
type alert struct {
	Tier     string `json:"tier"`
	Kind     string `json:"kind"`
	DedupKey string `json:"dedup_key"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
}

// channel is one row of the exposure table.
type channel struct {
	ID     int64 `json:"id"`
	Threat struct {
		State            string `json:"state"`
		HeadlineDeadline *struct {
			RemainingBlocks int32 `json:"remaining_blocks"`
			DeadlineHeight  int32 `json:"deadline_height"`
		} `json:"headline_deadline"`
	} `json:"threat"`
	Display struct {
		Partner   string `json:"partner"`
		AtRiskSat int64  `json:"at_risk_sat"`
		TimeLeft  string `json:"time_left"`
		Status    string `json:"status"`
	} `json:"display"`
}

// spend is one thing that happened on a chain.
type spend struct {
	ID          int64  `json:"id"`
	Branch      string `json:"branch"`
	ChannelID   int64  `json:"channel_id"`
	SpendTxID   string `json:"spend_txid"`
	BlockHeight int32  `json:"block_height"`
	Shape       string `json:"shape"`
	Status      string `json:"status"`
}

// countdown is one clock running against the user.
type countdown struct {
	ID              int64  `json:"id"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	DeadlineHeight  int32  `json:"deadline_height"`
	RemainingBlocks int32  `json:"remaining_blocks"`
	Assumed         bool   `json:"assumed"`
}

// tower is one watchtower as the dashboard sees it.
type tower struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	URI     string `json:"uri"`
	Status  string `json:"status"`
	Display struct {
		State     string `json:"state"`
		Summary   string `json:"summary"`
		Covered   int    `json:"covered"`
		Uncovered int    `json:"uncovered"`
	} `json:"display"`
	Coverage []struct {
		ChannelID int64  `json:"channel_id"`
		Coverable bool   `json:"coverable"`
		Reason    string `json:"reason"`
	} `json:"coverage"`
}

type towersPayload struct {
	Towers []tower `json:"towers"`
}

// mirrorDecision is one transaction the mirror considered.
type mirrorDecision struct {
	TxID    string `json:"txid"`
	From    string `json:"from"`
	To      string `json:"to"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Display struct {
		What      string `json:"what"`
		ShortTxID string `json:"short_txid"`
		Copied    bool   `json:"copied"`
		Refused   bool   `json:"refused"`
		NeedsYou  bool   `json:"needs_you"`
	} `json:"display"`
}

type mirrorPayload struct {
	Decisions []mirrorDecision `json:"decisions"`
}

// mirrorDecisions is what the mirror decided, optionally narrowed to one state.
func mirrorDecisions(t *testing.T, state string) []mirrorDecision {
	t.Helper()
	path := "/api/v1/mirror"
	if state != "" {
		path += "?state=" + state
	}
	return get[mirrorPayload](t, path).Decisions
}

func towers(t *testing.T) []tower {
	t.Helper()
	return get[towersPayload](t, "/api/v1/towers").Towers
}

func alerts(t *testing.T) []alert { t.Helper(); return get[[]alert](t, "/api/v1/alerts") }

func channels(t *testing.T) []channel { t.Helper(); return get[[]channel](t, "/api/v1/channels") }
func spends(t *testing.T) []spend     { t.Helper(); return get[[]spend](t, "/api/v1/spends") }

// countdowns is the clocks in a given state. The scenarios only ever ask for the
// running ones; the parameter is here because "which state" is the question this
// endpoint exists to answer, and a helper that could only ask one of them would
// be hiding that.
func countdowns(t *testing.T, state string) []countdown {
	t.Helper()
	return get[[]countdown](t, "/api/v1/deadlines?state="+state)
}

// alertOfKind finds the alert of a kind, if it has been raised.
func alertOfKind(t *testing.T, kind string) (alert, bool) {
	t.Helper()
	for _, a := range alerts(t) {
		if a.Kind == kind {
			return a, true
		}
	}
	return alert{}, false
}

// --- waiting ---------------------------------------------------------------

// waitFor polls until something is true, or gives up loudly.
//
// Polled rather than slept, because these are driven by blocks: a fixed wait is
// either too short on a loaded machine or wasted minutes on a fast one.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond) //nolint:forbidigo // polling real containers
	}
	t.Fatalf("timed out after %s waiting for %s", settle, what)
}

// blocksOn mines on one chain, which is how everything here is made to advance.
func (w *world) blocksOn(t *testing.T, node string, count int) {
	t.Helper()
	w.forkbench(t, "mine", "-node", node, "-blocks", strconv.Itoa(count))
}

// staged brings the world to the point every scenario starts from: a channel
// with payments through it, a saved counterparty state, more payments, and the
// chains separated.
func (w *world) staged(t *testing.T) {
	t.Helper()
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "snapshot-mallory")
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
}

// forkbenchFails runs a command that is expected not to work, and fails the test
// if it does.
//
// For the assertions that are about an absence: "this transaction is not on that
// chain" is only worth checking if the check would have noticed it being there.
func (w *world) forkbenchFails(t *testing.T, what string, args ...string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "forkbench")
	build := exec.Command("go", "build", "-o", binary, "./cmd/forkbench") //nolint:gosec // this repo's own
	build.Dir = w.repoDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building forkbench: %v\n%s", err, out)
	}

	cmd := exec.Command(binary, args...) //nolint:gosec // this repository's own, just built
	cmd.Dir = w.repoDir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("%s\n%s", what, out)
	}
}

// mineUntil waits for something, mining on the other chain as it goes.
//
// For the scenarios where what is being waited for needs a block to happen in:
// a transaction sitting in a memory pool confirms when somebody mines, and in a
// world where nobody does, waiting is waiting for the wrong thing. One block per
// poll, on the chain the user's own node does not follow, which is where the
// interesting transactions are.
func (w *world) mineUntil(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s\n%s", settle, what, w.describe(t))
		}
		w.forkbench(t, "mine", "-node", "sq", "-blocks", "1")
		time.Sleep(2 * time.Second) //nolint:forbidigo // pacing real blocks
	}
}

// describe is a failure message worth reading: what the dashboard thought, and
// what the daemon said.
func (w *world) describe(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, c := range channels(t) {
		fmt.Fprintf(&b, "channel %d threat=%s status=%q\n",
			c.ID, c.Threat.State, c.Display.Status)
	}
	for _, sp := range spends(t) {
		fmt.Fprintf(&b, "spend %d %s shape=%s status=%s height=%d\n",
			sp.ID, sp.Branch, sp.Shape, sp.Status, sp.BlockHeight)
	}
	for _, a := range alerts(t) {
		fmt.Fprintf(&b, "alert %s %s\n", a.Tier, a.Kind)
	}
	if tail := lastLines(w.logs(), 25); tail != "" {
		fmt.Fprintf(&b, "\nwhat the daemon said:\n%s", tail)
	}
	return b.String()
}

// lastLines is the end of something long, which is the part worth reading when
// a scenario has just failed.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
