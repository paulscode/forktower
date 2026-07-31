//go:build integration

package scenarios

import (
	"strings"
	"testing"
)

// S6: the exposure nobody expects.
//
// A channel is closed by agreement on the user's own chain. To them it is
// finished: the balance is settled, the channel is gone from their node, and
// there is nothing left to think about. On the chain nobody is watching, that
// close never happened — so the funding output is still there, and every old
// commitment the counterparty holds is still spendable against it.
//
// Nothing happens to announce this. That is exactly what makes it dangerous, and
// why the warning comes from a sweep of stored state rather than from an event.
func TestS6AChannelClosedOnOneChainIsStillExposedOnTheOther(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)

	// Closed by agreement, on the user's own chain only.
	w.forkbench(t, "coop-close")

	waitFor(t, "the close to be seen on the user's own chain", func() bool {
		for _, c := range channels(t) {
			if c.Threat.State != "none" && strings.TrimSpace(c.Display.Status) != "" {
				return true
			}
		}
		return false
	})

	// Nothing has spent the funding output on the other chain, which is the
	// whole exposure.
	for _, sp := range spends(t) {
		if sp.Branch == "sq" {
			t.Fatalf("something already spent the funding output on the other chain: %+v\n%s",
				sp, w.describe(t))
		}
	}

	// The sweep runs at startup, so a restart is what a user's daily cycle looks
	// like here — and is also how a channel closed while nothing was running gets
	// noticed at all.
	w.restartDaemon(t)

	waitFor(t, "the slow-burn warning", func() bool {
		a, ok := alertOfKind(t, "closed_only_on_your_chain")
		return ok && a.Tier == "warning"
	})

	raised, _ := alertOfKind(t, "closed_only_on_your_chain")
	lower := strings.ToLower(raised.Message)
	// The sentence has to carry the surprise, or a user reads "closed" and stops.
	if !strings.Contains(lower, "still be spent") {
		t.Errorf("the warning does not explain the exposure: %q", raised.Message)
	}

	// Nothing is counting, and nothing has been lost: a cooperative close starts
	// no clock, because there is nothing to wait for and nothing to contest.
	if running := countdowns(t, "counting"); len(running) != 0 {
		t.Errorf("a cooperative close started %d countdowns\n%s", len(running), w.describe(t))
	}
	if lost := countdowns(t, "expired"); len(lost) != 0 {
		t.Errorf("a cooperative close produced %d losses", len(lost))
	}

	// And the channel is still being watched, which is the point.
	rows := channels(t)
	if len(rows) != 1 {
		t.Fatalf("the dashboard shows %d channels", len(rows))
	}
	if rows[0].Threat.State == "none" {
		t.Errorf("a closed-but-exposed channel reads as nothing to watch\n%s", w.describe(t))
	}
}
