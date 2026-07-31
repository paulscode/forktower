//go:build integration

package scenarios

import (
	"testing"
)

// A transaction copied to the other chain, and then that chain changing its
// mind.
//
// The mirror puts a transaction into the other chain's memory pool and records
// that it was accepted. If that chain then reorganises, the transaction is not
// necessarily gone — an unconfirmed transaction survives a reorg and is usually
// mined again — but it may be, and the honest position is that "accepted" was
// about the moment it was offered rather than a promise about for ever.
//
// What must not happen is the record turning into a claim the chain no longer
// supports, with nothing said. This checks the state the daemon reports still
// matches what the chain actually has.
func TestAMirroredTransactionSurvivesTheOtherChainChangingItsMind(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	w.forkbench(t, "coop-close")

	waitFor(t, "the close to reach the other chain", func() bool {
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
	// It is really there before anything is disturbed.
	w.forkbench(t, "tx-present", "-node", "sq", "-txid", copied.TxID)

	// The other chain replaces its tip.
	w.forkbench(t, "reorg", "-node", "sq", "-blocks", "3")

	// The daemon keeps working, and does not start claiming something the chain
	// contradicts. Either the transaction is still there — which is the usual
	// outcome, because an unconfirmed transaction survives a reorganisation — or
	// the record no longer says it was accepted.
	waitFor(t, "the daemon to settle after the reorganisation", func() bool {
		return len(mirrorDecisions(t, "")) > 0
	})

	after := mirrorDecisions(t, "")
	if len(after) != 1 {
		t.Fatalf("the reorganisation turned one decision into %d: %+v", len(after), after)
	}
	if after[0].TxID != copied.TxID {
		t.Errorf("the decision is now about a different transaction: %q became %q",
			copied.TxID, after[0].TxID)
	}

	if after[0].State == "accepted" {
		// The daemon still says it got there, so it had better be there.
		w.forkbench(t, "tx-present", "-node", "sq", "-txid", copied.TxID)
		return
	}
	// Or it says otherwise, and then it must say why rather than going quiet.
	if after[0].Reason == "" {
		t.Errorf("the decision changed after the reorganisation with no reason: %+v", after[0])
	}
}

// A reorganisation on the chain the mirror *reads* must not lose the record of
// what it decided.
//
// The transactions the mirror copies are the user's own closes, on the chain
// their node follows. A reorganisation there is ordinary, and the decision about
// a transaction that is still perfectly valid must survive it — otherwise the
// close stops being copied and nothing says so.
func TestAReorganisationOnTheUsersOwnChainKeepsTheDecision(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	// The counterparty's close, which is refused — the record most worth keeping,
	// because it is the answer to "why was that not copied?".
	w.forkbench(t, "force-close", "-ln-node", "mallory")

	waitFor(t, "the refusal to be recorded", func() bool {
		return len(mirrorDecisions(t, "denied")) > 0
	})
	before := mirrorDecisions(t, "denied")[0]

	w.forkbench(t, "reorg", "-node", "sf", "-blocks", "2")

	waitFor(t, "the daemon to settle", func() bool {
		return len(mirrorDecisions(t, "")) > 0
	})

	after := mirrorDecisions(t, "denied")
	if len(after) != 1 {
		t.Fatalf("the refusal did not survive the reorganisation: %+v", mirrorDecisions(t, ""))
	}
	if after[0].Reason != before.Reason {
		t.Errorf("the reason for refusing changed: %q became %q",
			before.Reason, after[0].Reason)
	}
}
