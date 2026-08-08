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

	// Disagreement is a direct comparison of the two views' block hashes at one
	// height they both reach, or nil when they agreed there or it could not be read.
	//
	// Everything else about a separation is learned by walking back from the two
	// tips, which is the only way to find *where* they parted — but it is also a
	// walk that can fail, on a pruned view or a separation older than the search
	// allows, and when it fails nothing else notices anything at all. Asking each
	// node for its hash at one shared height cannot fail that way: it is two
	// lookups, it needs no history, and it answers the one question a user would
	// ask by hand. It is also the number they can check against a block explorer,
	// which is why it is carried through to the screen rather than only used here.
	Disagreement *HeightDisagreement
	// SharedHeightAgreed is true only when that comparison was actually made and
	// came back equal.
	//
	// A separate field because a nil Disagreement means two opposite things —
	// "asked, and they matched" and "could not ask" — and only the first ends a
	// disagreement. Inferring it from the surrounding fields instead let a single
	// failed lookup read as reconciliation and wipe a standing disagreement,
	// restarting a clock that is supposed to measure how long one has lasted.
	SharedHeightAgreed bool

	// OperatorOutcome, when set, is an operator recording which chain persisted.
	// Only meaningful while resolving.
	OperatorOutcome Phase

	// Configuration, supplied per tick so this file need not read it.
	SplitConfirmDepth int32
	// SplitConfirmSecs is how long a disagreement must persist before it is
	// believed on the strength of one chain alone having built past it.
	SplitConfirmSecs int64
	// SplitSuspectSecs is how long a disagreement must persist before the user is
	// told it may be a split. Presentation only: it confirms nothing and starts no
	// countdown, it decides when a factual note becomes a warning.
	SplitSuspectSecs int64
	StallFactor      float64
	// DivergenceHeight is the first height at which the two rule sets could
	// disagree, or zero when unknown.
	DivergenceHeight int32
	// ReorgMargin is the safety margin below the user's tip when bounding how far
	// their chain is treated as already verified.
	ReorgMargin int32
}

// HeightDisagreement is the two views answering "what is the block at height N?"
// differently.
//
// The plainest evidence of a split there is, and the only one a user can check for
// themselves in a few seconds against any block explorer. Both hashes are kept, not
// just the fact that they differ, because "they disagree" is something to be taken
// on trust and "yours is X, theirs is Y" is something to be verified.
type HeightDisagreement struct {
	Height         int32
	SFHash, SQHash chainhash.Hash
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
	// ForkCandidateSince is when the chains were first seen to have *both* built
	// past the current candidate, and have done so without interruption since. Zero
	// whenever that is not the case.
	//
	// The clock this starts is the only evidence that reaches a stalling chain. The
	// depth rule needs both sides to keep producing blocks, so a user whose own node
	// is on the side that has slowed to a crawl — the side whose owner most needs
	// telling — could wait a very long time for it. How long two chains have
	// refused to reconcile does not depend on either of them making progress.
	ForkCandidateSince int64
	// Disagreement is the direct hash comparison, and DisagreementSince is when the
	// two views were first seen to differ at a shared height without interruption.
	//
	// Kept beside the separation candidate rather than folded into it because they
	// fail independently: the walk that finds a separation point can come up short
	// while a comparison at one height still proves the chains differ. That case
	// used to leave the daemon with nothing to say.
	Disagreement      *HeightDisagreement
	DisagreementSince int64

	// SplitSuspected means the chains are disagreeing in a way that is worth
	// warning about, without that having been confirmed.
	//
	// Kept apart from the phase deliberately. The phase is what the daemon commits
	// to — it fixes a separation point, anchors rescans and decides which channels
	// count as exposed — so it is right to be slow. What the *user* is told need not
	// wait for any of that, and must not: anyone can open a block explorer and see
	// two chains, and software that stays quiet through that has taught them it is
	// not worth reading.
	SplitSuspected bool
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
	// Before the blocks, so that this tick's blocks are measured against this
	// tick's separation point rather than the previous one's.
	trackDisagreement(&next, obs)
	trackForkCandidate(&next, obs)
	effects = append(effects, trackBlocks(&next, obs)...)
	effects = append(effects, trackTrustAnchor(&next, obs)...)
	// After the blocks, so it reads tips that are both current and *retained*.
	//
	// Reading this tick's observation instead made the answer depend on whether
	// every view happened to answer this second: a node that dips into
	// resynchronising — which one observed installation did every few minutes —
	// reports no tip, the depths read as zero, and inside the first couple of
	// minutes the warning would switch off and back on again. That is not a
	// display flicker; each edge publishes, so it would have announced that the
	// possible split had passed and then re-announced it, repeatedly.
	next.SplitSuspected = suspectSplit(next, obs)

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

// trackDisagreement maintains the direct hash comparison and how long it has held.
//
// Independent of the separation search on purpose. That search is the only thing
// that can say *where* two chains parted, and it is rightly what a confirmed split
// is anchored to — but it walks history, and a walk can come up short against a
// pruned view or a separation older than its budget. This cannot: it is one lookup
// per node at a height they both reach. When the walk fails, this is the whole of
// what the daemon knows, and without it that was nothing.
//
// Cleared only on a comparison that came back equal, never on a tick that could not
// compare, for the same reason the separation clock is: an unreadable node has not
// reconciled with anything.
func trackDisagreement(next *State, obs Observation) {
	if obs.Disagreement == nil {
		// Only a comparison that was made and came back equal ends a disagreement.
		// Anything else is silence, and silence is not reconciliation.
		if obs.SharedHeightAgreed {
			next.Disagreement = nil
			next.DisagreementSince = 0
		}
		return
	}

	found := *obs.Disagreement
	moved := next.Disagreement == nil ||
		next.Disagreement.SFHash != found.SFHash ||
		next.Disagreement.SQHash != found.SQHash
	next.Disagreement = &found
	if moved || next.DisagreementSince == 0 {
		next.DisagreementSince = obs.At
	}
}

// trackForkCandidate maintains the current separation point and how long the two
// chains have been refusing to reconcile across it.
//
// Runs every tick, which the previous arrangement did not: the candidate was
// written only by applyPhase, and applyPhase runs only when the phase changes. So
// while armed — the state a split actually begins in — it held whatever had been
// found at the moment arming happened, which for a daemon that started next to two
// agreeing chains is nothing at all. Every reader of it downstream was therefore
// reading a value that never moved.
//
// **Mutual is the requirement, not merely a candidate.** A search returns a
// separation point whenever the two tips differ, and one view simply being behind
// the other satisfies that. Only when both chains hold a block the other does not
// have they actually disagreed, and only then is there anything to time.
//
// **The clock stops only on evidence that the disagreement ended**, never on the
// absence of evidence — the same rule the both-views-down grace period is built on,
// and for the same reason. A view that is resynchronising reports no tip, and a
// node briefly catching up after each new block is completely ordinary: one
// observed installation did it every few minutes, all day. Treating that as "the
// chains agree again" would restart the clock every few minutes, so a threshold
// measured in tens of minutes would rarely be reached and one measured in hours
// never — the rule would be present, tested, and dead.
func trackForkCandidate(next *State, obs Observation) {
	// Nothing was seen this tick, so nothing is concluded and the clock keeps
	// running. A view that cannot be read has not reconciled with anything.
	if obs.SFTip == nil || obs.SQTip == nil || obs.ForkSearchFailed {
		return
	}

	mutual := obs.ForkCandidate != nil &&
		obs.SFTip.Height > obs.ForkCandidate.Height &&
		obs.SQTip.Height > obs.ForkCandidate.Height

	if !mutual {
		next.ForkCandidate = nil
		next.ForkCandidateSince = 0
		return
	}

	candidate := *obs.ForkCandidate
	moved := next.ForkCandidate == nil || next.ForkCandidate.Hash != candidate.Hash
	next.ForkCandidate = &candidate
	// A separation point that moves is a different disagreement, so its clock
	// starts again. Reorganisation churn shifts it about; a real split does not.
	if moved || next.ForkCandidateSince == 0 {
		next.ForkCandidateSince = obs.At
	}
}

// suspectSplit reports whether a disagreement is worth warning about yet.
//
// Two ways in, because two quite different things are both worth saying early:
//
//   - it has outlasted a stale-block race. Two blocks found at once are reconciled
//     by the next block on either side, so a disagreement still standing after a
//     couple of minutes is already unusual.
//
//   - one chain has pulled ahead and the other has not followed. This is the
//     signal a depth rule discards by insisting both sides advance, and it is the
//     strongest thing available short of a rejected block: a node adopts a heavier
//     valid chain within seconds of seeing it, so a view sitting behind one it can
//     reach is either partitioned from it or refusing it, and both are what this
//     software was installed to notice.
//
// Neither confirms anything. They decide when a calm note becomes a warning.
// Reads the tips from the state rather than the observation, because the state
// keeps the last tip each view reported and the observation only holds what
// answered this second. A view that went quiet has not lost its chain, and this
// must not conclude anything from the silence.
func suspectSplit(s State, obs Observation) bool {
	lasted := func(since int64) bool {
		return obs.SplitSuspectSecs > 0 && since > 0 &&
			obs.At-since >= obs.SplitSuspectSecs
	}

	// Two nodes answering "what is the block at height N?" differently, and going on
	// doing so, is a split as far as anyone reading this can tell — and it holds
	// whether or not the separation point was ever found. This is the rung that
	// survives a failed search, where previously there was nothing to report.
	if s.Disagreement != nil && lasted(s.DisagreementSince) {
		return true
	}

	if s.ForkCandidate == nil || s.ForkCandidateSince == 0 {
		return false
	}
	sfDepth := divergedDepth(tipHeight(s.SFTip), s.ForkCandidate.Height)
	sqDepth := divergedDepth(tipHeight(s.SQTip), s.ForkCandidate.Height)
	if sfDepth > 1 || sqDepth > 1 {
		return true
	}
	return lasted(s.ForkCandidateSince)
}

func tipHeight(tip *chainview.BlockMeta) int32 {
	if tip == nil {
		return 0
	}
	return tip.Height
}

// separation is the point the chains are measured against: the recorded one once
// a split is confirmed, and the candidate before that.
//
// Depths reported while armed were all zero without this, because the recorded
// fork is only set at confirmation — so the dashboard showed a user watching two
// chains visibly drift apart a divergence of nought blocks on both of them.
func separation(s State) *chainview.BlockRef {
	if s.Fork != nil {
		return s.Fork
	}
	return s.ForkCandidate
}

// trackBlocks folds new blocks into the cadence estimates and reports them.
func trackBlocks(next *State, obs Observation) []Effect {
	var effects []Effect

	fork := separation(*next)
	if e, ok := trackBranch(chainview.BranchSF, obs.SFTip, &next.SFTip,
		&next.SFCadence, &next.LastSFBlockAt, fork, obs.At); ok {
		effects = append(effects, e)
	}
	if e, ok := trackBranch(chainview.BranchSQ, obs.SQTip, &next.SQTip,
		&next.SQCadence, &next.LastSQBlockAt, fork, obs.At); ok {
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
		return decideFromUnarmed(s, obs)

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

// decideFromUnarmed decides what to do before there is anything to compare
// against.
//
// Two ways out. The ordinary one is that the chains agree, which establishes the
// shared baseline everything downstream is measured from.
//
// The other exists because arming is impossible during a split — it requires
// agreement, and there is none — so a daemon started *after* a split began would
// otherwise sit here forever, showing "Getting set up, nothing to do yet" while
// the user's funds were exposed. That is the calmest message this software has,
// at the worst possible moment, and someone who installed Forktower *because*
// they heard the chains had split is the likely user, not an edge case.
//
// The evidence required is exactly the same as from armed. Nothing about having
// watched the separation happen makes it more real than finding it already
// there; what having watched provides is the shared history, and that comes from
// the trust anchor instead, which is bounded below the separation point for
// precisely this case.
func decideFromUnarmed(s State, obs Observation) (phase Phase, detail string) {
	if !s.SFHealth.Usable() || !s.SQHealth.Usable() {
		return PhaseUnarmed, ""
	}
	if tipsAgree(s.SFTip, s.SQTip, obs.SplitConfirmDepth, obs.SFAncestry, obs.SQAncestry) {
		return PhaseArmed, "both chains can be seen and they agree"
	}

	rejected, diverged, sustained := splitEvidence(s, obs)
	switch {
	case rejected:
		return PhaseSplit, "your node had already rejected a block from the other chain"
	case diverged:
		return PhaseSplit, "the chains had already separated before Forktower started"
	case sustained:
		return PhaseSplit, "the chains have been holding different blocks for long enough " +
			"that this is not an ordinary reorganisation"
	default:
		// They disagree, but not yet by enough to tell a split from one node being
		// briefly behind. Staying here is right: this is the state that says so.
		return PhaseUnarmed, ""
	}
}

func decideFromArmed(s State, obs Observation) (phase Phase, detail string) {
	if tipsAgree(s.SFTip, s.SQTip, obs.SplitConfirmDepth, obs.SFAncestry, obs.SQAncestry) {
		return PhaseArmed, ""
	}

	rejected, diverged, sustained := splitEvidence(s, obs)
	switch {
	case rejected:
		return PhaseSplit, "your node has rejected a block from the other chain"
	case diverged:
		return PhaseSplit, "both chains have built past the point where they separated"
	case sustained:
		return PhaseSplit, "the chains have gone on holding different blocks for long " +
			"enough that this is not an ordinary reorganisation"
	default:
		return PhaseArmed, ""
	}
}

// splitEvidence reports what has been seen that would justify believing the two
// chains have genuinely separated.
//
// Shared by both callers, deliberately: the same facts settle it whether the
// separation was watched happening or found already there. Only the sentence
// differs, and that is decided at the call site where it can be read.
//
// rejectedBlock is the strongest thing available — the user's own node fetched a
// block from the other chain and refused it. It needs no agreement from any peer
// and cannot be fabricated by one, so it is enough on its own.
//
// divergedFar means both chains have built past the separation point by more than
// ordinary reorganisation noise would explain.
//
// divergedSustained is the same conclusion reached from one chain rather than
// two, and it exists because divergedFar cannot be reached from one side.
//
// **Requiring both chains to advance discards the clearest evidence there is.** A
// node adopts a heavier valid chain within seconds of seeing it. So a view holding
// its own block while the other view has built several on top of a different one
// is not a view that is lagging — it is one that cannot see that chain or will not
// have it, and there is nothing else that produces that. Insisting the stalled side
// also advance ties the alarm to the progress of the chain that has, by
// hypothesis, stopped progressing; in a split with any real imbalance of hashing
// power that is the user's own node, so the worse their side does, the longer they
// wait to be told. That inverts the urgency exactly.
//
// The persistence requirement alongside it is what keeps this honest: relay is not
// instant, and a view can legitimately be a block or two behind for a short while.
// Minutes settle that. Nothing about a stale-block race survives them — it is
// resolved by the next block on either side.
func splitEvidence(s State, obs Observation) (rejectedBlock, divergedFar, divergedSustained bool) {
	if s.SQTip != nil {
		for _, tip := range obs.SFTips {
			if invalidTipMatchesSQ(obs.SQAncestry, s.SQTip.Hash, tip) {
				return true, false, false
			}
		}
	}

	depth := obs.SplitConfirmDepth
	if depth < 1 {
		depth = 1
	}

	if obs.ForkCandidate != nil && s.SFTip != nil && s.SQTip != nil {
		sfDepth := divergedDepth(s.SFTip.Height, obs.ForkCandidate.Height)
		sqDepth := divergedDepth(s.SQTip.Height, obs.ForkCandidate.Height)
		if sfDepth >= depth && sqDepth >= depth {
			return false, true, false
		}
	}

	// Read from the tracked candidate rather than this tick's search result: the
	// question is how long *this* disagreement has stood, and only the tracked one
	// carries that. It is non-zero only while both chains hold a block the other
	// does not, so the mutual test is already behind us here.
	if obs.SplitConfirmSecs <= 0 || s.ForkCandidateSince == 0 ||
		obs.At-s.ForkCandidateSince < obs.SplitConfirmSecs {
		return false, false, false
	}
	if s.ForkCandidate == nil {
		return false, false, false
	}
	lead := divergedDepth(tipHeight(s.SFTip), s.ForkCandidate.Height)
	if d := divergedDepth(tipHeight(s.SQTip), s.ForkCandidate.Height); d > lead {
		lead = d
	}
	return false, false, lead >= depth
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
		// The tracked candidate first, this tick's search second. When the split is
		// confirmed by persistence the tracked one is the separation that persisted,
		// and it is the one whose clock ran out; falling back to the search result
		// covers the tick where a rejected branch settles it before any search has.
		if s.Fork == nil && s.ForkCandidate != nil {
			fork := *s.ForkCandidate
			s.Fork = &fork
			s.DetectedAt = obs.At
		} else if s.Fork == nil && obs.ForkCandidate != nil {
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
	case PhaseArmed, PhaseUnarmed, PhaseResolvedSFWon, PhaseResolvedSQWon:
		// Nothing extra. A recorded outcome deliberately changes no behaviour, and
		// the separation candidate is now maintained every tick by
		// trackForkCandidate rather than only when the phase happens to move.
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
