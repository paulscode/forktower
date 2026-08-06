package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/paulscode/forktower/internal/bootstrap"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

// buildBootstrap sets up the UTXO-snapshot shortcut, or leaves it absent.
//
// Absent rather than disabled when the second view cannot support it: only a
// full node can be handed a snapshot, and only one running beside us can read a
// file this process wrote. A runner that existed but could never work would put
// an offer on the dashboard that fails the moment somebody accepts it.
func (a *App) buildBootstrap(cfg config.Config, log *slog.Logger, now func() time.Time) error {
	snap := cfg.SQ.Snapshot
	if !snap.Enabled && !snap.AutoStart {
		return nil
	}

	node, ok := a.sq.(bootstrap.Node)
	if !ok {
		// A backend that cannot load a snapshot. Said out loud rather than
		// ignored: somebody switched this on and is entitled to know it did
		// nothing.
		log.Warn("the snapshot shortcut is switched on, but the second chain view " +
			"is not a full node that can accept one — it will not be offered")
		return nil
	}

	dir := snap.Dir
	if dir == "" {
		// Beside the database, which is on the volume sized for chain data. The
		// packaging normally sets this explicitly; this is for a hand-written
		// configuration that did not.
		dir = filepath.Join(filepath.Dir(cfg.Store.Path), "sq")
	}

	shot := bootstrap.MainnetHeight935000
	if snap.BaseURL != "" {
		// Only where to fetch from. The checksums and the base block hash stay
		// exactly as compiled in, so a mirror can be faster but cannot be
		// different.
		shot.BaseURL = snap.BaseURL
		log.Info("the UTXO snapshot will be fetched from a configured mirror")
	}

	runner, err := bootstrap.New(bootstrap.Config{
		Enabled:   true,
		AutoStart: snap.AutoStart,
		Dir:       dir,
		Snapshot:  shot,
		Client:    bootstrap.NewHTTPClient(snap.Proxy),
	}, node, metaJournal{store: a.store},
		log.With(slog.String("component", "snapshot")), now)
	if err != nil {
		return fmt.Errorf("setting up the snapshot shortcut: %w", err)
	}
	a.bootstrap = runner

	if snap.Proxy == "" {
		// Worth one line at startup. The transfer works perfectly well without a
		// proxy; what it gives up is that the request itself says who is asking,
		// and that is a decision the user should know they have made.
		log.Warn("the UTXO snapshot will be downloaded over a direct connection, " +
			"so whoever serves it learns this machine's address")
	}
	log.Info("the snapshot shortcut is available",
		slog.String("stage_dir", dir),
		slog.Bool("through_a_proxy", snap.Proxy != ""))
	return nil
}

// metaJournal keeps the bootstrap's handful of remembered facts in the store's
// meta table.
//
// A narrow adapter rather than handing the whole store over, so that the
// bootstrap package can be tested against a map and cannot reach anything else in
// the database.
type metaJournal struct {
	store *store.Store
}

// Get returns the empty string for a key that was never set, which is what the
// bootstrap's "have we been asked yet" questions want.
func (j metaJournal) Get(ctx context.Context, key string) (string, error) {
	value, err := j.store.GetMeta(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return value, err
}

func (j metaJournal) Set(ctx context.Context, key, value string) error {
	return j.store.SetMeta(ctx, key, value)
}
