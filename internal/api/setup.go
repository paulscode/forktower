package api

import (
	"net/http"

	"github.com/paulscode/forktower/internal/config"
)

// SetupState is what a first-time user is shown instead of the whole dashboard.
//
// **It asks nothing.** Everything Forktower needs it either already knows or can
// find out: which Lightning node is installed, where the Bitcoin node is, how
// much disk there is, what the fork's heights are. What is left is a small number
// of things it genuinely cannot do on somebody's behalf, and this walks them
// through those one at a time.
//
// The steps are the readiness checks, in the order they already have — ordered by
// how much the user's protection depends on them. Deriving the wizard from the
// same list the dashboard shows means the two can never disagree about whether
// something is done, which is the failure mode of every setup flow that keeps its
// own idea of progress.
type SetupState struct {
	// Complete is true when nothing is left that would stop the user being
	// protected.
	Complete bool `json:"complete"`
	// Step is what to do now, or null when complete. One at a time: eleven red
	// items is a wall, and a wall is where people stop.
	Step *SetupStep `json:"step"`
	// Done and Total count the blocking checks, so there is a sense of progress
	// rather than an unbounded list of things wrong.
	Done  int `json:"done"`
	Total int `json:"total"`
	// Waiting lists things that are not done and are nobody's fault — a chain
	// still syncing. Shown so that a user who cannot finish knows they are
	// waiting rather than stuck.
	Waiting []string `json:"waiting"`
}

// SetupStep is one thing for the user to do.
type SetupStep struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Why    string  `json:"why"`
	Detail string  `json:"detail"`
	Action *Action `json:"action"`
	// Guidance is the platform's own directions, when Forktower has any. Empty
	// where it does not know the platform, rather than guessing — sending
	// somebody to a screen that does not exist wastes more of their time than
	// saying nothing.
	Guidance []string `json:"guidance,omitempty"`
	// Skippable marks a step a user may reasonably decline.
	Skippable bool `json:"skippable"`
	// SkipCost says what protection remains if they do. Stated plainly, because
	// a setup somebody cannot finish is a setup they abandon, and an abandoned
	// install protects nobody.
	SkipCost string `json:"skip_cost,omitempty"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	writeData(w, s.setupState(r))
}

func (s *Server) setupState(r *http.Request) SetupState {
	items := s.Readiness(r.Context())

	out := SetupState{Waiting: []string{}}
	for i := range items {
		item := items[i]
		// Informational checks report a fact rather than a problem. They do not
		// hold up setup, and counting them would tell a user they are unfinished
		// when they are not — except for tower protection, which is
		// informational precisely because it is optional, and is the one thing
		// here worth walking somebody through.
		if item.informational && item.ID != CheckTowerProtection {
			continue
		}
		out.Total++
		if item.OK {
			out.Done++
			continue
		}
		if waitingOn(item) {
			out.Waiting = append(out.Waiting, item.Label)
			out.Done++
			continue
		}
		if out.Step == nil {
			out.Step = s.stepFor(item)
		}
	}

	out.Complete = out.Step == nil
	return out
}

// waitingOn reports whether a failing check is something that resolves itself
// given time.
//
// A chain that has not finished syncing is not a task. Presenting it as one
// leaves a user clicking at something they cannot affect, and eventually
// concluding the software is broken.
//
// **An action changes that answer, whatever the check is.** "Waiting" and "there
// is a button" are contradictory claims, and the case that forced this was the
// snapshot shortcut: the second node syncing is normally nothing to do, and for
// the first hour of an installation's life it is the single most useful thing a
// user could act on. Keying off the action rather than adding an exception keeps
// the two from drifting apart the next time something similar appears.
func waitingOn(item ReadinessItem) bool {
	if item.Action != nil {
		return false
	}
	// On its way and needs nobody, whatever check it came from. Keyed on the
	// fact rather than on a list of ids, because the list is what kept failing
	// to include the case in front of the user.
	if item.settling {
		return true
	}
	switch item.ID {
	case CheckSQSynced, CheckWatcherProgressing, CheckChannelsInventoried:
		return true
	default:
		return false
	}
}

func (s *Server) stepFor(item ReadinessItem) *SetupStep {
	step := &SetupStep{
		ID:     item.ID,
		Label:  item.Label,
		Why:    item.Why,
		Detail: item.Detail,
		Action: item.Action,
	}

	switch item.ID {
	case CheckTowerProtection:
		step.Guidance = watchtowerGuidance(s.cfg.Platform)
		// **Skippable, and honestly so.** A watchtower is the difference between
		// being told about a breach and something being done about it, so this
		// is the step most worth completing — but it needs a tower to register
		// with, which not everybody has to hand. A user who cannot finish this
		// today should be able to get on with the rest rather than abandon the
		// install, and come back.
		step.Skippable = true
		step.SkipCost = "Forktower will still watch both chains and tell you when " +
			"one of your channels is at risk, with a countdown. What it cannot do " +
			"without a watchtower is respond while you are asleep — you would be " +
			"the response."

	case CheckAlertTransports:
		step.Guidance = alertsGuidance(s.cfg.Platform)
		step.Skippable = true
		step.SkipCost = "Alerts will still appear on this dashboard. They will " +
			"only reach you if you are looking at it, and the ones that matter " +
			"most tend to arrive when you are not."
	}

	return step
}

// watchtowerGuidance is where to click, per platform.
//
// **Three platforms, three genuinely different answers**, and on one of them it
// is not a toggle at all: StartOS 0.4.x asks the user to add a tower, and the
// package turns the client on because the list is no longer empty. Naming a
// switch there would send somebody looking for something that is not on their
// screen.
//
// Empty for an unknown platform. A self-hoster knows where their own lnd.conf
// is, and guessing at it would be worse than the silence.
//
// **All three now end by pointing at an address on this page rather than
// telling somebody to go and find one.** They used to say "register a
// watchtower with your node", which was the whole of the advice and none of the
// help: the packaged builds ran no tower, shipped no list, and left the single
// most valuable step in the setup as an exercise.
func watchtowerGuidance(platform config.Platform) []string {
	switch platform {
	case config.PlatformStartOS04:
		return []string{
			"Open the LND service in StartOS.",
			"Go to Actions → Watchtower → Watchtower Client Settings.",
			"Paste the address from the watchtower card on this page. LND turns " +
				"its watchtower client on by itself once there is one in the list " +
				"— there is no separate switch.",
		}
	case config.PlatformStartOS035:
		// **Both in one save, address first.** That package refuses to save the
		// client as enabled with no tower listed, so the order this used to give
		// — switch it on, save, then register — cannot be carried out at all.
		return []string{
			"Copy the address from the watchtower card on this page.",
			"Open the LND service in StartOS, and go to Config.",
			"Add that address to the watchtower list, and turn on the watchtower " +
				"client (wtclient.active), in the same edit. LND will not save the " +
				"client as enabled with an empty list, so both have to go in together.",
			"Save, and restart LND so it takes effect.",
		}
	case config.PlatformUmbrel:
		return []string{
			"Open the Lightning app in Umbrel.",
			"Go to Advanced Settings, and turn on wtclient.active.",
			"Restart the Lightning app so the setting takes effect.",
			"Then register the address from the watchtower card on this page.",
		}
	case config.PlatformUnknown:
		return nil
	default:
		return nil
	}
}

// alertsGuidance is where to set a notification up, per platform.
//
// **The action beside this used to be a button that scrolled to the alerts
// card**, which lists what has happened and can send a test — and configures
// nothing. On a screen where that card was already visible the button did
// nothing at all, which is precisely how it was reported.
//
// Saying where the setting lives is the smallest honest thing that helps.
func alertsGuidance(platform config.Platform) []string {
	switch platform {
	case config.PlatformStartOS04:
		return []string{
			"Open Forktower's own settings in StartOS.",
			"Fill in an ntfy address or a webhook URL, and save.",
			"Forktower sends itself a test message on the next start, so you " +
				"find out the path works while nothing is wrong.",
		}
	case config.PlatformStartOS035:
		return []string{
			"Open Forktower's Config screen in StartOS.",
			"Fill in an ntfy address or a webhook URL under Notifications, and save.",
			"Restart Forktower so the setting takes effect.",
		}
	case config.PlatformUmbrel:
		return []string{
			"Umbrel gives an app no settings screen, so this one is edited by hand.",
			"In the Forktower app's docker-compose.yml, set FORKTOWER_NTFY_URL or " +
				"FORKTOWER_WEBHOOK_URL.",
			"Restart the app.",
			"If that is more than you want to do, the dashboard still shows " +
				"everything — it just cannot come and find you.",
		}
	case config.PlatformUnknown:
		return nil
	default:
		return nil
	}
}
