//go:build integration

package scenarios

import (
	"strings"
	"testing"
)

// S4: the counterparty's *current* commitment confirms on the other chain.
//
// Nobody has done anything wrong. No penalty exists, because the state was never
// revoked — and from the chain there is no way to tell this apart from S1, which
// is the whole reason Forktower says "somebody's commitment" rather than naming
// whose.
//
// The point of this scenario is that Forktower behaves *identically*. Anything
// else would mean it was claiming a distinction it cannot make; and being quieter
// here would mean being quiet about a real breach that happened to look like
// this one.
func TestS4TheLatestCommitmentIsTreatedTheSame(t *testing.T) {
	w := freshWorld(t)
	w.startDaemon(t)
	w.staged(t)

	// The counterparty publishes what it currently holds, with no rollback.
	w.forkbench(t, "breach", "-branch", "sq", "-revoked=false")

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
	// The same answer as S1, and that is correct. Claiming to know it was
	// legitimate would be claiming evidence that does not exist.
	if recorded.Shape != "commitment_unknown" {
		t.Errorf("an honest close was classified as %q, which claims a distinction "+
			"the chain does not carry\n%s", recorded.Shape, w.describe(t))
	}

	// Alerted, and counted, exactly as before. There is still money to move here
	// — payments in flight time out, and the user's own output has to be claimed.
	waitFor(t, "an alert", func() bool {
		a, ok := alertOfKind(t, "channel_spent")
		return ok && a.Tier == "critical"
	})
	waitFor(t, "a countdown", func() bool { return len(countdowns(t, "counting")) > 0 })

	// And nothing anywhere calls it theft.
	for _, a := range alerts(t) {
		text := strings.ToLower(a.Subject + " " + a.Message)
		for _, word := range []string{"revoked", "stole", "stealing", "theft", "attack"} {
			if strings.Contains(text, word) {
				t.Errorf("an honest close was described as %q: %q", word, a.Message)
			}
		}
	}
}
