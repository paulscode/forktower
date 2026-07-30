package sentinel

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
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
		At:                at,
		SFTip:             meta(1, 100, at),
		SQTip:             meta(1, 100, at),
		SFHealth:          chainview.HealthOK,
		SQHealth:          chainview.HealthOK,
		SplitConfirmDepth: 3,
		StallFactor:       6.0,
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
