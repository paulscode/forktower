package standdown_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/paulscode/forktower/internal/standdown"
	"github.com/paulscode/forktower/internal/store"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(dir, "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// A fresh daemon is watching. Anything else would mean a first install that
// protects nobody until somebody finds the switch.
func TestAFreshInstallIsWatching(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())

	sw, err := standdown.New(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Down() || !sw.Active() {
		t.Error("a fresh install started stood down")
	}
}

// The decision survives a restart, because it is a decision rather than a
// condition. A daemon that quietly resumed would be overruling somebody who had
// said no.
func TestTheDecisionSurvivesARestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()

	first := openStore(t, dir)
	sw, err := standdown.New(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Set(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openStore(t, dir)
	again, err := standdown.New(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Down() {
		t.Error("a restart quietly started watching again")
	}

	// And turning it back on survives too.
	if err := again.Set(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !again.Active() {
		t.Error("resuming did not resume")
	}
}

// Written first, then believed. A switch that flipped in memory and failed to
// persist would resume at the next restart with nothing to say it had ever been
// turned off.
func TestAFailedWriteDoesNotChangeTheAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, t.TempDir())

	sw, err := standdown.New(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := sw.Set(ctx, true); err == nil {
		t.Fatal("a write to a closed store reported success")
	}
	if sw.Down() {
		t.Error("the switch changed in memory despite the write having failed")
	}
}

func TestNewRefusesWithoutAStore(t *testing.T) {
	t.Parallel()

	if _, err := standdown.New(context.Background(), nil); err == nil {
		t.Error("a switch with nowhere to record itself was accepted")
	}
}

// A value nobody wrote, or one written by something else, reads as watching.
// Failing toward watching is the only safe direction for this particular flag.
func TestAnythingUnrecognisedMeansWatching(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, t.TempDir())

	for _, raw := range []string{"", "0", "false", "no", "yes", "something"} {
		if err := st.SetMeta(ctx, standdown.MetaKey, raw); err != nil {
			t.Fatal(err)
		}
		sw, err := standdown.New(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		if raw == "1" {
			continue
		}
		if sw.Down() {
			t.Errorf("the stored value %q was read as stood down", raw)
		}
	}
}

// A store that cannot be read is a startup failure worth reporting, not a
// silent assumption that watching is on. Assuming would be the safe direction
// for most flags and the wrong one here: it would quietly overrule somebody who
// had turned watching off, and the log would say nothing.
func TestAnUnreadableStoreIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := standdown.New(ctx, st); err == nil {
		t.Error("a switch was built from a store that could not be read")
	}
}
