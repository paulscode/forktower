package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Phase is where the bootstrap has got to. One of a fixed set, so the dashboard
// renders from a known value rather than from prose.
type Phase string

const (
	// PhaseOff means the shortcut is switched off in configuration.
	PhaseOff Phase = "off"
	// PhaseUnavailable means it would not help this node. The assessment says why.
	PhaseUnavailable Phase = "unavailable"
	// PhaseOffered means it would help and nobody has said yes yet.
	PhaseOffered Phase = "offered"
	// PhaseDownloading means the file is being fetched.
	PhaseDownloading Phase = "downloading"
	// PhaseLoading means the node is reading the file. Minutes, during which the
	// node answers nothing — worth its own phase so the dashboard does not report
	// the node as having failed.
	PhaseLoading Phase = "loading"
	// PhaseDone means the node adopted the snapshot.
	PhaseDone Phase = "done"
	// PhaseFailed means an attempt ended badly and can be retried.
	PhaseFailed Phase = "failed"
)

// Journal keys. Everything worth surviving a restart, and nothing else.
const (
	// JournalArmed records that somebody asked for this. Kept because the
	// download outlives any number of restarts and asking again after each one
	// would be its own kind of broken.
	JournalArmed = "snapshot_bootstrap_armed"
	// JournalDoneHeight records the base height of a snapshot that was
	// successfully loaded, so a restart does not re-offer a shortcut already
	// taken. The height rather than a flag: when the pinned Core version moves to
	// a new assumeutxo height, a stale "true" would suppress the new offer for
	// ever.
	JournalDoneHeight = "snapshot_bootstrap_done_height"
)

// ChainInfo is what the second node says about its own progress.
type ChainInfo struct {
	Network        string
	Blocks         int32
	Headers        int32
	SnapshotLoaded bool
}

// Loaded is what the node reports after accepting a snapshot.
type Loaded struct {
	Coins      uint64
	BaseHeight int32
	TipHash    string
}

// Node is the second Bitcoin node, narrowed to the two things this needs.
//
// An interface rather than the concrete chain view, because the whole of this
// package's behaviour is then testable against a node that returns whatever a
// test needs — including the interesting cases, which are a node that refuses
// the snapshot and a node that takes six minutes to answer.
type Node interface {
	// ChainInfo reports the node's progress. Called on every tick.
	ChainInfo(ctx context.Context) (ChainInfo, error)
	// LoadSnapshot hands the node a file and blocks until it has read it, which
	// takes minutes. Implementations must not impose their usual RPC timeout on
	// it; the context is the only bound.
	LoadSnapshot(ctx context.Context, path string) (Loaded, error)
}

// Journal is the small amount of state that outlives a restart.
type Journal interface {
	// Get returns the empty string for a key that was never set.
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
}

// State is the whole of what the dashboard shows about the bootstrap.
type State struct {
	Phase      Phase
	Snapshot   Snapshot
	Assessment Assessment
	Progress   Progress
	// Error is the last failure, in the user's words. Cleared by a retry.
	Error string
	// LoadedAt is when the node accepted the snapshot, or zero.
	LoadedAt int64
	// StagedBytes is how much of the file is on disk. Shown while stopped, so
	// somebody who cancelled can see that resuming would not start over.
	StagedBytes int64
}

// Config is how the runner is set up.
type Config struct {
	// Enabled turns the whole feature on. False means the dashboard never
	// mentions it.
	Enabled bool
	// AutoStart begins without being asked. For deployments with no one at a
	// screen — a compose file, an unattended rebuild — where waiting to be asked
	// means waiting for ever.
	AutoStart bool
	// Dir is where the file is staged. The snapshot is deleted from here as soon
	// as the node has read it.
	Dir string
	// Snapshot is what to fetch. Zero value uses MainnetHeight935000.
	Snapshot Snapshot
	// Client makes the requests, already carrying whatever proxy was decided on.
	Client *http.Client
	// PollInterval is how often the node is asked where it has got to.
	PollInterval time.Duration
}

// DefaultPollInterval is how often the node's progress is checked. Slow, because
// nothing here is urgent and the node has better things to do while it syncs.
const DefaultPollInterval = 30 * time.Second

// Runner owns the bootstrap from the offer through to the loaded snapshot.
type Runner struct {
	cfg  Config
	node Node
	jrnl Journal
	log  *slog.Logger
	now  func() time.Time

	// kick wakes the loop when something has been asked of it, so a click is
	// acted on immediately rather than at the next tick.
	kick chan struct{}

	mu         sync.Mutex
	state      State
	armed      bool
	failedAt   time.Time
	cancelWork context.CancelFunc
}

// New builds a runner.
func New(cfg Config, node Node, jrnl Journal, log *slog.Logger, now func() time.Time) (*Runner, error) {
	if node == nil {
		return nil, errors.New("bootstrap: no node to bootstrap")
	}
	if jrnl == nil {
		return nil, errors.New("bootstrap: no journal to remember with")
	}
	if cfg.Snapshot.Network == "" {
		cfg.Snapshot = MainnetHeight935000
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Dir == "" {
		return nil, errors.New("bootstrap: no directory to stage the download in")
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	phase := PhaseOff
	if cfg.Enabled {
		phase = PhaseUnavailable
	}
	return &Runner{
		cfg: cfg, node: node, jrnl: jrnl, log: log, now: now,
		kick:  make(chan struct{}, 1),
		state: State{Phase: phase, Snapshot: cfg.Snapshot},
	}, nil
}

// StagedPath is where the assembled file lives.
func (r *Runner) StagedPath() string {
	return filepath.Join(r.cfg.Dir, StagedFileName)
}

// State returns a copy of what the dashboard should show.
func (r *Runner) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Start arms the bootstrap. Safe to call when it is already running.
func (r *Runner) Start(ctx context.Context) error {
	if !r.cfg.Enabled {
		return errors.New("the snapshot shortcut is switched off in this " +
			"installation's configuration")
	}
	r.mu.Lock()
	already := r.armed
	if r.state.Phase == PhaseFailed {
		// A retry clears the last complaint, so a stale one cannot be mistaken
		// for a fresh failure.
		r.state.Error = ""
		// **And it clears the backoff.** The fifteen-minute wait after a failure
		// exists so that an unattended daemon does not hammer a broken network;
		// somebody pressing a button labelled "Try again now" has said now. Left
		// in place, that button would appear to do nothing for a quarter of an
		// hour, which is indistinguishable from being broken.
		r.failedAt = time.Time{}
	}
	r.armed = true
	// **Moved now rather than at the next tick.** The API answers a start
	// request with the current view, and the phase is otherwise still whatever
	// it was — so somebody who pressed "Use the faster sync" got a reply still
	// offering it, and the button stayed until the next poll. The loop
	// recomputes this immediately and will correct it if the assessment has
	// changed underneath; what it cannot do is un-press the button.
	if r.state.Phase == PhaseOffered || r.state.Phase == PhaseFailed {
		r.state.Phase = PhaseDownloading
	}
	r.mu.Unlock()

	if !already {
		if err := r.jrnl.Set(ctx, JournalArmed, "1"); err != nil {
			return fmt.Errorf("remembering that you asked for this: %w", err)
		}
		r.log.Info("the snapshot shortcut was started")
	}
	r.wake()
	return nil
}

// Cancel stops a running transfer and forgets that it was asked for.
//
// The staged file is deleted. Keeping it would leave nine gigabytes on somebody's
// disk that nothing would ever pick up again — cancelling is how a person says
// they have changed their mind, and the space is usually the reason.
func (r *Runner) Cancel(ctx context.Context) error {
	r.mu.Lock()
	r.armed = false
	if r.cancelWork != nil {
		r.cancelWork()
	}
	r.mu.Unlock()

	if err := r.jrnl.Set(ctx, JournalArmed, ""); err != nil {
		return fmt.Errorf("remembering that you stopped this: %w", err)
	}
	if err := os.Remove(r.StagedPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Warn("could not delete the part-downloaded snapshot",
			slog.String("path", r.StagedPath()), slog.String("error", err.Error()))
	}
	r.log.Info("the snapshot shortcut was stopped")
	r.wake()
	return nil
}

func (r *Runner) wake() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run drives the bootstrap until ctx ends.
func (r *Runner) Run(ctx context.Context) error {
	if !r.cfg.Enabled {
		<-ctx.Done()
		return nil
	}

	// Made now rather than when the download starts, because the free-space check
	// runs long before that and statfs on a path that does not exist reports
	// "unknown" — which is treated as "do not refuse". A machine with no room
	// would be offered the shortcut, accept it, and fail on the first write.
	if err := os.MkdirAll(r.cfg.Dir, 0o700); err != nil {
		r.log.Warn("could not prepare the directory for the faster first sync",
			slog.String("dir", r.cfg.Dir), slog.String("error", err.Error()))
	}

	r.restore(ctx)

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		r.step(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-r.kick:
		}
	}
}

// restore reads back what a previous run decided.
func (r *Runner) restore(ctx context.Context) {
	if height, err := r.jrnl.Get(ctx, JournalDoneHeight); err == nil && height != "" {
		if height == fmt.Sprint(r.cfg.Snapshot.BaseHeight) {
			r.mu.Lock()
			r.state.Phase = PhaseDone
			r.mu.Unlock()
			return
		}
		// A different height was loaded once. That snapshot is not this one, so
		// the offer stands or falls on what the node says now.
		r.log.Info("a snapshot was loaded before, at a different height",
			slog.String("previous", height))
	}

	armed, err := r.jrnl.Get(ctx, JournalArmed)
	if err != nil {
		r.log.Warn("could not read whether the snapshot shortcut was asked for",
			slog.String("error", err.Error()))
		return
	}
	if armed == "1" || r.cfg.AutoStart {
		r.mu.Lock()
		r.armed = true
		r.mu.Unlock()
	}
}

// step performs one round: look at the node, decide, and act if armed.
//
// The work happens inline rather than in a goroutine of its own. While a download
// is running there is nothing else for this loop to do — progress reaches the
// dashboard through the fetcher's callback — and a single sequence is far easier
// to reason about than a worker with its own lifetime.
func (r *Runner) step(ctx context.Context) {
	if r.State().Phase == PhaseDone {
		return
	}

	info, err := r.node.ChainInfo(ctx)
	if err != nil {
		// Not a failure of the bootstrap. The node is unreachable, which the
		// dashboard already says far more prominently than this would.
		r.log.Debug("could not ask the second node where it has got to",
			slog.String("error", err.Error()))
		return
	}

	staged := stagedBytes(r.StagedPath())
	assessment := Assess(r.cfg.Snapshot, NodeState{
		Network:        info.Network,
		Blocks:         info.Blocks,
		Headers:        info.Headers,
		SnapshotLoaded: info.SnapshotLoaded,
		FreeBytes:      FreeBytes(r.cfg.Dir),
		StagedBytes:    staged,
	})

	r.mu.Lock()
	r.state.Assessment = assessment
	r.state.StagedBytes = staged
	armed := r.armed
	cooling := r.state.Phase == PhaseFailed && r.now().Sub(r.failedAt) < RetryAfter
	switch {
	case assessment.Code == ReasonAlreadyLoaded:
		// The node has a snapshot chainstate. Whether this run put it there or a
		// previous one did, the shortcut has been taken.
		r.state.Phase = PhaseDone
	case !assessment.Usable:
		r.state.Phase = PhaseUnavailable
	case !armed:
		r.state.Phase = PhaseOffered
	case cooling:
		// Left where it is. The retry is coming; saying "downloading" in the
		// meantime would be a claim about something that is not happening.
	default:
		r.state.Phase = PhaseDownloading
	}
	phase := r.state.Phase
	r.mu.Unlock()

	if phase != PhaseDownloading || !armed {
		return
	}

	err = r.work(ctx, info)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The daemon is shutting down.
	case !r.isArmed():
		// Somebody cancelled while it ran. That is not a failure, and reporting
		// it as one would leave a red message on the dashboard describing the
		// user's own decision back to them.
		r.log.Info("the transfer stopped because it was cancelled")
	default:
		r.fail(err)
	}
}

// RetryAfter is how long a failed attempt waits before trying again.
//
// Long, and deliberately so. The fetcher already retries a stalled transfer
// thirty times before giving up, so reaching this point means something is
// properly wrong — no network, a full disk, a proxy that is not running. Coming
// straight back would turn that into a request every thirty seconds for however
// many days it takes somebody to notice.
const RetryAfter = 15 * time.Minute

func (r *Runner) isArmed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.armed
}

// work fetches the file and hands it to the node.
func (r *Runner) work(ctx context.Context, info ChainInfo) error {
	workCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancelWork = cancel
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.cancelWork = nil
		r.mu.Unlock()
	}()

	fetcher := &Fetcher{
		Snapshot: r.cfg.Snapshot,
		Path:     r.StagedPath(),
		Client:   r.cfg.Client,
		Logger:   r.log,
		Now:      r.now,
		OnProgress: func(p Progress) {
			r.mu.Lock()
			r.state.Progress = p
			r.mu.Unlock()
		},
	}

	r.log.Info("fetching the UTXO snapshot",
		slog.String("into", r.StagedPath()),
		slog.String("size", HumanBytes(r.cfg.Snapshot.TotalBytes())))
	if err := fetcher.Fetch(workCtx); err != nil {
		return err
	}

	// **Headers are only required now, not before the download.** They arrive in
	// minutes and the file takes hours, so the two run at once; waiting for
	// headers first would have added the shorter to the longer instead of hiding
	// it inside it.
	if err := r.awaitHeaders(workCtx, info); err != nil {
		return err
	}

	r.setPhase(PhaseLoading)
	r.log.Info("handing the snapshot to the second node — this takes several minutes")

	loaded, err := r.node.LoadSnapshot(workCtx, r.StagedPath())
	if err != nil {
		return fmt.Errorf("the second node would not accept the snapshot: %w", err)
	}
	r.log.Info("the second node accepted the snapshot",
		slog.Int("base_height", int(loaded.BaseHeight)),
		slog.Uint64("coins", loaded.Coins))

	// Deleted straight away. It is nine gigabytes that nothing will read again,
	// and leaving it would be the single largest thing this program ever put on
	// somebody's disk.
	if err := os.Remove(r.StagedPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.log.Warn("the snapshot was loaded but its file could not be deleted",
			slog.String("path", r.StagedPath()), slog.String("error", err.Error()))
	}

	if err := r.jrnl.Set(ctx, JournalDoneHeight,
		fmt.Sprint(r.cfg.Snapshot.BaseHeight)); err != nil {
		r.log.Warn("could not record that the snapshot was loaded",
			slog.String("error", err.Error()))
	}
	_ = r.jrnl.Set(ctx, JournalArmed, "")

	r.mu.Lock()
	r.armed = false
	r.state.Phase = PhaseDone
	r.state.Error = ""
	r.state.LoadedAt = r.now().Unix()
	r.state.StagedBytes = 0
	r.mu.Unlock()
	return nil
}

// awaitHeaders waits until the node knows of the snapshot's base block.
func (r *Runner) awaitHeaders(ctx context.Context, info ChainInfo) error {
	for {
		if ok, _ := ReadyToLoad(r.cfg.Snapshot, NodeState{Headers: info.Headers}); ok {
			return nil
		}
		r.log.Info("waiting for the second node's headers to reach the snapshot",
			slog.Int("headers", int(info.Headers)),
			slog.Int("needed", int(r.cfg.Snapshot.BaseHeight)))
		if err := sleep(ctx, r.cfg.PollInterval); err != nil {
			return err
		}
		next, err := r.node.ChainInfo(ctx)
		if err != nil {
			return fmt.Errorf("asking the second node about its headers: %w", err)
		}
		info = next
	}
}

func (r *Runner) setPhase(p Phase) {
	r.mu.Lock()
	r.state.Phase = p
	r.mu.Unlock()
}

// fail records a failure without disarming.
//
// **Deliberately still armed.** Almost everything that goes wrong here is a
// transfer that stopped, and the file on disk is most of the way there; a later
// tick resumes it from where it got to. A failure that disarmed would turn a
// dropped connection into a shortcut the user has to notice and restart by hand
// — and the whole reason this exists is that nobody is watching the screen.
func (r *Runner) fail(err error) {
	r.log.Warn("the snapshot shortcut did not finish, and will be tried again",
		slog.String("error", err.Error()),
		slog.String("retry_in", HumanDuration(RetryAfter)))
	r.mu.Lock()
	r.state.Phase = PhaseFailed
	r.state.Error = err.Error()
	r.failedAt = r.now()
	r.mu.Unlock()
}

// stagedBytes is how much of the file is already there, or zero.
func stagedBytes(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
