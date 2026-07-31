//go:build integration

package scenarios

import "testing"

// S11: the early warning.
//
// The commitment is broadcast on the other chain and not yet mined. Seeing it
// now buys the user a block of notice they would not otherwise have had — and on
// a minority chain, where blocks can be half an hour apart, a block of notice is
// a great deal of time.
func TestS11ACommitmentIsNoticedBeforeItIsMined(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)

	// Published to the other chain, deliberately unmined.
	w.forkbench(t, "breach", "-branch", "sq", "-confirm=false")

	waitFor(t, "the unconfirmed sighting", func() bool {
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Status == "mempool" {
				return true
			}
		}
		return false
	})

	// Raised at the loudest tier despite nothing having confirmed: this is the
	// moment the user has the most time and therefore the most options.
	waitFor(t, "an early-warning alert", func() bool {
		a, ok := alertOfKind(t, "channel_spent_unconfirmed")
		return ok && a.Tier == "critical"
	})

	// No countdown yet. The delay it would measure runs from a block this
	// transaction is not in, so starting one now would be counting from nothing.
	if running := countdowns(t, "counting"); len(running) != 0 {
		t.Errorf("a countdown was started from an unmined transaction: %+v\n%s",
			running, w.describe(t))
	}

	// And when it is mined, the same record becomes a fact rather than a
	// sighting — one event, not two.
	before := len(spends(t))
	w.blocksOn(t, "sq", 1)

	waitFor(t, "the sighting to be confirmed", func() bool {
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Status == "confirmed" {
				return true
			}
		}
		return false
	})
	if after := len(spends(t)); after != before {
		t.Errorf("confirming turned %d records into %d\n%s", before, after, w.describe(t))
	}
	waitFor(t, "the countdown to start", func() bool { return len(countdowns(t, "counting")) > 0 })
}
