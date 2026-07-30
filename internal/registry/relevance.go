package registry

import "github.com/paulscode/forktower/internal/store"

// Facts are everything the classifier is allowed to look at.
//
// A struct rather than a long argument list, because every field here is a
// number or a flag and a caller swapping two of them would compile.
type Facts struct {
	// ForkKnown reports whether the chains are known to have separated, and
	// ForkHeight is the last block they had in common. Before a separation
	// exists there is nothing to be irrelevant to.
	ForkKnown  bool
	ForkHeight int32

	// OpenHeight is the block that confirmed the funding transaction. Zero means
	// the node did not say, which is treated as not knowing rather than as zero.
	OpenHeight int32

	// CloseState is what is currently believed about this channel on the user's
	// own chain, and CloseHeight is the block that confirmed the close. Zero
	// means it has not confirmed — a close the node has announced but the chain
	// has not yet recorded is not a close for this purpose.
	CloseState  store.CloseState
	CloseHeight int32

	// FundingSeenOnSQ records that the funding transaction was also found on the
	// other chain. Relevant only for channels funded after the separation: a
	// funding transaction broadcast near the fork can propagate to both sides,
	// and one that did is exposed on both. Set by the watcher, which is the only
	// thing that looks.
	FundingSeenOnSQ bool
}

// Relevance reasons, as the user reads them.
//
// Constants rather than literals at the call sites because these strings are
// shown in the dashboard, recorded in the timeline, and pinned by tests — three
// places that must not drift apart.
const (
	ReasonProvisional = "we are watching it until there is a split to judge it against"

	ReasonFundedBeforeFork = "it was funded before the chains separated, so its funding " +
		"output exists on the other chain too"

	// ReasonClosedOnlyOnSF is the S6 case, and the sentence exists because it is
	// the exposure people do not expect. A closed channel feels finished. On the
	// other chain it is not: the close has not happened there, so the old
	// commitments the counterparty holds are still spendable.
	ReasonClosedOnlyOnSF = "it was closed on your own chain, but that close has not " +
		"happened on the other one — the old commitments there can still be spent"

	ReasonClosedBeforeFork = "it was closed before the chains separated, so it is " +
		"closed on both of them"

	ReasonFundedAfterFork = "it was funded after the chains separated, so it does not " +
		"exist on the other chain"

	ReasonFundedAfterForkButMirrored = "it was funded after the chains separated, but " +
		"its funding transaction reached the other chain as well"

	ReasonOpenHeightUnknown = "we do not know which block funded it, so it is being " +
		"watched anyway"
)

// Classify decides whether a channel is exposed on the chain the user's node
// does not follow, and says why in words a user can read.
//
// Pure: it reads no storage, talks to nothing, and is the same function every
// time. That is what makes the exposure scenarios testable directly rather than
// through a running daemon.
//
// The bias throughout is toward watching. `unknown` is a watching instruction,
// not a shrug — the watchset includes it — so a channel this function cannot
// place still gets looked at. Only a positive reason removes one.
func Classify(f Facts) (relevance store.Relevance, reason string) {
	if !f.ForkKnown {
		// Before a split, every channel is provisionally relevant rather than
		// unknown. The users this product most wants to serve are the ones who
		// installed it *before* anything happened, and showing them a column of
		// "unknown" would tell them nothing about the coverage they came for.
		// Re-classification runs on every split state change, so this corrects
		// itself the moment there is something real to judge against.
		return store.Relevant, ReasonProvisional
	}

	// S8: a close that confirmed before the separation confirmed on both chains,
	// because before the separation there was only one. Checked first: it is the
	// only thing that makes a channel funded before the fork genuinely safe.
	if f.CloseHeight > 0 && f.CloseHeight <= f.ForkHeight {
		return store.Irrelevant, ReasonClosedBeforeFork
	}

	if f.OpenHeight <= 0 {
		return store.RelevanceUnknown, ReasonOpenHeightUnknown
	}

	// S7: funded after the chains separated, so the funding output was never
	// created on the other one — with the exception the watcher can find.
	//
	// Strictly greater, and the strictness matters. The fork point is the last
	// block the two chains *shared*, so a funding transaction confirmed in that
	// block exists on both of them; treating the fork height itself as "after"
	// would stop watching a channel that is exposed. One block, resolved toward
	// watching.
	if f.OpenHeight > f.ForkHeight {
		if f.FundingSeenOnSQ {
			return store.Relevant, ReasonFundedAfterForkButMirrored
		}
		return store.Irrelevant, ReasonFundedAfterFork
	}

	// S6: funded before the separation and closed after it — or not closed at
	// all. Either way the funding output exists on the other chain and, as far
	// as anyone here knows, is still unspent there.
	if f.CloseState != "" && f.CloseState != store.CloseOpen {
		return store.Relevant, ReasonClosedOnlyOnSF
	}
	return store.Relevant, ReasonFundedBeforeFork
}
