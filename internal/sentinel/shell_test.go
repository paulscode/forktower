package sentinel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
)

var errNode = errors.New("the node is not answering")

// Every startup check exists because its failure is otherwise invisible: the
// daemon would report green indicators while watching nothing, or the wrong thing.
// So each one is asserted to actually stop startup.
func TestPreflightRefusesAViewOnTheWrongNetwork(t *testing.T) {
	t.Parallel()

	for _, which := range []string{"the node Forktower reads from", "the second view"} {
		t.Run(which, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			// A genesis nobody's chain starts with, which is what a backend pointed at
			// a test network looks like from here.
			h.sen.cfg.Network.Genesis = chainviewtest.TaggedHash("elsewhere", 0)
			if which == "the second view" {
				// Leave the first passing so the failure can only come from the second.
				genesis, err := chainview.GenesisOf(context.Background(), h.sf)
				if err != nil {
					t.Fatal(err)
				}
				h.sen.cfg.Network.Genesis = genesis
				h.sq.Fail("BlockHashByHeight", errNode)
			}

			err := h.sen.Preflight(context.Background())
			if err == nil {
				t.Fatal("Preflight accepted a view it could not confirm was on the right network")
			}
			if h.sen.Checks().SameNetwork {
				t.Error("the network check was recorded as passed")
			}
		})
	}
}

// One node behind both configurations produces views that agree by construction,
// so a split becomes unrepresentable. A single mis-wired setting does it.
func TestPreflightRefusesTwoViewsOfOneNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	same := chainview.Identity{Endpoint: "http://node:8332", LocalAddresses: []string{"node:8333"}}
	h.sf.SetIdentity(same)
	h.sq.SetIdentity(same)

	err := h.sen.Preflight(context.Background())
	if !errors.Is(err, chainview.ErrSameNode) {
		t.Fatalf("got %v, want ErrSameNode", err)
	}
	if !strings.Contains(err.Error(), "could never see a split") {
		t.Errorf("the error does not explain the consequence: %v", err)
	}
}

// A check that could not be run is reported as such. Recording it as passed would
// be the same class of false assurance the check exists to prevent.
func TestPreflightReportsACheckItCouldNotRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.sq.Fail("Identity", errNode)

	if err := h.sen.Preflight(context.Background()); err != nil {
		t.Fatalf("an unavailable check stopped startup: %v", err)
	}
	checks := h.sen.Checks()
	if !checks.SameNetwork {
		t.Error("the network check should still have passed")
	}
	if checks.DistinctNodes || checks.DistinctVerified {
		t.Error("an unrunnable check was recorded as verified")
	}
	if checks.Detail == "" {
		t.Error("nothing was recorded to explain why the check is missing")
	}
}

func TestPreflightPasses(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	if err := h.sen.Preflight(context.Background()); err != nil {
		t.Fatalf("a correct configuration was rejected: %v", err)
	}
	checks := h.sen.Checks()
	if !checks.SameNetwork || !checks.DistinctNodes || !checks.DistinctVerified {
		t.Errorf("passing checks were not all recorded: %+v", checks)
	}
}

// The divergence height bounds how far the user's own chain is trusted, so where
// it comes from matters more than most settings.
func TestDeriveForkDescriptor(t *testing.T) {
	t.Parallel()

	const deployment = "testfork"

	t.Run("read from the node when nothing is configured", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *Config) { c.DeploymentName = deployment })
		h.sf.SetDeployment(deployment, chainview.Deployment{
			Name: deployment, Status: "locked_in", MaxActivationHeight: 1000, Period: 100,
		})

		if err := h.sen.deriveForkDescriptor(context.Background()); err != nil {
			t.Fatalf("reading the fork's parameters failed: %v", err)
		}
		// Two signalling periods before the rules can bind: the chains separate when
		// blocks start being rejected, not when the new rules apply to transactions.
		if want := int32(800); h.sen.cfg.DivergenceHeight != want {
			t.Errorf("derived %d, want %d", h.sen.cfg.DivergenceHeight, want)
		}
	})

	t.Run("a configured value wins over the node", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *Config) {
			c.DeploymentName = deployment
			c.DivergenceHeight = 777
		})
		h.sf.SetDeployment(deployment, chainview.Deployment{
			Name: deployment, Status: "locked_in", MaxActivationHeight: 1000, Period: 100,
		})

		if err := h.sen.deriveForkDescriptor(context.Background()); err != nil {
			t.Fatal(err)
		}
		if h.sen.cfg.DivergenceHeight != 777 {
			t.Errorf("the configured height was overwritten with %d", h.sen.cfg.DivergenceHeight)
		}
	})

	t.Run("nothing to derive without a deployment name", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		if err := h.sen.deriveForkDescriptor(context.Background()); err != nil {
			t.Errorf("got %v, want no error when there is no deployment to read", err)
		}
	})

	t.Run("a node that does not know the deployment", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *Config) { c.DeploymentName = deployment })

		if err := h.sen.deriveForkDescriptor(context.Background()); err == nil {
			t.Error("an unknown deployment was reported as read successfully")
		}
		// Not fatal to startup: the caller widens its margins instead. Asserted here
		// so that changing it to fatal is a deliberate act.
		if h.sen.cfg.DivergenceHeight != 0 {
			t.Errorf("a height was invented from a failed read: %d", h.sen.cfg.DivergenceHeight)
		}
	})

	t.Run("a deployment with no usable height", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *Config) { c.DeploymentName = deployment })
		h.sf.SetDeployment(deployment, chainview.Deployment{
			Name: deployment, Status: "defined",
		})

		if err := h.sen.deriveForkDescriptor(context.Background()); err == nil {
			t.Error("a deployment with no usable height was accepted")
		}
	})
}

// Restoring a corrupt record must fail loudly. Both fields here decide what is
// watched and how far back history is trusted, so a quietly wrong value is worse
// than refusing to start.
func TestLoadRejectsARecordItCannotTrust(t *testing.T) {
	t.Parallel()

	t.Run("an unreadable separation point", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		if err := h.store.SaveSplitState(ctx, store.Split{
			State: store.StateSplit, ForkHash: "not a hash", ForkHeight: 100,
		}); err != nil {
			t.Fatal(err)
		}

		err := h.sen.Load(ctx)
		if err == nil || !strings.Contains(err.Error(), "unreadable") {
			t.Fatalf("got %v, want a complaint about the recorded separation point", err)
		}
	})

	t.Run("a trust anchor that is not a block height", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		// Larger than any height can be. Truncating this would move the anchor
		// somewhere arbitrary and silently widen what is treated as shared history.
		if err := h.store.SetMetaInt64(ctx, store.MetaTrustAnchorHeight, 1<<40); err != nil {
			t.Fatal(err)
		}

		err := h.sen.Load(ctx)
		if err == nil || !strings.Contains(err.Error(), "not a possible block height") {
			t.Fatalf("got %v, want a complaint about the recorded trust anchor", err)
		}
	})
}

// A view that cannot be read is not a view that disagrees. Treating a transport
// failure as a chain observation would manufacture splits out of downtime.
func TestObserveTreatsAnUnreadableViewAsDownRatherThanDisagreeing(t *testing.T) {
	t.Parallel()

	t.Run("health cannot be read", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.sq.Fail("Health", errNode)

		obs := h.sen.observe(context.Background())
		if obs.SQHealth != chainview.HealthDown {
			t.Errorf("got %q, want the view reported as down", obs.SQHealth)
		}
		if obs.SQTip != nil {
			t.Error("a tip was reported for a view that could not be read")
		}
	})

	t.Run("the tip cannot be read", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.sq.Fail("BestBlock", errNode)

		obs := h.sen.observe(context.Background())
		if obs.SQHealth != chainview.HealthDown || obs.SQTip != nil {
			t.Errorf("got health %q tip %v, want down with no tip", obs.SQHealth, obs.SQTip)
		}
	})

	t.Run("the view is still syncing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.sq.SetHealth(chainview.BackendHealth{State: chainview.HealthSyncing, SyncProgress: 0.4})

		obs := h.sen.observe(context.Background())
		if obs.SQHealth != chainview.HealthSyncing {
			t.Errorf("got %q, want the syncing state preserved", obs.SQHealth)
		}
		// A syncing view's tip is behind for reasons that have nothing to do with a
		// split; comparing against it would read as divergence.
		if obs.SQTip != nil {
			t.Error("a syncing view's tip was used for comparison")
		}
	})
}

// The failure this pause exists for: a second view that has quietly adopted the
// same chain as the user's own node. Continuing to scan would produce a clean
// report about a chain nobody needs watched.
func TestWatchingPausesWhenTheSecondViewIsOnTheWrongChain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	ctx := context.Background()

	// Both views on the same chain above the point where they were meant to differ.
	h.sf.Extend("ours", 5)
	h.sq.Extend("ours", 5)

	st := State{
		TrustAnchor: sharedHistory - 1,
		Fork: &chainview.BlockRef{
			Hash: chainviewtest.TaggedHash("shared", sharedHistory), Height: sharedHistory,
		},
	}
	h.sen.maybeReverifyBranch(ctx, st, []Effect{{Kind: EffectPhaseChanged, NewPhase: PhaseSplit}})

	if !h.sen.Paused() {
		t.Fatal("watching continued against a view on the wrong chain")
	}
	if checks := h.sen.Checks(); checks.OnExpectedBranch || checks.Detail == "" {
		t.Errorf("the pause was not explained in the checks: %+v", checks)
	}

	// And it resumes by itself once the view is where it should be, so a transient
	// fault does not need an operator to clear it.
	h.sq.Reorg(sharedHistory, "theirs", 5)
	h.sen.blocksSinceVerify = BranchReverifyBlocks
	h.sen.maybeReverifyBranch(ctx, st, nil)

	if h.sen.Paused() {
		t.Error("watching did not resume after the view returned to its own chain")
	}
	if !h.sen.Checks().OnExpectedBranch {
		t.Error("the branch check was not recorded as passing again")
	}
}

// Not enough history is not a fault. During the first blocks of a split there is
// nothing above the separation to compare, and pausing then would disable the
// daemon exactly when it is needed.
func TestNotEnoughHistoryDoesNotPauseWatching(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	st := State{
		TrustAnchor: sharedHistory - 1,
		Fork: &chainview.BlockRef{
			Hash: chainviewtest.TaggedHash("shared", sharedHistory), Height: sharedHistory,
		},
	}
	h.sen.maybeReverifyBranch(context.Background(), st,
		[]Effect{{Kind: EffectPhaseChanged, NewPhase: PhaseSplit}})

	if h.sen.Paused() {
		t.Error("watching paused merely because the split was too young to check")
	}
}
