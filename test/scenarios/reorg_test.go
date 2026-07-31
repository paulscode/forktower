//go:build integration

package scenarios

import "testing"

// A breach that disappears.
//
// The block carrying the commitment is replaced. A spend Forktower had recorded
// is no longer on the chain — and the tempting reading, that the danger has
// passed, is wrong. A counterparty replacing their transaction with a higher fee
// looks exactly like this, and so does a reorganisation that will put it
// straight back.
//
// What must happen: the record is kept and marked, the user is told, and the
// countdown keeps running in case it comes back.
func TestABreachThatLeavesTheChainIsNotTreatedAsRelief(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)
	w.forkbench(t, "breach", "-branch", "sq")

	waitFor(t, "the commitment and its countdown", func() bool {
		var confirmed bool
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Status == "confirmed" {
				confirmed = true
			}
		}
		return confirmed && len(countdowns(t, "counting")) > 0
	})
	before := len(spends(t))

	// The block it landed in is replaced by two others, so the chain also grows —
	// which is what makes the daemon look at it rather than simply seeing a
	// shorter chain.
	w.forkbench(t, "reorg", "-node", "sq", "-blocks", "2")

	waitFor(t, "the spend to be marked as gone", func() bool {
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Status == "reorged_out" {
				return true
			}
		}
		return false
	})

	// Marked, not deleted: it happened, and the record of it is the audit trail.
	if after := len(spends(t)); after != before {
		t.Errorf("the record was destroyed rather than marked: %d became %d\n%s",
			before, after, w.describe(t))
	}

	// Told about, and not as good news.
	waitFor(t, "a warning that it has gone", func() bool {
		a, ok := alertOfKind(t, "spend_disappeared")
		return ok && a.Tier != "resolved"
	})

	// And the countdown is still running. Dropping it at the first sign of a
	// reorganisation would be dropping it precisely when it mattered.
	if running := countdowns(t, "counting"); len(running) == 0 {
		t.Errorf("the countdown was abandoned the moment the spend left the chain\n%s",
			w.describe(t))
	}
}
