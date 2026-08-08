package sentinel

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
)

// harness wires a sentinel to two scriptable views and a real store, which is the
// smallest arrangement that exercises the shell end to end.
type harness struct {
	sf, sq *chainviewtest.View
	store  *store.Store
	bus    *bus.Bus
	sen    *Sentinel
	clock  *atomic.Int64

	running chan error
}

const sharedHistory = 200

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	ctx := context.Background()

	sf, sq := chainviewtest.NewSharedHistory(sharedHistory)
	// Distinct identities by default; the interesting case is when they are not.
	sf.SetIdentity(chainview.Identity{
		Endpoint: "http://own-node:8332", LocalAddresses: []string{"own:8333"},
	})
	sq.SetIdentity(chainview.Identity{
		Endpoint: "http://other-node:8432", LocalAddresses: []string{"other:8433"},
	})

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	genesis, err := chainview.GenesisOf(ctx, sf)
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		PollInterval:      20 * time.Millisecond,
		SplitConfirmDepth: 3,
		StallFactor:       6,
		ReorgMargin:       10,
		Network:           chainview.NetworkParams{Name: "test", Genesis: genesis},
		AncestryDepth:     100,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	sen, err := New(sf, sq, st, b, cfg, nil, func() time.Time {
		return time.Unix(clock.Load(), 0)
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &harness{sf: sf, sq: sq, store: st, bus: b, sen: sen, clock: clock}
}

// newSentinelOver builds a second sentinel over the same storage, views and clock.
//
// That is what a restart is, from the daemon's point of view, and it is the only
// way to test what does and does not survive one: everything the process kept in
// memory is gone, everything it wrote is still there.
func newSentinelOver(t *testing.T, h *harness) *Sentinel {
	t.Helper()

	sen, err := New(h.sf, h.sq, h.store, h.bus, h.sen.cfg, nil, h.sen.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sen
}

// start runs the sentinel for the duration of the test.
//
// One run per test: starting a fresh one for each wait would reload state from
// storage and undo whatever the previous run had reached.
func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sen.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the sentinel did not stop when its context ended")
		}
	})
	h.running = done
}

// waitFor blocks until cond holds, or fails the test.
func (h *harness) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case err := <-h.running:
			t.Fatalf("the sentinel stopped before %s: %v", what, err)
		case <-deadline:
			t.Fatalf("timed out waiting for %s (phase %q)", what, h.sen.State().Phase)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitForRecorded waits until storage reflects a phase.
//
// The in-memory phase changes an instant before it is written, so a test that
// asserts on storage has to wait for storage. The ordering that matters is
// unaffected: the write still happens before the announcement.
func (h *harness) waitForRecorded(t *testing.T, want store.SplitState) store.Split {
	t.Helper()
	var last store.Split
	h.waitFor(t, "storage to record "+string(want), func() bool {
		recorded, err := h.store.GetSplitState(context.Background())
		if err != nil {
			return false
		}
		last = recorded
		return recorded.State == want
	})
	return last
}

// runUntil starts the sentinel if needed and waits for cond.
func (h *harness) runUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if h.running == nil {
		h.start(t)
	}
	h.waitFor(t, what, cond)
}

// The worst failure in the design: one node behind both configurations makes the
// two views agree by construction, so divergence becomes unrepresentable and every
// indicator stays green forever while nothing is watched. It has to be refused.
func TestStartupRefusesWhenBothViewsAreTheSameNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	same := chainview.Identity{
		Endpoint:       "http://the-only-node:8332",
		LocalAddresses: []string{"node:8333"},
	}
	h.sf.SetIdentity(same)
	h.sq.SetIdentity(same)

	err := h.sen.Preflight(context.Background())
	if err == nil {
		t.Fatal("startup accepted two views of the same node")
	}
	if !errors.Is(err, chainview.ErrSameNode) {
		t.Errorf("got %v, want ErrSameNode", err)
	}
	// The message must be usable by whoever has to fix it.
	if !strings.Contains(err.Error(), "same Bitcoin node") {
		t.Errorf("error does not explain the problem: %v", err)
	}
	if h.sen.Checks().DistinctNodes {
		t.Error("the distinctness check was reported as passed")
	}
}

// Different addresses reaching one node, which a plain endpoint comparison would
// miss.
func TestStartupRefusesWhenTwoEndpointsReachOneNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.sf.SetIdentity(chainview.Identity{
		Endpoint: "http://127.0.0.1:8332", LocalAddresses: []string{"node.example:8333"},
	})
	h.sq.SetIdentity(chainview.Identity{
		Endpoint: "http://localhost:8332", LocalAddresses: []string{"node.example:8333"},
	})

	err := h.sen.Preflight(context.Background())
	if !errors.Is(err, chainview.ErrSameNode) {
		t.Errorf("got %v, want ErrSameNode for two names reaching one node", err)
	}
}

// A view pointed at another network answers every request correctly and diverges
// permanently, which would read as a chain split rather than a misconfiguration.
func TestStartupRefusesAViewOnTheWrongNetwork(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	// A view built with a different tag has a different first block.
	elsewhere := chainviewtest.New("elsewhere")
	sen, err := New(h.sf, elsewhere, h.store, h.bus, Config{
		Network: chainview.NetworkParams{
			Name:    "test",
			Genesis: chainviewtest.TaggedHash("genesis-shared", 0),
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	preflightErr := sen.Preflight(context.Background())
	if !errors.Is(preflightErr, chainview.ErrWrongNetwork) {
		t.Fatalf("got %v, want ErrWrongNetwork", preflightErr)
	}
	if !strings.Contains(preflightErr.Error(), "network") {
		t.Errorf("error does not mention the network: %v", preflightErr)
	}
}

// A backend that cannot describe itself leaves the check unavailable. Reported as
// such rather than as a pass — claiming "verified" would be the same class of lie
// the check exists to prevent.
func TestStartupReportsAnUnverifiableDistinctnessCheck(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.sq.Fail("Identity", errors.New("this backend cannot say"))

	if err := h.sen.Preflight(context.Background()); err != nil {
		t.Fatalf("an unavailable check should not stop startup: %v", err)
	}
	checks := h.sen.Checks()
	if checks.DistinctVerified {
		t.Error("an unavailable check was reported as verified")
	}
	if checks.DistinctNodes {
		t.Error("distinctness was claimed without being established")
	}
	if checks.Detail == "" {
		t.Error("nothing explains why the check is unavailable")
	}
}

func TestArmsWhenBothViewsAgree(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.runUntil(t, "the sentinel to arm", func() bool {
		return h.sen.State().Phase == PhaseArmed
	})
	h.waitForRecorded(t, store.StateArmed)
}

func TestDetectsASplitAndRecordsWhereTheChainsSeparated(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	events := h.bus.Subscribe("test", bus.KindSplitStateChanged)

	h.runUntil(t, "the sentinel to arm", func() bool {
		return h.sen.State().Phase == PhaseArmed
	})

	// The chains part company and both build well past it.
	h.sf.Extend("ours", 6)
	h.sq.Extend("theirs", 6)

	h.runUntil(t, "a split to be detected", func() bool {
		return h.sen.State().Phase == PhaseSplit
	})

	st := h.sen.State()
	if st.Fork == nil {
		t.Fatal("a split was recorded without a separation point")
	}
	if st.Fork.Height != sharedHistory {
		t.Errorf("separation at height %d, want %d", st.Fork.Height, sharedHistory)
	}

	recorded := h.waitForRecorded(t, store.StateSplit)
	if recorded.ForkHeight != sharedHistory {
		t.Errorf("recorded separation height %d, want %d", recorded.ForkHeight, sharedHistory)
	}
	if recorded.ForkHash == "" {
		t.Error("the recorded split has no separation point")
	}

	// The change is announced, not merely recorded.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-events:
			if changed, ok := e.(bus.SplitStateChanged); ok && changed.New == string(PhaseSplit) {
				if changed.Fork == nil {
					t.Error("the announcement omits where the chains separated")
				}
				return
			}
		case <-deadline:
			t.Fatal("the split was never announced")
		}
	}
}

// A restart must resume, not restart. Losing a recorded split because the machine
// rebooted would silently drop the exposure the daemon exists to track.
func TestARecordedSplitSurvivesARestart(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.runUntil(t, "the sentinel to arm", func() bool {
		return h.sen.State().Phase == PhaseArmed
	})
	h.sf.Extend("ours", 6)
	h.sq.Extend("theirs", 6)
	h.runUntil(t, "a split to be detected", func() bool {
		return h.sen.State().Phase == PhaseSplit
	})
	h.waitForRecorded(t, store.StateSplit)
	before := h.sen.State()

	// A fresh sentinel over the same store, as a restart would produce.
	revived, err := New(h.sf, h.sq, h.store, h.bus, Config{
		PollInterval: 20 * time.Millisecond, SplitConfirmDepth: 3,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := revived.Load(context.Background()); err != nil {
		t.Fatalf("restoring the recorded state: %v", err)
	}

	after := revived.State()
	if after.Phase != PhaseSplit {
		t.Fatalf("phase after restart = %q, want %q", after.Phase, PhaseSplit)
	}
	if before.Fork == nil {
		t.Fatal("the split was detected without a separation point being held in memory")
	}
	if after.Fork == nil {
		t.Fatal("the separation point did not survive the restart")
	}
	if after.Fork.Height != before.Fork.Height || after.Fork.Hash != before.Fork.Hash {
		t.Errorf("separation point after restart = %v at %d, want %v at %d",
			after.Fork.Hash, after.Fork.Height, before.Fork.Hash, before.Fork.Height)
	}
}

// A view that has quietly ended up on the same chain as the user's own node is
// worse than no view at all: it produces a clean report about a chain that needs no
// watching, while the exposure goes unseen. Watching must stop.
func TestWatchingPausesWhenTheSecondViewFollowsTheSameChain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.runUntil(t, "the sentinel to arm", func() bool {
		return h.sen.State().Phase == PhaseArmed
	})

	// A genuine split, so a separation point is recorded.
	h.sf.Extend("ours", 6)
	h.sq.Extend("theirs", 6)
	h.runUntil(t, "a split to be detected", func() bool {
		return h.sen.State().Phase == PhaseSplit
	})

	// Now the second view abandons its chain and adopts the user's own — the
	// failure this check exists for.
	st := h.sen.State()
	h.sq.Reorg(st.Fork.Height, "ours", 6)

	err := chainview.VerifyBranch(context.Background(), h.sf, h.sq, st.TrustAnchor, st.Fork)
	if !errors.Is(err, chainview.ErrWrongBranch) {
		t.Fatalf("got %v, want ErrWrongBranch once the second view adopted the same chain", err)
	}
	if !strings.Contains(err.Error(), "watching nothing") {
		t.Errorf("the error does not explain the consequence: %v", err)
	}
}

func TestTrustAnchorNeverExceedsItsBounds(t *testing.T) {
	t.Parallel()

	// Installed after a split has begun: the user's own tip is already on one side,
	// so anchoring there would tie the second view to blocks that exist only on the
	// first. This is the case the bound exists for.
	h := newHarness(t, func(c *Config) {
		c.DivergenceHeight = sharedHistory + 1
		c.ReorgMargin = 10
	})

	h.sf.Extend("ours", 50) // well past the divergence height
	h.sq.Extend("theirs", 50)

	h.runUntil(t, "a trust anchor to be computed", func() bool {
		return h.sen.State().TrustAnchor > 0
	})

	anchor := h.sen.State().TrustAnchor
	if anchor > sharedHistory {
		t.Errorf("anchor %d is above the divergence height %d, so it straddles the separation",
			anchor, sharedHistory+1)
	}
	if anchor > h.sf.Tip().Height-10 {
		t.Errorf("anchor %d ignores the margin below the tip at %d",
			anchor, h.sf.Tip().Height)
	}

	// Storage lags the in-memory value by an instant, so wait for it rather than
	// racing it.
	h.waitFor(t, "the anchor to be recorded", func() bool {
		persisted, err := h.store.GetMetaInt64(context.Background(), store.MetaTrustAnchorHeight, -1)
		return err == nil && persisted == int64(anchor)
	})
}

func TestBlocksAreRecordedForBothChains(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.runUntil(t, "the sentinel to arm", func() bool {
		return h.sen.State().Phase == PhaseArmed
	})
	h.sf.Extend("ours", 3)

	h.runUntil(t, "blocks to be recorded", func() bool {
		n, err := h.store.CountBranchBlocks(context.Background(), store.BranchSF)
		return err == nil && n > 0
	})

	hashes, err := h.store.RecentBranchHashes(context.Background(), store.BranchSF, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) == 0 {
		t.Error("no blocks were recorded for the chain your node follows")
	}
}

// The daemon keeps working while a view is unreachable, and says so.
func TestAnUnreachableViewDegradesRatherThanStopping(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	h.sq.Fail("Health", errors.New("connection refused"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.sen.Run(ctx) }()

	// It must not arm — one view is unusable — and must not stop either.
	time.Sleep(200 * time.Millisecond) //nolint:forbidigo // observing that nothing happens needs elapsed time
	if got := h.sen.State().Phase; got != PhaseUnarmed {
		t.Errorf("phase = %q, want %q while a view is unreachable", got, PhaseUnarmed)
	}
	select {
	case err := <-done:
		t.Fatalf("the sentinel stopped because a view was unreachable: %v", err)
	default:
	}

	cancel()
	<-done
}

func TestDivergenceHeightIsDerivedFromTheNode(t *testing.T) {
	t.Parallel()

	// The mandatory window opens two retarget periods before the latest the rules
	// can bind — and it is the window, not the activation height, that decides when
	// the chains can first separate.
	d := &chainview.Deployment{
		MaxActivationHeight: 965664,
		Period:              2016,
	}
	got, ok := DivergenceHeightFrom(d)
	if !ok {
		t.Fatal("no divergence height derived from a complete report")
	}
	if got != 961632 {
		t.Errorf("derived %d, want 961632", got)
	}

	for _, incomplete := range []*chainview.Deployment{
		nil,
		{MaxActivationHeight: 0, Period: 2016},
		{MaxActivationHeight: 965664, Period: 0},
		{MaxActivationHeight: 100, Period: 2016}, // would be negative
	} {
		if _, ok := DivergenceHeightFrom(incomplete); ok {
			t.Errorf("derived a height from an unusable report: %+v", incomplete)
		}
	}
}

func TestConfigRequirements(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(1)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := New(nil, sq, st, nil, Config{}, nil, nil); err == nil {
		t.Error("New accepted a missing view")
	}
	if _, err := New(sf, sq, nil, nil, Config{}, nil, nil); err == nil {
		t.Error("New accepted a missing store")
	}
	sen, err := New(sf, sq, st, nil, Config{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sen.cfg.PollInterval != DefaultPollInterval {
		t.Errorf("poll interval = %v, want the default", sen.cfg.PollInterval)
	}
}

// A node that has not finished starting is waited for, not accused.
//
// **Reported from a live install, and the whole of the outage.** The second
// Bitcoin node was loading its block index, which makes it refuse every call
// with "Loading block index…". Preflight read that as a failed network check,
// the daemon exited, the platform restarted it a second later, and it asked too
// early again — crash-looping for as long as the node took to load, serving no
// dashboard at all, and reporting that the node was "not on the expected
// network" while it was on the right one throughout.
func TestANodeStillStartingIsWaitedForRatherThanFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	warmup := errors.New("bitcoind rpc error -28: Loading block index…")
	warmup = errors.Join(chainview.ErrWarmingUp, warmup)
	h.sq.Fail("BlockHashByHeight", warmup)

	// It clears while preflight is waiting, exactly as a node finishes loading.
	go func() {
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // standing in for a real node
		h.sq.Fail("BlockHashByHeight", nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.sen.Preflight(ctx); err != nil {
		t.Fatalf("a node that was merely still starting failed the checks: %v", err)
	}
}

// A node whose RPC socket has not opened yet is waited for too.
//
// **Observed on hardware, after the warmup fix had already shipped.** In a
// container that starts the daemon and its node together, the node is not
// listening for the first second or two — which is connection-refused, not the
// warmup code, so it was still fatal. The daemon exited once and was restarted;
// the message blamed the network, about a node that had said nothing at all.
func TestANodeWhoseSocketIsNotOpenYetIsWaitedFor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	refused := errors.Join(chainview.ErrUnreachable,
		errors.New(`dial tcp 127.0.0.1:8432: connect: connection refused`))
	h.sq.Fail("BlockHashByHeight", refused)

	go func() {
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // standing in for a real node
		h.sq.Fail("BlockHashByHeight", nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.sen.Preflight(ctx); err != nil {
		t.Fatalf("a node that had not opened its socket yet failed the checks: %v", err)
	}
}

// A node genuinely on the wrong network still fails immediately. That is the
// misconfiguration this check exists for, and waiting on it would be waiting
// for something that will never change.
func TestANodeOnTheWrongNetworkStillFailsAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.sq.Fail("BlockHashByHeight", chainview.ErrWrongNetwork)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.sen.Preflight(ctx); err == nil {
		t.Fatal("a node on the wrong network passed the checks")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %v on a condition that will never clear", waited)
	}
}
