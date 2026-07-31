//go:build integration

package scenarios

import (
	"strings"
	"testing"
)

// **The refusal, first, because it is the one that can lose money.**
//
// The counterparty force-closes on the chain the user's own node follows. That
// transaction is valid on the other chain too — the funding output is there,
// because the channel was opened before the chains parted — so a mirror that
// simply rebroadcast what it saw would put it there.
//
// Doing so would create exposure on the other chain that did not exist a moment
// earlier: their commitment would now be spendable there, and the user's channel
// would be at risk on both chains instead of one. Nothing else in the system
// would notice, because from every other angle the mirror would look like it was
// working.
func TestMirrorRefusesTheCounterpartysCloseAndSaysWhy(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	// Their close, on the user's own chain.
	w.forkbench(t, "force-close", "-ln-node", "mallory")

	waitFor(t, "the mirror to decide about their close", func() bool {
		return len(mirrorDecisions(t, "")) > 0
	})

	decisions := mirrorDecisions(t, "")
	if len(decisions) != 1 {
		t.Fatalf("got %d decisions, want the one about their close: %+v", len(decisions), decisions)
	}
	d := decisions[0]

	if d.State != "denied" {
		t.Fatalf("their commitment was not refused: state=%q reason=%q", d.State, d.Reason)
	}
	// **Asserted on the reason, not on absence.** A mirror that refused
	// everything for the wrong reason, or that had simply not got round to it,
	// would pass a test that only checked nothing was copied.
	if !strings.Contains(d.Reason, "at risk there when it is not at risk now") {
		t.Errorf("the refusal does not name the harm it is preventing: %q", d.Reason)
	}
	if !d.Display.Refused {
		t.Errorf("the refusal does not read as a decision on the page: %+v", d.Display)
	}

	// And the bytes are genuinely not on the other chain.
	w.forkbenchFails(t, "the counterparty's commitment reached the other chain",
		"tx-present", "-node", "sq", "-txid", d.TxID)
}

// S6: a close both parties agreed to belongs on both chains.
//
// Left on one only, the channel is settled where the user's node is looking and
// still open where it is not — which is the exposure this rule closes.
func TestMirrorCopiesAnAgreedCloseToTheOtherChain(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	w.forkbench(t, "coop-close")

	waitFor(t, "the agreed close to be copied", func() bool {
		for _, d := range mirrorDecisions(t, "") {
			if d.State == "accepted" {
				return true
			}
		}
		return false
	})

	var copied mirrorDecision
	for _, d := range mirrorDecisions(t, "") {
		if d.State == "accepted" {
			copied = d
		}
	}
	if !strings.Contains(copied.Reason, "agreed") {
		t.Errorf("the reason for copying it is not the agreement: %q", copied.Reason)
	}
	if copied.To != "sq" {
		t.Errorf("copied to %q, want the chain the user's node does not follow", copied.To)
	}

	// The assertion that matters: the transaction really is over there. A record
	// saying "accepted" proves what Forktower believed, not what happened.
	w.forkbench(t, "tx-present", "-node", "sq", "-txid", copied.TxID)
}

// S5: the user's own force-close belongs on both chains too.
//
// Their money is being claimed back on one chain; leaving the other untouched
// means a channel that is closed where they are looking and open where they are
// not, with an old commitment still spendable there.
func TestMirrorCopiesTheUsersOwnCloseToTheOtherChain(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	w.forkbench(t, "force-close", "-ln-node", "user")

	waitFor(t, "the user's own close to be copied", func() bool {
		for _, d := range mirrorDecisions(t, "") {
			if d.State == "accepted" {
				return true
			}
		}
		return false
	})

	var copied mirrorDecision
	for _, d := range mirrorDecisions(t, "") {
		if d.State == "accepted" {
			copied = d
		}
	}
	if !strings.Contains(strings.ToLower(copied.Reason), "yourself") {
		t.Errorf("the reason does not say it was the user's own close: %q", copied.Reason)
	}
	w.forkbench(t, "tx-present", "-node", "sq", "-txid", copied.TxID)
}

// Everything the mirror decided is on the page, refusals included, each with the
// sentence explaining it.
//
// A view that showed only what was copied would make the feature look like it
// barely did anything, and would leave the question it exists to answer — "why
// was that not copied?" — with nowhere to go.
func TestTheMirrorsRefusalsReachTheDashboardWithTheirReasons(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)
	w.forkbench(t, "force-close", "-ln-node", "mallory")

	waitFor(t, "the refusal to reach the dashboard", func() bool {
		return len(mirrorDecisions(t, "denied")) > 0
	})

	for _, d := range mirrorDecisions(t, "denied") {
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("a refusal reached the page with no reason: %+v", d)
		}
		if strings.TrimSpace(d.Display.What) == "" {
			t.Errorf("a refusal reached the page with nothing to read: %+v", d)
		}
		// No internal vocabulary, the same rule the rest of the dashboard follows.
		for _, jargon := range []string{" sf ", " sq ", "commitment_unknown", "outpoint"} {
			if strings.Contains(strings.ToLower(d.Display.What), jargon) {
				t.Errorf("the refusal uses %q: %q", jargon, d.Display.What)
			}
		}
	}
}
