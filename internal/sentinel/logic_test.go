package sentinel

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/store"
)

// The phases are declared in this package so that the decision logic depends on
// nothing that performs I/O. That duplicates the names the storage layer persists,
// which is a drift risk — so it is checked rather than trusted.
func TestPhasesMatchThePersistedStates(t *testing.T) {
	t.Parallel()

	persisted := []store.SplitState{
		store.StateUnarmed, store.StateArmed, store.StateSplit, store.StateResolving,
		store.StateResolvedSFWon, store.StateResolvedSQWon,
	}
	phases := AllPhases()

	if len(phases) != len(persisted) {
		t.Fatalf("%d phases but %d persisted states — one was added without the other",
			len(phases), len(persisted))
	}
	for i := range phases {
		if string(phases[i]) != string(persisted[i]) {
			t.Errorf("phase %q does not match persisted state %q", phases[i], persisted[i])
		}
	}
	for _, p := range phases {
		if !p.Valid() {
			t.Errorf("%q is listed but not valid", p)
		}
		if !store.SplitState(p).Valid() {
			t.Errorf("%q is a phase the storage layer would reject", p)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func hash(n byte) chainhash.Hash {
	var h chainhash.Hash
	h[0] = n
	return h
}

func meta(h byte, height int32, unixTime int64) *chainview.BlockMeta {
	return &chainview.BlockMeta{
		BlockRef: chainview.BlockRef{Hash: hash(h), Height: height},
		Time:     time.Unix(unixTime, 0),
	}
}

// ref builds a separation-point reference. The hash is fixed: every test that
// uses one cares about its height, which is what the depth arithmetic reads.
func ref(height int32) *chainview.BlockRef {
	return &chainview.BlockRef{Hash: hash(1), Height: height}
}

// healthy is an observation with both views reporting well and agreeing.
func healthy(at int64) Observation {
	return Observation{
		At:       at,
		SFTip:    meta(1, 100, at),
		SQTip:    meta(1, 100, at),
		SFHealth: chainview.HealthOK,
		SQHealth: chainview.HealthOK,
		// Both tips are the same block, which is what the shell reports as a
		// comparison that was made and came back equal.
		SharedHeightAgreed: true,
		SplitConfirmDepth:  3,
		SplitConfirmSecs:   600,
		SplitSuspectSecs:   120,
		StallFactor:        6.0,
	}
}

func hasEffect(effects []Effect, kind EffectKind) bool {
	for _, e := range effects {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func findEffect(t *testing.T, effects []Effect, kind EffectKind) Effect {
	t.Helper()
	for _, e := range effects {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("no %s effect among %v", kind, kinds(effects))
	return Effect{}
}

func kinds(effects []Effect) []EffectKind {
	out := make([]EffectKind, 0, len(effects))
	for _, e := range effects {
		out = append(out, e.Kind)
	}
	return out
}

// --- transition table: one named test per row --------------------------------

func TestRowUnarmedToArmedWhenBothHealthyAndAgreeing(t *testing.T) {
	t.Parallel()

	got, effects := Step(NewState(), healthy(1000))
	if got.Phase != PhaseArmed {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseArmed)
	}
	// Storage is written before the announcement, so a crash between them leaves
	// storage ahead of the notification rather than behind it.
	persistAt, changeAt := -1, -1
	for i, e := range effects {
		if e.Kind == EffectPersistState && persistAt < 0 {
			persistAt = i
		}
		if e.Kind == EffectPhaseChanged && changeAt < 0 {
			changeAt = i
		}
	}
	if persistAt < 0 || changeAt < 0 {
		t.Fatalf("expected both a persist and an announcement, got %v", kinds(effects))
	}
	if persistAt > changeAt {
		t.Error("the announcement was emitted before the state was persisted")
	}
}

func TestRowUnarmedStaysWhileAViewIsStillSyncing(t *testing.T) {
	t.Parallel()

	// The ordinary state of a fresh install for hours or days. It must not be
	// mistaken for readiness.
	obs := healthy(1000)
	obs.SQHealth = chainview.HealthSyncing

	got, effects := Step(NewState(), obs)
	if got.Phase != PhaseUnarmed {
		t.Errorf("phase = %q, want %q while a view is still catching up", got.Phase, PhaseUnarmed)
	}
	if hasEffect(effects, EffectPhaseChanged) {
		t.Error("announced a phase change without changing phase")
	}
}

func TestRowArmedToSplitWhenBothChainsBuildPastTheSeparation(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 105, 2000)
	obs.SQTip = meta(0x20, 106, 2000)
	obs.ForkCandidate = ref(100) // both are 5 and 6 past it, depth is 3

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseSplit)
	}
	if got.Fork == nil || got.Fork.Height != 100 {
		t.Errorf("separation point = %v, want height 100", got.Fork)
	}
	if got.DetectedAt != 2000 {
		t.Errorf("detected at %d, want 2000", got.DetectedAt)
	}
}

func TestRowArmedToSplitOnARejectedBranch(t *testing.T) {
	t.Parallel()

	// The strongest evidence there is: the user's own node fetched a block from the
	// other chain and refused it. No peer has to agree, so this alone is enough —
	// and notably it does not need the separation-point search to have succeeded.
	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.SQTip = meta(0x20, 101, 1900)

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x20, 101, 2000)
	obs.ForkCandidate = nil
	obs.ForkSearchFailed = true
	obs.SFTips = []chainview.ChainTip{
		{Hash: hash(0x99), Height: 100, Status: chainview.TipActive},
		{Hash: hash(0x20), Height: 101, BranchLen: 1, Status: chainview.TipInvalid},
	}

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseSplit)
	}
	// Raising the alarm does not wait for a detail it does not need.
	if got.DetectedAt != 2000 {
		t.Errorf("detected at %d, want 2000 even without a known separation point", got.DetectedAt)
	}
}

func TestRowArmedStaysWhenDivergenceIsTooShallow(t *testing.T) {
	t.Parallel()

	// Ordinary reorganisation noise: the chains differ but neither has built far
	// enough for it to mean anything.
	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x20, 101, 2000)
	obs.ForkCandidate = ref(100) // one block each, depth required is 3

	got, effects := Step(prev, obs)
	if got.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q for shallow divergence", got.Phase, PhaseArmed)
	}
	if hasEffect(effects, EffectPhaseChanged) {
		t.Error("announced a split on reorganisation noise")
	}
}

func TestRowArmedStaysWhenTipsAgreeAgain(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)

	got, effects := Step(prev, healthy(2000))
	if got.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q", got.Phase, PhaseArmed)
	}
	if hasEffect(effects, EffectPhaseChanged) {
		t.Error("announced a change when nothing changed")
	}
}

func TestRowSplitToResolvingWhenTheOtherChainGoesQuiet(t *testing.T) {
	t.Parallel()

	prev := splitState()
	// The other chain has not produced for far longer than its own pace explains.
	prev.LastSQBlockAt = 1000
	prev.LastSFBlockAt = 100_000

	obs := healthy(100_000)
	obs.SFTip = meta(0x10, 200, 100_000)
	obs.SQTip = prev.SQTip

	got, _ := Step(prev, obs)
	if got.Phase != PhaseResolving {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseResolving)
	}
	if got.ResolvingCandidate != PhaseResolvedSFWon {
		t.Errorf("candidate = %q, want %q", got.ResolvingCandidate, PhaseResolvedSFWon)
	}
}

// The mirror image, and the more likely one: whether the user's own node is the
// quiet side depends entirely on where the hashing power goes.
func TestRowSplitToResolvingWhenTheUsersOwnChainGoesQuiet(t *testing.T) {
	t.Parallel()

	prev := splitState()
	prev.LastSFBlockAt = 1000
	prev.LastSQBlockAt = 100_000

	obs := healthy(100_000)
	obs.SFTip = prev.SFTip
	obs.SQTip = meta(0x20, 200, 100_000)

	got, _ := Step(prev, obs)
	if got.Phase != PhaseResolving {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseResolving)
	}
	if got.ResolvingCandidate != PhaseResolvedSQWon {
		t.Errorf("candidate = %q, want %q", got.ResolvingCandidate, PhaseResolvedSQWon)
	}
}

func TestRowSplitStaysWhenBothChainsGoQuiet(t *testing.T) {
	t.Parallel()

	// Both silent says nothing about the chains — it is a problem with our own
	// connectivity, and reading it as a resolution would be exactly wrong.
	prev := splitState()
	prev.LastSFBlockAt = 1000
	prev.LastSQBlockAt = 1000

	obs := healthy(100_000)
	obs.SFTip, obs.SQTip = prev.SFTip, prev.SQTip

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Errorf("phase = %q, want %q when neither chain is producing", got.Phase, PhaseSplit)
	}
}

func TestRowResolvingBackToSplitWhenTheQuietChainRecovers(t *testing.T) {
	t.Parallel()

	prev := splitState()
	prev.Phase = PhaseResolving
	prev.ResolvingCandidate = PhaseResolvedSFWon
	prev.LastSQBlockAt = 100_000 // producing again
	prev.LastSFBlockAt = 100_000

	obs := healthy(100_100)
	obs.SFTip, obs.SQTip = prev.SFTip, prev.SQTip

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Errorf("phase = %q, want %q once the quiet chain produces again", got.Phase, PhaseSplit)
	}
}

func TestRowResolvingToResolvedOnlyByAnOperator(t *testing.T) {
	t.Parallel()

	prev := splitState()
	prev.Phase = PhaseResolving
	prev.ResolvingCandidate = PhaseResolvedSFWon
	prev.LastSQBlockAt = 1000
	prev.LastSFBlockAt = 100_000

	// Without an operator's word it stays put, however obvious it looks. The daemon
	// observes; it does not adjudicate.
	obs := healthy(100_000)
	obs.SFTip, obs.SQTip = prev.SFTip, prev.SQTip
	got, _ := Step(prev, obs)
	if got.Phase != PhaseResolving {
		t.Fatalf("phase = %q, want to stay %q without an operator", got.Phase, PhaseResolving)
	}

	obs.OperatorOutcome = PhaseResolvedSFWon
	got, _ = Step(prev, obs)
	if got.Phase != PhaseResolvedSFWon {
		t.Errorf("phase = %q, want %q once an operator recorded it", got.Phase, PhaseResolvedSFWon)
	}
}

func TestRowBothViewsDownNeverChangesThePhase(t *testing.T) {
	t.Parallel()

	// The most damaging thing this component could do is forget a recorded split
	// because the machine lost sight of both chains.
	prev := splitState()

	obs := healthy(1000)
	obs.SFHealth, obs.SQHealth = chainview.HealthDown, chainview.HealthDown
	obs.SFTip, obs.SQTip = nil, nil

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want the recorded %q kept through an outage", got.Phase, PhaseSplit)
	}
	if got.Fork == nil {
		t.Error("the separation point was forgotten during an outage")
	}

	// Reported once the grace period has passed, so an outage is visible without
	// being mistaken for a resolution.
	later := obs
	later.At = 1000 + int64(bothDownGrace/time.Second)
	got2, effects := Step(got, later)
	if !hasEffect(effects, EffectBothViewsDown) {
		t.Errorf("a sustained outage was not reported; effects were %v", kinds(effects))
	}
	if got2.Phase != PhaseSplit {
		t.Errorf("phase = %q, want %q", got2.Phase, PhaseSplit)
	}
}

func TestResolvedPhasesAreTerminalAndChangeNothing(t *testing.T) {
	t.Parallel()

	for _, resolved := range []Phase{PhaseResolvedSFWon, PhaseResolvedSQWon} {
		prev := splitState()
		prev.Phase = resolved

		// Even conditions that would otherwise mean something leave it alone, and
		// nothing about watching stops as a result.
		obs := healthy(200_000)
		obs.SFTip = meta(0x10, 500, 200_000)
		obs.SQTip = meta(0x20, 500, 200_000)
		obs.ForkCandidate = ref(100)

		got, _ := Step(prev, obs)
		if got.Phase != resolved {
			t.Errorf("phase moved from %q to %q; recorded outcomes are terminal",
				resolved, got.Phase)
		}
		if got.Fork == nil {
			t.Errorf("%q lost the separation point", resolved)
		}
	}
}

func TestUnrecognisedStoredPhaseResetsRatherThanActing(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = "SOMETHING_ELSE"

	got, _ := Step(prev, healthy(1000))
	if got.Phase != PhaseUnarmed {
		t.Errorf("phase = %q, want a reset to %q", got.Phase, PhaseUnarmed)
	}
}

func splitState() State {
	s := NewState()
	s.Phase = PhaseSplit
	s.SFHealth, s.SQHealth = chainview.HealthOK, chainview.HealthOK
	s.Fork = ref(100)
	s.DetectedAt = 1500
	s.SFTip = meta(0x10, 150, 50_000)
	s.SQTip = meta(0x20, 151, 50_000)
	s.LastSFBlockAt = 50_000
	s.LastSQBlockAt = 50_000
	return s
}

// --- flapping ----------------------------------------------------------------

// Tips that disagree briefly and then agree again must not produce a split, or the
// alarm would go off every time one node was momentarily ahead.
func TestFlappingTipsDoNotProduceASplit(t *testing.T) {
	t.Parallel()

	state := NewState()
	state, _ = Step(state, healthy(1000))
	if state.Phase != PhaseArmed {
		t.Fatalf("did not arm: %q", state.Phase)
	}

	for i := range 20 {
		at := int64(1000 + (i+1)*10)

		disagree := healthy(at)
		disagree.SFTip = meta(0x10, 101, at)
		disagree.SQTip = meta(0x20, 101, at)
		disagree.ForkCandidate = ref(100)

		var effects []Effect
		state, effects = Step(state, disagree)
		if state.Phase != PhaseArmed {
			t.Fatalf("round %d: phase became %q on a one-block disagreement", i, state.Phase)
		}
		if hasEffect(effects, EffectPhaseChanged) {
			t.Fatalf("round %d: announced a phase change", i)
		}

		state, _ = Step(state, healthy(at+5))
		if state.Phase != PhaseArmed {
			t.Fatalf("round %d: phase became %q after agreeing again", i, state.Phase)
		}
	}
}

// --- tipsAgree ---------------------------------------------------------------

func TestTipsAgree(t *testing.T) {
	t.Parallel()

	sfAncestry := []chainhash.Hash{hash(0x13), hash(0x12), hash(0x11), hash(0x10)}
	sqAncestry := []chainhash.Hash{hash(0x23), hash(0x22), hash(0x21), hash(0x20)}

	cases := []struct {
		name string
		sf   *chainview.BlockMeta
		sq   *chainview.BlockMeta
		want bool
		why  string
	}{
		{
			name: "identical tips",
			sf:   meta(0x10, 100, 0), sq: meta(0x10, 100, 0), want: true,
			why: "the obvious case",
		},
		{
			name: "one behind but on the same chain",
			sf:   meta(0x22, 101, 0), sq: meta(0x23, 102, 0), want: true,
			why: "two nodes are routinely a block apart; that is lag, not divergence",
		},
		{
			name: "same height, different block",
			sf:   meta(0x10, 100, 0), sq: meta(0x20, 100, 0), want: false,
			why: "a genuine disagreement cannot be explained by one view being behind",
		},
		{
			name: "behind but not on the same chain",
			sf:   meta(0x99, 101, 0), sq: meta(0x23, 102, 0), want: false,
			why: "close in height is not the same as being on the same chain",
		},
		{
			name: "further behind than the confirmation depth",
			sf:   meta(0x20, 98, 0), sq: meta(0x23, 102, 0), want: false,
			why: "beyond the depth this is divergence, not lag",
		},
		{
			name: "a view that could not be read",
			sf:   nil, sq: meta(0x23, 102, 0), want: false,
			why: "silence is not agreement",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tipsAgree(tc.sf, tc.sq, 3, sfAncestry, sqAncestry)
			if got != tc.want {
				t.Errorf("tipsAgree = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// --- invalidTipMatchesSQ -----------------------------------------------------

func TestInvalidTipMatchesSQ(t *testing.T) {
	t.Parallel()

	ancestry := []chainhash.Hash{hash(0x23), hash(0x22), hash(0x21), hash(0x20)}
	sqTip := hash(0x23)

	cases := []struct {
		name string
		tip  chainview.ChainTip
		want bool
		why  string
	}{
		{
			name: "rejected branch is the watched chain's tip",
			tip:  chainview.ChainTip{Hash: sqTip, Status: chainview.TipInvalid},
			want: true, why: "unambiguous",
		},
		{
			name: "rejected branch is in the watched chain's recent history",
			tip:  chainview.ChainTip{Hash: hash(0x21), BranchLen: 3, Status: chainview.TipInvalid},
			want: true, why: "within its own branch length",
		},
		{
			name: "rejected branch beyond its own branch length",
			tip:  chainview.ChainTip{Hash: hash(0x20), BranchLen: 1, Status: chainview.TipInvalid},
			want: false,
			why:  "a rejected branch cannot imply more divergence than it has",
		},
		{
			name: "rejected branch we have never seen",
			tip:  chainview.ChainTip{Hash: hash(0x77), BranchLen: 3, Status: chainview.TipInvalid},
			want: false, why: "a node may know of rejected branches for other reasons",
		},
		{
			name: "the active tip",
			tip:  chainview.ChainTip{Hash: sqTip, Status: chainview.TipActive},
			want: false, why: "not having pursued a branch says nothing",
		},
		{
			name: "headers-only branch",
			tip:  chainview.ChainTip{Hash: sqTip, Status: chainview.TipHeadersOnly},
			want: false, why: "never fetched, so never refused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := invalidTipMatchesSQ(ancestry, sqTip, tc.tip); got != tc.want {
				t.Errorf("got %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// --- divergedDepth -----------------------------------------------------------

func TestDivergedDepth(t *testing.T) {
	t.Parallel()

	if got := divergedDepth(105, 100); got != 5 {
		t.Errorf("divergedDepth(105, 100) = %d, want 5", got)
	}
	if got := divergedDepth(100, 100); got != 0 {
		t.Errorf("divergedDepth(100, 100) = %d, want 0", got)
	}
	// The separation point is an ancestor of both tips by construction, so a
	// negative result means the inputs disagree. Clamped rather than propagated,
	// because a negative depth flowing into the confirmation test would be worse
	// than a zero.
	if got := divergedDepth(90, 100); got != 0 {
		t.Errorf("divergedDepth(90, 100) = %d, want it clamped to 0", got)
	}
}

// --- trust anchor ------------------------------------------------------------

func TestTrustAnchorTakesTheLowestBound(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		obs  Observation
		fork *chainview.BlockRef
		want int32
		ok   bool
		why  string
	}{
		{
			name: "known divergence height wins when it is lowest",
			obs: Observation{
				DivergenceHeight: 961632,
				SFTip:            meta(1, 970000, 0),
				ReorgMargin:      100,
			},
			want: 961631, ok: true,
			why: "one below the first height at which the rules could disagree",
		},
		{
			name: "tip margin wins when it is lower",
			obs: Observation{
				DivergenceHeight: 961632,
				SFTip:            meta(1, 900000, 0),
				ReorgMargin:      100,
			},
			want: 899900, ok: true,
			why: "a chain far below the divergence height is bounded by its own tip",
		},
		{
			name: "a rejected branch bounds it too",
			obs: Observation{
				SFTip:       meta(1, 970000, 0),
				ReorgMargin: 100,
				SFTips: []chainview.ChainTip{
					{Hash: hash(9), Height: 961700, BranchLen: 68, Status: chainview.TipInvalid},
				},
			},
			want: 961631, ok: true,
			why: "the base of a branch the user's own node refused, minus one",
		},
		{
			name: "nothing bounds it",
			obs:  Observation{},
			ok:   false,
			why:  "with no bound at all, refuse rather than guess",
		},
		{
			name: "a recorded separation point bounds it",
			obs: Observation{
				SFTip:       meta(1, 970000, 0),
				ReorgMargin: 100,
			},
			fork: ref(500000),
			want: 500000, ok: true,
			why: "history below the separation is shared",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := TrustAnchorHeight(tc.obs, tc.fork)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v — %s", ok, tc.ok, tc.why)
			}
			if ok && got != tc.want {
				t.Errorf("anchor = %d, want %d — %s", got, tc.want, tc.why)
			}
		})
	}
}

// Installing after a split has begun is the case the bound exists for: the user's
// own tip is already on one side, so anchoring there would tie the second view to
// blocks that only exist on the first.
func TestTrustAnchorStaysBelowTheDivergenceForALateInstall(t *testing.T) {
	t.Parallel()

	obs := Observation{
		DivergenceHeight: 961632,
		SFTip:            meta(1, 963000, 0), // already past it, on one side
		ReorgMargin:      100,
	}
	got, ok := TrustAnchorHeight(obs, nil)
	if !ok {
		t.Fatal("no anchor computed")
	}
	if got >= obs.DivergenceHeight {
		t.Errorf("anchor %d is not below the divergence height %d — it would straddle "+
			"the separation", got, obs.DivergenceHeight)
	}
}

func TestTrustAnchorIsReportedWhenItChanges(t *testing.T) {
	t.Parallel()

	obs := healthy(1000)
	obs.DivergenceHeight = 961632
	obs.ReorgMargin = 100
	obs.SFTip = meta(1, 970000, 1000)
	obs.SQTip = meta(1, 970000, 1000)

	_, effects := Step(NewState(), obs)
	e := findEffect(t, effects, EffectTrustAnchorChanged)
	if e.Height != 961631 {
		t.Errorf("anchor effect height = %d, want 961631", e.Height)
	}
}

// --- blocks and health -------------------------------------------------------

func TestNewBlocksAreReportedWithTheirCadence(t *testing.T) {
	t.Parallel()

	state, _ := Step(NewState(), healthy(1000))

	obs := healthy(1600)
	obs.SFTip = meta(0x11, 101, 1600)
	obs.SQTip = meta(1, 100, 1000) // unchanged

	next, effects := Step(state, obs)

	e := findEffect(t, effects, EffectBranchExtended)
	if e.Branch != chainview.BranchSF {
		t.Errorf("effect branch = %q, want %q", e.Branch, chainview.BranchSF)
	}
	if e.IntervalSecs <= 0 {
		t.Error("no cadence estimate on the effect; a countdown in blocks cannot be " +
			"turned into one in time without it")
	}
	if next.LastSFBlockAt != 1600 {
		t.Errorf("last seen = %d, want 1600", next.LastSFBlockAt)
	}

	// The unchanged chain must not be reported as having advanced.
	for _, eff := range effects {
		if eff.Kind == EffectBranchExtended && eff.Branch == chainview.BranchSQ {
			t.Error("a chain that did not move was reported as extended")
		}
	}
}

func TestHealthChangesAreReportedOnce(t *testing.T) {
	t.Parallel()

	state, _ := Step(NewState(), healthy(1000))

	degraded := healthy(1100)
	degraded.SQHealth = chainview.HealthDegraded

	next, effects := Step(state, degraded)
	e := findEffect(t, effects, EffectHealthChanged)
	if e.Branch != chainview.BranchSQ || e.NewHealth != chainview.HealthDegraded {
		t.Errorf("unexpected health effect: %+v", e)
	}

	// Reported on change, not on every tick, or the timeline would be unreadable.
	_, again := Step(next, degraded)
	if hasEffect(again, EffectHealthChanged) {
		t.Error("an unchanged health state was reported again")
	}
}

// A phase read back from storage is not necessarily one this build knows: a
// database written by a newer version, or corrupted, can hold anything. Both
// predicates have to answer safely for a value they have never seen, because the
// alternative is acting on a phase whose meaning is unknown.
func TestPhasePredicatesAnswerSafelyForAnUnknownValue(t *testing.T) {
	t.Parallel()

	for _, p := range AllPhases() {
		if !p.Valid() {
			t.Errorf("%q is a real phase but Valid() says otherwise", p)
		}
	}

	const unknown Phase = "RESOLVED_BY_A_LATER_VERSION"
	if unknown.Valid() {
		t.Error("an unrecognised phase was accepted as valid")
	}
	if unknown.Resolved() {
		t.Error("an unrecognised phase was treated as a recorded outcome, which would " +
			"stop the daemon acting on a split it can still see")
	}
}

// Which chain has gone quiet depends entirely on where the hashing power goes, so
// the check is symmetric. Assuming it will be the other chain would leave the more
// likely case — the user's own node on the minority branch — unhandled.
func TestStalledBranchLooksAtWhicheverChainWouldHaveLost(t *testing.T) {
	t.Parallel()

	const now int64 = 1_790_000_000
	// One side produced a block recently; the other has been quiet far longer than
	// any ordinary gap.
	st := State{
		SFCadence:     Cadence{},
		SQCadence:     Cadence{},
		LastSFBlockAt: now - 10,
		LastSQBlockAt: now - 100_000,
	}
	obs := Observation{At: now, StallFactor: 6}

	// If the user's own chain is what persisted, the quiet one is the other.
	if !stalledBranch(st, obs, PhaseResolvedSFWon) {
		t.Error("the other chain has been quiet for a day and was not seen as stalled")
	}
	// And the reverse: the user's own chain is producing, so it is not the stalled one.
	if stalledBranch(st, obs, PhaseResolvedSQWon) {
		t.Error("a chain producing blocks ten seconds ago was called stalled")
	}

	// Phases that are not an outcome say nothing about a stall. Answering otherwise
	// would let a phase that is still unfolding be reported as decided.
	for _, p := range []Phase{PhaseUnarmed, PhaseArmed, PhaseSplit, PhaseResolving} {
		if stalledBranch(st, obs, p) {
			t.Errorf("%q is not an outcome, so no chain has lost yet", p)
		}
	}
	if stalledBranch(st, obs, Phase("SOMETHING_ELSE")) {
		t.Error("an unrecognised phase produced a stall verdict")
	}
}

// The cache is what keeps the separation search from re-fetching the same
// ancestors on every tick, and it is keyed by branch so that one view's answers
// can never be served for the other — a misconfiguration where both views point at
// one node must not be masked here.
func TestHeaderCacheKeepsBranchesApartAndEvictsTheOldest(t *testing.T) {
	t.Parallel()

	c := NewHeaderCache(2)

	meta := func(tag string, h int32) chainview.BlockMeta {
		return chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: chainviewtest.TaggedHash(tag, h), Height: h},
		}
	}

	shared := meta("shared", 1)
	c.Put(chainview.BranchSF, shared)
	if _, ok := c.Get(chainview.BranchSQ, shared.Hash); ok {
		t.Fatal("a header stored for one branch was served for the other")
	}

	// Re-putting the same header updates it in place rather than growing the cache.
	c.Put(chainview.BranchSF, shared)
	if c.Len() != 1 {
		t.Errorf("cache holds %d entries after storing one header twice, want 1", c.Len())
	}

	c.Put(chainview.BranchSF, meta("shared", 2))
	// Touching the first makes the second the least recently used.
	if _, ok := c.Get(chainview.BranchSF, shared.Hash); !ok {
		t.Fatal("a header stored a moment ago was already gone")
	}
	c.Put(chainview.BranchSF, meta("shared", 3))

	if c.Len() != 2 {
		t.Errorf("cache holds %d entries, want its limit of 2", c.Len())
	}
	if _, ok := c.Get(chainview.BranchSF, shared.Hash); !ok {
		t.Error("the recently used header was evicted instead of the idle one")
	}
	if _, ok := c.Get(chainview.BranchSF, chainviewtest.TaggedHash("shared", 2)); ok {
		t.Error("the idle header survived eviction, so the cache is over its limit")
	}
}

// A limit of zero means "use the default", not "remember nothing" — a cache that
// silently held nothing would turn every tick into a full re-walk.
func TestHeaderCacheWithoutALimitUsesTheDefault(t *testing.T) {
	t.Parallel()

	c := NewHeaderCache(0)
	for h := int32(0); h < 50; h++ {
		c.Put(chainview.BranchSQ, chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: chainviewtest.TaggedHash("theirs", h), Height: h},
		})
	}
	if c.Len() != 50 {
		t.Errorf("cache holds %d of 50 headers", c.Len())
	}
}

// Someone who installs Forktower *because* they heard the chains had split is the
// likely user during a real fork, not an edge case. Arming is impossible then —
// it requires the chains to agree — so without this the daemon sits at "getting
// set up, nothing to do yet" forever while their funds are exposed.
func TestRowUnarmedStraightToSplitWhenTheChainsAlreadyDisagree(t *testing.T) {
	t.Parallel()

	obs := Observation{
		At:                1_790_000_000,
		SplitConfirmDepth: 3,
		SFTip:             meta(2, 850_010, 1_790_000_000),
		SQTip:             meta(3, 850_008, 1_790_000_000),
		SFHealth:          chainview.HealthOK,
		SQHealth:          chainview.HealthOK,
		ForkCandidate:     ref(850_000),
	}

	next, effects := Step(NewState(), obs)

	if next.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want SPLIT — a daemon that starts during a split must "+
			"still find it", next.Phase)
	}
	if next.Fork == nil || next.Fork.Height != 850_000 {
		t.Errorf("fork = %+v, want the separation point recorded", next.Fork)
	}
	if next.DetectedAt != obs.At {
		t.Errorf("detected_at = %d, want when it was found", next.DetectedAt)
	}

	// And it says so, rather than describing something it did not witness.
	var announced bool
	for _, e := range effects {
		if e.Kind == EffectPhaseChanged && e.NewPhase == PhaseSplit {
			announced = true
			if !strings.Contains(e.Detail, "already") {
				t.Errorf("detail = %q, want it to say the split was found, not watched",
					e.Detail)
			}
		}
	}
	if !announced {
		t.Error("entering a split from a standing start was not announced")
	}
}

// The same from a rejected block, which is the strongest evidence there is and
// needs no separation point to have been found.
func TestRowUnarmedStraightToSplitOnARejectedBranch(t *testing.T) {
	t.Parallel()

	sqTip := meta(3, 850_008, 1_790_000_000)
	obs := Observation{
		At:                1_790_000_000,
		SplitConfirmDepth: 3,
		SFTip:             meta(2, 850_010, 1_790_000_000),
		SQTip:             sqTip,
		SFHealth:          chainview.HealthOK,
		SQHealth:          chainview.HealthOK,
		SFTips: []chainview.ChainTip{
			{Hash: sqTip.Hash, Height: sqTip.Height, Status: chainview.TipInvalid},
		},
	}

	next, _ := Step(NewState(), obs)
	if next.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want SPLIT", next.Phase)
	}
	// No separation point was found, and that must not hold up the alarm.
	if next.DetectedAt != obs.At {
		t.Errorf("detected_at = %d, want the split recorded anyway", next.DetectedAt)
	}
}

// The evidence bar is exactly the same from a standing start as from watching.
// Nothing about having witnessed a separation makes it more real, and nothing
// about arriving late makes weaker evidence sufficient.
func TestUnarmedDoesNotBelieveLessEvidenceThanArmed(t *testing.T) {
	t.Parallel()

	// One block past the separation, against a confirmation depth of three: a
	// difference this small is ordinary reorganisation noise.
	obs := Observation{
		At:                1_790_000_000,
		SplitConfirmDepth: 3,
		SFTip:             meta(2, 850_001, 1_790_000_000),
		SQTip:             meta(3, 850_001, 1_790_000_000),
		SFHealth:          chainview.HealthOK,
		SQHealth:          chainview.HealthOK,
		ForkCandidate:     ref(850_000),
	}

	fromUnarmed, _ := Step(NewState(), obs)
	if fromUnarmed.Phase != PhaseUnarmed {
		t.Errorf("phase = %q, want UNARMED: shallow divergence is not a split",
			fromUnarmed.Phase)
	}

	armed := NewState()
	armed.Phase = PhaseArmed
	fromArmed, _ := Step(armed, obs)
	if fromArmed.Phase != PhaseArmed {
		t.Errorf("phase = %q, want ARMED", fromArmed.Phase)
	}
}

// A view that cannot be trusted must not produce a split from a standing start
// either: an unusable view's tip says nothing about which chain it is on.
func TestUnarmedIgnoresAnUnusableView(t *testing.T) {
	t.Parallel()

	base := Observation{
		At:                1_790_000_000,
		SplitConfirmDepth: 1,
		SFTip:             meta(2, 850_010, 1_790_000_000),
		SQTip:             meta(3, 850_008, 1_790_000_000),
		ForkCandidate:     ref(850_000),
	}

	for _, health := range []chainview.HealthState{
		chainview.HealthSyncing, chainview.HealthEclipseSuspect,
		chainview.HealthWrongBranch, chainview.HealthDown,
	} {
		obs := base
		obs.SFHealth, obs.SQHealth = chainview.HealthOK, health
		if next, _ := Step(NewState(), obs); next.Phase != PhaseUnarmed {
			t.Errorf("with the other view %q the phase became %q, want UNARMED",
				health, next.Phase)
		}

		obs.SFHealth, obs.SQHealth = health, chainview.HealthOK
		if next, _ := Step(NewState(), obs); next.Phase != PhaseUnarmed {
			t.Errorf("with your own view %q the phase became %q, want UNARMED",
				health, next.Phase)
		}
	}
}

// A view that is merely behind is not a chain that disagrees, and from a standing
// start there is no prior agreement to fall back on — so this is the case where
// getting it wrong would invent a split out of one slow node.
//
// What stops it is that the depth test applies to *both* chains. A lagging node's
// tip is the separation point itself, so it has built nothing past it.
func TestUnarmedDoesNotMistakeALaggingNodeForASplit(t *testing.T) {
	t.Parallel()

	// Eight blocks behind on the same chain, which is further than the
	// confirmation depth, so this cannot be waved through as agreement either.
	obs := Observation{
		At:                1_790_000_000,
		SplitConfirmDepth: 3,
		SFTip:             meta(10, 850_010, 1_790_000_000),
		SQTip:             meta(2, 850_002, 1_790_000_000),
		SFHealth:          chainview.HealthOK,
		SQHealth:          chainview.HealthOK,
		// The separation point a real search would return for a node that is
		// simply behind: its own tip.
		ForkCandidate: ref(850_002),
	}

	next, _ := Step(NewState(), obs)
	if next.Phase == PhaseSplit {
		t.Fatal("one node being eight blocks behind was reported as a chain split")
	}
	if next.Phase != PhaseUnarmed {
		t.Errorf("phase = %q, want UNARMED until the node catches up", next.Phase)
	}

	// And once it has caught up, it arms normally.
	obs.SQTip = meta(10, 850_010, 1_790_000_000)
	if caught, _ := Step(NewState(), obs); caught.Phase != PhaseArmed {
		t.Errorf("phase = %q after catching up, want ARMED", caught.Phase)
	}
}

// The split this rule was added for, and the one that was happening in production
// when it was written.
//
// The user's own chain holds one block past the separation and then all but stops,
// because it is the side with little hashing power behind it. The other chain keeps
// building. The depth rule needs *both* sides to reach the confirmation depth, so
// it never triggers — and the person whose node is on the stalling side, who has
// the most to lose, is the one it leaves uninformed.
func TestRowArmedToSplitWhenTheUsersOwnChainStallsJustPastTheSeparation(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	obs := healthy(2000 + 600)
	obs.SFTip = meta(0x10, 101, 2000)     // one past, and going nowhere
	obs.SQTip = meta(0x20, 103, 2000+600) // three past, still building
	obs.ForkCandidate = ref(100)

	got, effects := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want %q — the depth rule cannot reach this split", got.Phase, PhaseSplit)
	}
	if got.Fork == nil || got.Fork.Height != 100 {
		t.Errorf("separation point = %v, want the candidate that persisted, at height 100", got.Fork)
	}
	if got.DetectedAt != 2000+600 {
		t.Errorf("detected at %d, want %d", got.DetectedAt, 2000+600)
	}
	if !hasEffect(effects, EffectPhaseChanged) {
		t.Error("the split was reached without being announced")
	}
}

// One second short of the threshold is still ordinary reorganisation noise.
func TestRowArmedStaysUntilTheDisagreementHasLastedLongEnough(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	obs := healthy(2000 + 599)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x20, 103, 2000+599)
	obs.ForkCandidate = ref(100)

	if got, _ := Step(prev, obs); got.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q one second before the threshold", got.Phase, PhaseArmed)
	}
}

// The clock measures one continuous disagreement, so anything that ends it stops
// the clock. Otherwise a node that repeatedly fell a block behind and caught up
// would accumulate its way to a split that never happened.
func TestTheDisagreementClockStopsWhenTheChainsReconcile(t *testing.T) {
	t.Parallel()

	diverged := func(prev State, at int64) State {
		obs := healthy(at)
		obs.SFTip = meta(0x10, 101, at)
		obs.SQTip = meta(0x20, 101, at)
		obs.ForkCandidate = ref(100)
		next, _ := Step(prev, obs)
		return next
	}

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	apart := diverged(prev, 2000)
	if apart.ForkCandidateSince != 2000 {
		t.Fatalf("clock started at %d, want 2000", apart.ForkCandidateSince)
	}

	// They agree again: one tip, no candidate.
	obs := healthy(2100)
	together, _ := Step(apart, obs)
	if together.ForkCandidateSince != 0 || together.ForkCandidate != nil {
		t.Fatalf("the clock kept running after the chains agreed: %+v", together)
	}

	// And a later disagreement is timed from when *it* began, not from the first.
	again := diverged(together, 2200)
	if again.ForkCandidateSince != 2200 {
		t.Errorf("clock restarted at %d, want 2200", again.ForkCandidateSince)
	}
	if again.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q — the earlier disagreement must not count", again.Phase, PhaseArmed)
	}
}

// A separation point that moves is a different disagreement. Reorganisation churn
// shifts it about; a real split leaves it where it is.
func TestTheDisagreementClockRestartsWhenTheSeparationMoves(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	obs := healthy(2000 + 600)
	obs.SFTip = meta(0x10, 105, 2000+600)
	obs.SQTip = meta(0x20, 105, 2000+600)
	obs.ForkCandidate = &chainview.BlockRef{Hash: hash(0x77), Height: 102}

	got, _ := Step(prev, obs)
	if got.ForkCandidateSince != 2000+600 {
		t.Errorf("clock = %d, want it restarted at %d for a moved separation",
			got.ForkCandidateSince, 2000+600)
	}
}

// One view merely being behind the other is not a disagreement. The separation
// search returns a point whenever the tips differ, so without this the clock would
// run for any node that lagged.
func TestNoDisagreementClockWhileOneViewIsSimplyBehind(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 100, 2000) // sitting on the separation itself
	obs.SQTip = meta(0x20, 103, 2000)
	obs.ForkCandidate = ref(100)

	got, _ := Step(prev, obs)
	if got.ForkCandidateSince != 0 || got.ForkCandidate != nil {
		t.Errorf("a lagging view started the disagreement clock: %+v", got)
	}
}

// How far apart the chains have got is the number this screen exists to show, and
// it was nought on both branches for the whole time it mattered — the recorded
// separation is only set at confirmation, and nothing else was consulted.
func TestBlocksAreReportedAgainstTheCandidateSeparationBeforeASplitIsConfirmed(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.SFTip = meta(0x10, 101, 2000)
	prev.SQTip = meta(0x20, 102, 2000)
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	obs := healthy(2100)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x21, 103, 2100) // a new block on the other chain
	obs.ForkCandidate = ref(100)

	_, effects := Step(prev, obs)

	var found bool
	for _, e := range effects {
		if e.Kind != EffectBranchExtended || e.Branch != chainview.BranchSQ {
			continue
		}
		found = true
		if e.SinceForkDepth != 3 {
			t.Errorf("depth past the separation = %d, want 3", e.SinceForkDepth)
		}
	}
	if !found {
		t.Fatal("the new block was not reported at all")
	}
}

// A view that cannot be read must not stop the clock.
//
// The threshold is measured in hours, and a second node that resynchronises for a
// few seconds after each new block — which one observed installation did all day —
// would restart it every few minutes. The rule would then be unreachable in exactly
// the deployment it was written for. Losing sight of a chain is not evidence that
// the two came back together, which is the same reasoning the both-views-down grace
// period rests on.
func TestTheDisagreementClockKeepsRunningWhileAViewIsUnreadable(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	for name, blind := range map[string]func(o *Observation){
		"the other view is resynchronising": func(o *Observation) {
			o.SQTip, o.SQHealth = nil, chainview.HealthSyncing
		},
		"the user's own view is unreachable": func(o *Observation) {
			o.SFTip, o.SFHealth = nil, chainview.HealthDown
		},
		"the separation search came up short": func(o *Observation) {
			o.ForkCandidate, o.ForkSearchFailed = nil, true
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obs := healthy(2500)
			obs.SFTip = meta(0x10, 101, 2000)
			obs.SQTip = meta(0x20, 103, 2500)
			obs.ForkCandidate = ref(100)
			blind(&obs)

			got, _ := Step(prev, obs)
			if got.ForkCandidateSince != 2000 {
				t.Errorf("clock = %d, want it still running from 2000", got.ForkCandidateSince)
			}
			if got.ForkCandidate == nil {
				t.Error("the separation was forgotten because a view went quiet")
			}
		})
	}
}

// The whole sequence, as it actually unfolds: a disagreement opens, the second
// node dips in and out of resynchronising throughout, and the split is confirmed on
// time regardless.
func TestASplitIsConfirmedDespiteAViewFlappingThroughout(t *testing.T) {
	t.Parallel()

	state := NewState()
	state.Phase = PhaseArmed
	state.SFHealth, state.SQHealth = chainview.HealthOK, chainview.HealthOK

	const start, threshold = int64(2000), int64(600)
	at := start
	for at <= start+threshold {
		obs := healthy(at)
		obs.SFTip = meta(0x10, 101, start) // the user's chain has stalled
		obs.SQTip = meta(0x20, 103, at)
		obs.ForkCandidate = ref(100)
		// Every other tick the second view is busy catching up and reports no tip.
		if at/60%2 == 0 {
			obs.SQTip, obs.SQHealth = nil, chainview.HealthSyncing
		}
		state, _ = Step(state, obs)
		at += 60
	}

	if state.Phase != PhaseSplit {
		t.Fatalf("phase = %q after the disagreement outlasted the threshold, want %q",
			state.Phase, PhaseSplit)
	}
	if state.Fork == nil || state.Fork.Height != 100 {
		t.Errorf("separation point = %v, want height 100", state.Fork)
	}
}

// The suspicion ladder: what the user is told, at each stage, on the way to a
// confirmed split. Silence is not one of the rungs.
func TestSuspicionRisesBeforeASplitIsConfirmed(t *testing.T) {
	t.Parallel()

	armed := func() State {
		s := NewState()
		s.Phase = PhaseArmed
		s.SFHealth, s.SQHealth = chainview.HealthOK, chainview.HealthOK
		return s
	}
	// Distinct hashes per height, because new blocks are recognised by hash: a
	// helper that reuses one up a chain produces tips the engine correctly ignores,
	// and a test written on it proves nothing while appearing to pass.
	diverging := func(at int64, sfHeight, sqHeight int32) Observation {
		o := healthy(at)
		o.SFTip = blockOn(0x10, sfHeight, at)
		o.SQTip = blockOn(0x20, sqHeight, at)
		o.ForkCandidate = ref(100)
		o.SharedHeightAgreed = false
		return o
	}

	// A block each, just noticed: a disagreement, but nothing yet to distinguish it
	// from two blocks found at the same instant.
	fresh, _ := Step(armed(), diverging(2000, 101, 101))
	if fresh.ForkCandidate == nil {
		t.Fatal("the disagreement was not noticed at all")
	}
	if fresh.SplitSuspected {
		t.Error("a pair of blocks found at once was called a possible split immediately")
	}

	// The same disagreement, still standing after the suspect threshold.
	prev := fresh
	lasted, _ := Step(prev, diverging(2000+120, 101, 101))
	if !lasted.SplitSuspected {
		t.Error("a disagreement that outlasted a stale-block race was not called out")
	}
	if lasted.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q — suspicion is not confirmation", lasted.Phase, PhaseArmed)
	}

	// Or, without waiting at all: one chain pulls further ahead and the other has
	// not followed it. That is not something a lagging node does.
	ahead, _ := Step(fresh, diverging(2001, 101, 102))
	if !ahead.SplitSuspected {
		t.Error("one chain pulling ahead unfollowed was not called out")
	}
}

// Suspicion is dropped the moment the chains agree again, so a resolved
// stale-block race does not leave a warning standing.
func TestSuspicionIsDroppedWhenTheChainsReconcile(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000
	prev.SplitSuspected = true

	got, _ := Step(prev, healthy(2200))
	if got.SplitSuspected {
		t.Error("a warning outlived the disagreement that caused it")
	}
}

// The confirmation rule that reaches a stalling chain: one side has built the
// confirmation depth past the separation and the other has not followed it, for
// long enough that relay cannot explain it.
//
// Requiring *both* sides to build discards this entirely — and it is the strongest
// evidence available short of the user's own node rejecting a block, because a node
// adopts a heavier valid chain within seconds of seeing one.
func TestASplitIsConfirmedWhenOneChainLeadsAndTheOtherDoesNotFollow(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	// Symmetric: it must not matter which side is the one that stalled.
	for name, tc := range map[string]struct{ sf, sq int32 }{
		"the user's own chain stalled": {101, 103},
		"the other chain stalled":      {103, 101},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obs := healthy(2000 + 600)
			obs.SFTip = meta(0x10, tc.sf, 2000+600)
			obs.SQTip = meta(0x20, tc.sq, 2000+600)
			obs.ForkCandidate = ref(100)

			got, _ := Step(prev, obs)
			if got.Phase != PhaseSplit {
				t.Errorf("phase = %q, want %q", got.Phase, PhaseSplit)
			}
		})
	}
}

// Persistence alone is not enough: neither chain having built past the separation
// means there is nothing to say a node refused to follow, so it stays a warning
// rather than becoming a finding.
func TestPersistenceAloneDoesNotConfirmASplit(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000

	obs := healthy(2000 + 6000)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x20, 101, 2000)
	obs.ForkCandidate = ref(100)

	got, _ := Step(prev, obs)
	if got.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q with one block on each side and nothing to follow",
			got.Phase, PhaseArmed)
	}
	if !got.SplitSuspected {
		t.Error("a long-standing disagreement was not even flagged as possible")
	}
}

// Straight after a restart the clock is restored from storage but no tip has been
// observed yet, so the depths are unknown. Unknown must read as "not enough to
// confirm", never as a leading chain nobody followed.
func TestARestoredClockWithNoTipsYetConfirmsNothing(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.ForkCandidate = ref(100)
	prev.ForkCandidateSince = 2000 // restored from storage, long expired
	// Tips deliberately absent, as they are before the first successful read.

	obs := healthy(2000 + 6000)
	obs.SFTip, obs.SFHealth = nil, chainview.HealthDown
	obs.SQTip, obs.SQHealth = nil, chainview.HealthDown

	got, _ := Step(prev, obs)
	if got.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q with no tips to measure", got.Phase, PhaseArmed)
	}
}

// A rejected branch settles a split on its own, and it can land on a tick where the
// tracked candidate is empty — one view sitting on the separation rather than past
// it. The separation from this tick's search is then the only one available, and
// recording nothing would lose where the chains parted.
func TestARejectedBranchRecordsTheSeparationFromTheSearchWhenNoneIsTracked(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.SQTip = meta(0x20, 101, 1900)

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 100, 2000) // on the separation, not past it
	obs.SQTip = meta(0x20, 101, 2000)
	obs.ForkCandidate = ref(100)
	obs.SFTips = []chainview.ChainTip{
		{Hash: hash(0x20), Height: 101, BranchLen: 1, Status: chainview.TipInvalid},
	}

	got, _ := Step(prev, obs)
	if got.Phase != PhaseSplit {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseSplit)
	}
	if got.ForkCandidate != nil {
		t.Errorf("a view on the separation was tracked as being past it: %v", got.ForkCandidate)
	}
	if got.Fork == nil || got.Fork.Height != 100 {
		t.Errorf("separation = %v, want the search result at height 100", got.Fork)
	}
}

// Two nodes answering "what is the block at height N?" differently is the plainest
// evidence of a split, and the only one a user can check against a block explorer
// in a few seconds. It has to work when the separation search does not.
//
// That search walks back from both tips and can come up short — a pruned view, or a
// separation older than its budget — and when it did, nothing noticed anything at
// all. A comparison at one shared height cannot fail that way.
func TestADirectHashDisagreementIsCalledOutEvenWhenTheSeparationCannotBeFound(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	diverging := func(at int64) Observation {
		o := healthy(at)
		o.SFTip = meta(0x10, 101, at)
		o.SQTip = meta(0x20, 101, at)
		// The walk failed, so there is no separation point and no depth arithmetic.
		o.ForkCandidate, o.ForkSearchFailed = nil, true
		o.Disagreement = &HeightDisagreement{Height: 101, SFHash: hash(0x10), SQHash: hash(0x20)}
		return o
	}

	seen, _ := Step(prev, diverging(2000))
	if seen.Disagreement == nil || seen.DisagreementSince != 2000 {
		t.Fatalf("the direct disagreement was not recorded: %+v", seen.Disagreement)
	}
	if seen.ForkCandidate != nil {
		t.Error("a separation point was invented without the search having found one")
	}
	if seen.SplitSuspected {
		t.Error("called it before it had outlasted a stale-block race")
	}

	lasted, _ := Step(seen, diverging(2000+120))
	if !lasted.SplitSuspected {
		t.Error("a standing hash disagreement was not called out with no separation point")
	}
	// Suspicion is not confirmation: a split fixes a separation point, and there is
	// none to fix.
	if lasted.Phase != PhaseArmed {
		t.Errorf("phase = %q, want %q with nowhere recorded as the separation",
			lasted.Phase, PhaseArmed)
	}
}

// Both hashes are kept, not just the fact that they differ, because that is what
// makes the claim checkable rather than something to be taken on trust.
func TestTheDisagreementCarriesBothChainsAnswers(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK

	obs := healthy(2000)
	obs.SFTip = meta(0x10, 101, 2000)
	obs.SQTip = meta(0x20, 103, 2000)
	obs.ForkCandidate = ref(100)
	obs.Disagreement = &HeightDisagreement{Height: 101, SFHash: hash(0x10), SQHash: hash(0xAB)}

	got, _ := Step(prev, obs)
	if got.Disagreement == nil {
		t.Fatal("nothing was recorded")
	}
	if got.Disagreement.Height != 101 {
		t.Errorf("height = %d, want 101", got.Disagreement.Height)
	}
	if got.Disagreement.SFHash != hash(0x10) || got.Disagreement.SQHash != hash(0xAB) {
		t.Errorf("both answers were not kept: %+v", got.Disagreement)
	}
}

// A comparison that came back equal ends the disagreement. A tick that could not
// compare does not — the same rule the separation clock follows, because an
// unreadable node has not reconciled with anything.
func TestTheHashDisagreementClearsOnlyOnAComparisonThatMatched(t *testing.T) {
	t.Parallel()

	prev := NewState()
	prev.Phase = PhaseArmed
	prev.SFHealth, prev.SQHealth = chainview.HealthOK, chainview.HealthOK
	prev.Disagreement = &HeightDisagreement{Height: 101, SFHash: hash(0x10), SQHash: hash(0x20)}
	prev.DisagreementSince = 2000

	blind := healthy(2100)
	blind.SQTip, blind.SQHealth = nil, chainview.HealthSyncing
	blind.SharedHeightAgreed = false // nothing was compared, because nothing answered
	if got, _ := Step(prev, blind); got.Disagreement == nil {
		t.Error("a disagreement was forgotten because a node could not be read")
	}

	// A tick where both views answered with tips but the comparison itself could
	// not be made — a lookup that errored. Still not reconciliation.
	unread := healthy(2100)
	unread.SFTip = meta(0x10, 101, 2100)
	unread.SQTip = meta(0x20, 101, 2100)
	unread.SharedHeightAgreed = false
	if got, _ := Step(prev, unread); got.Disagreement == nil {
		t.Error("a failed lookup was treated as the chains having reconciled")
	}

	// Now they agree: compared, and equal.
	if got, _ := Step(prev, healthy(2100)); got.Disagreement != nil ||
		got.DisagreementSince != 0 {
		t.Errorf("a matched comparison did not clear the disagreement: %+v", got.Disagreement)
	}
}

// A view that goes quiet for a tick must not switch the warning off and on again.
//
// Every change of this flag publishes, so a flicker is not cosmetic: it announces
// that the possible split has passed and then re-announces the split. The second
// node dips into resynchronising as it follows a chain — one observed installation
// did it every few minutes all day — so this would have been the normal case, not
// an edge one.
func TestTheEarlyWarningDoesNotFlickerWhenAViewGoesQuiet(t *testing.T) {
	t.Parallel()

	state := NewState()
	state.Phase = PhaseArmed
	state.SFHealth, state.SQHealth = chainview.HealthOK, chainview.HealthOK

	seen := func(at int64, sqSyncing bool) Observation {
		o := healthy(at)
		o.SFTip = blockOn(0x10, 101, at)
		o.SQTip = blockOn(0x20, 103, at) // two clear of the separation, unfollowed
		o.ForkCandidate = ref(100)
		o.SharedHeightAgreed = false
		if sqSyncing {
			o.SQTip, o.SQHealth = nil, chainview.HealthSyncing
		}
		return o
	}

	// Well inside the suspect window, so only the depth rule can be carrying it.
	state, _ = Step(state, seen(2000, false))
	if !state.SplitSuspected {
		t.Fatal("a chain two clear of the separation was not called out")
	}

	for i, at := range []int64{2010, 2020, 2030} {
		state, _ = Step(state, seen(at, i%2 == 0))
		if !state.SplitSuspected {
			t.Fatalf("the warning switched off at %d because a view went quiet", at)
		}
	}
}
