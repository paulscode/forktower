// Package sentinel decides whether the two chain views have diverged.
//
// It owns the authoritative answer to "are we in a split", the point at which
// the chains separated, and per-branch telemetry. Pure decision logic is kept
// separate from the I/O that feeds it so the logic is testable without a
// network or a database.
package sentinel

import (
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// Phase is how far apart the two chains are known to be.
//
// Declared here rather than taken from the storage layer, because this file may
// not depend on storage — the separation is what lets every transition be tested
// without a database. A test asserts the two sets of names agree, so they cannot
// drift apart.
type Phase string

// Phases. Reaching a resolved phase is an operator's judgement, never the
// daemon's: it observes divergence and does not adjudicate outcomes.
const (
	// PhaseUnarmed means there are not yet two healthy views to compare. A fresh
	// install sits here for as long as its second view takes to sync, which is
	// hours or days — user-facing text must say "getting ready", because "unarmed"
	// reads as "not protecting you" when the honest answer is "not yet".
	PhaseUnarmed Phase = "UNARMED"
	// PhaseArmed means both views are healthy and agree.
	PhaseArmed Phase = "ARMED"
	// PhaseSplit means they have persistently diverged.
	PhaseSplit Phase = "SPLIT"
	// PhaseResolving means one chain appears to have stopped advancing.
	PhaseResolving Phase = "RESOLVING"
	// PhaseResolvedSFWon and PhaseResolvedSQWon are set only by an operator
	// confirming what happened, and change no behaviour: watching continues.
	PhaseResolvedSFWon Phase = "RESOLVED_SF_WON"
	PhaseResolvedSQWon Phase = "RESOLVED_SQ_WON"
)

// Valid reports whether p is a known phase.
func (p Phase) Valid() bool {
	switch p {
	case PhaseUnarmed, PhaseArmed, PhaseSplit, PhaseResolving,
		PhaseResolvedSFWon, PhaseResolvedSQWon:
		return true
	default:
		return false
	}
}

// Resolved reports whether an operator has recorded an outcome.
func (p Phase) Resolved() bool {
	switch p {
	case PhaseResolvedSFWon, PhaseResolvedSQWon:
		return true
	case PhaseUnarmed, PhaseArmed, PhaseSplit, PhaseResolving:
		return false
	default:
		return false
	}
}

// AllPhases lists every phase, for diagnostics and parity checks.
func AllPhases() []Phase {
	return []Phase{
		PhaseUnarmed, PhaseArmed, PhaseSplit, PhaseResolving,
		PhaseResolvedSFWon, PhaseResolvedSQWon,
	}
}

// bothDownGrace is how long both views must be unreachable before it is reported.
//
// An outage never changes the phase. Losing sight of the chains is not evidence
// that they came back together, and forgetting a recorded split because the
// machine was rebooted would be the most damaging thing this component could do.
const bothDownGrace = 10 * time.Minute

// Observation is everything the decision logic is told about one tick.
//
// Assembled by the shell, which does all the I/O. Anything requiring a network
// call — the separation-point search, recent ancestry — arrives here as a result,
// so that every transition can be exercised by constructing a value.
type Observation struct {
	// At is the current time in unix seconds, injected rather than read, so
	// escalation and stall behaviour can be tested without waiting.
	At int64

	// SFTip and SQTip are the two views' tips, nil when a view could not be read
	// this tick.
	SFTip *chainview.BlockMeta
	SQTip *chainview.BlockMeta

	// SFHealth and SQHealth are what each view says about itself.
	SFHealth chainview.HealthState
	SQHealth chainview.HealthState

	// SFTips is every branch tip the user's own node knows about, including the
	// ones it has rejected. A rejected branch is local, unforgeable evidence of a
	// rule disagreement, which is why it is worth carrying.
	SFTips []chainview.ChainTip

	// SQAncestry is recent block hashes on the other chain, newest first, used to
	// decide whether a rejected branch is the one we are watching and whether two
	// nearby tips are on the same chain.
	SQAncestry []chainhash.Hash
	// SFAncestry is the same for the user's own chain.
	SFAncestry []chainhash.Hash

	// ForkCandidate is the separation point the shell found this tick, or nil if
	// the search came up short.
	ForkCandidate *chainview.BlockRef
	// ForkSearchFailed distinguishes "not searched" from "searched and could not
	// tell", which are different pieces of evidence.
	ForkSearchFailed bool

	// OperatorOutcome, when set, is an operator recording which chain persisted.
	// Only meaningful while resolving.
	OperatorOutcome Phase

	// Configuration, supplied per tick so this file need not read it.
	SplitConfirmDepth int32
	StallFactor       float64
	// DivergenceHeight is the first height at which the two rule sets could
	// disagree, or zero when unknown.
	DivergenceHeight int32
	// ReorgMargin is the safety margin below the user's tip when bounding how far
	// their chain is treated as already verified.
	ReorgMargin int32
}

// State is everything the decision logic remembers between ticks.
type State struct {
	Phase Phase

	// Fork is the recorded separation point, set when a split is confirmed. Once
	// set it does not move: it anchors rescans and decides which channels are
	// exposed, so revising it would silently invalidate both.
	Fork *chainview.BlockRef
	// ForkCandidate is the most recent search result while still armed — a
	// separation that has not yet persisted long enough to be believed.
	ForkCandidate *chainview.BlockRef
	// DetectedAt is when the split was confirmed, in unix seconds.
	DetectedAt int64

	SFTip *chainview.BlockMeta
	SQTip *chainview.BlockMeta

	SFHealth chainview.HealthState
	SQHealth chainview.HealthState

	SFCadence Cadence
	SQCadence Cadence

	// LastSFBlockAt and LastSQBlockAt are when a new block was last *seen*, in
	// unix seconds. A measurement, unlike a header timestamp, which is why the
	// stall test uses these.
	LastSFBlockAt int64
	LastSQBlockAt int64

	// BothDownSince is when both views last became unreachable, or zero.
	BothDownSince int64

	// ResolvingCandidate is which chain appears to have persisted, while
	// resolving. A suggestion for the operator, never a conclusion.
	ResolvingCandidate Phase

	// TrustAnchor is the height up to which the user's own chain is treated as
	// already verified.
	TrustAnchor int32
}

// NewState returns the starting state for a daemon that has never run.
func NewState() State {
	return State{
		Phase:     PhaseUnarmed,
		SFHealth:  chainview.HealthDown,
		SQHealth:  chainview.HealthDown,
		SFCadence: NewCadence(),
		SQCadence: NewCadence(),
	}
}

// EffectKind names something the shell must do as a result of a tick.
type EffectKind string

// Effects. The decision logic decides; the shell performs. Keeping them apart is
// what lets the transition table be tested without a database, a bus or a node.
const (
	// EffectPersistState saves the split record. Always emitted before any event
	// describing the same change, so a crash between the two leaves storage ahead
	// of the notification rather than behind it.
	EffectPersistState EffectKind = "persist_state"
	// EffectPhaseChanged announces a phase transition.
	EffectPhaseChanged EffectKind = "phase_changed"
	// EffectBranchExtended announces a new block on one chain, carrying the
	// cadence estimate that turns a countdown in blocks into one in time.
	EffectBranchExtended EffectKind = "branch_extended"
	// EffectHealthChanged announces that a view became more or less trustworthy.
	EffectHealthChanged EffectKind = "health_changed"
	// EffectBothViewsDown reports that neither chain can be seen. Deliberately not
	// a phase change: an outage is not evidence that a split ended.
	EffectBothViewsDown EffectKind = "both_views_down"
	// EffectTrustAnchorChanged records how far the user's own chain is treated as
	// verified.
	EffectTrustAnchorChanged EffectKind = "trust_anchor_changed"
)

// Effect is one thing for the shell to do.
type Effect struct {
	Kind EffectKind

	// Branch is which chain an effect concerns, where that applies.
	Branch chainview.Branch

	// OldPhase and NewPhase are set on a phase change.
	OldPhase Phase
	NewPhase Phase

	// Block is set when a chain gained one.
	Block *chainview.BlockMeta
	// SinceForkDepth is how far that chain has advanced past the separation point.
	SinceForkDepth int32
	// IntervalSecs is the cadence estimate for that chain.
	IntervalSecs float64

	// OldHealth and NewHealth are set on a health change.
	OldHealth chainview.HealthState
	NewHealth chainview.HealthState

	// Height carries a height, for the trust anchor.
	Height int32

	// Detail is a short human explanation, safe to show a user.
	Detail string
}

// Step advances the state by one observation, reporting what the shell must do.
//
// Pure: no clock, no storage, no network, no logging. Every transition in the
// specification is reachable by constructing an Observation, which is the point —
// the behaviour that matters here is almost impossible to provoke against real
// chains, so it has to be provable without them.
func Step(prev State, obs Observation) (State, []Effect) {
	next := prev
	var effects []Effect

	effects = append(effects, trackHealth(&next, obs)...)
	effects = append(effects, trackBlocks(&next, obs)...)
	effects = append(effects, trackTrustAnchor(&next, obs)...)

	// The phase is decided last, so it sees this tick's blocks and health.
	newPhase, detail := decidePhase(next, obs)
	if newPhase != next.Phase {
		old := next.Phase
		applyPhase(&next, newPhase, obs)
		effects = append(effects,
			Effect{Kind: EffectPersistState},
			Effect{
				Kind:     EffectPhaseChanged,
				OldPhase: old,
				NewPhase: newPhase,
				Detail:   detail,
			})
	}
	return next, effects
}

// trackHealth records each view's health and reports changes.
func trackHealth(next *State, obs Observation) []Effect {
	var effects []Effect

	if obs.SFHealth != "" && obs.SFHealth != next.SFHealth {
		effects = append(effects, Effect{
			Kind:      EffectHealthChanged,
			Branch:    chainview.BranchSF,
			OldHealth: next.SFHealth,
			NewHealth: obs.SFHealth,
		})
		next.SFHealth = obs.SFHealth
	}
	if obs.SQHealth != "" && obs.SQHealth != next.SQHealth {
		effects = append(effects, Effect{
			Kind:      EffectHealthChanged,
			Branch:    chainview.BranchSQ,
			OldHealth: next.SQHealth,
			NewHealth: obs.SQHealth,
		})
		next.SQHealth = obs.SQHealth
	}

	// Both views unreachable. Reported once the grace period has passed, and never
	// as a phase change: an outage is not evidence about the chains.
	bothDown := next.SFHealth == chainview.HealthDown && next.SQHealth == chainview.HealthDown
	switch {
	case bothDown && next.BothDownSince == 0:
		next.BothDownSince = obs.At
	case bothDown && obs.At-next.BothDownSince >= int64(bothDownGrace/time.Second):
		effects = append(effects, Effect{
			Kind:   EffectBothViewsDown,
			Detail: "neither chain can be reached; the recorded state is kept",
		})
		// Reset so this is reported periodically rather than on every tick.
		next.BothDownSince = obs.At
	case !bothDown:
		next.BothDownSince = 0
	}
	return effects
}

// trackBlocks folds new blocks into the cadence estimates and reports them.
func trackBlocks(next *State, obs Observation) []Effect {
	var effects []Effect

	if e, ok := trackBranch(chainview.BranchSF, obs.SFTip, &next.SFTip,
		&next.SFCadence, &next.LastSFBlockAt, next.Fork, obs.At); ok {
		effects = append(effects, e)
	}
	if e, ok := trackBranch(chainview.BranchSQ, obs.SQTip, &next.SQTip,
		&next.SQCadence, &next.LastSQBlockAt, next.Fork, obs.At); ok {
		effects = append(effects, e)
	}
	return effects
}

func trackBranch(
	branch chainview.Branch,
	observed *chainview.BlockMeta,
	stored **chainview.BlockMeta,
	cadence *Cadence,
	lastSeen *int64,
	fork *chainview.BlockRef,
	now int64,
) (Effect, bool) {
	if observed == nil {
		return Effect{}, false
	}
	if *stored != nil && (*stored).Hash == observed.Hash {
		return Effect{}, false
	}

	tip := *observed
	*stored = &tip
	*cadence = cadence.Observe(tip.Time.Unix())
	*lastSeen = now

	var depth int32
	if fork != nil {
		depth = tip.Height - fork.Height
	}

	return Effect{
		Kind:           EffectBranchExtended,
		Branch:         branch,
		Block:          &tip,
		SinceForkDepth: depth,
		IntervalSecs:   cadence.IntervalSecs,
	}, true
}

// trackTrustAnchor recomputes how far the user's own chain is treated as verified.
func trackTrustAnchor(next *State, obs Observation) []Effect {
	anchor, ok := TrustAnchorHeight(obs, next.Fork)
	if !ok || anchor == next.TrustAnchor {
		return nil
	}
	next.TrustAnchor = anchor
	return []Effect{{
		Kind:   EffectTrustAnchorChanged,
		Height: anchor,
		Detail: "history below this height is shared, so it is taken as already verified",
	}}
}

// TrustAnchorHeight bounds how far the user's own node counts as verified history.
//
// The minimum of every bound available, never one of them alone:
//
//   - one below the first height at which the rule sets could disagree, when that
//     is known;
//   - one below where the user's own node reports having rejected a branch;
//   - a margin below their tip, to stay clear of an ordinary reorganisation.
//
// History from before the chains could differ is shared, so trusting it costs
// nothing. Trusting anything above that point is the failure this bound exists to
// prevent: for someone installing after a split has begun, their tip is already on
// one side, and anchoring there would tie the second view to blocks that only
// exist on the first. An anchor set too low costs a little redundant checking; one
// set too high costs the chain. So it is biased low, and reports false when nothing
// bounds it at all.
func TrustAnchorHeight(obs Observation, fork *chainview.BlockRef) (int32, bool) {
	const none = int32(-1)
	best := none

	consider := func(candidate int32) {
		if candidate < 0 {
			return
		}
		if best == none || candidate < best {
			best = candidate
		}
	}

	if obs.DivergenceHeight > 0 {
		consider(obs.DivergenceHeight - 1)
	}
	if h, ok := earliestRejectedHeight(obs.SFTips); ok {
		consider(h - 1)
	}
	if fork != nil {
		consider(fork.Height)
	}
	if obs.SFTip != nil && obs.ReorgMargin > 0 {
		consider(obs.SFTip.Height - obs.ReorgMargin)
	}

	if best == none {
		return 0, false
	}
	if best < 0 {
		best = 0
	}
	return best, true
}

// earliestRejectedHeight returns the lowest height at which the user's own node
// reports having rejected a branch.
//
// Evidence from their own node, so no peer can fabricate it — which is why it is
// allowed to bound the anchor.
func earliestRejectedHeight(tips []chainview.ChainTip) (int32, bool) {
	found := false
	lowest := int32(0)
	for _, t := range tips {
		if !t.Rejected() {
			continue
		}
		// The rejected tip's height is the top of that branch; its base is where the
		// disagreement began.
		base := t.Height - t.BranchLen
		if base < 0 {
			base = 0
		}
		if !found || base < lowest {
			lowest, found = base, true
		}
	}
	return lowest, found
}

// decidePhase applies the transition table, in order, returning the phase for
// this tick and a short explanation when it changes.
func decidePhase(s State, obs Observation) (phase Phase, detail string) {
	// An operator's recorded outcome wins from anywhere, because it is a statement
	// about the world rather than an inference about it.
	if s.Phase == PhaseResolving && obs.OperatorOutcome.Resolved() {
		return obs.OperatorOutcome, "an operator recorded which chain persisted"
	}

	switch s.Phase {
	case PhaseUnarmed:
		if s.SFHealth.Usable() && s.SQHealth.Usable() &&
			tipsAgree(s.SFTip, s.SQTip, obs.SplitConfirmDepth, obs.SFAncestry, obs.SQAncestry) {
			return PhaseArmed, "both chains can be seen and they agree"
		}
		return PhaseUnarmed, ""

	case PhaseArmed:
		return decideFromArmed(s, obs)

	case PhaseSplit:
		return decideFromSplit(s, obs)

	case PhaseResolving:
		// The chain that had gone quiet has started producing again, so the split is
		// not ending after all.
		if !stalledBranch(s, obs, s.ResolvingCandidate) {
			return PhaseSplit, "the chain that had gone quiet is producing blocks again"
		}
		return PhaseResolving, ""

	case PhaseResolvedSFWon, PhaseResolvedSQWon:
		// Terminal. Recorded outcomes are not revisited by inference, and they change
		// no behaviour: watching, deadlines and alerts all continue regardless.
		return s.Phase, ""

	default:
		// An unrecognised stored phase. Start over rather than acting on a value
		// nothing here understands.
		return PhaseUnarmed, "the recorded state was not recognised, so it was reset"
	}
}

func decideFromArmed(s State, obs Observation) (phase Phase, detail string) {
	if tipsAgree(s.SFTip, s.SQTip, obs.SplitConfirmDepth, obs.SFAncestry, obs.SQAncestry) {
		return PhaseArmed, ""
	}

	// The user's own node has fetched a block from the other chain and refused it.
	// The strongest evidence available, and it needs no agreement from any peer, so
	// it is enough on its own.
	if s.SQTip != nil {
		for _, tip := range obs.SFTips {
			if invalidTipMatchesSQ(obs.SQAncestry, s.SQTip.Hash, tip) {
				return PhaseSplit, "your node has rejected a block from the other chain"
			}
		}
	}

	// Otherwise both chains must have built far enough past the separation point
	// that this cannot be ordinary reorganisation noise.
	if obs.ForkCandidate != nil && s.SFTip != nil && s.SQTip != nil {
		depth := obs.SplitConfirmDepth
		if depth < 1 {
			depth = 1
		}
		sfDepth := divergedDepth(s.SFTip.Height, obs.ForkCandidate.Height)
		sqDepth := divergedDepth(s.SQTip.Height, obs.ForkCandidate.Height)
		if sfDepth >= depth && sqDepth >= depth {
			return PhaseSplit, "both chains have built past the point where they separated"
		}
	}

	return PhaseArmed, ""
}

func decideFromSplit(s State, obs Observation) (phase Phase, detail string) {
	sfStalled := stalledBranch(s, obs, PhaseResolvedSQWon)
	sqStalled := stalledBranch(s, obs, PhaseResolvedSFWon)

	// Exactly one side quiet suggests the other persisted. Both quiet says nothing
	// — that is a network problem, not a resolution.
	switch {
	case sqStalled && !sfStalled:
		return PhaseResolving, "the other chain has stopped producing blocks"
	case sfStalled && !sqStalled:
		return PhaseResolving, "the chain your node follows has stopped producing blocks"
	default:
		return PhaseSplit, ""
	}
}

// stalledBranch reports whether the chain implied by candidate has gone quiet.
//
// The candidate names which chain would have *persisted*, so the stalled one is
// the other. Symmetric on purpose: whether the user's own node is the quiet one
// depends entirely on where the hashing power goes, and assuming it will be the
// other chain would leave the more likely case unhandled.
func stalledBranch(s State, obs Observation, candidate Phase) bool {
	switch candidate {
	case PhaseResolvedSFWon:
		return s.SQCadence.Stalled(obs.At, s.LastSQBlockAt, obs.StallFactor)
	case PhaseResolvedSQWon:
		return s.SFCadence.Stalled(obs.At, s.LastSFBlockAt, obs.StallFactor)
	case PhaseUnarmed, PhaseArmed, PhaseSplit, PhaseResolving:
		return false
	default:
		return false
	}
}

// applyPhase records the bookkeeping that goes with entering a phase.
func applyPhase(s *State, to Phase, obs Observation) {
	switch to {
	case PhaseSplit:
		if s.Fork == nil && obs.ForkCandidate != nil {
			fork := *obs.ForkCandidate
			s.Fork = &fork
			s.DetectedAt = obs.At
		} else if s.Fork == nil {
			// Confirmed by a rejected branch without the search having succeeded. The
			// split is real and the separation point is not known yet; the search keeps
			// running and will fill it in. Better to record the split now than to wait
			// for a detail that is not needed to raise the alarm.
			s.DetectedAt = obs.At
		}
		s.ResolvingCandidate = ""
	case PhaseResolving:
		s.ResolvingCandidate = resolvingCandidate(*s, obs)
	case PhaseArmed:
		if obs.ForkCandidate != nil {
			candidate := *obs.ForkCandidate
			s.ForkCandidate = &candidate
		} else {
			s.ForkCandidate = nil
		}
	case PhaseUnarmed, PhaseResolvedSFWon, PhaseResolvedSQWon:
		// Nothing extra. A recorded outcome deliberately changes no behaviour.
	}
	s.Phase = to
}

func resolvingCandidate(s State, obs Observation) Phase {
	if s.SQCadence.Stalled(obs.At, s.LastSQBlockAt, obs.StallFactor) {
		return PhaseResolvedSFWon
	}
	return PhaseResolvedSQWon
}

// divergedDepth is how far a tip has advanced past the separation point.
//
// Clamped at zero: the separation point is an ancestor of both tips by
// construction, so a negative result means the inputs disagree, and reporting a
// negative depth would let a nonsense value flow into the confirmation test.
func divergedDepth(tipHeight, forkHeight int32) int32 {
	if tipHeight < forkHeight {
		return 0
	}
	return tipHeight - forkHeight
}

// tipsAgree reports whether two tips are on the same chain.
//
// Not exact equality. Two independently peered nodes are routinely a block apart
// for tens of seconds, so requiring identical tips would leave the daemon flapping
// or never settling. Within the confirmation depth, one tip being an ancestor of
// the other counts as agreement — the chains have not diverged, one view is merely
// slightly behind.
func tipsAgree(sf, sq *chainview.BlockMeta, depth int32, sfAncestry, sqAncestry []chainhash.Hash) bool {
	if sf == nil || sq == nil {
		// A view that could not be read is not evidence of agreement.
		return false
	}
	if sf.Hash == sq.Hash {
		return true
	}
	if depth < 1 {
		depth = 1
	}

	// Whichever is behind should appear in the other's recent history.
	if sf.Height < sq.Height && sq.Height-sf.Height <= depth {
		return containsWithin(sqAncestry, sf.Hash, int(depth)+1)
	}
	if sq.Height < sf.Height && sf.Height-sq.Height <= depth {
		return containsWithin(sfAncestry, sq.Hash, int(depth)+1)
	}
	// Same height, different hash: a genuine disagreement, not a lag.
	return false
}

// containsWithin reports whether h is among the first limit entries, which are
// ordered newest first.
func containsWithin(hashes []chainhash.Hash, h chainhash.Hash, limit int) bool {
	for i, candidate := range hashes {
		if i >= limit {
			return false
		}
		if candidate == h {
			return true
		}
	}
	return false
}

// invalidTipMatchesSQ reports whether a branch the user's own node rejected is the
// chain being watched.
//
// Matching matters because a node may know of rejected branches for all sorts of
// reasons; only one that leads to the chain under watch is evidence about this
// split. Either the rejected tip is that chain's tip outright, or it appears
// within the recent history the shell supplied — bounded by the branch's own
// length, since a rejected branch cannot imply more divergence than it has.
func invalidTipMatchesSQ(sqAncestry []chainhash.Hash, sqTip chainhash.Hash, tip chainview.ChainTip) bool {
	if !tip.Rejected() {
		return false
	}
	if tip.Hash == sqTip {
		return true
	}
	limit := int(tip.BranchLen)
	if limit < 1 {
		limit = 1
	}
	return containsWithin(sqAncestry, tip.Hash, limit)
}
