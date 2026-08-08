package sentinel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
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

// The regression this file exists to prevent recurring.
//
// The check used to run only when the phase moved to SPLIT or when a hundred new
// blocks had arrived on the watched chain, counted from zero in memory at every
// start. A hundred blocks is most of a day, so a freshly started daemon — which,
// on a platform that restarts its apps, is the usual condition — had no answer to
// give, and the dashboard said so in words that blamed the chains for agreeing
// while they were in fact holding different blocks at the same height.
func TestTheBranchCheckDoesNotWaitAHundredBlocksForItsFirstAnswer(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	st := State{TrustAnchor: sharedHistory - 1}

	if h.sen.blocksSinceVerify != 0 {
		t.Fatalf("the block counter starts at %d, so this proves nothing", h.sen.blocksSinceVerify)
	}
	// No split, no blocks, no effects: exactly the state of a daemon that has just
	// started next to two agreeing nodes.
	h.sen.maybeReverifyBranch(context.Background(), st, nil)

	checks := h.sen.Checks()
	if !checks.OnExpectedBranch {
		t.Error("the branch check had produced no answer on the first tick")
	}
	if checks.BranchCheckedAt == 0 {
		t.Error("nothing recorded that the check had run, so the dashboard reads it as unchecked")
	}
	if checks.BranchVerifiedAt == 0 {
		t.Error("a passing check did not record when it passed")
	}
}

// A verdict survives a restart, because a restart is not evidence about the chain.
//
// The verification time was being written to storage and read back nowhere, so
// every restart dropped the result and the dashboard fell back to "not yet
// checked" — permanently, on any deployment restarting more often than the
// re-check cadence came round.
func TestABranchVerificationSurvivesARestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)
	h.sen.maybeReverifyBranch(ctx, State{TrustAnchor: sharedHistory - 1}, nil)

	verifiedAt := h.sen.Checks().BranchVerifiedAt
	if verifiedAt == 0 {
		t.Fatal("nothing was verified, so there is nothing to restore")
	}

	// A second sentinel over the same storage is what a restart looks like.
	restarted := newSentinelOver(t, h)
	if err := restarted.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	checks := restarted.Checks()
	if checks.BranchVerifiedAt != verifiedAt {
		t.Errorf("BranchVerifiedAt = %d, want the recorded %d", checks.BranchVerifiedAt, verifiedAt)
	}
	if !checks.OnExpectedBranch || checks.BranchCheckedAt == 0 {
		t.Errorf("a restart dropped a verification it had recorded: %+v", checks)
	}
}

// Checked-and-wrong must be distinguishable from never-checked, and must keep
// being retried.
//
// Both halves were broken together, and the pair is what made it dangerous: the
// failing verdict stamped no time at all, so the dashboard reported a daemon that
// had stopped watching as having nothing to report — and the only thing that could
// lift the pause was the hundred-block cadence, leaving it stopped for most of a
// day after a fault that had already cleared.
func TestAWrongChainVerdictIsRecordedAndRetriedOnTheClock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)

	// Both views on the same chain above where they were meant to differ.
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
	checks := h.sen.Checks()
	if checks.BranchCheckedAt == 0 {
		t.Error("a verdict was reached and not stamped, so it reads as never checked")
	}
	if checks.BranchVerifiedAt != 0 {
		t.Error("a failing verdict was recorded as a verification")
	}

	// The fault clears, and nothing else happens: no new blocks, no phase change.
	h.sq.Reorg(sharedHistory, "theirs", 5)
	h.sen.blocksSinceVerify = 0
	h.clock.Add(int64(BranchRetryInterval.Seconds()))
	h.sen.maybeReverifyBranch(ctx, st, nil)

	if h.sen.Paused() {
		t.Error("watching stayed paused after the view returned to its own chain")
	}
	if !h.sen.Checks().OnExpectedBranch {
		t.Error("the branch check was not recorded as passing again")
	}
}

// Retrying is paced, not run on every tick. The check is cheap but not free, and
// a daemon next to a still-syncing node would otherwise hammer both nodes for as
// long as the sync took.
func TestAnUnresolvedBranchCheckIsRetriedOnACadenceRatherThanEveryTick(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)
	// A view with nothing above genesis to compare leaves the check unresolved.
	h.sq.Fail("BlockHashByHeight", chainview.ErrNotFound)
	st := State{TrustAnchor: sharedHistory - 1}

	h.sen.maybeReverifyBranch(ctx, st, nil)
	first := h.sen.lastBranchAttempt
	if first.IsZero() {
		t.Fatal("the first attempt did not run")
	}

	h.clock.Add(int64(BranchRetryInterval.Seconds()) - 1)
	h.sen.maybeReverifyBranch(ctx, st, nil)
	if !h.sen.lastBranchAttempt.Equal(first) {
		t.Error("the check ran again before the retry interval had elapsed")
	}

	h.clock.Add(1)
	h.sen.maybeReverifyBranch(ctx, st, nil)
	if h.sen.lastBranchAttempt.Equal(first) {
		t.Error("the check did not run again once the retry interval had elapsed")
	}
}

// A node that will not answer has not disagreed about anything.
//
// VerifyBranch keeps a transport fault distinct from a disagreement; the shell
// used to discard that distinction and pause on both, announcing a busy node as
// following the wrong chain. Since the check now runs within the first minutes of
// a start — when the second node is most likely to still be opening its RPC
// socket — that would turn every slow start into an alarm.
func TestABackendThatWillNotAnswerDoesNotPauseWatching(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.sq.Fail("BlockHashByHeight", errNode)

	h.sen.maybeReverifyBranch(context.Background(), State{TrustAnchor: sharedHistory - 1}, nil)

	if h.sen.Paused() {
		t.Error("watching paused because a node was not answering")
	}
	checks := h.sen.Checks()
	if checks.BranchCheckedAt != 0 {
		t.Error("a fault was recorded as a verdict about the chain")
	}
	if strings.Contains(checks.Detail, "not on the chain it should be") {
		t.Errorf("a fault was described as the wrong chain: %q", checks.Detail)
	}
}

// The disagreement clock survives a restart, because a split can be confirmed by
// how long the chains have refused to reconcile.
//
// Held only in memory it would restart at zero every time the daemon did, and on a
// platform that restarts an app more often than the threshold it would never
// arrive — leaving the confirmation unreachable in precisely the deployments least
// able to notice that it was.
func TestTheDisagreementClockSurvivesARestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)

	candidate := chainview.BlockRef{
		Hash: chainviewtest.TaggedHash("shared", sharedHistory), Height: sharedHistory,
	}
	before := State{ForkCandidate: &candidate, ForkCandidateSince: 1_790_000_000}
	h.sen.persistForkCandidate(ctx, State{}, before)

	restarted := newSentinelOver(t, h)
	if err := restarted.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := restarted.State()
	if got.ForkCandidateSince != before.ForkCandidateSince {
		t.Errorf("clock = %d, want the recorded %d",
			got.ForkCandidateSince, before.ForkCandidateSince)
	}
	if got.ForkCandidate == nil || got.ForkCandidate.Hash != candidate.Hash {
		t.Errorf("separation candidate = %v, want the recorded %v", got.ForkCandidate, candidate)
	}
	if got.ForkCandidate != nil && got.ForkCandidate.Height != candidate.Height {
		t.Errorf("height = %d, want %d", got.ForkCandidate.Height, candidate.Height)
	}
}

// And it is cleared, not merely left behind, when the chains agree again — so a
// restart cannot resurrect a disagreement that has since ended.
func TestAClearedDisagreementClockDoesNotComeBackAfterARestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)

	candidate := chainview.BlockRef{
		Hash: chainviewtest.TaggedHash("shared", sharedHistory), Height: sharedHistory,
	}
	apart := State{ForkCandidate: &candidate, ForkCandidateSince: 1_790_000_000}
	h.sen.persistForkCandidate(ctx, State{}, apart)
	h.sen.persistForkCandidate(ctx, apart, State{})

	restarted := newSentinelOver(t, h)
	if err := restarted.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := restarted.State(); got.ForkCandidateSince != 0 || got.ForkCandidate != nil {
		t.Errorf("a disagreement that had ended was restored: %+v", got)
	}
}

// A recorded separation candidate that cannot be read back is re-derived rather
// than treated as fatal.
//
// It is a timing hint, not something a decision rests on: the next tick recomputes
// the separation from the two chains regardless, so the worst a bad value costs is
// one threshold of delay. The trust anchor in the same function is deliberately the
// opposite — it bounds how far history is trusted, so an unreadable one stops the
// daemon.
func TestAnUnreadableSeparationCandidateDoesNotStopTheDaemon(t *testing.T) {
	t.Parallel()

	for name, corrupt := range map[string]func(ctx context.Context, t *testing.T, h *harness){
		"the hash is not a hash": func(ctx context.Context, t *testing.T, h *harness) {
			t.Helper()
			mustSetMeta(ctx, t, h, store.MetaForkCandidateHash, "not-a-hash")
			mustSetMetaInt(ctx, t, h, store.MetaForkCandidateHeight, 100)
		},
		"the height is not a height": func(ctx context.Context, t *testing.T, h *harness) {
			t.Helper()
			mustSetMeta(ctx, t, h, store.MetaForkCandidateHash,
				chainviewtest.TaggedHash("shared", sharedHistory).String())
			mustSetMetaInt(ctx, t, h, store.MetaForkCandidateHeight, 1<<40)
		},
		"the hash was never written": func(ctx context.Context, t *testing.T, h *harness) {
			t.Helper()
			mustSetMeta(ctx, t, h, store.MetaForkCandidateHash, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			ctx := context.Background()
			mustSetMetaInt(ctx, t, h, store.MetaForkCandidateSince, 1_790_000_000)
			corrupt(ctx, t, h)

			sen := newSentinelOver(t, h)
			if err := sen.Load(ctx); err != nil {
				t.Fatalf("an unreadable timing hint stopped the daemon: %v", err)
			}
			if got := sen.State(); got.ForkCandidate != nil || got.ForkCandidateSince != 0 {
				t.Errorf("a value that could not be read was used anyway: %+v", got)
			}
		})
	}
}

// Nothing recorded is the ordinary case, and it must not be mistaken for a
// disagreement that began at the start of the epoch.
func TestNoRecordedSeparationCandidateLoadsAsNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	sen := newSentinelOver(t, h)
	if err := sen.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := sen.State(); got.ForkCandidate != nil || got.ForkCandidateSince != 0 {
		t.Errorf("a fresh install started with a disagreement already running: %+v", got)
	}
}

// Writing on every tick would be one write per poll for as long as a split lasted,
// so the record is only touched when it actually moves.
func TestTheSeparationCandidateIsOnlyRecordedWhenItChanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, nil)
	candidate := chainview.BlockRef{
		Hash: chainviewtest.TaggedHash("shared", sharedHistory), Height: sharedHistory,
	}
	steady := State{ForkCandidate: &candidate, ForkCandidateSince: 1_790_000_000}

	h.sen.persistForkCandidate(ctx, State{}, steady)
	mustSetMetaInt(ctx, t, h, store.MetaForkCandidateSince, 1) // a value only a write would overwrite
	h.sen.persistForkCandidate(ctx, steady, steady)

	got, err := h.store.GetMetaInt64(ctx, store.MetaForkCandidateSince, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("an unchanged candidate was written again (since = %d)", got)
	}

	// A different separation at the same moment is still a change.
	moved := State{
		ForkCandidate:      &chainview.BlockRef{Hash: chainviewtest.TaggedHash("other", 1), Height: 7},
		ForkCandidateSince: steady.ForkCandidateSince,
	}
	h.sen.persistForkCandidate(ctx, steady, moved)
	if got, err = h.store.GetMetaInt64(ctx, store.MetaForkCandidateHeight, 0); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("height = %d, want the moved separation at 7", got)
	}
}

func mustSetMeta(ctx context.Context, t *testing.T, h *harness, key, value string) {
	t.Helper()
	if err := h.store.SetMeta(ctx, key, value); err != nil {
		t.Fatal(err)
	}
}

func mustSetMetaInt(ctx context.Context, t *testing.T, h *harness, key string, value int64) {
	t.Helper()
	if err := h.store.SetMetaInt64(ctx, key, value); err != nil {
		t.Fatal(err)
	}
}

// The shared-height comparison is done against the nodes themselves, at the lower
// of the two tips.
//
// The lower one because that is the highest block both chains are certain to hold;
// comparing at the taller tip would report a disagreement every time one view was a
// block behind, which is most of the time.
func TestObserveComparesTheTwoChainsAtAHeightTheyBothReach(t *testing.T) {
	t.Parallel()

	t.Run("genuinely different chains", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		h.sf.Extend("ours", 4)
		h.sq.Extend("theirs", 2) // shorter, so the comparison happens at its tip

		obs := h.sen.observe(ctx)
		if obs.Disagreement == nil {
			t.Fatal("two chains holding different blocks were not compared as differing")
		}
		if want := int32(sharedHistory + 2); obs.Disagreement.Height != want {
			t.Errorf("compared at height %d, want the lower tip at %d",
				obs.Disagreement.Height, want)
		}
		if obs.Disagreement.SFHash == obs.Disagreement.SQHash {
			t.Error("recorded a disagreement in which both chains agree")
		}
		if obs.Disagreement.SFHash != chainviewtest.TaggedHash("ours", sharedHistory+2) {
			t.Errorf("sf hash = %v, want your own chain's block at that height",
				obs.Disagreement.SFHash)
		}
	})

	t.Run("one view simply behind the other", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		// Same chain, one view has just not caught up.
		h.sf.Extend("shared-ext", 3)

		if obs := h.sen.observe(ctx); obs.Disagreement != nil {
			t.Errorf("a lagging view was reported as a different chain: %+v", obs.Disagreement)
		}
	})

	t.Run("the chains agree exactly", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		if obs := h.sen.observe(ctx); obs.Disagreement != nil {
			t.Errorf("agreeing chains were reported as differing: %+v", obs.Disagreement)
		}
	})

	t.Run("a node that will not answer", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		ctx := context.Background()
		h.sf.Extend("ours", 4)
		h.sq.Extend("theirs", 2)
		h.sq.Fail("BlockHashByHeight", errNode)

		// Nothing is claimed from a node that did not answer. The comparison is
		// evidence or it is absent; it is never a guess.
		if obs := h.sen.observe(ctx); obs.Disagreement != nil {
			t.Errorf("a disagreement was claimed without both answers: %+v", obs.Disagreement)
		}
	})
}

// The early warning has to leave the daemon, not merely be displayed by it.
//
// On the platforms this runs on, the container cannot notify anybody: the wrapper
// reads the alert list and announces what it finds. A suspicion that stays in
// memory therefore reaches only somebody who thought to open the dashboard, which
// during a possible chain split is the person who least needs telling.
func TestASuspectedSplitIsPublishedAndWithdrawn(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	events := h.bus.Subscribe("test", bus.KindSplitSuspected)

	apart := State{
		SplitSuspected:    true,
		DisagreementSince: 1_790_000_000,
		Disagreement: &HeightDisagreement{
			Height: 961_632,
			SFHash: chainviewtest.TaggedHash("ours", 961_632),
			SQHash: chainviewtest.TaggedHash("theirs", 961_632),
		},
	}
	h.sen.announceSuspicion(State{}, apart)

	select {
	case ev := <-events:
		got, ok := ev.(bus.SplitSuspected)
		if !ok {
			t.Fatalf("published %T, want a SplitSuspected", ev)
		}
		if !got.Suspected {
			t.Error("published a withdrawal instead of a warning")
		}
		if got.Height != 961_632 || got.SFHash == got.SQHash {
			t.Errorf("the two chains' answers did not travel with it: %+v", got)
		}
		if got.Since != 1_790_000_000 {
			t.Errorf("since = %d, want when the disagreement began", got.Since)
		}
	case <-time.After(time.Second):
		t.Fatal("a possible split was never announced")
	}

	// Only on the edges: a disagreement standing for hours must not be one event
	// per poll.
	h.sen.announceSuspicion(apart, apart)
	select {
	case ev := <-events:
		t.Fatalf("an unchanged suspicion was announced again: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	// And it is withdrawn when the chains agree again.
	h.sen.announceSuspicion(apart, State{})
	select {
	case ev := <-events:
		if got := ev.(bus.SplitSuspected); got.Suspected {
			t.Error("the warning was not withdrawn when the disagreement ended")
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was said when the possible split passed")
	}
}

// A confirmed split announces itself, loudly and separately. Saying both on one
// tick would be two notifications about one event.
func TestASuspicionConfirmedOnTheSameTickIsNotAnnouncedTwice(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	events := h.bus.Subscribe("test", bus.KindSplitSuspected)

	h.sen.announceSuspicion(State{}, State{SplitSuspected: true, Phase: PhaseSplit})

	select {
	case ev := <-events:
		t.Fatalf("a confirmed split also announced a suspicion: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// The early warning goes quiet once a split is confirmed, in both directions.
//
// Announcing "this may be a split" beside "the chains have separated" is two
// notifications about one event. The other direction is worse: the tracked
// candidate clears the moment the two chains agree at a tip, which during a
// confirmed split would announce that the possible split had passed — while the
// daemon went on reporting a split. Two of the loudest things it can say,
// contradicting each other.
func TestTheEarlyWarningStaysQuietOnceASplitIsConfirmed(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ prev, next State }{
		"suspicion arriving during a confirmed split": {
			prev: State{Phase: PhaseSplit},
			next: State{Phase: PhaseSplit, SplitSuspected: true},
		},
		"suspicion clearing during a confirmed split": {
			prev: State{Phase: PhaseSplit, SplitSuspected: true},
			next: State{Phase: PhaseSplit},
		},
		"suspicion clearing while the split is resolving": {
			prev: State{Phase: PhaseResolving, SplitSuspected: true},
			next: State{Phase: PhaseResolving},
		},
		"suspicion clearing after an operator recorded the outcome": {
			prev: State{Phase: PhaseResolvedSFWon, SplitSuspected: true},
			next: State{Phase: PhaseResolvedSFWon},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			events := h.bus.Subscribe("test", bus.KindSplitSuspected)

			h.sen.announceSuspicion(tc.prev, tc.next)

			select {
			case ev := <-events:
				t.Errorf("spoke over the confirmed split: %+v", ev)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}
