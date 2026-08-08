package nodeaddr

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fixed is a clock that does not move, so a cooldown is never accidentally
// waited out by a slow machine.
func fixed() func() time.Time {
	return func() time.Time { return time.Unix(1_790_000_000, 0) }
}

// A node that comes back somewhere else is followed.
func TestANodeThatMovedIsFollowed(t *testing.T) {
	t.Parallel()
	f := New("https://10.0.3.6:8080", "lnd.startos", fixed())
	f.resolve = func(context.Context, string) ([]string, error) {
		return []string{"10.0.3.99"}, nil
	}

	if !f.Moved(t.Context()) {
		t.Fatal("a node that moved was not followed")
	}
	if got := f.Base(); got != "https://10.0.3.99:8080" {
		t.Errorf("base = %q, want the new address with the port kept", got)
	}
}

// A node that is simply down produces no retry at all.
//
// **This is the guard against a spin**, and it is the one that matters: a down
// node resolves to the same address it always did, so nothing has moved and the
// caller is told not to retry. Without it every failed request would become two.
func TestANodeThatIsDownIsNotReportedAsMoved(t *testing.T) {
	t.Parallel()
	f := New("https://10.0.3.6:8080", "lnd.startos", fixed())
	f.resolve = func(context.Context, string) ([]string, error) {
		return []string{"10.0.3.6"}, nil
	}

	if f.Moved(t.Context()) {
		t.Error("a node that had not moved was reported as moved, which would " +
			"retry every failed request")
	}
}

// The lookup itself is rate-limited, so a node that stays down does not produce
// one lookup per request either.
func TestLookupsAreRateLimited(t *testing.T) {
	t.Parallel()
	var lookups int
	clock := time.Unix(1_790_000_000, 0)
	f := New("https://10.0.3.6:8080", "lnd.startos", func() time.Time { return clock })
	f.resolve = func(context.Context, string) ([]string, error) {
		lookups++
		return []string{"10.0.3.6"}, nil
	}

	f.Moved(t.Context())
	f.Moved(t.Context())
	f.Moved(t.Context())
	if lookups != 1 {
		t.Errorf("%d lookups inside the cooldown, want 1", lookups)
	}

	clock = clock.Add(ReresolveEvery + time.Second)
	f.Moved(t.Context())
	if lookups != 2 {
		t.Errorf("%d lookups after the cooldown passed, want 2", lookups)
	}
}

// Every doubtful case answers "do not retry".
func TestDoubtfulCasesDoNotRetry(t *testing.T) {
	t.Parallel()

	t.Run("no name to look up", func(t *testing.T) {
		t.Parallel()
		f := New("https://10.0.3.6:8080", "", fixed())
		if f.Moved(t.Context()) {
			t.Error("a client with no name reported a move")
		}
	})

	t.Run("the lookup failed", func(t *testing.T) {
		t.Parallel()
		f := New("https://10.0.3.6:8080", "lnd.startos", fixed())
		f.resolve = func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		}
		if f.Moved(t.Context()) {
			t.Error("a failed lookup reported a move")
		}
	})

	t.Run("the lookup answered nothing", func(t *testing.T) {
		t.Parallel()
		f := New("https://10.0.3.6:8080", "lnd.startos", fixed())
		f.resolve = func(context.Context, string) ([]string, error) {
			return nil, nil
		}
		if f.Moved(t.Context()) {
			t.Error("an empty answer reported a move")
		}
	})
}

// The port and scheme survive; only the address changes.
func TestOnlyTheAddressChanges(t *testing.T) {
	t.Parallel()
	got, changed := swapHost("https://10.0.3.6:8080", "10.0.3.99")
	if !changed || got != "https://10.0.3.99:8080" {
		t.Errorf("swapHost = %q, %v", got, changed)
	}
	if _, changed := swapHost("https://10.0.3.6:8080", "10.0.3.6"); changed {
		t.Error("an unchanged address was reported as a move")
	}
}
