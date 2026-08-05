// Package app wires the daemon together: configuration, storage, the event bus,
// the engines, and the HTTP server, with a single lifecycle that starts and stops
// them in a defined order.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/api"
	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/bitcoindview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/deadline"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/registry/cln"
	"github.com/paulscode/forktower/internal/registry/lnd"
	"github.com/paulscode/forktower/internal/responder/mirror"
	"github.com/paulscode/forktower/internal/responder/tower"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/standdown"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
)

// Lifecycle bounds.
const (
	// ShutdownTimeout is how long the daemon has to stop cleanly before it is
	// abandoned. A supervisor that has sent SIGTERM will send SIGKILL soon after,
	// so taking longer than this achieves nothing except looking hung.
	ShutdownTimeout = 5 * time.Second
	// ReadHeaderTimeout bounds how long a client may take to send its headers,
	// which is what stops an idle connection holding a slot open indefinitely.
	ReadHeaderTimeout = 10 * time.Second
	// startupTimeout bounds the checks that run before anything is watched.
	startupTimeout = 60 * time.Second
	// bytesPerMB converts the configured timeline limit, which is written in
	// megabytes because that is the unit a person thinks about disk in.
	bytesPerMB = 1024 * 1024
)

// App is the whole daemon: configuration, storage, the event bus, the engines,
// and the HTTP server, started and stopped in a defined order.
type App struct {
	cfg config.Config
	log *slog.Logger

	store     *store.Store
	bus       *bus.Bus
	sf, sq    chainview.ChainView
	sentinel  *sentinel.Sentinel
	registry  *registry.Registry
	watcher   *watcher.Watcher
	deadline  *deadline.Engine
	wardens   []*tower.Warden
	scouts    []*tower.Scout
	mirrors   []*mirror.Runner
	standDown *standdown.Switch
	alerter   *alert.Alerter
	timeline  *store.TimelineSubscriber
	api       *api.Server
	server    *http.Server
	listener  net.Listener
}

// Deps lets a test substitute the parts that would otherwise need real nodes.
//
// Everything here is nil in production, where the real thing is built from
// configuration. The alternative — a test that needs two Bitcoin nodes to prove
// the daemon starts in the right order — is a test nobody runs.
type Deps struct {
	SF, SQ chainview.ChainView
	// Now overrides the clock. Nil reads the real one.
	Now func() time.Time
	// Listener overrides where the API listens, so a test can take a free port.
	Listener net.Listener
	// LNSources replaces the Lightning nodes built from configuration, so a test
	// can prove the daemon reads channels without needing a real one. A non-nil
	// empty slice means "no Lightning nodes", which is different from nil.
	LNSources []registry.Source
}

// New builds the daemon.
//
// The order below is not arbitrary, and two steps in it are load-bearing:
//
//   - the alerter and the timeline are constructed *before* the sentinel,
//     because both subscribe to the bus in their constructors, and the sentinel
//     starts publishing as soon as it runs. Building them the other way round
//     leaves a window where the first thing that happens is announced to nobody.
//
//   - the network is derived from the user's own node and then *required* of the
//     second view. There is nothing to compare a lone node against, and a
//     configured expectation would be one more thing to get wrong; the node the
//     user already trusts is the reference.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, deps Deps) (*App, error) {
	if log == nil {
		log = slog.New(discardHandler{})
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	a := &App{cfg: cfg, log: log}

	st, err := store.Open(ctx, cfg.Store.Path)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	a.store = st
	for _, warning := range st.Warnings {
		log.Warn("database", slog.String("note", warning))
	}
	log.Info("storage ready", slog.String("path", st.Path()))

	// Said once, at startup. A setting somebody switched on that does nothing is
	// exactly the sort of quiet the rest of this daemon is built to avoid.
	for _, unused := range cfg.UnusedSettings() {
		log.Warn("a setting in your configuration does nothing yet",
			slog.String("note", unused))
	}

	a.bus = bus.New(log)

	if a.sf, err = a.buildView(deps.SF, cfg.SF, "sf", log); err != nil {
		a.closeOnFailure()
		return nil, err
	}
	if a.sq, err = a.buildView(deps.SQ, cfg.SQ.Bitcoind, "sq", log); err != nil {
		a.closeOnFailure()
		return nil, err
	}

	routes, err := alert.RoutesFromConfig(cfg.Alerts.Transport, 0)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up notifications: %w", err)
	}
	a.alerter, err = alert.New(st, a.bus, routes, alert.Config{
		CriticalRepeat:        time.Duration(cfg.Alerts.CriticalRepeatMins) * time.Minute,
		SelfTestInterval:      time.Duration(cfg.Alerts.SelfTestIntervalHours) * time.Hour,
		PlatformNotifications: cfg.Alerts.PlatformNotifications,
	}, log.With(slog.String("component", "alerts")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up notifications: %w", err)
	}
	log.Info("notifications ready", slog.Int("transports", len(routes)))

	a.timeline = store.NewTimelineSubscriber(st, a.bus,
		int64(cfg.Store.TimelineMaxMB)*bytesPerMB,
		log.With(slog.String("component", "timeline")),
		func() int64 { return now().Unix() })

	network, err := a.deriveNetwork(ctx)
	if err != nil {
		a.closeOnFailure()
		return nil, err
	}
	log.Info("chain identified", slog.String("network", network.Name))

	a.sentinel, err = sentinel.New(a.sf, a.sq, st, a.bus, sentinel.Config{
		PollInterval:      time.Duration(cfg.Sentinel.PollIntervalSecs) * time.Second,
		SplitConfirmDepth: cfg.Sentinel.SplitConfirmDepth,
		StallFactor:       cfg.Sentinel.SQStallFactor,
		ReorgMargin:       cfg.EffectiveReorgMargin(),
		DivergenceHeight:  cfg.Fork.DivergenceHeight,
		DeploymentName:    cfg.Fork.Name,
		Network:           network,
		MaxAncestorWalk:   int(cfg.Sentinel.MaxAncestorWalk),
		// Zero means "use the engine's own default", which is what is wanted here:
		// there is no configuration knob for it, and spelling a number in this
		// file would put the real value two places instead of one.
		AncestryDepth: 0,
	}, log.With(slog.String("component", "sentinel")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up the detection engine: %w", err)
	}

	sources, err := a.buildLNSources(deps.LNSources, cfg.LN, log)
	if err != nil {
		a.closeOnFailure()
		return nil, err
	}
	// The user's own node first and the other chain's backend second. Before the
	// fork the two hold the same block, so the second is a real fallback for a
	// funding transaction the first has pruned away.
	a.registry, err = registry.New(st, a.bus, sources,
		[]registry.BlockSource{a.sf, a.sq}, registry.Config{},
		log.With(slog.String("component", "registry")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up channel watching: %w", err)
	}

	a.standDown, err = standdown.New(ctx, st)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("reading whether watching was stood down: %w", err)
	}
	if a.standDown.Down() {
		// Said every time, because this is the one condition where everything else
		// looks normal and nothing is being watched.
		log.Warn("watching the other chain is stood down, because somebody turned " +
			"it off — nothing there is being checked until it is turned back on")
	}

	// Two reasons to stop scanning, and they are different in kind. The sentinel
	// pauses when it cannot be sure the second view is on the chain it should be,
	// which is a fault. The switch is a person deciding they do not want it
	// watched, which is not. Either one stops the reading; only the first is
	// something to fix.
	guard := watchGuard{sentinel: a.sentinel, standDown: a.standDown}
	a.watcher, err = watcher.New(st, a.bus, a.sq, store.BranchSQ, guard,
		watcher.Config{}, log.With(slog.String("component", "watcher")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up spend watching: %w", err)
	}

	// Built after the watcher, because the countdowns it keeps are started by
	// what the watcher finds — and both subscribe in their constructors, so the
	// order they are built in is the order they hear about anything.
	a.deadline, err = deadline.New(st, a.bus, store.BranchSQ,
		log.With(slog.String("component", "deadline")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up the countdowns: %w", err)
	}

	// The companion towers, if any are configured. Built after the deadline
	// engine because nothing subscribes here — a warden only publishes — so its
	// place in the order is a matter of reading rather than of correctness.
	if err := a.buildWardens(cfg, log, now); err != nil {
		a.closeOnFailure()
		return nil, err
	}

	// Copying transactions between the chains. Built unconditionally: the policy
	// declines almost everything by default, and the useful half of what this
	// records is the refusals — which a user can only be shown if something was
	// there to decline them.
	if err := a.buildMirrors(log); err != nil {
		a.closeOnFailure()
		return nil, err
	}

	a.api, err = api.New(st, a.sentinel, a.alerter, a.registry, a.deadline,
		a.watcher, a.standDown, api.Config{
			Auth:                  cfg.UI.Auth,
			PasswordHash:          cfg.UI.PasswordHash,
			AllowedOrigins:        cfg.UI.AllowedOrigins,
			FrameAncestors:        cfg.UI.FrameAncestors,
			PlatformNotifications: cfg.Alerts.PlatformNotifications,
			Platform:              cfg.Platform,
		}, log.With(slog.String("component", "api")), now)
	if err != nil {
		a.closeOnFailure()
		return nil, fmt.Errorf("setting up the dashboard: %w", err)
	}
	a.api.MountUI()

	a.listener = deps.Listener
	if a.listener == nil {
		var lc net.ListenConfig
		if a.listener, err = lc.Listen(ctx, "tcp", cfg.UI.Listen); err != nil {
			a.closeOnFailure()
			return nil, fmt.Errorf("listening on %s: %w", cfg.UI.Listen, err)
		}
	}
	a.server = &http.Server{
		Handler:           a.api,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	log.Info("dashboard ready",
		slog.String("address", a.listener.Addr().String()),
		slog.String("authentication", string(cfg.UI.Auth)))
	// `platform` serves the dashboard unauthenticated on a non-loopback address,
	// trusting that a platform proxy is in front of it. On StartOS and Umbrel
	// that proxy is genuinely there and saying so on every start would be noise
	// nobody reads.
	//
	// **On anything else it is a claim nobody checked.** The likeliest way to
	// arrive here is copying a configuration from a packaged install, which is a
	// sensible thing to do and quietly removes the only thing standing between
	// the dashboard and the network it is bound to. So the warning is narrow
	// enough to mean something when it appears.
	if cfg.UI.Auth == config.AuthPlatform && cfg.Platform == config.PlatformUnknown {
		log.Warn("serving the dashboard without a password, because the configuration "+
			"says a platform proxy authenticates it — but this does not look like a "+
			"packaged install, so nothing here can confirm that proxy exists. If the "+
			"port is reachable, so is the dashboard",
			slog.String("address", a.listener.Addr().String()))
	}

	if cfg.UI.Auth == config.AuthNone && cfg.UI.AccessRestrictedExternally {
		// Said out loud every time, because it is the one place Forktower is
		// trusting a claim it cannot check. If whatever was supposed to restrict
		// this port ever stops doing so, the dashboard is open and nothing here
		// would notice.
		log.Warn("serving the dashboard without a password, because the configuration " +
			"says access to it is restricted elsewhere — check that whatever does " +
			"that is still doing it")
	}

	return a, nil
}

// watchGuard is the two reasons scanning stops, in one answer.
//
// Kept as its own type rather than a closure so that the reasoning above has
// somewhere to live: a fault and a decision both stop the reading, and only one
// of them is a problem.
type watchGuard struct {
	sentinel  *sentinel.Sentinel
	standDown *standdown.Switch
}

// Paused implements watcher.Guard.
func (g watchGuard) Paused() bool {
	return g.sentinel.Paused() || g.standDown.Down()
}

// buildView makes one chain view, or takes the one a test supplied.
func (a *App) buildView(
	supplied chainview.ChainView, ep config.RPCEndpoint, name string, log *slog.Logger,
) (chainview.ChainView, error) {
	if supplied != nil {
		return supplied, nil
	}

	view, err := bitcoindview.New(bitcoindview.Options{
		RPCURL:      ep.RPCURL,
		CookiePath:  ep.RPCCookiePath,
		User:        ep.RPCUser,
		Pass:        ep.RPCPass,
		ZMQRawBlock: ep.ZMQRawBlock,
		ZMQRawTx:    ep.ZMQRawTx,
		UserAgent:   "forktower",
		Logger:      log.With(slog.String("component", "chain-"+name)),
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to the %s Bitcoin node: %w", name, err)
	}
	// Deliberately does not log the URL: it may carry credentials, and the
	// component name is enough to tell the two apart in a log.
	log.Info("chain view ready", slog.String("view", name))
	return view, nil
}

// buildLNSources turns the configured Lightning nodes into registry sources.
//
// Named by implementation and position rather than by address, because two nodes
// of the same implementation are a supported arrangement and an address in a log
// can carry a credential.
func (a *App) buildLNSources(
	supplied []registry.Source, cfg config.LNConfig, log *slog.Logger,
) ([]registry.Source, error) {
	if supplied != nil {
		return supplied, nil
	}

	var out []registry.Source
	for i, n := range cfg.LND {
		name := fmt.Sprintf("lnd-%d", i+1)
		client, err := lnd.New(lnd.Options{
			BaseURL:      n.RESTAddr,
			TLSCertPath:  n.TLSCertPath,
			MacaroonPath: n.MacaroonPath,
			Logger:       log.With(slog.String("component", name)),
		})
		if err != nil {
			return nil, fmt.Errorf("connecting to your LND node: %w", err)
		}
		out = append(out, registry.Source{Name: name, Client: client})
	}
	for i, n := range cfg.CLN {
		name := fmt.Sprintf("cln-%d", i+1)
		client, err := cln.New(cln.Options{
			BaseURL:     n.RESTAddr,
			RunePath:    n.RunePath,
			TLSCertPath: n.TLSCertPath,
			Logger:      log.With(slog.String("component", name)),
		})
		if err != nil {
			return nil, fmt.Errorf("connecting to your Core Lightning node: %w", err)
		}
		out = append(out, registry.Source{Name: name, Client: client})
	}
	if len(out) > 0 {
		log.Info("channel watching ready", slog.Int("lightning_nodes", len(out)))
	}
	return out, nil
}

// deriveNetwork reads which chain the user's own node is on.
//
// Taken from their node rather than from configuration because it is the one
// they have already validated, and because a name typed into a file is one more
// thing that can be wrong. The second view is then held to it, which is the check
// that actually matters: a backend pointed at a test network answers every
// request correctly and diverges permanently.
func (a *App) deriveNetwork(ctx context.Context) (chainview.NetworkParams, error) {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	genesis, err := chainview.GenesisOf(ctx, a.sf)
	if err != nil {
		return chainview.NetworkParams{}, fmt.Errorf(
			"asking your Bitcoin node which chain it is on: %w", err)
	}

	params := chainview.NetworkParams{Genesis: genesis}
	if named, ok := a.sf.(interface {
		Network(context.Context) (string, error)
	}); ok {
		if name, nameErr := named.Network(ctx); nameErr == nil {
			params.Name = name
		}
	}
	return params, nil
}

// Run starts everything and returns when the context ends.
//
// Preflight comes first and its failure is fatal. A view on the wrong network, or
// two views that turn out to be one node, make every later comparison meaningless
// — and meaningless comparisons are indistinguishable from calm ones, which is
// the failure this whole daemon exists to prevent.
func (a *App) Run(ctx context.Context) error {
	preflightCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	if err := a.sentinel.Preflight(preflightCtx); err != nil {
		return fmt.Errorf("startup checks did not pass: %w", err)
	}
	a.log.Info("startup checks passed")

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error { return a.timeline.Run(groupCtx) })
	group.Go(func() error { return a.alerter.Run(groupCtx) })
	group.Go(func() error { return a.sentinel.Run(groupCtx) })
	group.Go(func() error { return a.registry.Run(groupCtx) })
	group.Go(func() error { return a.watcher.Run(groupCtx) })
	group.Go(func() error { return a.deadline.Run(groupCtx) })
	for _, w := range a.wardens {
		group.Go(func() error { return w.Run(groupCtx) })
	}
	for _, sc := range a.scouts {
		group.Go(func() error { return sc.Run(groupCtx) })
	}
	for _, m := range a.mirrors {
		group.Go(func() error { return m.Run(groupCtx) })
	}

	group.Go(func() error {
		a.log.Info("Forktower is running")
		if err := a.server.Serve(a.listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving the dashboard: %w", err)
		}
		return nil
	})

	// The listener is closed first so nothing new arrives, then the engines are
	// given the remaining time to finish what they were doing. Storage writes
	// already outlive their own contexts, so an engine interrupted mid-write still
	// records what it had decided.
	group.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
		defer stop()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			a.log.Warn("the dashboard did not stop cleanly",
				slog.String("error", err.Error()))
		}
		return nil
	})

	err := group.Wait()
	a.log.Info("Forktower has stopped")
	return err
}

// Addr is where the dashboard is listening.
func (a *App) Addr() string {
	if a.listener == nil {
		return ""
	}
	return a.listener.Addr().String()
}

// Close releases what New acquired. Safe to call more than once.
func (a *App) Close() error {
	var err error
	if a.listener != nil {
		// Already closed by Shutdown on the ordinary path; closing twice is
		// harmless and covers the paths where Run never got that far.
		_ = a.listener.Close()
	}
	if a.bus != nil {
		a.bus.Close()
	}
	if a.store != nil {
		if closeErr := a.store.Close(); closeErr != nil {
			err = closeErr
		}
	}
	return err
}

// closeOnFailure unwinds a half-built daemon, so a startup error does not leave a
// database handle or a listener behind.
func (a *App) closeOnFailure() {
	if closeErr := a.Close(); closeErr != nil {
		a.log.Warn("could not clean up after a failed start",
			slog.String("error", closeErr.Error()))
	}
}
