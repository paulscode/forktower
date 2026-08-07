package alert

import (
	"fmt"
	"strings"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/responder/tower"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// Alert kinds for the watchtowers. Stable machine-readable strings, like the
// rest: a user's own automation may key off them.
const (
	// KindTowerDown means the tower that would answer a breach is not answering.
	KindTowerDown = "tower_down"
	// KindTowerRecovered says it came back.
	KindTowerRecovered = "tower_recovered"
	// KindTowerNotProtecting means the tower is fine and is protecting nothing —
	// the failure with no other symptom.
	KindTowerNotProtecting = "tower_not_protecting"
	// KindTowerProtecting says a gap the user had to close is closed. Its own
	// kind, so the resolution reads as news rather than as the same warning
	// again.
	KindTowerProtecting = "tower_protecting"
	// KindTowerMisbehaving means the tower returned a receipt that does not check
	// out. Proof, not suspicion.
	KindTowerMisbehaving = "tower_misbehaving"
	// KindTowerSubscription means the subscription is running out or has.
	KindTowerSubscription = "tower_subscription"
)

// mapTowerHealth turns a change in a tower's condition into an alert.
//
// **Recovery is announced as well as failure**, and at a tier somebody can
// ignore. A user who was told their protection had gone is owed the sentence
// saying it came back; otherwise the only way to find out is to go and look,
// which is the behaviour this whole project exists to replace.
func mapTowerHealth(ev bus.TowerHealthChanged) (Candidate, bool) {
	// One key for the failure and its recovery, deliberately: they are one
	// thread, and the entry becomes resolved rather than the dashboard showing
	// both at once. The store closes it — see ResolveAlert, which exists because
	// this used to bump the warning and clear its acknowledgement instead.
	key := fmt.Sprintf("%s:%d", KindTowerDown, ev.TowerID)

	switch store.TowerStatus(ev.Status) {
	case store.TowerReachable:
		if ev.Previous == "" || store.TowerStatus(ev.Previous) == store.TowerStatusUnknown {
			// Coming up for the first time is not recovery, and telling somebody
			// their tower "came back" when it has only just started is confusing.
			return Candidate{}, false
		}
		return Candidate{
			Tier: store.TierResolved, Kind: KindTowerRecovered, DedupKey: key,
			Subject: "Your watchtower is answering again",
			Message: "The watchtower that would answer a breach on " + words.OtherChain +
				" is working again.",
		}, true

	case store.TowerUnreachable:
		// **Never having come up is not the same as having gone down**, and this
		// is the mirror of the rule two cases above. A tower that has not been
		// reachable yet is starting; saying its protection "has stopped" would
		// tell a user something has been lost when nothing was ever there.
		//
		// Seen on a fresh install: lnd took a few seconds to open its listener,
		// this fired, and the warning then sat on the dashboard for days.
		if neverReachable(ev.Previous) {
			return Candidate{}, false
		}
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerDown, DedupKey: key,
			Subject: "Your watchtower is not answering",
			Message: "The watchtower that would punish a broken promise on " +
				words.OtherChain + " has stopped answering. Until it is back, a " +
				"channel closed against you there would not be answered. " +
				detailSentence(ev.Detail),
		}, true

	case store.TowerSubscriptionError:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerSubscription,
			DedupKey: fmt.Sprintf("%s:%d", KindTowerSubscription, ev.TowerID),
			Subject:  "Your watchtower has stopped accepting backups",
			Message: "The subscription with your watchtower has run out, so it is no " +
				"longer taking new channel states. The states it already has are " +
				"still protected. Register with it again to start a new subscription.",
		}, true

	case store.TowerMisbehaving:
		return Candidate{
			Tier: store.TierCritical, Kind: KindTowerMisbehaving,
			DedupKey: fmt.Sprintf("%s:%d", KindTowerMisbehaving, ev.TowerID),
			Subject:  "Your watchtower is not behaving as it should",
			Message: "Your watchtower returned a receipt whose signature does not " +
				"check out. That is proof rather than suspicion, and it means this " +
				"tower cannot be relied on to answer a breach. Register with another " +
				"one.",
		}, true

	case store.TowerTemporarilyUnreachable:
		// **A tower that was down and is now merely settling has come back**, and
		// the standing warning has to be closed or it never will be.
		//
		// The resolve used to fire only on fully reachable, which a tower cannot
		// reach until its own chain backend has caught up — days, on a node
		// syncing from nothing. So "your watchtower is not answering" stayed on
		// the dashboard, true for the few seconds it described and false for
		// every one after.
		if store.TowerStatus(ev.Previous) == store.TowerUnreachable {
			return Candidate{
				Tier: store.TierResolved, Kind: KindTowerRecovered, DedupKey: key,
				Subject: "Your watchtower is answering again",
				Message: "It is running and answering. It cannot see " +
					words.OtherChain + " yet, so it could not act on a breach there — " +
					"that finishes on its own. " + detailSentence(ev.Detail),
			}, true
		}
		// Otherwise: starting up, or still being asked. Not worth waking anybody.
		return Candidate{}, false

	case store.TowerStatusUnknown:
		return Candidate{}, false

	default:
		return Candidate{}, false
	}
}

// mapTowerConcern turns a per-channel or per-tower concern into an alert.
//
// The concerns differ in what they say rather than in how they travel, which is
// why they share one event. The tiers differ sharply, though: "your node is
// backing up to nothing" and "one channel of an unusual type is not covered" are
// not the same news.
func mapTowerConcern(ev bus.TowerConcern) (Candidate, bool) {
	if ev.Cleared {
		return clearedConcern(ev)
	}
	switch tower.ConcernKind(ev.Concern) {
	case tower.ConcernClientOff, tower.ConcernPluginMissing:
		// Nothing is being backed up anywhere. The largest gap this arm can have,
		// and one the user has to close on their own node.
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:client:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Nothing is being backed up to a watchtower",
			Message:  ev.Message,
		}, true

	case tower.ConcernNotRegistered:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:unregistered:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Your watchtower has nothing registered with it",
			Message:  ev.Message + " Open Forktower for the steps.",
		}, true

	case tower.ConcernChannelUncovered:
		// Per channel, because that is how it fails: one channel uncovered while
		// the rest are fine and the tower reports itself healthy.
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:channel:%d", KindTowerNotProtecting, ev.ChannelID),
			Subject:  "One of your channels is not protected by your watchtower",
			Message:  ev.Message,
		}, true

	case tower.ConcernBackupsStalled:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:stalled:%d", KindTowerNotProtecting, ev.ChannelID),
			Subject:  "Your watchtower has stopped receiving backups",
			Message:  ev.Message,
		}, true

	case tower.ConcernSubscriptionExpiring:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerSubscription,
			DedupKey: fmt.Sprintf("%s:expiry:%d", KindTowerSubscription, ev.TowerID),
			Subject:  "Your watchtower subscription is running out",
			Message:  ev.Message,
		}, true

	case tower.ConcernSlotsLow:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerSubscription,
			DedupKey: fmt.Sprintf("%s:slots:%d", KindTowerSubscription, ev.TowerID),
			Subject:  "Your watchtower is running out of room",
			Message:  ev.Message,
		}, true

	case tower.ConcernAppointmentsUndelivered, tower.ConcernAppointmentsInvalid:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:undelivered:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Some channel updates have not reached your watchtower",
			Message:  ev.Message,
		}, true

	case tower.ConcernTowerMisbehaving:
		return Candidate{
			Tier: store.TierCritical, Kind: KindTowerMisbehaving,
			DedupKey: fmt.Sprintf("%s:%d", KindTowerMisbehaving, ev.TowerID),
			Subject:  "Your watchtower is not behaving as it should",
			Message:  ev.Message,
		}, true

	case tower.ConcernOursNotRegistered:
		// **A warning rather than an aside, unlike external-only beside it.** This
		// is the single registration the split-specific protection depends on, and
		// the user is the only one who can make it: Forktower holds a read-only
		// credential to their node and could not do it if it wanted to.
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: KindTowerNotProtecting + ":ours-not-registered",
			Subject:  "Forktower's watchtower is not registered with your node",
			Message:  ev.Message,
		}, true

	case tower.ConcernRegistrationStale:
		// **A warning, because the protection is gone and it looks fine.** The
		// registration is listed on the node, the tower is running, and nothing
		// between them works. Of everything this arm reports, this is the one most
		// likely to be believed only after it is too late.
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:stale-registration:%d", KindTowerNotProtecting, ev.TowerID),
			// The subject has to carry the action on its own, because it is what gets
			// read on a phone banner and in a notification list. "Unreachable" states
			// a symptom and invites nothing.
			Subject: "Your watchtower moved — your node needs its new address",
			Message: ev.Message,
		}, true

	case tower.ConcernUnreachableFromNode:
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:unreachable:%d", KindTowerNotProtecting, ev.TowerID),
			// Not "cannot reach", which reads as a transient network wobble to wait
			// out. What makes somebody act is that it has never worked at all.
			Subject: "Your node has never backed up to your watchtower",
			Message: ev.Message,
		}, true

	case tower.ConcernExternalOnly:
		// A description of the deployment rather than a fault with it. Worth
		// saying once because it changes what can be done when a tower stops —
		// there is no process here to restart and no settings here to correct —
		// and worth saying quietly, because the arrangement itself is fine.
		return Candidate{
			Tier: store.TierInfo, Kind: KindTowerNotProtecting,
			DedupKey: KindTowerNotProtecting + ":external-only",
			Subject:  "Your watchtowers belong to somebody else",
			Message:  ev.Message,
		}, true

	case tower.ConcernFeeRateFixed, tower.ConcernSessionsExhausted, tower.ConcernDiskFilling:
		// Worth knowing and not worth waking anyone: none of these is protection
		// failing, and two of them are ordinary events explained rather than
		// alarmed about.
		return Candidate{
			Tier: store.TierInfo, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:%s:%d", KindTowerNotProtecting, ev.Concern, ev.TowerID),
			Subject:  "Something worth knowing about your watchtower",
			Message:  ev.Message,
		}, true

	default:
		// A concern this build does not recognise still reaches the user. Silence
		// would be the one outcome worse than an unfamiliar subject line.
		return Candidate{
			Tier: store.TierWarning, Kind: KindTowerNotProtecting,
			DedupKey: fmt.Sprintf("%s:%s:%d", KindTowerNotProtecting, ev.Concern, ev.TowerID),
			Subject:  "Something is wrong with your watchtower",
			Message:  ev.Message,
		}, true
	}
}

// detailSentence appends a detail as its own sentence, or nothing.
func detailSentence(detail string) string {
	if detail == "" {
		return ""
	}
	return "What Forktower saw: " + tidyDetail(detail) + "."
}

// tidyDetail trims a machine's answer down to the part a person can act on.
//
// **What arrived here was a sentence with an HTTP status, a JSON body and a gRPC
// code embedded in it**, wrapped in prose about a broken promise. All of it is
// true and almost none of it helps: the reader wants to know whether their
// protection is gone, and is instead handed `{"code":2, "message":"...",
// "details":[]}` to interpret.
//
// The useful part of these is the message a server wrote for a human, which is
// exactly the part buried deepest. Where one can be recovered it is used, and
// where it cannot the original is left alone rather than mangled — a detail
// nobody can read beats a detail this turned into nonsense.
func tidyDetail(detail string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(detail), ".")

	// A JSON body with a "message" field, which is how gRPC-gateway reports a
	// refusal. Everything around it is transport bookkeeping.
	if start := strings.Index(trimmed, `"message":`); start >= 0 {
		rest := trimmed[start+len(`"message":`):]
		if open := strings.Index(rest, `"`); open >= 0 {
			if end := strings.Index(rest[open+1:], `"`); end > 0 {
				if msg := strings.TrimSpace(rest[open+1 : open+1+end]); msg != "" {
					return strings.TrimRight(msg, ".")
				}
			}
		}
	}
	return trimmed
}

// neverReachable reports whether a tower has yet been seen working.
//
// An empty previous status is a tower whose first observation this is; unknown
// is one that has been looked at and never answered. Neither has protection to
// lose.
func neverReachable(previous string) bool {
	switch store.TowerStatus(previous) {
	case "", store.TowerStatusUnknown:
		return true
	case store.TowerReachable, store.TowerTemporarilyUnreachable,
		store.TowerUnreachable, store.TowerSubscriptionError,
		store.TowerMisbehaving:
		return false
	default:
		return false
	}
}

// clearedConcern announces that something the user had to fix is fixed.
//
// **Under the warning's key, so it closes it** rather than sitting beside it.
// The store's ResolveAlert marks that entry resolved and keeps when it first
// happened; a user who fixed the thing sees the item they acted on turn over,
// not a second line under a warning that still reads as current.
//
// Only for the concerns a person acts on. A channel becoming coverable because
// it closed is not news anybody was waiting for, and an entry for every one of
// those would bury the two that matter.
func clearedConcern(ev bus.TowerConcern) (Candidate, bool) {
	switch tower.ConcernKind(ev.Concern) {
	case tower.ConcernClientOff, tower.ConcernPluginMissing:
		return Candidate{
			Tier: store.TierResolved, Kind: KindTowerProtecting,
			DedupKey: fmt.Sprintf("%s:client:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Your node is backing up to a watchtower",
			Message: "The watchtower client on your Lightning node is on, and " +
				"channel states are reaching the tower. That was the one step " +
				"Forktower could not take for you.",
		}, true

	case tower.ConcernNotRegistered:
		return Candidate{
			Tier: store.TierResolved, Kind: KindTowerProtecting,
			DedupKey: fmt.Sprintf("%s:unregistered:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Your watchtower has your channels registered",
			Message: "Your node has registered with the tower and is sending it " +
				"channel states.",
		}, true

	case tower.ConcernOursNotRegistered:
		return Candidate{
			Tier: store.TierResolved, Kind: KindTowerProtecting,
			DedupKey: KindTowerNotProtecting + ":ours-not-registered",
			Subject:  "Forktower's watchtower is registered with your node",
			Message: "Your node is backing up to the tower here, which watches " +
				words.OtherChain + ". That was the one step Forktower could not " +
				"take for you.",
		}, true

	case tower.ConcernRegistrationStale, tower.ConcernUnreachableFromNode:
		// **Announced, because somebody went and did this.** Both are corrected by
		// the user on their own node — editing a registration, or giving the tower
		// an address their node can dial — and both are the sort of fiddly remote
		// change where the only way to know it took is to be told. That is the
		// complaint that produced half of 0.6.2.
		return Candidate{
			Tier: store.TierResolved, Kind: KindTowerProtecting,
			DedupKey: fmt.Sprintf("%s:reachable:%d", KindTowerNotProtecting, ev.TowerID),
			Subject:  "Your node is reaching the watchtower again",
			Message: "Your node and the tower here are talking again, and channel " +
				"states are reaching it.",
			// Declared every pass by both producers so that a restart cannot
			// strand the warning, which means it must say nothing when there was
			// no warning.
			OnlyIfStanding: true,
			Closes: []string{
				fmt.Sprintf("%s:stale-registration:%d", KindTowerNotProtecting, ev.TowerID),
				fmt.Sprintf("%s:unreachable:%d", KindTowerNotProtecting, ev.TowerID),
			},
		}, true

	case tower.ConcernChannelUncovered, tower.ConcernBackupsStalled,
		tower.ConcernFeeRateFixed, tower.ConcernSessionsExhausted,
		tower.ConcernExternalOnly, tower.ConcernSubscriptionExpiring,
		tower.ConcernSlotsLow, tower.ConcernAppointmentsUndelivered,
		tower.ConcernAppointmentsInvalid, tower.ConcernTowerMisbehaving,
		tower.ConcernDiskFilling:
		// Real while they last and not worth an announcement when they stop.
		//
		// Every one of these ends without anybody doing anything — a subscription
		// renewed, a channel closed, disk freed — so nobody is sitting there
		// waiting to hear. The readiness list carries the current answer, and an
		// entry for each of these would bury the two above, which are the ones
		// somebody went and worked for.
		//
		// Listed rather than defaulted, so a concern added later has to be
		// classified here on purpose instead of falling silently into the quiet
		// half.
		return Candidate{}, false

	default:
		return Candidate{}, false
	}
}
