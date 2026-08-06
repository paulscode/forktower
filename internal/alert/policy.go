package alert

import (
	"fmt"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// Alert kinds. Stable machine-readable strings: they are part of every payload,
// including the content-free ones, and a user's own automation may key off them.
// Renaming one is a breaking change.
const (
	KindWatchingStarted = "watching_started"
	KindWatchingStopped = "watching_stopped"
	KindSplitDetected   = "split_detected"
	KindSplitResolving  = "split_resolving"
	KindSplitResolved   = "split_resolved"
	KindViewDegraded    = "view_degraded"
	KindViewWrongBranch = "view_wrong_branch"
	KindViewRecovered   = "view_recovered"

	KindChannelSpent      = "channel_spent"
	KindChannelSpentSoon  = "channel_spent_unconfirmed"
	KindSpendDisappeared  = "spend_disappeared"
	KindDeadlineWarning   = "deadline_warning"
	KindDeadlineResolved  = "deadline_resolved"
	KindLoss              = "loss"
	KindClosedOnlyOnYours = "closed_only_on_your_chain"
)

// Candidate is an alert the mapping decided to raise.
//
// Separate from store.Alert because the mapping supplies no timestamps: when
// something happened is the caller's business, and keeping the clock out of here
// is what lets the whole mapping be tested as a table.
type Candidate struct {
	Tier     store.Tier
	Kind     string
	DedupKey string
	Subject  string
	Message  string
	// Closes names further threads this news retires, when it retires any beyond
	// its own key. A view coming back ends both "cannot see it" and "it is on
	// the wrong branch", and a resolution that closed only one of them would
	// leave the other reading as current directly beside the news that it is not.
	//
	// Only meaningful on a resolved candidate; ignored elsewhere.
	Closes []string
}

// tierRank ranks the five alert tiers onto the three severities `min_tier`
// offers.
//
// `resolved` ranks as info and `loss` as critical, so a user who asked for
// critical-only still hears that they lost money, and is not paged at 3am to be
// told something is over. This is the only place that mapping exists.
func tierRank(t store.Tier) int {
	switch t {
	case store.TierInfo, store.TierResolved:
		return 1
	case store.TierWarning:
		return 2
	case store.TierCritical, store.TierLoss:
		return 3
	default:
		// An unknown tier is delivered rather than dropped. Silently discarding an
		// alert nobody recognised is the wrong direction to fail in.
		return 3
	}
}

func minTierRank(m config.MinTier) int {
	switch m {
	case config.MinTierInfo:
		return 1
	case config.MinTierWarning:
		return 2
	case config.MinTierCritical:
		return 3
	default:
		// Unset means unset, not "critical only": a transport nobody configured a
		// threshold for should still ring.
		return 1
	}
}

// Deliverable reports whether an alert of this tier passes a transport's
// threshold.
func Deliverable(t store.Tier, threshold config.MinTier) bool {
	return tierRank(t) >= minTierRank(threshold)
}

// Urgent reports whether an alert is severe enough to be repeated until the user
// acknowledges it.
func Urgent(t store.Tier) bool { return tierRank(t) >= minTierRank(config.MinTierCritical) }

// The short phrase a transport puts before the message: an ntfy title, an email
// subject. Defined once, here with the rest of the user-facing vocabulary, so
// that two transports cannot drift into describing the same urgency differently.
//
// Never a bare severity word. "warning" tells a user nothing they can act on, and
// this is the line they read on a lock screen before deciding whether to look.
const (
	headlineUrgent    = "Forktower: urgent"
	headlineAttention = "Forktower: attention needed"
	headlineRoutine   = "Forktower: an update"
)

// Headline is the phrase that introduces an alert of this tier.
func Headline(t store.Tier) string {
	switch tierRank(t) {
	case 3:
		return headlineUrgent
	case 2:
		return headlineAttention
	default:
		return headlineRoutine
	}
}

// ContentFreeMessage is what a third-party transport is told when detail is off.
//
// It carries an instruction rather than a bare tier name: "warning" tells a user
// nothing they can act on, and a notification that cannot be acted on trains them
// to ignore the next one.
const ContentFreeMessage = "Open your Forktower dashboard to see what happened."

// PayloadFor renders an alert for one transport.
//
// With detail off the payload carries the tier, the stable kind, and the
// instruction — no subject, no message. Everything specific stays on the user's
// own machine, because the operator of a third-party notification service would
// otherwise be told that this user is under attack and how long they have, which
// is precisely the attacker's ideal input.
func PayloadFor(a store.Alert, includeDetail bool) Payload {
	p := Payload{
		Version: PayloadVersion,
		Tier:    string(a.Tier),
		Kind:    a.Kind,
		Message: ContentFreeMessage,
	}
	if includeDetail {
		p.Subject = a.Subject
		p.Message = a.Message
	}
	return p
}

// MapEventToAlert turns an event into the alert it warrants, if any.
//
// One function, deliberately: every decision about what the user is told, and how
// urgently, is visible in a single table. Events that warrant nothing return
// false rather than an empty candidate, so "no alert" is a stated outcome instead
// of something inferred from a blank field.
func MapEventToAlert(e bus.Event) (Candidate, bool) {
	switch ev := e.(type) {
	case bus.SplitStateChanged:
		return mapSplitState(ev)
	case bus.ViewHealthChanged:
		return mapViewHealth(ev)
	case bus.FundingSpent:
		return mapFundingSpent(ev)
	case bus.MempoolSighting:
		return mapMempoolSighting(ev)
	case bus.SpendReorgedOut:
		return mapSpendReorgedOut(ev)
	case bus.DeadlineEscalated:
		return mapDeadlineEscalated(ev)
	case bus.DeadlineResolved:
		return mapDeadlineResolved(ev)
	case bus.DeadlineExpiredLoss:
		return mapExpiredLoss(ev)
	case bus.TowerHealthChanged:
		return mapTowerHealth(ev)
	case bus.TowerConcern:
		return mapTowerConcern(ev)
	default:
		return Candidate{}, false
	}
}

func mapSplitState(ev bus.SplitStateChanged) (Candidate, bool) {
	switch store.SplitState(ev.New) {
	case store.StateArmed:
		if store.SplitState(ev.Old) != store.StateUnarmed {
			// Only the first transition into watching is news. Anything else
			// arriving here would be a state machine that had gone backwards.
			return Candidate{}, false
		}
		return Candidate{
			Tier:     store.TierInfo,
			Kind:     KindWatchingStarted,
			DedupKey: KindWatchingStarted,
			Message:  "Forktower is now watching both chains.",
		}, true

	case store.StateUnarmed:
		// Going back to not-watching is not routine: it means the daemon stopped
		// doing the one thing it is for. A quiet alarm is the failure this project
		// cares most about, so it is said out loud.
		return Candidate{
			Tier:     store.TierWarning,
			Kind:     KindWatchingStopped,
			DedupKey: KindWatchingStopped,
			Message:  "Forktower has stopped watching. Open the dashboard to see why.",
		}, true

	case store.StateSplit:
		return Candidate{
			Tier:     store.TierWarning,
			Kind:     KindSplitDetected,
			DedupKey: KindSplitDetected,
			Message: "The chains have separated: your node's chain and the other chain " +
				"no longer agree. Open Forktower to see what this means for you.",
		}, true

	case store.StateResolving:
		return Candidate{
			Tier:     store.TierInfo,
			Kind:     KindSplitResolving,
			DedupKey: KindSplitResolving,
			Message:  "The split may be ending — one of the chains has stopped producing blocks.",
		}, true

	case store.StateResolvedSFWon, store.StateResolvedSQWon:
		return Candidate{
			Tier:     store.TierResolved,
			Kind:     KindSplitResolved,
			DedupKey: KindSplitResolved,
			Message:  "The split has ended.",
		}, true

	default:
		return Candidate{}, false
	}
}

func mapViewHealth(ev bus.ViewHealthChanged) (Candidate, bool) {
	view := chainview.Branch(ev.View)
	label := viewLabel(view)

	// The dedup key names the condition, not just the view: the store keeps an
	// alert's original message, so one key per view would leave a row whose text
	// no longer describes what it is reporting.
	//
	// **Which is why the recovery has to say what it closes.** Its own key names
	// nothing that was ever raised, so it would announce that Forktower can see
	// the chain again and leave "Forktower cannot see the status-quo chain"
	// sitting above it, unchanged and reading as current — the same complaint a
	// tester made about the watchtower warning.
	switch chainview.HealthState(ev.New) {
	case chainview.HealthOK:
		return Candidate{
			Tier:     store.TierResolved,
			Kind:     KindViewRecovered,
			DedupKey: fmt.Sprintf("%s:%s", KindViewRecovered, ev.View),
			Closes: []string{
				fmt.Sprintf("%s:%s", KindViewWrongBranch, ev.View),
				fmt.Sprintf("%s:%s", KindViewDegraded, ev.View),
			},
			Subject: label,
			Message: fmt.Sprintf("Forktower can see %s again.", label),
		}, true

	case chainview.HealthWrongBranch:
		// Urgent, though nothing is being attacked: watching is paused, so the
		// daemon is reporting calm about a chain nobody needs watched. It stays
		// broken until someone changes the configuration, which is exactly the
		// condition worth repeating until acknowledged.
		return Candidate{
			Tier:     store.TierCritical,
			Kind:     KindViewWrongBranch,
			DedupKey: fmt.Sprintf("%s:%s", KindViewWrongBranch, ev.View),
			Subject:  label,
			Message: "Setup problem: Forktower paused watching to be safe, because " +
				"it is not looking at the chain it should be. Open Forktower to fix this.",
		}, true

	case chainview.HealthSyncing, chainview.HealthDegraded,
		chainview.HealthEclipseSuspect, chainview.HealthDown:
		return Candidate{
			Tier:     store.TierWarning,
			Kind:     KindViewDegraded,
			DedupKey: fmt.Sprintf("%s:%s", KindViewDegraded, ev.View),
			Subject:  label,
			Message:  fmt.Sprintf("Forktower is having trouble seeing %s.", label),
		}, true

	default:
		return Candidate{}, false
	}
}

// mapFundingSpent is the event this whole product exists to deliver.
//
// Nothing about the channel is in the words — not which one, not how much, not
// how long. The message says what happened and where to look, because these
// alerts travel through services whose operators would otherwise be told that
// this user is under attack and roughly what it is worth, which is precisely an
// attacker's ideal input. The dashboard, on the user's own machine, says
// everything.
func mapFundingSpent(ev bus.FundingSpent) (Candidate, bool) {
	if store.SpendStatus(ev.Status) != store.SpendConfirmed {
		// The unconfirmed sighting has its own mapping and its own wording.
		return Candidate{}, false
	}

	switch store.SpendShape(ev.Shape) {
	case store.ShapeMutualClose:
		// A close both sides agreed to. Worth recording, not worth waking anyone.
		return Candidate{
			Tier:     store.TierInfo,
			Kind:     KindChannelSpent,
			DedupKey: fmt.Sprintf("%s:%d", KindChannelSpent, ev.ChannelID),
			Subject:  "A channel was closed on the other chain",
			Message: "One of your channels was closed by agreement on the other chain. " +
				"Nothing needs doing.",
		}, true

	case store.ShapeCommitmentOurs:
		return Candidate{
			Tier:     store.TierWarning,
			Kind:     KindChannelSpent,
			DedupKey: fmt.Sprintf("%s:%d", KindChannelSpent, ev.ChannelID),
			Subject:  "Your own channel close reached the other chain",
			Message: "A channel close your own node made has confirmed on the other " +
				"chain too. Open Forktower to see when those funds can be claimed.",
		}, true

	case store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked:
		return Candidate{
			Tier:     store.TierCritical,
			Kind:     KindChannelSpent,
			DedupKey: fmt.Sprintf("%s:%d", KindChannelSpent, ev.ChannelID),
			Subject:  "One of your channels is being closed on the other chain",
			Message: "Somebody has published a channel close on the chain your node " +
				"does not follow, and Forktower cannot tell whether it is an old one. " +
				"There is a time limit on responding. Open Forktower now.",
		}, true

	case store.ShapeJustice, store.ShapeDelayedSweep, store.ShapeHTLCClaim,
		store.ShapeUnknown:
		// Something spent the funding output and it fits none of the shapes.
		// Never silently ignored: an unrecognised spend of a channel is exactly
		// the thing that must not pass quietly.
		return Candidate{
			Tier:     store.TierCritical,
			Kind:     KindChannelSpent,
			DedupKey: fmt.Sprintf("%s:%d", KindChannelSpent, ev.ChannelID),
			Subject:  "One of your channels was closed on the other chain",
			Message: "One of your channels was closed on the chain your node does not " +
				"follow, in a way Forktower does not recognise. Open Forktower now.",
		}, true

	default:
		return Candidate{}, false
	}
}

// mapMempoolSighting is the early warning, and it is worth as much as the
// confirmation.
//
// Raised at critical even though nothing has confirmed: a commitment seen before
// it is mined buys the user a block of notice, and on a chain producing blocks
// slowly that can be a great deal of time. Told about it afterwards, they would
// have had less.
func mapMempoolSighting(ev bus.MempoolSighting) (Candidate, bool) {
	if store.SpendShape(ev.Shape) == store.ShapeMutualClose {
		// A cooperative close on its way is not an emergency.
		return Candidate{}, false
	}
	return Candidate{
		Tier:     store.TierCritical,
		Kind:     KindChannelSpentSoon,
		DedupKey: fmt.Sprintf("%s:%d", KindChannelSpentSoon, ev.ChannelID),
		Subject:  "A channel close is about to land on the other chain",
		Message: "Forktower has seen a channel close waiting to be mined on the chain " +
			"your node does not follow. It has not confirmed yet, which means you have " +
			"a little more time than you otherwise would. Open Forktower now.",
	}, true
}

// mapSpendReorgedOut says a close has left the chain, and takes care not to
// sound like good news.
//
// It is not. A counterparty replacing their transaction with a higher fee looks
// exactly like this, and so does a reorganisation that will put it straight
// back. Reading it as relief is the wrong instinct, so the words do not offer
// it.
func mapSpendReorgedOut(ev bus.SpendReorgedOut) (Candidate, bool) {
	return Candidate{
		Tier:     store.TierWarning,
		Kind:     KindSpendDisappeared,
		DedupKey: fmt.Sprintf("%s:%d", KindSpendDisappeared, ev.SpendEventID),
		Subject:  "A channel close has left the other chain",
		Message: "A close Forktower was watching is not in a block on the other chain " +
			"any more. That does not mean the danger has passed — it may be replaced " +
			"by another, or come back. Forktower is still watching. Open it to see " +
			"where things stand.",
	}, true
}

// mapDeadlineEscalated is the countdown getting louder.
//
// Deduplicated per level rather than per deadline, so each tier is a fresh alert
// the user has to acknowledge on its own. One row per countdown would mean the
// second and third warnings quietly updating a message the user had already
// dismissed — which is the same as not sending them.
func mapDeadlineEscalated(ev bus.DeadlineEscalated) (Candidate, bool) {
	key := fmt.Sprintf("%s:%d:%d", KindDeadlineWarning, ev.DeadlineID, ev.Level)

	// The time estimate is the part a person can act on. A block count on its own
	// invites them to assume ten minutes a block, and on the chain this is
	// counting that assumption can be wrong by a factor of four.
	timing := fmt.Sprintf("%d blocks left", ev.RemainingBlocks)
	if ev.EstWallClock != "" {
		timing = fmt.Sprintf("%d blocks left, which is %s at the rate that chain is "+
			"currently going", ev.RemainingBlocks, ev.EstWallClock)
	}

	subject := "Time is running out on one of your channels"
	message := "Forktower is counting down on a channel close on the other chain: " +
		timing + ". Open Forktower now."
	if ev.RemainingBlocks == 0 {
		subject = "The time on one of your channels has run out"
		message = "The window to respond to a channel close on the other chain has " +
			"closed. Open Forktower."
	}

	return Candidate{
		Tier:     store.TierCritical,
		Kind:     KindDeadlineWarning,
		DedupKey: key,
		Subject:  subject,
		Message:  message,
	}, true
}

// mapDeadlineResolved is the one piece of unambiguously good news this software
// has to give.
func mapDeadlineResolved(ev bus.DeadlineResolved) (Candidate, bool) {
	message := "A countdown on one of your channels has stopped: what started it is " +
		"no longer on the other chain."
	if ev.ByTxid != "" {
		message = "A countdown on one of your channels was answered before it ran out. " +
			"Open Forktower to see what happened."
	}
	return Candidate{
		Tier:     store.TierResolved,
		Kind:     KindDeadlineResolved,
		DedupKey: fmt.Sprintf("%s:%d", KindDeadlineResolved, ev.DeadlineID),
		Subject:  "A countdown has stopped",
		Message:  message,
	}, true
}

// mapExpiredLoss is the worst message this software sends, and the one it exists
// to make rare.
//
// The amount stays out of the words for the same reason everything else does:
// these travel through other people's servers. It is on the dashboard.
func mapExpiredLoss(ev bus.DeadlineExpiredLoss) (Candidate, bool) {
	return Candidate{
		Tier:     store.TierLoss,
		Kind:     KindLoss,
		DedupKey: fmt.Sprintf("%s:%d", KindLoss, ev.DeadlineID),
		Subject:  "A channel was lost on the other chain",
		Message: "The window to respond to a channel close on the other chain has " +
			"passed with nothing having answered it. Open Forktower for the details, " +
			"which you will want if you are reporting this to anyone.",
	}, true
}

// ClosedOnlyOnYourChain is the slow-burn warning for the exposure people do not
// expect.
//
// A channel that closed on the user's own chain feels finished. On the chain
// nobody is watching it is not: the close has not happened there, so the old
// commitments the counterparty still holds remain spendable. This is not raised
// by an event, because nothing happens — the danger is precisely that nothing
// happens, for as long as it takes somebody to notice the opportunity.
func ClosedOnlyOnYourChain(channelID int64) Candidate {
	return Candidate{
		Tier:     store.TierWarning,
		Kind:     KindClosedOnlyOnYours,
		DedupKey: fmt.Sprintf("%s:%d", KindClosedOnlyOnYours, channelID),
		Subject:  "A closed channel is still open on the other chain",
		Message: "One of your channels is closed on your own chain, but that close has " +
			"not happened on the other one — so the old commitments your counterparty " +
			"holds can still be spent there. Open Forktower to see what can be done.",
	}
}

// viewLabel is the phrase a user sees for one of the two chains.
func viewLabel(b chainview.Branch) string {
	return words.Chain(string(b))
}
