//go:build integration

package scenarios

import (
	"strings"
	"testing"
)

// S1: the attack this whole project exists for.
//
// A counterparty is rolled back to a channel state they had already promised
// never to publish, and they publish it — on the chain the user's own node does
// not follow. On that node, nothing has happened: the channel is open, the
// balance is what it was, and every tool the user has says everything is fine.
//
// What must happen is that they are told, urgently, with a clock they can act
// on.
func TestS1ABreachOnTheOtherChainIsDetectedAndCounted(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)
	w.forkbench(t, "breach", "-branch", "sq")

	// The spend, on the other chain and only there.
	waitFor(t, "the commitment to be recorded", func() bool {
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Status == "confirmed" {
				return true
			}
		}
		return false
	})

	var recorded spend
	for _, sp := range spends(t) {
		if sp.Branch == "sq" && sp.Status == "confirmed" {
			recorded = sp
		}
	}
	// Forktower cannot tell a revoked commitment from a current one — nothing on
	// the chain says which — so it must not claim to. What it can say is that
	// somebody's commitment confirmed and it does not know whose.
	if recorded.Shape != "commitment_unknown" {
		t.Errorf("the commitment was recorded as %q\n%s", recorded.Shape, w.describe(t))
	}

	// The alert, and it has to be the loudest tier there is.
	waitFor(t, "a critical alert", func() bool {
		a, ok := alertOfKind(t, "channel_spent")
		return ok && a.Tier == "critical"
	})
	raised, _ := alertOfKind(t, "channel_spent")
	if !strings.Contains(strings.ToLower(raised.Message), "open forktower") {
		t.Errorf("the alert does not tell the user what to do: %q", raised.Message)
	}

	// The countdown, which is the thing that turns an alarm into something
	// actionable.
	waitFor(t, "a countdown", func() bool { return len(countdowns(t, "counting")) > 0 })
	clock := countdowns(t, "counting")[0]
	if clock.DeadlineHeight <= recorded.BlockHeight {
		t.Errorf("the deadline is at or before the block it started from: %+v", clock)
	}
	if clock.RemainingBlocks <= 0 {
		t.Errorf("the countdown had already run out when it started: %+v", clock)
	}
	// And nothing has been declared lost. A window that opens already closed is
	// how a missing input turns into a false loss report, and it would arrive
	// looking exactly like the real thing.
	if lost := countdowns(t, "expired"); len(lost) != 0 {
		t.Errorf("something was declared lost the moment it was detected: %+v\n%s",
			lost, w.describe(t))
	}

	// And the exposure table says who, how much, and what is being done.
	rows := channels(t)
	if len(rows) != 1 {
		t.Fatalf("the dashboard shows %d channels\n%s", len(rows), w.describe(t))
	}
	row := rows[0]
	if row.Threat.State != "confirmed" {
		t.Errorf("the channel reads as %q", row.Threat.State)
	}
	if row.Display.Partner == "" || row.Display.AtRiskSat == 0 || row.Display.Status == "" {
		t.Errorf("the row does not say who, how much, or what is happening: %+v", row.Display)
	}
	if row.Threat.HeadlineDeadline == nil {
		t.Errorf("the channel shows no countdown\n%s", w.describe(t))
	}
}
