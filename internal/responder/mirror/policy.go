// Package mirror decides which of the user's transactions should exist on both
// branches, and moves the ones that should.
//
// It never constructs a transaction and never signs one. Everything it moves is
// bytes that already exist, signed by somebody else, observed on one chain and
// offered to the other unchanged.
package mirror

import (
	"fmt"

	"github.com/paulscode/forktower/internal/store"
)

// Rule names which allowlist entry decided a transaction's fate.
//
// A stable identifier alongside the sentence, so that grouping and testing do
// not depend on prose that will be reworded.
type Rule string

// The rules that permit a transaction to be mirrored.
const (
	// RuleCoopClose mirrors a cooperative close. Both parties agreed to it, and
	// leaving it settled on one branch and open on the other is the exposure this
	// closes.
	RuleCoopClose Rule = "cooperative_close"
	// RuleOwnCommitment mirrors the user's own force-close.
	RuleOwnCommitment Rule = "own_force_close"
	// RuleOwnSweep mirrors a sweep or HTLC claim following the user's own
	// force-close.
	RuleOwnSweep Rule = "own_sweep"
	// RuleJusticeWeSent mirrors a justice transaction the user's node published,
	// as redundancy: it punishes the same breach on the branch nobody is
	// watching.
	RuleJusticeWeSent Rule = "justice_we_sent"
	// RuleFundingOptIn mirrors a funding transaction, and only ever with the
	// per-channel opt-in.
	RuleFundingOptIn Rule = "funding_opt_in"
)

// The rules that refuse one. Every refusal names one of these: a denial with no
// reason is indistinguishable from a bug.
const (
	// DenyDefault is the case this whole design exists for. **Anything not
	// positively matched lands here.**
	DenyDefault Rule = "not_on_the_allowlist"
	// DenyTheirCommitment refuses the counterparty's force-close. Mirroring it
	// would manufacture exposure on the other branch that did not exist — the
	// precise harm the funding-transaction rule is written to prevent, arrived at
	// by a different route.
	DenyTheirCommitment Rule = "their_commitment"
	// DenyUnknownShape refuses a transaction we could not classify.
	DenyUnknownShape Rule = "unrecognised"
	// DenyUnknownChannel refuses anything not tied to one of the user's channels.
	DenyUnknownChannel Rule = "not_one_of_yours"
	// DenyFundingNotOptedIn refuses a funding transaction without the opt-in.
	DenyFundingNotOptedIn Rule = "funding_without_opt_in"
	// DenyNotOursToSend refuses a second-order transaction following somebody
	// else's commitment.
	DenyNotOursToSend Rule = "follows_their_commitment"
	// DenyDirection refuses a transaction whose direction has no rule at all.
	DenyDirection Rule = "wrong_direction"
	// DenySameBranch refuses a direction that is not one.
	DenySameBranch Rule = "not_a_direction"
)

// Inputs are everything the decision is allowed to look at.
//
// Deliberately small. Every field here is one the classifier or the registry
// already established; nothing is re-derived, because a policy that recomputed
// its own evidence would be a second place for the classification to be wrong.
type Inputs struct {
	// Shape is what the classifier made of the transaction.
	Shape store.SpendShape
	// From and To are the branches. From is where it was observed.
	From, To store.Branch

	// ChannelKnown is whether this transaction belongs to one of the user's
	// registered channels. The mirror is not a general relay: a transaction that
	// is not ours is not ours to move.
	ChannelKnown bool

	// SourceShape is what the commitment a second-order transaction spends was
	// classified as. Empty when this is not a second-order transaction.
	//
	// **This is what decides whose sweep or justice transaction it is.** A sweep
	// following our own commitment is ours; one following theirs is theirs. A
	// justice transaction is the mirror image: punishing their commitment means
	// we sent it, and punishing ours means they did.
	SourceShape store.SpendShape

	// IsFundingTx marks a channel's funding transaction, which is not a spend of
	// anything we watch and arrives by a different route.
	IsFundingTx bool
	// FundingOptIn is the user's per-channel decision. The only input here that
	// comes from a person rather than from the chain.
	FundingOptIn bool
}

// Verdict is whether to mirror a transaction, and why.
type Verdict struct {
	Mirror bool
	// Rule names the allowlist entry that decided it, in either direction.
	Rule Rule
	// Reason is the sentence a user reads. Present whichever way it went: a
	// refusal without one is an accusation with no evidence, and a "yes" without
	// one gives a reader nothing to check.
	Reason string
}

// Decide works out whether a transaction should exist on the other branch.
//
// Pure: no storage, no clock, no network.
//
// **An allowlist, evaluated as an explicit predicate, with deny as the default
// case.** The obvious implementation of this feature — watch for spends of
// registered outpoints and rebroadcast what turns up — is deny-shaped: anything
// nobody thought about gets moved, and the counterparty controls some of those
// transactions. Mirroring their latest commitment would manufacture exposure on
// the other branch that did not exist, which is exactly the harm this is written
// to prevent.
//
// So every branch below either names a rule that permits the transaction or
// refuses it, and a shape added to the classifier later lands in the default
// case without anybody having to remember to say so.
func Decide(in Inputs) Verdict {
	if in.From == in.To {
		return deny(DenySameBranch, fmt.Sprintf(
			"mirroring %s to itself is not a direction", in.From))
	}
	if !in.From.Valid() || !in.To.Valid() {
		return deny(DenySameBranch,
			"this transaction does not name two chains to move between")
	}

	// Not ours, not our business. The mirror moves the user's own transactions;
	// bridging anybody else's affects their money without their consent.
	if !in.ChannelKnown {
		return deny(DenyUnknownChannel,
			"this transaction is not part of any of your channels, so Forktower "+
				"does not move it")
	}

	if in.IsFundingTx {
		return decideFunding(in)
	}

	switch in.To {
	case store.BranchSQ:
		return towardsTheOtherChain(in)
	case store.BranchSF:
		return towardsYourOwnChain(in)
	default:
		return deny(DenyDirection, "there is no rule for moving a transaction this way")
	}
}

// decideFunding handles a channel's funding transaction.
//
// **The one rule here that adds exposure rather than reducing it.** Mirroring a
// funding transaction puts the user's money on a branch it was not on before, so
// it happens only on an explicit per-channel decision and never by default. The
// posture during a split is "do not open channels", not "make the channels you
// have span both".
func decideFunding(in Inputs) Verdict {
	if !in.FundingOptIn {
		return deny(DenyFundingNotOptedIn,
			"this is a channel's funding transaction. Copying it would put money "+
				"on the other chain that is not there now, so Forktower does it only "+
				"if you turn it on for that channel")
	}
	return allow(RuleFundingOptIn,
		"this is a channel's funding transaction, and you turned on copying it "+
			"for this channel")
}

// towardsTheOtherChain is the main direction: things happening on the chain the
// user's node follows that ought also to exist on the one it does not.
func towardsTheOtherChain(in Inputs) Verdict {
	switch in.Shape {
	case store.ShapeMutualClose:
		return allow(RuleCoopClose,
			"both of you agreed to close this channel, so the close belongs on "+
				"both chains — otherwise it stays open on the other one")

	case store.ShapeCommitmentOurs:
		return allow(RuleOwnCommitment,
			"you closed this channel yourself, so your close belongs on both chains")

	case store.ShapeDelayedSweep, store.ShapeHTLCClaim:
		// Ours only if it follows our own commitment. Following theirs, it is
		// their transaction and moving it would help them collect on a chain
		// where they had not yet.
		if in.SourceShape == store.ShapeCommitmentOurs {
			return allow(RuleOwnSweep,
				"this collects money from a channel you closed yourself, so it "+
					"belongs on both chains")
		}
		return deny(DenyNotOursToSend,
			"this collects money from a close the other party made. It is their "+
				"transaction, not yours, and Forktower does not move it for them")

	case store.ShapeJustice:
		// A justice transaction punishes a commitment. If the commitment it
		// punishes is not ours, we are the ones who sent it.
		if in.SourceShape == store.ShapeCommitmentUnknown ||
			in.SourceShape == store.ShapeCommitmentRevoked {
			return allow(RuleJusticeWeSent,
				"your node punished a broken promise on this chain, and the same "+
					"punishment belongs on the other one, where nobody else is watching")
		}
		return deny(DenyNotOursToSend,
			"this punishes a close you made, which means the other party sent it. "+
				"Forktower does not move it for them")

	case store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked:
		return deny(DenyTheirCommitment,
			"the other party closed this channel on this chain. Copying that to "+
				"the other chain would put your money at risk there when it is not "+
				"at risk now, so Forktower will not do it")

	case store.ShapeUnknown:
		return deny(DenyUnknownShape,
			"Forktower could not work out what this transaction does, so it does "+
				"not copy it")

	default:
		return deny(DenyDefault,
			"this is not one of the kinds of transaction Forktower copies between "+
				"chains")
	}
}

// towardsYourOwnChain is the other direction, and is deliberately much narrower.
//
// The user's node follows this chain and is already acting on it. Almost
// anything moved this way would be doing something on their behalf that their
// own node did not choose to do — copying their force-close back would close a
// channel here that is still live, which is a decision that belongs to them and
// not to us.
//
// A cooperative close is the exception, and the one doc 05 names: both parties
// agreed to it, so settling it on this chain too finishes something already
// agreed rather than starting something new.
func towardsYourOwnChain(in Inputs) Verdict {
	if in.Shape == store.ShapeMutualClose {
		return allow(RuleCoopClose,
			"both of you agreed to close this channel and it settled on the other "+
				"chain first, so the close belongs here too")
	}
	return deny(DenyDefault,
		"Forktower only copies an agreed close back to your own chain. Anything "+
			"else would be acting on your node's behalf on the chain it is already "+
			"watching")
}

func allow(rule Rule, reason string) Verdict {
	return Verdict{Mirror: true, Rule: rule, Reason: reason}
}

func deny(rule Rule, reason string) Verdict {
	return Verdict{Mirror: false, Rule: rule, Reason: reason}
}
