//go:build integration

package scenarios

import (
	"path/filepath"
	"strings"
	"testing"
)

// watchingTheTower is the configuration a scenario needs for Forktower to see
// the companion watchtower at all.
//
// Its credentials are copied out of the container by the caller, the same way
// the Lightning node's are: the daemon runs on the host here, and a macaroon
// inside a container is one it cannot read.
func watchingTheTower(credsDir string) []string {
	return []string{
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_API_URL=https://127.0.0.1:8083",
		"FORKTOWER_TOWER_LND_LISTEN=tower:9911",
		"FORKTOWER_TOWER_LND_MACAROON_PATH=" +
			filepath.Join(credsDir, "tower", "readonly.macaroon"),
		"FORKTOWER_TOWER_LND_TLS_CERT_PATH=" +
			filepath.Join(credsDir, "tower", "tls.cert"),
	}
}

// S1 with the answer in place: a breach on the chain nobody is watching, and a
// watchtower that punishes it.
//
// The detection scenario next door proves the user is *told*. This proves
// something is *done* — that the whole arm works end to end, on real software,
// with the money actually moving back.
//
// The ordering here is load-bearing and is the thing most likely to be got wrong
// by hand: a tower only holds the states that were revoked *after* the node
// registered with it. Register after the payments and you get a tower that is
// running, healthy, reporting no errors, and holding nothing.
func TestS1ABreachOnTheOtherChainIsPunishedByTheTower(t *testing.T) {
	w := freshWorld(t)

	// Register first, then pay. The other order produces a tower that protects
	// nothing, and looks fine while doing it.
	w.forkbench(t, "tower-up")
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "tower-backups", "-min", "3")

	// The state the counterparty will roll back to, and then the payments that
	// revoke it. Both are backed up before the split.
	w.forkbench(t, "snapshot-mallory")
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "tower-backups", "-min", "6")

	w.forkbench(t, "split")
	w.forkbench(t, "ln-credentials", "-ln-node", "tower",
		"-out", filepath.Join("deploy", "forkbench", "creds"))
	w.startDaemonWith(t, watchingTheTower(filepath.Join("deploy", "forkbench", "creds")))

	// The attack: the revoked commitment, on the chain the user's own node does
	// not follow.
	w.forkbench(t, "breach", "-branch", "sq")

	// What the tower is for. Justice appears on that chain without anybody
	// touching the user's node — which is the whole point, because in the real
	// situation they are asleep.
	//
	// Blocks keep coming while we wait, because they would: the tower publishes
	// its justice transaction into the memory pool within a second of seeing the
	// breach, and it confirms when the rest of the network next mines. Without
	// this the scenario would wait for a chain that had stopped, which is a
	// different situation with a different name.
	w.mineUntil(t, "the tower to punish the breach on the other chain", func() bool {
		return justiceOnOtherChain(t)
	})

	// And Forktower saw it happen, rather than only the breach.
	waitFor(t, "the punishment to reach the dashboard", func() bool {
		for _, sp := range spends(t) {
			if sp.Branch == "sq" && sp.Shape == "justice" {
				return true
			}
		}
		return false
	})

	// The countdown that was running against the user must end, and end as
	// resolved rather than as a loss: the money came back.
	waitFor(t, "the countdown to be answered", func() bool {
		for _, d := range countdowns(t, "resolved") {
			if d.Kind == "csv" {
				return true
			}
		}
		return false
	})

	for _, d := range countdowns(t, "expired") {
		if d.Kind == "csv" {
			t.Errorf("a countdown expired as a loss even though the tower answered it: %+v", d)
		}
	}
}

// justiceOnOtherChain reports whether a justice transaction has confirmed there.
//
// Read from the daemon's own record rather than from the chain, deliberately: a
// test that went to bitcoind directly would prove the tower worked and prove
// nothing about whether Forktower noticed — and noticing is the product.
func justiceOnOtherChain(t *testing.T) bool {
	t.Helper()
	for _, sp := range spends(t) {
		if sp.Branch == "sq" && sp.Shape == "justice" && sp.Status == "confirmed" {
			return true
		}
	}
	return false
}

// **The negative case, and the one that matters more.** A watchtower that is not
// there is indistinguishable from a working one unless somebody is checking, and
// the word the user needs is that they are not protected.
func TestS1AMissingTowerIsReportedAsUnprotected(t *testing.T) {
	w := freshWorld(t)

	w.forkbench(t, "tower-up")
	// Copied while it is still up: a stopped container will not hand them over,
	// and the daemon needs them to be able to say the tower is missing.
	w.forkbench(t, "ln-credentials", "-ln-node", "tower",
		"-out", filepath.Join("deploy", "forkbench", "creds"))
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "tower-backups", "-min", "3")

	// And then it goes away, which is how this fails in life: not by never being
	// set up, but by stopping quietly some time after it was.
	w.forkbench(t, "tower-stop")

	w.forkbench(t, "split")
	w.startDaemonWith(t, watchingTheTower(filepath.Join("deploy", "forkbench", "creds")))

	// The alert is what matters here, and it is the thing that reaches somebody
	// who is not looking at a dashboard — which is the person this is for.
	//
	// Note what is *not* asserted: a card. A tower that has never answered since
	// the daemon started has told us no identity, and the stored row is keyed by
	// that identity, so there is nothing to file and nothing to render. The alert
	// path does not need a row, which is why it is the one checked.
	// Asked for by kind rather than by prefix. Several things about towers are
	// worth saying at once — that the node is using one Forktower does not run,
	// for instance — and a test that took whichever arrived last would be
	// asserting about whichever of them happened to be newest.
	waitFor(t, "an alert saying the tower is not answering", func() bool {
		_, found := alertOfKind(t, "tower_down")
		return found
	})

	raised, _ := alertOfKind(t, "tower_down")
	if !strings.Contains(strings.ToLower(raised.Message), "would not be answered") {
		t.Errorf("the alert does not say what is lost while the tower is down: %q",
			raised.Message)
	}
}

// A tower that is up and holding nothing is the failure with no other symptom:
// every check passes, and the channels are unprotected.
func TestS1ATowerNobodyRegisteredWithIsReportedAsProtectingNothing(t *testing.T) {
	w := freshWorld(t)

	// The tower is started, and the node is *not* pointed at it. Nothing about
	// the tower itself is wrong.
	w.forkbench(t, "tower-up", "-register=false")
	w.forkbench(t, "pay", "-times", "3")
	w.forkbench(t, "split")
	w.forkbench(t, "ln-credentials", "-ln-node", "tower",
		"-out", filepath.Join("deploy", "forkbench", "creds"))
	w.startDaemonWith(t, watchingTheTower(filepath.Join("deploy", "forkbench", "creds")))

	waitFor(t, "the tower to be reported as protecting nothing", func() bool {
		for _, tw := range towers(t) {
			if tw.Display.State != "protecting" && tw.Display.State != "unknown" {
				return true
			}
		}
		return false
	})
}
