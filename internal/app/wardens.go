package app

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/responder/mirror"
	"github.com/paulscode/forktower/internal/responder/tower"
	"github.com/paulscode/forktower/internal/store"
)

// buildWardens sets up watching for whichever companion towers are configured.
//
// **A tower that is switched off produces no warden and no complaint.** Most
// installations will not run one, and a dashboard card saying "no tower" on
// every one of them would be noise rather than information — the readiness
// model already covers "you have no watchtower protection" as a separate
// question about the whole deployment, not about a tower that does not exist.
func (a *App) buildWardens(cfg config.Config, log *slog.Logger, now func() time.Time) error {
	instances := []struct {
		kind store.TowerKind
		conf config.TowerInstance
	}{
		{store.TowerLND, cfg.Tower.LND},
		{store.TowerTeos, cfg.Tower.TEOS},
	}

	for _, inst := range instances {
		if !inst.conf.Enabled {
			continue
		}
		warden, err := a.buildWarden(inst.kind, inst.conf, cfg, log, now)
		if err != nil {
			return fmt.Errorf("setting up the %s watchtower: %w", inst.kind, err)
		}
		a.wardens = append(a.wardens, warden)
		log.Info("watching a companion watchtower",
			slog.String("kind", string(inst.kind)),
			slog.String("listen", inst.conf.Listen))
	}

	// And the towers the user registered with themselves.
	//
	// **Discovered rather than configured.** The node already knows which towers
	// it backs up to — they typed them in when they registered — so asking for
	// the same list again would be asking twice and getting it wrong once. Runs
	// whether or not a local tower is configured, because a user with no tower of
	// their own and a community one is an ordinary deployment and the one where
	// nobody would otherwise be watching.
	client := a.towerClient(store.TowerLND, cfg, log)
	if client == nil {
		return nil
	}
	scout, err := tower.NewScout(tower.ScoutOptions{
		Store: a.store, Client: client, Bus: a.bus,
		Log: log.With(slog.String("component", "tower")),
		Now: now,
	})
	if err != nil {
		return fmt.Errorf("setting up watching for towers you registered with: %w", err)
	}
	a.scouts = append(a.scouts, scout)
	return nil
}

// buildWarden assembles one tower's supervisor, its coverage monitor, and the
// warden that runs both.
func (a *App) buildWarden(
	kind store.TowerKind, conf config.TowerInstance, cfg config.Config,
	log *slog.Logger, now func() time.Time,
) (*tower.Warden, error) {
	reader, err := towerReader(kind, conf)
	if err != nil {
		return nil, err
	}

	supervisor, err := tower.New(tower.Options{
		Kind:    kind,
		Reader:  reader,
		DataDir: conf.DataDir,
		LimitMB: conf.DiskLimitMB(),
	})
	if err != nil {
		return nil, err
	}

	return tower.NewWarden(tower.WardenOptions{
		Store:      a.store,
		Bus:        a.bus,
		Log:        log.With(slog.String("component", "tower")),
		Supervisor: supervisor,
		// The user's own node, which is where the evidence about what is actually
		// being backed up lives. Nil when they have none configured, or when the
		// tower is a teos one: the LND coverage check reads sessions that a Core
		// Lightning node does not have. The tower is still watched either way.
		Client:  a.towerClient(kind, cfg, log),
		Kind:    kind,
		Managed: true,
		URI:     conf.Listen,
		Now:     now,
	})
}

// towerClient is the user's own LND, read for what it is backing up.
//
// The first LND in the configuration. Forktower supports several Lightning
// nodes, but a watchtower session belongs to one node, and asking a second node
// about a first node's sessions would produce a confident answer about the wrong
// thing. Returns nil when there is no LND to ask — a Core Lightning user has
// their own tower arm, and a user with no node at all has nothing to protect.
func (a *App) towerClient(
	kind store.TowerKind, cfg config.Config, log *slog.Logger,
) tower.ClientReader {
	if kind != store.TowerLND {
		return nil
	}
	for _, node := range cfg.LN.LND {
		client, err := tower.NewLND(tower.LNDOptions{
			BaseURL:      node.RESTAddr,
			TLSCertPath:  node.TLSCertPath,
			MacaroonPath: node.MacaroonPath,
		})
		if err != nil {
			// Not fatal. The tower is still worth watching, and the same credential
			// is already reported on by the registry — saying it twice would be
			// noise, and refusing to watch the tower over it would be worse.
			log.Warn("cannot read what this node is backing up",
				slog.String("node", node.RESTAddr),
				slog.String("error", err.Error()))
			continue
		}
		return client
	}
	return nil
}

// towerReader builds the right way of reading a tower for its kind.
//
// The two are not variations of one thing. An LND tower answers a rich
// diagnostic API with a read-only macaroon; a teos tower offers a single
// unauthenticated liveness endpoint and keeps everything else on a private
// interface. What can honestly be said about each differs, and the readers say
// so rather than pretending to a common shape.
func towerReader(kind store.TowerKind, conf config.TowerInstance) (tower.Reader, error) {
	switch kind {
	case store.TowerLND:
		return tower.NewLND(tower.LNDOptions{
			BaseURL:      conf.APIURL,
			TLSCertPath:  conf.TLSCertPath,
			MacaroonPath: conf.MacaroonPath,
		})
	case store.TowerTeos:
		return tower.NewTeos(tower.TeosOptions{
			APIURL: conf.APIURL,
			Pubkey: conf.Pubkey,
		})
	default:
		return nil, fmt.Errorf("%q is not a watchtower kind", kind)
	}
}

// buildMirrors sets up copying transactions between the two chains.
//
// Two runners, one each way, because the rules differ sharply between the
// directions: away from the user's own chain the mirror carries closes and
// sweeps and justice; back towards it, only a close both parties agreed to.
// A single engine serving both would need a flag at every decision.
func (a *App) buildMirrors(log *slog.Logger) error {
	directions := []struct {
		from, to store.Branch
		view     chainview.ChainView
		target   chainview.ChainView
	}{
		{store.BranchSF, store.BranchSQ, a.sf, a.sq},
		{store.BranchSQ, store.BranchSF, a.sq, a.sf},
	}

	for _, d := range directions {
		observer, err := mirror.NewObserver(mirror.ObserverOptions{
			Store: a.store, View: d.view, From: d.from, To: d.to,
			Log: log.With(slog.String("component", "mirror")),
		})
		if err != nil {
			return fmt.Errorf("setting up copying from %s: %w", d.from, err)
		}

		sender, err := mirror.New(mirror.Options{
			Store: a.store, Target: d.target, Branch: d.to,
			Log: log.With(slog.String("component", "mirror")),
		})
		if err != nil {
			return fmt.Errorf("setting up copying to %s: %w", d.to, err)
		}

		runner, err := mirror.NewRunner(mirror.RunnerOptions{
			Observer: observer, Mirror: sender, Bus: a.bus, From: d.from,
			Log: log.With(slog.String("component", "mirror")),
		})
		if err != nil {
			return fmt.Errorf("setting up copying from %s: %w", d.from, err)
		}
		a.mirrors = append(a.mirrors, runner)
	}
	return nil
}
