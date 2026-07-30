package sentinel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"golang.org/x/sync/errgroup"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// Config is what the sentinel needs to know that is not a view or a store.
type Config struct {
	// PollInterval is how often the two views are compared.
	PollInterval time.Duration
	// SplitConfirmDepth is how far past a separation both chains must build before
	// divergence is believed.
	SplitConfirmDepth int32
	// StallFactor multiplies a chain's own measured pace to decide when it has gone
	// quiet.
	StallFactor float64
	// ReorgMargin is the safety margin below the user's tip when bounding how far
	// their chain counts as verified.
	ReorgMargin int32
	// DivergenceHeight is the first height at which the two rule sets could
	// disagree, or zero when unknown.
	DivergenceHeight int32
	// DeploymentName, when set, is the soft fork to read from the user's own node so
	// the heights above can be derived rather than configured.
	DeploymentName string
	// Network identifies the chain both views must be on.
	Network chainview.NetworkParams
	// MaxAncestorWalk bounds the separation-point search.
	MaxAncestorWalk int
	// AncestryDepth is how many recent hashes are supplied to the decision logic.
	AncestryDepth int
}

// Defaults for anything a caller leaves unset.
const (
	DefaultPollInterval  = 10 * time.Second
	DefaultAncestryDepth = 100
	// BranchReverifyBlocks is how many new blocks pass between branch-identity
	// checks. Repeated rather than done once at startup, because the failure it
	// catches — a view quietly following the wrong chain — can begin at any time,
	// and a check that ran only at startup would miss all of them.
	BranchReverifyBlocks = 100
)

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.SplitConfirmDepth < 1 {
		c.SplitConfirmDepth = 3
	}
	if c.StallFactor <= 0 {
		c.StallFactor = defaultStallFactor
	}
	if c.MaxAncestorWalk <= 0 {
		c.MaxAncestorWalk = DefaultMaxAncestorWalk
	}
	if c.AncestryDepth <= 0 {
		c.AncestryDepth = DefaultAncestryDepth
	}
	return c
}

// Clock supplies the current time, injected so behaviour that depends on elapsed
// time can be tested without waiting for it.
type Clock func() time.Time

// Sentinel watches two chain views and decides whether they have diverged.
//
// The decision logic lives in this package but performs no I/O; this is the shell
// that feeds it. Keeping them apart is what makes the transitions provable without
// a node.
type Sentinel struct {
	sf, sq chainview.ChainView
	store  *store.Store
	bus    *bus.Bus
	cfg    Config
	log    *slog.Logger
	now    Clock

	cache *HeaderCache

	mu    sync.RWMutex
	state State

	// checks records what the startup verifications concluded, for the readiness
	// display. A check that could not be performed is reported as such rather than
	// as a pass.
	checks Checks

	// blocksSinceVerify counts new blocks on the watched chain since the last
	// branch-identity check.
	blocksSinceVerify int
	// paused is set when watching must stop because the view cannot be trusted.
	// Scanning the wrong chain produces false comfort, which is worse than
	// producing nothing.
	paused bool
}

// Checks is what the verifications concluded.
type Checks struct {
	// SameNetwork is false only when proven wrong, which is fatal at startup.
	SameNetwork bool
	// DistinctNodes reports whether the two views were proven to be different
	// nodes. Unknown means the backends could not say — reported honestly rather
	// than assumed.
	DistinctNodes    bool
	DistinctVerified bool
	// OnExpectedBranch reports the last branch-identity result.
	OnExpectedBranch bool
	BranchVerifiedAt int64
	// Detail explains the most recent problem, in words safe to show a user.
	Detail string
}

// New builds a sentinel. It contacts nothing; call Preflight for that.
func New(
	sf, sq chainview.ChainView,
	st *store.Store,
	b *bus.Bus,
	cfg Config,
	logger *slog.Logger,
	now Clock,
) (*Sentinel, error) {
	if sf == nil || sq == nil {
		return nil, errors.New("sentinel: both chain views are required")
	}
	if st == nil {
		return nil, errors.New("sentinel: a store is required")
	}
	if logger == nil {
		logger = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}
	return &Sentinel{
		sf:    sf,
		sq:    sq,
		store: st,
		bus:   b,
		cfg:   cfg.withDefaults(),
		log:   logger.With("engine", "sentinel"),
		now:   now,
		cache: NewHeaderCache(0),
		state: NewState(),
	}, nil
}

// State returns a copy of the current state.
func (s *Sentinel) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Checks returns what the verifications concluded.
func (s *Sentinel) Checks() Checks {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checks
}

// Paused reports whether watching has been suspended because a view cannot be
// trusted.
func (s *Sentinel) Paused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

// Preflight runs the checks that must pass before anything is watched.
//
// Two of them, both guarding against failures that are otherwise silent and would
// leave every other indicator green while nothing was being watched:
//
//   - both views on the expected network. A view pointed at a test network answers
//     every request correctly and diverges permanently, which would read as a
//     chain split rather than as a misconfiguration. Fatal.
//
//   - the two views are different nodes. One node behind both configurations
//     produces views that agree by construction, so divergence becomes
//     unrepresentable. Fatal when proven; reported as an unavailable check when the
//     backends cannot say.
func (s *Sentinel) Preflight(ctx context.Context) error {
	if err := chainview.VerifyNetwork(ctx, s.sf, s.cfg.Network); err != nil {
		return fmt.Errorf("the Bitcoin node Forktower reads from is not on the expected network: %w", err)
	}
	if err := chainview.VerifyNetwork(ctx, s.sq, s.cfg.Network); err != nil {
		return fmt.Errorf("the second view is not on the expected network: %w", err)
	}

	s.mu.Lock()
	s.checks.SameNetwork = true
	s.mu.Unlock()

	err := chainview.VerifyDistinct(ctx, s.sf, s.sq)
	switch {
	case errors.Is(err, chainview.ErrSameNode):
		return fmt.Errorf(
			"both chain views are the same Bitcoin node, so Forktower would compare a node "+
				"against itself and could never see a split: %w", err)
	case errors.Is(err, chainview.ErrCannotVerifyDistinct):
		// Reported, not assumed. Saying "verified" here would be the same class of
		// lie the check exists to prevent.
		s.mu.Lock()
		s.checks.DistinctVerified = false
		s.checks.Detail = "could not confirm the two views are different nodes"
		s.mu.Unlock()
		s.log.Warn("could not confirm the two chain views are different nodes")
	case err != nil:
		return fmt.Errorf("checking that the two views are different nodes: %w", err)
	default:
		s.mu.Lock()
		s.checks.DistinctNodes = true
		s.checks.DistinctVerified = true
		s.mu.Unlock()
	}
	return nil
}

// Load restores the persisted state, so a restart resumes rather than restarts.
//
// A recorded split in particular must survive: losing it because the machine
// rebooted would silently drop the exposure the daemon exists to track.
func (s *Sentinel) Load(ctx context.Context) error {
	persisted, err := s.store.GetSplitState(ctx)
	if err != nil {
		return fmt.Errorf("reading the recorded split state: %w", err)
	}

	state := NewState()
	state.Phase = Phase(persisted.State)
	if !state.Phase.Valid() {
		s.log.Warn("the recorded state was not recognised; starting over",
			slog.String("recorded", string(persisted.State)))
		state.Phase = PhaseUnarmed
	}
	if persisted.ForkKnown() {
		hash, hashErr := chainhash.NewHashFromStr(persisted.ForkHash)
		if hashErr != nil {
			return fmt.Errorf("the recorded separation point %q is unreadable: %w",
				persisted.ForkHash, hashErr)
		}
		state.Fork = &chainview.BlockRef{Hash: *hash, Height: persisted.ForkHeight}
	}
	state.DetectedAt = persisted.DetectedAt

	anchor, err := s.store.GetMetaInt64(ctx, store.MetaTrustAnchorHeight, 0)
	if err != nil {
		return err
	}
	// Stored as a 64-bit integer because that is what the column holds, but a
	// height is 32-bit everywhere else. A value that will not fit did not come from
	// this daemon, and silently truncating it would move the anchor — the one
	// number that decides how far back history is trusted — to somewhere arbitrary.
	if anchor > math.MaxInt32 || anchor < math.MinInt32 {
		return fmt.Errorf("the recorded trust anchor %d is not a possible block height", anchor)
	}
	state.TrustAnchor = int32(anchor)

	s.mu.Lock()
	s.state = state
	s.mu.Unlock()

	s.log.Info("restored recorded state",
		slog.String("phase", string(state.Phase)),
		slog.Bool("separation_known", state.Fork != nil))
	return nil
}

// Run watches both views until the context ends.
//
// Driven by a timer and by each view's tip notifications together: the timer
// guarantees progress even if notifications stop, and the notifications make the
// response prompt. Neither alone is enough — a socket can die without erroring,
// and a timer alone would leave a split unnoticed for a whole interval.
func (s *Sentinel) Run(ctx context.Context) error {
	if err := s.Load(ctx); err != nil {
		// A load abandoned by shutdown is not a failure, and Run reports a clean
		// shutdown as nil everywhere else.
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown, not a load failure
		}
		return err
	}
	if err := s.deriveForkDescriptor(ctx); err != nil {
		// Not fatal: the descriptor is an optimisation, and its absence widens a
		// safety margin rather than removing one.
		s.log.Warn("could not read the fork's parameters from your node",
			slog.String("error", err.Error()))
	}

	wake := make(chan struct{}, 1)
	group, gctx := errgroup.WithContext(ctx)

	group.Go(func() error { return s.followTips(gctx, s.sf, wake) })
	group.Go(func() error { return s.followTips(gctx, s.sq, wake) })
	group.Go(func() error { return s.loop(gctx, wake) })

	err := group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// followTips turns a view's notifications into wake-ups.
//
// Only a nudge: the tick reads both views afresh, so a notification's contents are
// not needed and a missed one costs latency rather than correctness.
func (s *Sentinel) followTips(ctx context.Context, v chainview.ChainView, wake chan<- struct{}) error {
	tips, err := v.SubscribeTip(ctx)
	if err != nil {
		// A view that cannot be subscribed to is still polled. Reported, not fatal.
		s.log.Warn("could not subscribe to a chain view; falling back to polling only",
			slog.String("error", err.Error()))
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-tips:
			if !ok {
				return nil
			}
			select {
			case wake <- struct{}{}:
			default:
				// A wake-up is already pending; one is as good as two.
			}
		}
	}
}

func (s *Sentinel) loop(ctx context.Context, wake <-chan struct{}) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx)
		case <-wake:
			s.tick(ctx)
		}
	}
}

// tick gathers one observation, advances the decision logic, and performs whatever
// it asks for.
func (s *Sentinel) tick(ctx context.Context) {
	obs := s.observe(ctx)

	s.mu.Lock()
	prev := s.state
	next, effects := Step(prev, obs)
	s.state = next
	s.mu.Unlock()

	s.apply(ctx, prev, next, effects)
	s.maybeReverifyBranch(ctx, next, effects)
}

// writeCtx returns a context for storage writes that survives shutdown.
//
// Reads may be abandoned when the daemon is stopping; writes may not. A split
// detected on the same tick as a shutdown must still be recorded, or the next
// start would resume from a state that omits it — and the whole point of
// persisting before announcing is that storage is never behind what was observed.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

// writeTimeout bounds a storage write that is deliberately outliving cancellation,
// so a stuck database cannot prevent the process from exiting.
const writeTimeout = 5 * time.Second

// observe reads both views and assembles what the decision logic needs.
//
// Every failure degrades rather than aborts: a view that cannot be read this tick
// contributes nothing, and the logic treats a missing tip as absence of evidence
// rather than as evidence of agreement.
func (s *Sentinel) observe(ctx context.Context) Observation {
	obs := Observation{
		At:                s.now().Unix(),
		SplitConfirmDepth: s.cfg.SplitConfirmDepth,
		StallFactor:       s.cfg.StallFactor,
		DivergenceHeight:  s.cfg.DivergenceHeight,
		ReorgMargin:       s.cfg.ReorgMargin,
	}

	obs.SFHealth, obs.SFTip = s.readView(ctx, s.sf, chainview.BranchSF)
	obs.SQHealth, obs.SQTip = s.readView(ctx, s.sq, chainview.BranchSQ)

	if tipper, ok := s.sf.(chainview.ChainTipper); ok {
		tips, err := tipper.ChainTips(ctx)
		if err != nil {
			s.log.Debug("could not read branch tips from your node",
				slog.String("error", err.Error()))
		} else {
			obs.SFTips = tips
		}
	}

	obs.SFAncestry = s.recentHashes(ctx, store.BranchSF)
	obs.SQAncestry = s.recentHashes(ctx, store.BranchSQ)

	// The separation-point search is I/O, so it happens here and its result is
	// handed to the decision logic.
	if obs.SFTip != nil && obs.SQTip != nil && obs.SFTip.Hash != obs.SQTip.Hash {
		fork, err := FindForkPoint(ctx,
			s.headerFetcher(s.sf), s.headerFetcher(s.sq),
			*obs.SFTip, *obs.SQTip, s.cfg.MaxAncestorWalk, s.cache)
		switch {
		case err == nil:
			obs.ForkCandidate = &fork
		case errors.Is(err, ErrForkTooDeep):
			obs.ForkSearchFailed = true
			s.log.Debug("could not find where the chains separated",
				slog.String("error", err.Error()))
		default:
			obs.ForkSearchFailed = true
			s.log.Warn("searching for where the chains separated failed",
				slog.String("error", err.Error()))
		}
	}
	return obs
}

func (s *Sentinel) readView(
	ctx context.Context, v chainview.ChainView, branch chainview.Branch,
) (chainview.HealthState, *chainview.BlockMeta) {
	health, err := v.Health(ctx)
	if err != nil {
		s.log.Warn("could not read a chain view's health",
			slog.String("branch", string(branch)), slog.String("error", err.Error()))
		return chainview.HealthDown, nil
	}
	if !health.State.Usable() {
		// Unusable views still report their state; their tip is not trusted for
		// comparison, because comparing against a syncing or suspect view produces
		// conclusions about a chain we cannot presently see.
		return health.State, nil
	}
	tip, err := v.BestBlock(ctx)
	if err != nil {
		s.log.Warn("could not read a chain view's tip",
			slog.String("branch", string(branch)), slog.String("error", err.Error()))
		return chainview.HealthDown, nil
	}
	return health.State, &tip
}

func (s *Sentinel) recentHashes(ctx context.Context, branch store.Branch) []chainhash.Hash {
	raw, err := s.store.RecentBranchHashes(ctx, branch, s.cfg.AncestryDepth)
	if err != nil {
		s.log.Debug("could not read recent block hashes",
			slog.String("branch", string(branch)), slog.String("error", err.Error()))
		return nil
	}
	out := make([]chainhash.Hash, 0, len(raw))
	for _, h := range raw {
		parsed, parseErr := chainhash.NewHashFromStr(h)
		if parseErr != nil {
			continue
		}
		out = append(out, *parsed)
	}
	return out
}

func (s *Sentinel) headerFetcher(v chainview.ChainView) HeaderFetcher {
	return func(ctx context.Context, h chainhash.Hash) (chainview.BlockMeta, error) {
		return v.BlockHeaderByHash(ctx, h)
	}
}

// apply performs the effects the decision logic asked for.
//
// Storage first, then announcements. A crash between the two leaves storage ahead
// of the notification, which startup reconciliation absorbs; the reverse would
// announce something that was never recorded.
func (s *Sentinel) apply(ctx context.Context, prev, next State, effects []Effect) {
	for _, e := range effects {
		switch e.Kind {
		case EffectPersistState:
			s.persistState(ctx, next)
		case EffectPhaseChanged:
			s.announcePhase(ctx, e)
		case EffectBranchExtended:
			s.recordBlock(ctx, e)
		case EffectHealthChanged:
			s.announceHealth(e)
		case EffectBothViewsDown:
			s.log.Error("neither chain can be reached; the recorded state is kept",
				slog.String("phase", string(next.Phase)))
			s.publish(bus.ViewHealthChanged{
				View: "both", Old: string(chainview.HealthOK), New: string(chainview.HealthDown),
				Detail: e.Detail,
			})
		case EffectTrustAnchorChanged:
			s.persistTrustAnchor(ctx, e.Height)
		}
	}
	_ = prev
}

func (s *Sentinel) persistState(ctx context.Context, st State) {
	ctx, cancel := writeCtx(ctx)
	defer cancel()

	record := store.Split{
		State:      store.SplitState(st.Phase),
		DetectedAt: st.DetectedAt,
		UpdatedAt:  s.now().Unix(),
	}
	if st.Fork != nil {
		record.ForkHeight = st.Fork.Height
		record.ForkHash = st.Fork.Hash.String()
	}
	if err := s.store.SaveSplitState(ctx, record); err != nil {
		s.log.Error("could not record the split state", slog.String("error", err.Error()))
	}
}

func (s *Sentinel) persistTrustAnchor(ctx context.Context, height int32) {
	ctx, cancel := writeCtx(ctx)
	defer cancel()

	if err := s.store.SetMetaInt64(ctx, store.MetaTrustAnchorHeight, int64(height)); err != nil {
		s.log.Error("could not record the trust anchor", slog.String("error", err.Error()))
		return
	}
	s.log.Info("history below this height is treated as already verified",
		slog.Int("height", int(height)))
}

func (s *Sentinel) announcePhase(_ context.Context, e Effect) {
	s.log.Info("the relationship between the chains changed",
		slog.String("from", string(e.OldPhase)),
		slog.String("to", string(e.NewPhase)),
		slog.String("detail", e.Detail))

	event := bus.SplitStateChanged{Old: string(e.OldPhase), New: string(e.NewPhase)}
	if fork := s.State().Fork; fork != nil {
		event.Fork = &bus.BlockRefJSON{Hash: fork.Hash.String(), Height: fork.Height}
	}
	s.publish(event)
}

func (s *Sentinel) recordBlock(ctx context.Context, e Effect) {
	if e.Block == nil {
		return
	}
	branch := store.BranchSF
	if e.Branch == chainview.BranchSQ {
		branch = store.BranchSQ
		s.mu.Lock()
		s.blocksSinceVerify++
		s.mu.Unlock()
	}

	ctx, cancel := writeCtx(ctx)
	defer cancel()

	if err := s.store.RecordBranchBlock(ctx, store.BranchBlock{
		Branch:     branch,
		Height:     e.Block.Height,
		Hash:       e.Block.Hash.String(),
		PrevHash:   e.Block.PrevHash.String(),
		BlockTime:  e.Block.Time.Unix(),
		ReceivedAt: s.now().Unix(),
	}); err != nil {
		s.log.Error("could not record a block", slog.String("error", err.Error()))
	}

	s.publish(bus.SplitBranchExtended{
		Branch: string(e.Branch),
		Block: bus.BlockMetaJSON{
			Hash:     e.Block.Hash.String(),
			Height:   e.Block.Height,
			PrevHash: e.Block.PrevHash.String(),
			Time:     e.Block.Time.Unix(),
		},
		SinceForkDepth:  e.SinceForkDepth,
		AvgIntervalSecs: e.IntervalSecs,
	})
}

func (s *Sentinel) announceHealth(e Effect) {
	s.log.Info("a chain view's health changed",
		slog.String("branch", string(e.Branch)),
		slog.String("from", string(e.OldHealth)),
		slog.String("to", string(e.NewHealth)))

	s.publish(bus.ViewHealthChanged{
		View:   string(e.Branch),
		Old:    string(e.OldHealth),
		New:    string(e.NewHealth),
		Detail: e.Detail,
	})
}

func (s *Sentinel) publish(e bus.Event) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(e)
}

// maybeReverifyBranch re-runs the branch-identity check on a cadence, and whenever
// a split is first recorded.
//
// Repeated because the failure it catches can begin at any time: a view's peers
// may all upgrade, leaving it quietly following the same chain as the user's own
// node. A check that ran only at startup would miss every such case.
func (s *Sentinel) maybeReverifyBranch(ctx context.Context, st State, effects []Effect) {
	justSplit := false
	for _, e := range effects {
		if e.Kind == EffectPhaseChanged && e.NewPhase == PhaseSplit {
			justSplit = true
		}
	}

	s.mu.RLock()
	due := s.blocksSinceVerify >= BranchReverifyBlocks
	s.mu.RUnlock()

	if !justSplit && !due {
		return
	}

	s.mu.Lock()
	s.blocksSinceVerify = 0
	s.mu.Unlock()

	err := chainview.VerifyBranch(ctx, s.sf, s.sq, st.TrustAnchor, st.Fork)
	switch {
	case err == nil:
		s.mu.Lock()
		s.checks.OnExpectedBranch = true
		s.checks.BranchVerifiedAt = s.now().Unix()
		s.checks.Detail = ""
		wasPaused := s.paused
		s.paused = false
		s.mu.Unlock()
		if wasPaused {
			s.log.Info("the second view is on the expected chain again; watching resumed")
		}
		if err := s.store.SetMetaInt64(ctx, store.MetaSQBranchVerifiedAt, s.now().Unix()); err != nil {
			s.log.Debug("could not record the verification time", slog.String("error", err.Error()))
		}

	case errors.Is(err, chainview.ErrCannotVerifyBranch):
		// Not enough history yet. Expected in the first blocks of a split, and not a
		// reason to stop.
		s.log.Debug("not enough history to check the second view's chain yet",
			slog.String("detail", err.Error()))

	default:
		// Watching pauses. Scanning the wrong chain produces a clean report about a
		// chain nobody needs watched, which is more dangerous than producing nothing
		// — the user would be told they are covered while the exposure went unseen.
		s.mu.Lock()
		s.checks.OnExpectedBranch = false
		s.checks.Detail = "the second view is not on the chain it should be; watching is paused"
		s.paused = true
		s.mu.Unlock()

		s.log.Error("the second chain view is not following the expected chain; watching paused "+
			"because scanning the wrong chain would look like safety",
			slog.String("error", err.Error()))
		s.publish(bus.ViewHealthChanged{
			View:   string(chainview.BranchSQ),
			Old:    string(chainview.HealthOK),
			New:    string(chainview.HealthWrongBranch),
			Detail: "this view is not on the chain it should be, so watching has been paused",
		})
	}
}

// deriveForkDescriptor reads the fork's parameters from the user's own node.
//
// Preferred over configuration: the node's own answer cannot go stale, cannot be a
// number copied wrongly out of a document, and generalises to any future fork
// deployed the same way. A configured value still wins, but a disagreement is
// worth saying out loud — a stale hand-edited height is more dangerous than a
// missing one, because it bounds how far the user's chain is trusted.
func (s *Sentinel) deriveForkDescriptor(ctx context.Context) error {
	if s.cfg.DeploymentName == "" {
		return nil
	}
	deployer, ok := s.sf.(chainview.Deployer)
	if !ok {
		return nil
	}

	d, err := deployer.Deployment(ctx, s.cfg.DeploymentName)
	if err != nil {
		return err
	}
	derived, ok := DivergenceHeightFrom(d)
	if !ok {
		return fmt.Errorf("the node's report of %q does not give a usable divergence height",
			s.cfg.DeploymentName)
	}

	switch {
	case s.cfg.DivergenceHeight == 0:
		s.cfg.DivergenceHeight = derived
		s.log.Info("read the fork's parameters from your node",
			slog.String("deployment", s.cfg.DeploymentName),
			slog.Int("divergence_height", int(derived)))
	case s.cfg.DivergenceHeight != derived:
		s.log.Warn("the configured divergence height disagrees with what your node reports; "+
			"using the configured value",
			slog.Int("configured", int(s.cfg.DivergenceHeight)),
			slog.Int("from_node", int(derived)))
	}
	return nil
}

// DivergenceHeightFrom derives the first height at which the two rule sets can
// disagree from a node's report of a deployment.
//
// For a signalled deployment with a mandatory window, that window opens one
// retarget period before lock-in, which is two periods before the latest the rules
// can begin to bind. It is the *window*, not the activation height, that matters:
// the chains can separate as soon as blocks start being rejected, which is well
// before the new rules apply to transactions.
func DivergenceHeightFrom(d *chainview.Deployment) (int32, bool) {
	if d == nil || d.MaxActivationHeight <= 0 || d.Period <= 0 {
		return 0, false
	}
	height := d.MaxActivationHeight - 2*d.Period
	if height <= 0 {
		return 0, false
	}
	return height, true
}
