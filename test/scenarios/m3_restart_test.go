//go:build integration

package scenarios

import (
	"path/filepath"
	"strings"
	"testing"
)

// A restart while a transaction is on its way to the other chain.
//
// Package upgrades happen; machines reboot. What must not happen is that a
// transaction the user needs on the other chain quietly stops being sent —
// which is what would follow from the decision, or the attempt count, living
// only in memory. The record would still say "waiting", and nobody would ever
// be told it had stopped.
func TestARestartDoesNotLoseATransactionOnItsWay(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	w.forkbench(t, "coop-close")
	waitFor(t, "the close to be decided about", func() bool {
		return len(mirrorDecisions(t, "")) > 0
	})

	before := mirrorDecisions(t, "")
	if len(before) != 1 {
		t.Fatalf("got %d decisions, want the one about the close", len(before))
	}

	w.restartDaemon(t)

	waitFor(t, "the decision to come back", func() bool {
		return len(mirrorDecisions(t, "")) > 0
	})
	after := mirrorDecisions(t, "")

	// One row, not two. A restart that re-read the same block and decided again
	// must produce the same record rather than a second one — otherwise the page
	// fills with duplicates of the same transaction and the counts stop meaning
	// anything.
	if len(after) != len(before) {
		t.Errorf("a restart turned %d decisions into %d", len(before), len(after))
	}
	if after[0].TxID != before[0].TxID {
		t.Errorf("the decision is about a different transaction: %q became %q",
			before[0].TxID, after[0].TxID)
	}
	// And the reason survives, because it is the record of why.
	if after[0].Reason != before[0].Reason {
		t.Errorf("the reason changed across a restart: %q became %q",
			before[0].Reason, after[0].Reason)
	}

	// It still gets there. A restart mid-flight must not leave a transaction
	// permanently waiting.
	waitFor(t, "the close to reach the other chain", func() bool {
		for _, d := range mirrorDecisions(t, "") {
			if d.State == "accepted" {
				return true
			}
		}
		return false
	})
	for _, d := range mirrorDecisions(t, "") {
		if d.State == "accepted" {
			w.forkbench(t, "tx-present", "-node", "sq", "-txid", d.TxID)
		}
	}
}

// A restart with a watchtower in the picture.
//
// The tower's own record — what it protects, and since when — lives in the
// daemon's database rather than in the tower, so a restart that lost it would
// leave a user looking at a page that had forgotten their protection existed.
// It would also restart the grace period, which is the window where a missing
// session is excused rather than reported.
func TestARestartKeepsWhatTheTowerProtects(t *testing.T) {
	w := freshWorld(t)
	creds := filepath.Join("deploy", "forkbench", "creds")

	w.forkbench(t, "tower-up")
	w.forkbench(t, "ln-credentials", "-ln-node", "tower", "-out", creds)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "tower-backups", "-min", "3")
	w.forkbench(t, "split")
	w.startDaemonWith(t, watchingTheTower(creds))

	// Waited for coverage rather than for the tower, because coverage is what is
	// being asserted about — and it cannot exist until the registry's first poll
	// has told the daemon what channels there are, which is up to a minute after
	// the tower itself is recorded.
	waitFor(t, "what the tower protects to be worked out", func() bool {
		rows := towers(t)
		return len(rows) > 0 && len(rows[0].Coverage) > 0
	})
	before := towers(t)[0]

	w.restartDaemon(t)

	waitFor(t, "the tower to come back", func() bool {
		return len(towers(t)) > 0 && len(towers(t)[0].Coverage) > 0
	})
	after := towers(t)[0]

	if after.ID != before.ID {
		t.Errorf("the tower was recorded again as a new one: %d became %d",
			before.ID, after.ID)
	}
	if len(after.Coverage) != len(before.Coverage) {
		t.Errorf("a restart turned %d channels' coverage into %d",
			len(before.Coverage), len(after.Coverage))
	}
	if after.Display.State != before.Display.State {
		t.Errorf("the tower's state changed across a restart: %q became %q",
			before.Display.State, after.Display.State)
	}
}

// **The Details view is about the other chain, and the mirror reads this one.**
//
// The mirror observer records the user's own chain's transactions so their bytes
// are there to send later. Those rows share a table with the ones the detection
// engine writes about the other chain — which is correct, and means an
// unfiltered listing now carries both. Every row says which chain it is about,
// and the exposure table asks for one chain explicitly, so nothing reads
// wrongly. This pins that.
func TestEverySpendSaysWhichChainItIsAbout(t *testing.T) {
	w := freshWorld(t)
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.startDaemon(t)

	// A close on the user's own chain, which the mirror records; and one on the
	// other, which the detection engine does.
	w.forkbench(t, "coop-close")

	waitFor(t, "the close to be recorded", func() bool { return len(spends(t)) > 0 })

	var ownChain, otherChain int
	for _, sp := range spends(t) {
		switch sp.Branch {
		case "sf":
			ownChain++
		case "sq":
			otherChain++
		default:
			t.Errorf("a spend was recorded against no chain at all: %+v", sp)
		}
	}
	if ownChain == 0 {
		t.Errorf("the mirror recorded nothing on the user's own chain\n%s", w.describe(t))
	}

	// The exposure table asks for one chain, so a close on the user's own chain
	// must not appear there as something happening on the other one.
	for _, c := range channels(t) {
		if strings.Contains(strings.ToLower(c.Display.Status), "other chain") &&
			otherChain == 0 {
			t.Errorf("the exposure table reports activity on the other chain when "+
				"there has been none: %q", c.Display.Status)
		}
	}
}
