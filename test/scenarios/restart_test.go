//go:build integration

package scenarios

import "testing"

// A restart in the middle of an incident.
//
// Package upgrades happen. Machines reboot. Somebody restarts the daemon because
// something else on the box was misbehaving. None of that is allowed to lose a
// countdown that is running against the user — and a clock that silently
// restarted, or silently vanished, would be worse than no clock, because the
// dashboard would carry on looking authoritative.
func TestARestartMidCountdownResumesWhereItLeftOff(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)
	w.forkbench(t, "breach", "-branch", "sq")

	waitFor(t, "the countdown", func() bool { return len(countdowns(t, "counting")) > 0 })
	before := countdowns(t, "counting")[0]
	spendsBefore := len(spends(t))
	alertsBefore := len(alerts(t))

	w.restartDaemon(t)

	// The same clock, at the same height, still counting.
	waitFor(t, "the countdown to come back", func() bool {
		return len(countdowns(t, "counting")) > 0
	})
	after := countdowns(t, "counting")[0]

	if after.ID != before.ID {
		t.Errorf("the countdown was replaced: %d became %d", before.ID, after.ID)
	}
	if after.DeadlineHeight != before.DeadlineHeight {
		t.Errorf("the deadline moved across a restart: %d became %d",
			before.DeadlineHeight, after.DeadlineHeight)
	}

	// Nothing was recorded twice. A restart that re-read the same blocks and
	// found the same spends must produce the same rows, not another set.
	if got := len(spends(t)); got != spendsBefore {
		t.Errorf("a restart turned %d spends into %d\n%s", spendsBefore, got, w.describe(t))
	}
	if got := len(alerts(t)); got != alertsBefore {
		t.Errorf("a restart turned %d alerts into %d", alertsBefore, got)
	}

	// And it keeps counting: the chain moves, and so does the clock.
	w.blocksOn(t, "sq", 3)
	waitFor(t, "the countdown to advance", func() bool {
		running := countdowns(t, "counting")
		return len(running) > 0 && running[0].RemainingBlocks < after.RemainingBlocks
	})
}
