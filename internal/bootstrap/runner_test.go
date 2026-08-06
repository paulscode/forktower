package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mapJournal is the whole of what the runner has to remember.
type mapJournal struct {
	mu     sync.Mutex
	values map[string]string
	// failSet makes writes fail, for the paths that have to survive it.
	failSet bool
}

func newJournal() *mapJournal {
	return &mapJournal{values: map[string]string{}}
}

func (j *mapJournal) Get(_ context.Context, key string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.values[key], nil
}

func (j *mapJournal) Set(_ context.Context, key, value string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failSet {
		return errors.New("the journal is broken")
	}
	j.values[key] = value
	return nil
}

// scriptedNode answers whatever a test needs, and records what it was asked.
type scriptedNode struct {
	mu sync.Mutex

	info    ChainInfo
	infoErr error
	// infoCalls counts how many times the node has been asked, and
	// headersReachAfter makes the headers arrive on a given call rather than
	// after a wall-clock delay — a test that sleeps to pass is slow now and
	// flaky later.
	infoCalls         int
	headersReachAfter int
	headersOnceThere  int32

	loaded    Loaded
	loadErr   error
	loadCalls int
	loadPath  string
	// onLoad runs inside LoadSnapshot, for a test that needs to observe the
	// phase while the node is busy.
	onLoad func()
}

func (n *scriptedNode) ChainInfo(context.Context) (ChainInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.infoCalls++
	info := n.info
	if n.headersReachAfter > 0 && n.infoCalls >= n.headersReachAfter {
		info.Headers = n.headersOnceThere
	}
	return info, n.infoErr
}

func (n *scriptedNode) LoadSnapshot(_ context.Context, path string) (Loaded, error) {
	n.mu.Lock()
	n.loadCalls++
	n.loadPath = path
	hook := n.onLoad
	err := n.loadErr
	loaded := n.loaded
	n.mu.Unlock()

	if hook != nil {
		hook()
	}
	return loaded, err
}

func (n *scriptedNode) setInfo(info ChainInfo) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.info = info
}

func (n *scriptedNode) loads() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.loadCalls
}

func behindNode() *scriptedNode {
	return &scriptedNode{info: ChainInfo{
		Network: "main",
		Blocks:  100_000,
		Headers: 940_000,
	}}
}

func newRunner(t *testing.T, node Node, jrnl Journal, cfg Config) *Runner {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.Snapshot.Network == "" {
		cfg.Snapshot = testSnapshot()
	}
	cfg.Enabled = true
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Millisecond
	}
	r, err := New(cfg, node, jrnl, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A node that would benefit is offered the shortcut, and is not started without
// being asked.
//
// The asking matters. Forktower fetches nothing else at all, and turning that
// into "fetches nine gigabytes on first run" without a decision from the user
// would quietly spend somebody's metered connection.
func TestTheShortcutIsOfferedButNotTakenUnasked(t *testing.T) {
	node := behindNode()
	r := newRunner(t, node, newJournal(), Config{})

	r.step(context.Background())

	if got := r.State().Phase; got != PhaseOffered {
		t.Errorf("phase = %q, want %q", got, PhaseOffered)
	}
	if node.loads() != 0 {
		t.Error("the snapshot was loaded without anybody asking for it")
	}
}

func TestAutoStartTakesItWithoutAsking(t *testing.T) {
	node := behindNode()
	jrnl := newJournal()
	r := newRunner(t, node, jrnl, Config{AutoStart: true})

	r.restore(context.Background())
	if !r.isArmed() {
		t.Error("auto-start did not arm the shortcut")
	}
}

// Being asked once survives a restart, because the download outlives any number
// of them and asking again after each would be its own kind of broken.
func TestBeingAskedIsRememberedAcrossARestart(t *testing.T) {
	jrnl := newJournal()
	ctx := context.Background()

	first := newRunner(t, behindNode(), jrnl, Config{})
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}

	second := newRunner(t, behindNode(), jrnl, Config{})
	second.restore(ctx)
	if !second.isArmed() {
		t.Error("a restart forgot that the shortcut had been asked for")
	}
}

// Having taken the shortcut is remembered by height, not by a flag.
//
// When the pinned Bitcoin Core version moves to a new assumeutxo height, a stale
// "true" would suppress the new offer for ever — which is precisely the moment
// somebody rebuilding their node would want it.
func TestACompletedShortcutIsRememberedByHeight(t *testing.T) {
	jrnl := newJournal()
	ctx := context.Background()

	r := newRunner(t, behindNode(), jrnl, Config{})
	if err := jrnl.Set(ctx, JournalDoneHeight, "935000"); err != nil {
		t.Fatal(err)
	}
	r.restore(ctx)
	if got := r.State().Phase; got != PhaseDone {
		t.Errorf("phase = %q, want %q for a snapshot already loaded at this height", got, PhaseDone)
	}

	// A different height must not suppress the offer.
	other := newRunner(t, behindNode(), jrnl, Config{})
	if err := jrnl.Set(ctx, JournalDoneHeight, "840000"); err != nil {
		t.Fatal(err)
	}
	other.restore(ctx)
	if got := other.State().Phase; got == PhaseDone {
		t.Error("a snapshot loaded at a different height suppressed this one's offer")
	}
}

// A node that already has a snapshot chainstate is done, however it got there —
// including a previous install whose database has since been deleted.
func TestANodeThatAlreadyHasASnapshotIsDone(t *testing.T) {
	node := behindNode()
	node.setInfo(ChainInfo{Network: "main", Blocks: 100, Headers: 940_000, SnapshotLoaded: true})

	r := newRunner(t, node, newJournal(), Config{})
	r.step(context.Background())

	if got := r.State().Phase; got != PhaseDone {
		t.Errorf("phase = %q, want %q", got, PhaseDone)
	}
}

func TestANodeThatCannotBenefitIsNotOfferedTheShortcut(t *testing.T) {
	node := behindNode()
	node.setInfo(ChainInfo{Network: "regtest", Blocks: 1, Headers: 1})

	r := newRunner(t, node, newJournal(), Config{})
	r.step(context.Background())

	st := r.State()
	if st.Phase != PhaseUnavailable {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseUnavailable)
	}
	if st.Assessment.Reason == "" {
		t.Error("nothing was said about why the shortcut is unavailable")
	}
}

// An unreachable node is not a failure of the shortcut. The dashboard already
// says the node is down, far more prominently, and a second red message about
// the same fact is noise.
func TestAnUnreachableNodeIsNotReportedAsAShortcutFailure(t *testing.T) {
	node := behindNode()
	node.infoErr = errors.New("connection refused")

	r := newRunner(t, node, newJournal(), Config{})
	before := r.State().Phase
	r.step(context.Background())

	st := r.State()
	if st.Phase != before {
		t.Errorf("phase moved to %q because the node was unreachable", st.Phase)
	}
	if st.Error != "" {
		t.Errorf("an unreachable node produced a shortcut error: %q", st.Error)
	}
}

// Cancelling deletes the part-downloaded file. Keeping it would leave nine
// gigabytes on a disk that nothing would ever pick up again — and the space is
// usually why somebody cancelled.
func TestCancellingDeletesWhatWasDownloaded(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, behindNode(), newJournal(), Config{Dir: dir})

	staged := filepath.Join(dir, StagedFileName)
	if err := os.WriteFile(staged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Error("cancelling left the part-downloaded file behind")
	}
	if r.isArmed() {
		t.Error("cancelling left the shortcut armed")
	}
}

func TestCancellingWhenNothingWasDownloadedIsNotAnError(t *testing.T) {
	r := newRunner(t, behindNode(), newJournal(), Config{})
	if err := r.Cancel(context.Background()); err != nil {
		t.Errorf("Cancel with no staged file: %v", err)
	}
}

// Starting clears a previous complaint, so a stale message cannot be mistaken for
// a fresh failure.
func TestRetryingClearsTheLastFailure(t *testing.T) {
	r := newRunner(t, behindNode(), newJournal(), Config{})
	r.fail(errors.New("something went wrong"))

	if r.State().Error == "" {
		t.Fatal("the failure was not recorded")
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.State().Error; got != "" {
		t.Errorf("a retry left the previous error in place: %q", got)
	}
}

// A failure keeps the shortcut armed but waits before trying again.
//
// The fetcher already retries a stalled transfer thirty times before giving up,
// so reaching this point means something is properly wrong. Coming straight back
// would turn that into a request every poll interval for however long it takes
// somebody to notice.
func TestAFailureBacksOffRatherThanRetryingImmediately(t *testing.T) {
	f := newFixture(t, 200, 100)
	f.serve(t, serverOpts{})

	node := behindNode()
	clock := &testClock{at: time.Unix(1_700_000_000, 0)}
	r := newRunner(t, node, newJournal(), Config{
		Snapshot: withBase(f.snapshot),
		Client:   srvClient(),
	})
	r.now = clock.now

	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.fail(errors.New("the transfer died"))

	// Straight afterwards it stays put rather than restarting. The node is not
	// asked for the file, which is the observable part: an immediate retry would
	// hand it over here.
	r.step(ctx)
	if got := r.State().Phase; got != PhaseFailed {
		t.Errorf("phase = %q straight after a failure, want %q", got, PhaseFailed)
	}
	if node.loads() != 0 {
		t.Error("the shortcut retried immediately instead of waiting")
	}
	if !r.isArmed() {
		t.Error("a failure disarmed the shortcut, so nothing would ever resume it")
	}

	// Once the wait has passed it picks up on its own, without being asked again.
	clock.advance(RetryAfter + time.Second)
	r.step(ctx)
	if got := r.State().Phase; got != PhaseDone {
		t.Errorf("phase = %q after the retry delay, want %q (error: %s)",
			got, PhaseDone, r.State().Error)
	}
	if node.loads() != 1 {
		t.Errorf("the node was handed the snapshot %d times, want 1", node.loads())
	}
}

// The whole sequence, against a node that accepts the file.
func TestASuccessfulRunLoadsTheSnapshotAndTidiesUp(t *testing.T) {
	f := newFixture(t, 600, 400)
	f.serve(t, serverOpts{})

	dir := t.TempDir()
	node := behindNode()
	node.loaded = Loaded{Coins: 42, BaseHeight: 935_000}

	jrnl := newJournal()
	r := newRunner(t, node, jrnl, Config{
		Dir:      dir,
		Snapshot: withBase(f.snapshot),
		Client:   srvClient(),
	})

	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.step(ctx)

	st := r.State()
	if st.Phase != PhaseDone {
		t.Fatalf("phase = %q, want %q (error: %s)", st.Phase, PhaseDone, st.Error)
	}
	if node.loads() != 1 {
		t.Errorf("the node was handed the snapshot %d times, want 1", node.loads())
	}
	if node.loadPath != filepath.Join(dir, StagedFileName) {
		t.Errorf("the node was given %q", node.loadPath)
	}

	// The file is nine gigabytes in production and nothing will read it again.
	if _, err := os.Stat(filepath.Join(dir, StagedFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staged snapshot was left on disk after being loaded")
	}
	if got, _ := jrnl.Get(ctx, JournalDoneHeight); got != "935000" {
		t.Errorf("the completed height was recorded as %q", got)
	}
	if armed, _ := jrnl.Get(ctx, JournalArmed); armed != "" {
		t.Error("the shortcut is still armed after completing")
	}
	if r.isArmed() {
		t.Error("the runner is still armed after completing")
	}
}

// A node that refuses the file is reported, and the shortcut does not claim to
// have worked.
func TestANodeRefusingTheSnapshotIsReported(t *testing.T) {
	f := newFixture(t, 300, 200)
	f.serve(t, serverOpts{})

	node := behindNode()
	node.loadErr = errors.New("Unable to load UTXO snapshot")

	r := newRunner(t, node, newJournal(), Config{
		Snapshot: withBase(f.snapshot),
		Client:   srvClient(),
	})

	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.step(ctx)

	st := r.State()
	if st.Phase != PhaseFailed {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseFailed)
	}
	if st.Error == "" {
		t.Error("the node's refusal was not reported")
	}
}

// The download does not wait for the node's headers, because they arrive in
// minutes and the file takes hours. Waiting first would add the shorter to the
// longer instead of hiding it inside it.
func TestTheDownloadRunsWhileTheHeadersAreStillArriving(t *testing.T) {
	f := newFixture(t, 400, 300)
	f.serve(t, serverOpts{})

	node := behindNode()
	// Headers short of the base height at the moment the run starts, arriving on
	// the third time the node is asked.
	node.setInfo(ChainInfo{Network: "main", Blocks: 1000, Headers: 900_000})
	node.headersReachAfter = 3
	node.headersOnceThere = 940_000

	dir := t.TempDir()
	r := newRunner(t, node, newJournal(), Config{
		Dir:          dir,
		Snapshot:     withBase(f.snapshot),
		Client:       srvClient(),
		PollInterval: time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.step(ctx)

	if got := r.State().Phase; got != PhaseDone {
		t.Errorf("phase = %q, want %q (error: %s)", got, PhaseDone, r.State().Error)
	}
	if node.loads() != 1 {
		t.Errorf("the snapshot was handed over %d times", node.loads())
	}
}

// While the node reads the file it answers nothing for several minutes. That has
// a phase of its own so the dashboard does not report a node that is working
// perfectly as having failed.
func TestLoadingHasAPhaseOfItsOwn(t *testing.T) {
	f := newFixture(t, 200)
	f.serve(t, serverOpts{})

	seen := make(chan Phase, 1)
	node := behindNode()
	r := newRunner(t, node, newJournal(), Config{
		Snapshot: withBase(f.snapshot),
		Client:   srvClient(),
	})
	node.onLoad = func() { seen <- r.State().Phase }

	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.step(ctx)

	select {
	case got := <-seen:
		if got != PhaseLoading {
			t.Errorf("while the node was reading the file the phase was %q, want %q",
				got, PhaseLoading)
		}
	default:
		t.Fatal("the node was never asked to load the snapshot")
	}
}

func TestStartIsRefusedWhenTheShortcutIsSwitchedOff(t *testing.T) {
	r, err := New(Config{Enabled: false, Dir: t.TempDir()}, behindNode(), newJournal(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err == nil {
		t.Error("Start succeeded with the shortcut switched off")
	}
	if got := r.State().Phase; got != PhaseOff {
		t.Errorf("phase = %q, want %q", got, PhaseOff)
	}
}

func TestNewRefusesAnIncompleteSetup(t *testing.T) {
	dir := t.TempDir()

	t.Run("no node", func(t *testing.T) {
		if _, err := New(Config{Dir: dir}, nil, newJournal(), nil, nil); err == nil {
			t.Error("New accepted a setup with no node to bootstrap")
		}
	})
	t.Run("no journal", func(t *testing.T) {
		if _, err := New(Config{Dir: dir}, behindNode(), nil, nil, nil); err == nil {
			t.Error("New accepted a setup with nothing to remember with")
		}
	})
	t.Run("no staging directory", func(t *testing.T) {
		if _, err := New(Config{}, behindNode(), newJournal(), nil, nil); err == nil {
			t.Error("New accepted a setup with nowhere to put nine gigabytes")
		}
	})
}

// Run returns when its context ends, including with the shortcut switched off,
// so the daemon's shutdown is not held up by a feature nobody is using.
func TestRunStopsWithItsContext(t *testing.T) {
	for name, enabled := range map[string]bool{"on": true, "off": false} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{Enabled: enabled, Dir: t.TempDir(), PollInterval: time.Millisecond}
			r, err := New(cfg, behindNode(), newJournal(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()

			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return when its context ended")
			}
		})
	}
}

// withBase gives a fixture snapshot the base height and network the plan checks
// against, so the runner tests exercise the real decision path.
func withBase(s Snapshot) Snapshot {
	s.Network = "main"
	s.BaseHeight = 935_000
	return s
}

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// The staging directory is made before anything asks how much room is in it.
//
// statfs on a path that does not exist reports "unknown", and unknown is
// deliberately not a refusal — so without this a machine with no room would be
// offered the shortcut, accept it, and fail on the first write.
func TestTheStagingDirectoryExistsBeforeTheSpaceIsMeasured(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "yet", "there")
	r := newRunner(t, behindNode(), newJournal(), Config{Dir: dir})

	if FreeBytes(dir) >= 0 {
		t.Fatal("the test's own premise is wrong: the directory already exists")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()

	// Wait for the directory rather than for a duration.
	deadline := time.Now().Add(5 * time.Second)
	for FreeBytes(dir) < 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("Run never created the directory it stages the download in")
		}
	}
	cancel()
	<-done

	if FreeBytes(dir) < 0 {
		t.Error("the space in the staging directory is still unmeasurable")
	}
}

// The button labelled "Try again now" has to mean now.
//
// The fifteen-minute backoff exists so an unattended daemon does not hammer a
// broken network. A person pressing a button is not that case, and a button that
// silently does nothing for a quarter of an hour is indistinguishable from a
// broken one.
func TestAnExplicitRetryDoesNotWaitOutTheBackoff(t *testing.T) {
	f := newFixture(t, 200, 100)
	f.serve(t, serverOpts{})

	node := behindNode()
	clock := &testClock{at: time.Unix(1_700_000_000, 0)}
	r := newRunner(t, node, newJournal(), Config{
		Snapshot: withBase(f.snapshot),
		Client:   srvClient(),
	})
	r.now = clock.now

	ctx := context.Background()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.fail(errors.New("the transfer died"))

	// Asked again, by a person, with no time passed at all.
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.step(ctx)

	if got := r.State().Phase; got != PhaseDone {
		t.Errorf("phase = %q after an explicit retry, want %q (error: %s)",
			got, PhaseDone, r.State().Error)
	}
	if node.loads() != 1 {
		t.Errorf("an explicit retry did not restart the transfer (%d loads)", node.loads())
	}
}

// Pressing the button changes what the button says.
//
// The API answers a start request with the current view. Without this the reply
// still offered the shortcut the user had just accepted, and the dashboard kept
// showing "Use the faster sync" until its next poll — which reads as a click
// that did not register, and invites a second one.
func TestAcceptingTheOfferIsReflectedImmediately(t *testing.T) {
	r := newRunner(t, behindNode(), newJournal(), Config{})
	r.step(context.Background())
	if got := r.State().Phase; got != PhaseOffered {
		t.Fatalf("phase = %q before accepting, want %q", got, PhaseOffered)
	}

	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.State().Phase; got != PhaseDownloading {
		t.Errorf("phase = %q straight after accepting, want %q — the reply to the "+
			"click still offers what was just accepted", got, PhaseDownloading)
	}
}
