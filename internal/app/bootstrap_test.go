package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/paulscode/forktower/internal/bootstrap"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

// snapshotCapableView is a chain view that can also take a snapshot, which is
// what the real bitcoind-backed one is.
type snapshotCapableView struct {
	chainview.ChainView
}

func (snapshotCapableView) ChainInfo(context.Context) (bootstrap.ChainInfo, error) {
	return bootstrap.ChainInfo{Network: "main"}, nil
}

func (snapshotCapableView) LoadSnapshot(context.Context, string) (bootstrap.Loaded, error) {
	return bootstrap.Loaded{}, nil
}

func newAppForBootstrap(t *testing.T, sq chainview.ChainView) *App {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &App{store: st, sq: sq, log: slog.New(discardHandler{})}
}

func snapshotConfig(t *testing.T, mutate func(*config.SnapshotConfig)) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
	cfg.SQ.Snapshot = config.SnapshotConfig{Enabled: true, Dir: t.TempDir()}
	if mutate != nil {
		mutate(&cfg.SQ.Snapshot)
	}
	return cfg
}

func TestTheShortcutIsAbsentUnlessItIsSwitchedOn(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	cfg := snapshotConfig(t, func(s *config.SnapshotConfig) {
		s.Enabled = false
		s.AutoStart = false
	})

	if err := a.buildBootstrap(cfg, a.log, nil); err != nil {
		t.Fatal(err)
	}
	if a.bootstrap != nil {
		t.Error("the shortcut was built despite being switched off")
	}
}

// auto_start is enough on its own. Somebody who sets only that has said clearly
// what they want, and refusing because a second flag was not also set would be
// pedantry with a three-day cost.
func TestAutoStartImpliesTheShortcutIsAvailable(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	cfg := snapshotConfig(t, func(s *config.SnapshotConfig) {
		s.Enabled = false
		s.AutoStart = true
	})

	if err := a.buildBootstrap(cfg, a.log, nil); err != nil {
		t.Fatal(err)
	}
	if a.bootstrap == nil {
		t.Fatal("auto_start alone did not make the shortcut available")
	}
}

// A backend that cannot be handed a snapshot must not produce an offer.
//
// The wiring type-asserts, and a runner that existed but could never work would
// put a button on the dashboard that fails the moment somebody presses it — after
// nine gigabytes have moved.
func TestABackendThatCannotTakeASnapshotIsNotOfferedOne(t *testing.T) {
	// A plain chain view: no ChainInfo, no LoadSnapshot.
	a := newAppForBootstrap(t, plainView{})

	if err := a.buildBootstrap(snapshotConfig(t, nil), a.log, nil); err != nil {
		t.Fatal(err)
	}
	if a.bootstrap != nil {
		t.Error("a backend that cannot load a snapshot was offered the shortcut")
	}
}

// A mirror changes where the parts come from and nothing else. The checksums and
// the base block hash are compiled in, so a faster host cannot become a
// different file.
func TestAMirrorChangesTheSourceAndNotTheContents(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	cfg := snapshotConfig(t, func(s *config.SnapshotConfig) {
		s.BaseURL = "https://mirror.invalid/snapshots/"
	})

	if err := a.buildBootstrap(cfg, a.log, nil); err != nil {
		t.Fatal(err)
	}
	got := a.bootstrap.State().Snapshot

	if got.BaseURL != "https://mirror.invalid/snapshots/" {
		t.Errorf("BaseURL = %q, the mirror was not used", got.BaseURL)
	}
	official := bootstrap.MainnetHeight935000
	if got.SHA256 != official.SHA256 || got.BaseHash != official.BaseHash {
		t.Error("configuring a mirror changed what the file is required to contain")
	}
	if len(got.Parts) != len(official.Parts) {
		t.Fatalf("the mirror changed the part list")
	}
	for i, p := range got.Parts {
		if p.SHA256 != official.Parts[i].SHA256 || p.Bytes != official.Parts[i].Bytes {
			t.Errorf("part %d was changed by configuring a mirror", i)
		}
	}
}

// Without a directory of its own the file lands beside the second node's data,
// which is the volume sized for a blockchain and therefore the one with room.
func TestTheDownloadDefaultsToTheSecondNodesVolume(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	cfg := snapshotConfig(t, func(s *config.SnapshotConfig) { s.Dir = "" })
	cfg.Store.Path = "/data/forktower.db"

	if err := a.buildBootstrap(cfg, a.log, nil); err != nil {
		t.Fatal(err)
	}
	want := "/data/sq/" + bootstrap.StagedFileName
	if got := a.bootstrap.StagedPath(); got != want {
		t.Errorf("the download would be staged at %q, want %q", got, want)
	}
}

// The journal answers "never set" with an empty string, which is what every
// question the bootstrap asks of it wants. An error there would be reported as a
// broken database on a first run, when nothing is wrong at all.
func TestAnUnsetJournalKeyReadsAsEmptyRatherThanMissing(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	jrnl := metaJournal{store: a.store}
	ctx := context.Background()

	got, err := jrnl.Get(ctx, bootstrap.JournalArmed)
	if err != nil {
		t.Fatalf("reading a key that was never set: %v", err)
	}
	if got != "" {
		t.Errorf("an unset key read back as %q", got)
	}

	if err := jrnl.Set(ctx, bootstrap.JournalDoneHeight, "935000"); err != nil {
		t.Fatal(err)
	}
	if got, err = jrnl.Get(ctx, bootstrap.JournalDoneHeight); err != nil || got != "935000" {
		t.Errorf("round trip gave %q, %v", got, err)
	}
}

// A store that has been closed underneath the journal must report the failure
// rather than answering "never set", which would silently re-offer a shortcut
// already taken.
func TestAJournalFailureIsNotMistakenForAnUnsetKey(t *testing.T) {
	a := newAppForBootstrap(t, snapshotCapableView{})
	jrnl := metaJournal{store: a.store}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := jrnl.Get(context.Background(), bootstrap.JournalDoneHeight)
	if err == nil {
		t.Error("a broken database answered as though the key was simply unset")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Error("a database failure was reported as a missing key")
	}
}

// plainView is a chain view with none of the snapshot methods.
type plainView struct{ chainview.ChainView }
