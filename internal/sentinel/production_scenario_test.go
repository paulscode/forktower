package sentinel

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// blockOn builds a block whose hash is unique to its branch *and* its height.
//
// Distinct per height because new blocks are recognised by hash, not by number: a
// helper that reuses one hash up a chain produces tips that are silently ignored,
// and a scenario written on top of it proves nothing while appearing to pass.
func blockOn(branch byte, height int32, at int64) *chainview.BlockMeta {
	var h chainhash.Hash
	h[0], h[1], h[2], h[3] = branch, byte(height), byte(height>>8), byte(height>>16)
	return &chainview.BlockMeta{
		BlockRef: chainview.BlockRef{Hash: h, Height: height},
		Time:     time.Unix(at, 0),
	}
}

// The fork observed in production on 2026-08-08, replayed at the heights and pace
// it actually happened, as a standing check that this case is handled.
//
// Knots at 961632 and Core at 961634, agreeing at 961631 and not reconciling. The
// user's own chain holds one block past the separation and stops; the other builds
// on. The depth rule alone never fires here — it wants three blocks from the chain
// that has stalled — so the dashboard reported the two as being on the same chain
// for as long as it lasted, while a block explorer showed otherwise.
func TestTheProductionForkOf2026_08_08IsReportedAndConfirmed(t *testing.T) {
	t.Parallel()

	const (
		forkHeight = int32(961_631)
		start      = int64(1_786_220_000)
		sf, sq     = byte(0x5F), byte(0x59)
	)
	fork := &chainview.BlockRef{Hash: blockOn(0xF0, forkHeight, start).Hash, Height: forkHeight}

	state := NewState()
	state.Phase = PhaseArmed
	state.SFHealth, state.SQHealth = chainview.HealthOK, chainview.HealthOK

	tick := func(s State, at int64, sfHeight, sqHeight int32, sqSyncing bool) State {
		obs := healthy(at)
		obs.SFTip = blockOn(sf, sfHeight, at)
		obs.SQTip = blockOn(sq, sqHeight, at)
		obs.ForkCandidate = fork
		// The second node dipped in and out of resynchronising all day.
		if sqSyncing {
			obs.SQTip, obs.SQHealth = nil, chainview.HealthSyncing
		}
		next, _ := Step(s, obs)
		return next
	}

	// Knots takes its own block at 961632. Both chains are now one past the
	// separation, which on its own is what two blocks found at once looks like.
	state = tick(state, start, 961_632, 961_632, false)
	if state.ForkCandidate == nil {
		t.Fatal("the disagreement was not noticed at all")
	}
	if state.Phase != PhaseArmed || state.SplitSuspected {
		t.Errorf("called it too early: phase=%q suspected=%v", state.Phase, state.SplitSuspected)
	}

	// Core builds on to 961634 and Knots does not follow. A node adopts a heavier
	// valid chain within seconds, so this is already worth telling the user about —
	// no waiting.
	state = tick(state, start+60, 961_632, 961_634, false)
	if !state.SplitSuspected {
		t.Fatal("a chain two blocks ahead and unfollowed was not called out")
	}

	// Ten minutes of exactly that, with the second view flapping throughout.
	at := start + 120
	for i := 0; at <= start+60+600; i++ {
		state = tick(state, at, 961_632, 961_634, i%2 == 0)
		at += 60
	}

	if state.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want %q — this is the fork the rule exists for",
			state.Phase, PhaseSplit)
	}
	if state.Fork == nil || state.Fork.Height != forkHeight {
		t.Errorf("separation = %v, want height %d", state.Fork, forkHeight)
	}
	if state.DetectedAt == 0 {
		t.Error("the split was confirmed without recording when")
	}
}
