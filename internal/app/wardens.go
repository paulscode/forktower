package app

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/paulscode/forktower/internal/config"
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
	return nil
}

// buildWarden assembles one tower's supervisor, its coverage monitor, and the
// warden that runs both.
func (a *App) buildWarden(
	kind store.TowerKind, conf config.TowerInstance, cfg config.Config,
	log *slog.Logger, now func() time.Time,
) (*tower.Warden, error) {
	reader, err := tower.NewLND(tower.LNDOptions{
		BaseURL:      conf.APIURL,
		TLSCertPath:  conf.TLSCertPath,
		MacaroonPath: conf.MacaroonPath,
	})
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
		// being backed up lives. Nil when they have none configured: the tower is
		// still watched, but nothing can be said about what it protects.
		Client:  a.towerClient(cfg, log),
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
func (a *App) towerClient(cfg config.Config, log *slog.Logger) tower.ClientReader {
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
