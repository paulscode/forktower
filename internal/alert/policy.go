package alert

import (
	"fmt"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
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

	// The dedup key names the condition, not just the view. A view that degrades,
	// recovers and degrades again must reuse the *degraded* row rather than
	// overwrite the recovery notice with it — the store keeps an alert's original
	// message, so one key per view would leave a row whose text no longer
	// describes what it is reporting.
	switch chainview.HealthState(ev.New) {
	case chainview.HealthOK:
		return Candidate{
			Tier:     store.TierResolved,
			Kind:     KindViewRecovered,
			DedupKey: fmt.Sprintf("%s:%s", KindViewRecovered, ev.View),
			Subject:  label,
			Message:  fmt.Sprintf("Forktower can see %s again.", label),
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

// The phrases a user sees for the two chains. The internal names mean nothing
// outside this codebase, and doc-level consistency here is what keeps the
// dashboard, the notifications and the documentation using one vocabulary.
const (
	labelSF      = "your node's chain"
	labelSQ      = "the other chain"
	labelUnknown = "one of the chains"
)

// viewLabel is the phrase a user sees for one of the two chains.
func viewLabel(b chainview.Branch) string {
	switch b {
	case chainview.BranchSF:
		return labelSF
	case chainview.BranchSQ:
		return labelSQ
	default:
		return labelUnknown
	}
}
