package api

import (
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// HeadlineState is what the user is told, in increasing urgency.
//
// Five values for sixteen internal ones. The state machines behind this are
// richer — six split states, six backend health states — and they map *down* to
// these once, here, so that the wording for each situation exists in one
// reviewable place instead of being reassembled in JavaScript, in notification
// templates and in platform health strings.
type HeadlineState string

// The five states. Nothing may add a sixth: colour, icon and urgency all follow
// from this one field, and a value the UI does not know becomes a blank screen.
const (
	StateGettingReady HeadlineState = "getting_ready"
	StateProtected    HeadlineState = "protected"
	StateAttention    HeadlineState = "attention"
	StateActionNeeded HeadlineState = "action_needed"
	StateAtRisk       HeadlineState = "at_risk"
)

// Where an action sends the user. Fragments rather than paths: the dashboard is
// one page, and everything here is a section of it.
const (
	anchorSetup         = "#setup"
	anchorNotifications = "#notifications"
	anchorTower         = "#towers-card"
	anchorExposure      = "#exposure"
)

// titleAttention is shared by every situation that needs a look but not a hurry.
// One sentence, worded once: a user who sees three different phrasings for the
// same level of concern learns that the wording means nothing.
const titleAttention = "Something needs a look — not urgent."

// The actions the M1 surfaces offer. Defined here with the rest of the
// user-facing wording, so that two places offering the same step cannot label it
// two different ways.
var (
	actionFixSetup    = func() *Action { return &Action{Label: "Fix the setup", Href: anchorSetup} }
	actionSetUpAlerts = func() *Action {
		return &Action{Label: "Set up notifications", Href: anchorNotifications}
	}
	actionTestAlerts = func() *Action {
		return &Action{Label: "Send a test alert", Endpoint: "/api/v1/alerts/test"}
	}
	// Points at the wizard rather than at a button. Forktower cannot set up a
	// watchtower for somebody: turning on their node's watchtower client is a
	// setting in their node's own configuration, and this daemon is only ever
	// allowed to read from it.
	actionSetUpTower = func() *Action {
		return &Action{Label: "Set up a watchtower", Href: anchorTower}
	}
)

// Action is the single next step a state carries, or nothing.
type Action struct {
	Label string `json:"label"`
	// Endpoint is called by the dashboard; Href navigates. Exactly one is set.
	Endpoint string `json:"endpoint,omitempty"`
	Href     string `json:"href,omitempty"`
}

// Headline is the answer to "am I OK?".
//
// Every user-facing surface renders this verbatim.
type Headline struct {
	State  HeadlineState `json:"state"`
	Title  string        `json:"title"`
	Detail string        `json:"detail"`
	// Action is nil when there is nothing for the user to do — a legitimate and
	// important answer, rendered as reassurance rather than an unexplained blank.
	Action *Action `json:"action"`
	// Since is when this situation began, in unix seconds, or zero when there is
	// no honest source for it. Zero means the dashboard shows no time at all
	// rather than a plausible-looking guess.
	Since int64 `json:"since"`
}

// HeadlineInput is everything the mapping considers.
//
// A struct rather than a pile of parameters so the whole table can be driven from
// a test without a running daemon, and so that a field added later cannot be
// silently forgotten at one of the call sites.
type HeadlineInput struct {
	Phase store.SplitState
	// DetectedAt is when a split was confirmed.
	DetectedAt int64

	SFHealth chainview.HealthState
	SQHealth chainview.HealthState

	// Paused means watching has stopped because a view cannot be trusted.
	Paused bool
	// PausedSince is when the branch check last passed.
	PausedSince int64

	// AlertsReachable is false when no notification transport is configured or
	// the last test of every one of them failed.
	AlertsReachable bool

	// FailingChecks are the readiness checks that are not passing, in the order
	// they are listed. Informational items are already excluded.
	FailingChecks []ReadinessItem

	// ExposedDeadline is set when a channel is being closed unfairly and there is
	// a running deadline to respond.
	//
	// Always nil in this version: nothing watches channels yet. Present because
	// this is the one state that must be right the day it becomes reachable, and
	// a branch that has never existed is a branch nobody has read.
	ExposedDeadline *ExposedDeadline
}

// ExposedDeadline describes money at risk with a clock running.
type ExposedDeadline struct {
	// Partner is who the channel is with, in whatever form is safe to show.
	Partner string
	// Since is when the spend was confirmed.
	Since int64
}

// ComputeHeadline maps the internal state onto what the user is told.
//
// Ordered by urgency, most urgent first. The order is the whole design: a user
// with money at risk must not be shown a note about a degraded backend, and a
// user whose alarm cannot reach them must not be told everything is fine.
func ComputeHeadline(in HeadlineInput) Headline {
	switch {
	case in.ExposedDeadline != nil:
		return Headline{
			State: StateAtRisk,
			Title: "Money is at risk right now.",
			Detail: "A channel is being closed unfairly and there is a limited time to " +
				"respond.",
			Action: &Action{Label: "See what is happening", Href: anchorExposure},
			Since:  in.ExposedDeadline.Since,
		}

	case in.Paused:
		// Watching has stopped. Every other indicator would read as calm, which is
		// exactly why this outranks them: a dashboard reporting a chain nobody
		// needs watched is worse than one reporting nothing.
		return Headline{
			State:  StateActionNeeded,
			Title:  "You need to do something now.",
			Detail: "Forktower stopped watching because it is not looking at the chain it should be.",
			Action: actionFixSetup(),
			Since:  in.PausedSince,
		}

	case !in.AlertsReachable:
		// Outranks the split below it, because a split is something to watch while
		// this is something only the user can fix. But it must not *hide* the
		// split: someone reading this during one needs both facts, and there is
		// only one line to give them.
		detail := "Forktower has no way to reach you, so you would only find out " +
			"something was wrong by looking at this page."
		if splitting(in.Phase) {
			detail = "The chains have separated — and Forktower has no way to reach " +
				"you, so you would only find out by looking at this page."
		}
		return Headline{
			State:  StateActionNeeded,
			Title:  "You need to do something now.",
			Detail: detail,
			Action: actionSetUpAlerts(),
			Since:  in.DetectedAt,
		}

	case in.Phase == store.StateUnarmed:
		// Starting up. A backend still syncing here is expected rather than a
		// fault, which is why this is checked before the degraded-view case.
		return Headline{
			State:  StateGettingReady,
			Title:  "Getting set up — nothing to do yet.",
			Detail: "Forktower is connecting to both chains. Nothing needs your attention while it does.",
		}

	case in.Phase == store.StateSplit:
		return Headline{
			State: StateAttention,
			Title: titleAttention,
			Detail: "The chains have separated: your node and the rest of the network " +
				"no longer agree. Forktower is watching both.",
			Since: in.DetectedAt,
		}

	case in.Phase == store.StateResolving:
		return Headline{
			State: StateAttention,
			Title: titleAttention,
			Detail: "The split may be ending — one of the chains has stopped producing " +
				"blocks. Forktower is still watching both.",
			Since: in.DetectedAt,
		}

	case len(in.FailingChecks) > 0:
		// The first failing check, because there is room for one thing and the list
		// is in priority order. The rest are on the page below.
		first := in.FailingChecks[0]
		return Headline{
			State:  StateAttention,
			Title:  "Something needs a look — not urgent.",
			Detail: headlineDetail(first),
			Action: first.Action,
		}

	case degraded(in.SFHealth) || degraded(in.SQHealth):
		return Headline{
			State: StateAttention,
			Title: titleAttention,
			Detail: "Forktower is having trouble seeing one of the chains. It is still " +
				"watching, and will keep trying.",
		}

	case in.Phase == store.StateResolvedSFWon || in.Phase == store.StateResolvedSQWon:
		return Headline{
			State:  StateProtected,
			Title:  "Watching. The split has ended.",
			Detail: "The chains agree again. Forktower is still watching, just in case.",
			Since:  in.DetectedAt,
		}

	default:
		// Most users will only ever see this one. A tool that looks worried when
		// nothing is wrong teaches people to ignore it.
		return Headline{
			State:  StateProtected,
			Title:  "Watching. Your channels look fine.",
			Detail: "Your node and the rest of the network are on the same chain.",
		}
	}
}

// headlineDetail states the consequence for the user, falling back to the label
// when a check has nothing to add. A warning with no explanation is anxiety, not
// information.
func headlineDetail(item ReadinessItem) string {
	if item.Why != "" {
		return item.Why
	}
	return item.Label
}

// splitting reports whether the chains are currently apart.
func splitting(phase store.SplitState) bool {
	return phase == store.StateSplit || phase == store.StateResolving
}

func degraded(h chainview.HealthState) bool {
	switch h {
	case chainview.HealthOK:
		return false
	case chainview.HealthSyncing, chainview.HealthDegraded,
		chainview.HealthEclipseSuspect, chainview.HealthWrongBranch, chainview.HealthDown:
		return true
	default:
		return true
	}
}
