package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// Readiness check ids. Machine-readable and stable; the label is what a user
// sees, and it never contains one of these.
const (
	CheckSQBackendDistinct = "sq_backend_distinct"
	CheckSQSynced          = "sq_synced"
	CheckSQOnBranch        = "sq_on_branch"
	CheckSFEnforcing       = "sf_enforcing"
	CheckAlertTransports   = "alert_transports"
	CheckLNConnected       = "ln_connected"
)

// ReadinessItem is one thing that is or is not in place.
//
// `why` states the consequence for the user's money and may be empty when the
// check passes. `action` is one action or nothing: a failing check the user
// cannot act on is a source of anxiety rather than information, so where there is
// genuinely nothing to do, the reason says so and the action is absent.
type ReadinessItem struct {
	ID     string  `json:"id"`
	OK     bool    `json:"ok"`
	Label  string  `json:"label"`
	Why    string  `json:"why"`
	Detail string  `json:"detail"`
	Action *Action `json:"action"`

	// informational marks a check that reports a fact rather than a problem, so
	// that it does not drag the headline down. Not serialised: it is an internal
	// distinction, and exposing it would invite the UI to make its own judgement
	// about which failures matter.
	informational bool
}

// Readiness returns the fixed, ordered list of checks.
//
// Ordered by how much the user's protection depends on them, because the headline
// shows the first failing one and the dashboard renders them in this order.
func (s *Server) Readiness(ctx context.Context) []ReadinessItem {
	checks := s.sentinel.Checks()
	sfView, sqView := s.sentinel.Views()
	sfIdentity, _ := s.sentinel.Identities()

	return []ReadinessItem{
		s.checkDistinct(checks),
		s.checkSQSynced(sqView),
		s.checkSQOnBranch(checks),
		s.checkSFEnforcing(sfIdentity, sfView),
		s.checkAlertTransports(ctx),
		s.checkLNConnected(),
	}
}

// blockingFailures are the failing checks the headline should react to.
//
// Informational items are excluded. A check that reports "this arrives in a later
// version" is not a fault, and letting it colour the whole dashboard red would
// teach the user that red means nothing.
func blockingFailures(items []ReadinessItem) []ReadinessItem {
	var out []ReadinessItem
	for _, item := range items {
		if !item.OK && !item.informational {
			out = append(out, item)
		}
	}
	return out
}

// checkDistinct is the one whose failure is invisible everywhere else: two
// configurations reaching one node produce views that agree by construction, so
// every other check passes forever while nothing is being watched.
func (s *Server) checkDistinct(c sentinel.Checks) ReadinessItem {
	switch {
	case c.DistinctNodes && c.DistinctVerified:
		return ReadinessItem{
			ID: CheckSQBackendDistinct, OK: true,
			Label: "Watching two independent views of the chain",
		}
	case !c.DistinctVerified:
		return ReadinessItem{
			ID: CheckSQBackendDistinct, OK: false,
			Label: "Cannot confirm the two chain views are separate",
			Why: "If they turned out to be the same node, Forktower would see them " +
				"agree forever and never notice a split.",
			Detail: c.Detail,
		}
	default:
		return ReadinessItem{
			ID: CheckSQBackendDistinct, OK: false,
			Label: "The two chain views are the same node",
			Why: "Forktower would be comparing a node against itself, so it could " +
				"never see a split.",
			Action: actionFixSetup(),
		}
	}
}

func (s *Server) checkSQSynced(v chainview.BackendHealth) ReadinessItem {
	switch v.State {
	case chainview.HealthOK:
		return ReadinessItem{ID: CheckSQSynced, OK: true, Label: "Watching the other chain"}

	case chainview.HealthSyncing:
		return ReadinessItem{
			ID: CheckSQSynced, OK: false,
			Label:  "Still catching up with the other chain",
			Why:    "Forktower cannot see the whole picture yet. This usually finishes on its own.",
			Detail: syncDetail(v.SyncProgress),
		}

	case chainview.HealthDown:
		return ReadinessItem{
			ID: CheckSQSynced, OK: false,
			Label:  "Cannot reach the other chain",
			Why:    "Forktower would not see a channel being closed on that chain.",
			Action: actionFixSetup(),
		}

	case chainview.HealthDegraded, chainview.HealthEclipseSuspect, chainview.HealthWrongBranch:
		return ReadinessItem{
			ID: CheckSQSynced, OK: false,
			Label:  "Having trouble seeing the other chain",
			Why:    "Forktower may not see everything happening there.",
			Detail: v.Detail,
		}

	default:
		return ReadinessItem{
			ID: CheckSQSynced, OK: false,
			Label: "Cannot tell whether the other chain is being watched",
			Why:   "Forktower may not see a channel being closed on that chain.",
		}
	}
}

func syncDetail(progress float64) string {
	if progress <= 0 || progress >= 1 {
		return ""
	}
	return fmt.Sprintf("About %.0f%% of the way there.", progress*100)
}

func (s *Server) checkSQOnBranch(c sentinel.Checks) ReadinessItem {
	if c.OnExpectedBranch {
		return ReadinessItem{
			ID: CheckSQOnBranch, OK: true,
			Label: "The other chain is the one it should be",
		}
	}
	// Not yet checked is different from checked and wrong, and the daemon
	// distinguishes them; both leave the user without the assurance, so both are
	// reported honestly rather than as a pass.
	if c.BranchVerifiedAt == 0 {
		return ReadinessItem{
			ID: CheckSQOnBranch, OK: false, informational: true,
			Label: "Not yet checked which chain the second view follows",
			Why: "There is nothing to compare until the chains actually differ. " +
				"Nothing for you to do.",
		}
	}
	return ReadinessItem{
		ID: CheckSQOnBranch, OK: false,
		Label: "The second view is following the wrong chain",
		Why: "Forktower paused watching, because reporting calmly about the wrong " +
			"chain would look like safety.",
		Detail: c.Detail,
		Action: actionFixSetup(),
	}
}

// checkSFEnforcing reports, best-effort, which set of rules the user's own node
// follows.
//
// Written for someone learning this for the first time rather than for someone
// auditing a configuration. Most affected operators never made a deliberate
// choice: their node software required a confirmation string before it would
// start, so they typed it to get running again. For many of them this line is the
// first thing that tells them where they stand.
//
// Only a real divergence settles it, so it is always presented as *likely*.
func (s *Server) checkSFEnforcing(id chainview.Identity, v chainview.BackendHealth) ReadinessItem {
	if id.Subversion == "" {
		reason := "Forktower could not ask your node which software it runs."
		if v.State == chainview.HealthDown {
			reason = "Forktower cannot reach your node at the moment."
		}
		return ReadinessItem{
			ID: CheckSFEnforcing, OK: false, informational: true,
			Label:  "Not sure which rules your own node follows",
			Why:    "Forktower watches both chains either way, so you are covered. " + reason,
			Detail: reason,
		}
	}

	// The node's own version string is what this is read from, but it is a raw
	// string from a Bitcoin node and belongs under Advanced with the rest of
	// them, not in a sentence on the front page. It travels in the chain-view
	// details instead.
	if enforcesNewRules(id.Subversion) {
		return ReadinessItem{
			ID: CheckSFEnforcing, OK: true, informational: true,
			Label: "Your node most likely follows the new rules",
			Why: "If the network splits, your node would be on the side enforcing " +
				"them. Forktower watches the other side for you.",
			Detail: "Based on how your node describes itself.",
		}
	}
	return ReadinessItem{
		ID: CheckSFEnforcing, OK: true, informational: true,
		Label: "Your node most likely follows the existing rules",
		Why: "If the network splits, Forktower watches the other side for you " +
			"either way.",
		Detail: "Based on how your node describes itself.",
	}
}

// enforcingClients are the software names that ship the new rules. Matched on the
// node's own self-description, which is the only thing available without asking
// it to prove anything.
var enforcingClients = []string{"knots"}

func enforcesNewRules(subversion string) bool {
	lower := strings.ToLower(subversion)
	for _, name := range enforcingClients {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

func (s *Server) checkAlertTransports(ctx context.Context) ReadinessItem {
	noTransports := s.alerter == nil || len(s.alerter.TransportNames()) == 0

	// On StartOS and Umbrel the platform raises the alerts, by reading this
	// daemon's API — neither platform's notification system can be reached from
	// inside an app container. So "no transports configured" is the normal,
	// correct state there, and reporting it as a problem would send people
	// hunting for a setting that should stay empty.
	if noTransports && s.cfg.PlatformNotifications {
		return ReadinessItem{
			ID: CheckAlertTransports, OK: true,
			Label: "Alerts reach you through this device's own notifications",
		}
	}

	if noTransports {
		return ReadinessItem{
			ID: CheckAlertTransports, OK: false,
			Label:  "No way to reach you",
			Why:    "If something goes wrong, you would only find out by looking at this page.",
			Action: actionSetUpAlerts(),
		}
	}

	tested, failing := s.transportHealth(ctx)
	switch {
	case !tested:
		return ReadinessItem{
			ID: CheckAlertTransports, OK: false, informational: true,
			Label: "Notifications not tested yet",
			Why:   "Forktower will send itself a test message shortly. Nothing for you to do.",
		}
	case len(failing) == 0:
		return ReadinessItem{ID: CheckAlertTransports, OK: true, Label: "Alerts can reach you"}
	default:
		return ReadinessItem{
			ID: CheckAlertTransports, OK: false,
			Label:  "Alerts are not getting through",
			Why:    "If something goes wrong we would have no way to tell you.",
			Detail: fmt.Sprintf("The last test to %s failed.", humanList(failing)),
			Action: actionTestAlerts(),
		}
	}
}

// transportHealth reads the outcome of the last self-test rather than the mere
// presence of configuration. A transport that is configured and broken is the
// case this check exists for.
func (s *Server) transportHealth(ctx context.Context) (tested bool, failing []string) {
	at, err := s.store.GetMetaInt64(ctx, store.MetaLastSelfTestAt, 0)
	if err != nil || at == 0 {
		return false, nil
	}

	alerts, err := s.store.ListAlerts(ctx, store.AlertFilter{UnackedOnly: true})
	if err != nil {
		return true, nil
	}
	for _, a := range alerts {
		if a.Kind == alert.KindTransportFailing {
			failing = append(failing, a.Subject)
		}
	}
	return true, failing
}

func humanList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// LNStaleAfter is how long a Lightning node may go unread before the dashboard
// says so. Several times the poll interval, so a single slow read is not news.
const LNStaleAfter = 5 * time.Minute

// checkLNConnected reports whether the user's Lightning nodes are being read.
//
// No node configured is informational rather than a fault: watching the chains
// is useful on its own, and a user who has not connected one yet has not done
// anything wrong. A node that *is* configured and cannot be read is a real
// failure, because it is the difference between protection the user believes
// they have and protection they have.
func (s *Server) checkLNConnected() ReadinessItem {
	if s.ln == nil {
		return s.noLightningConfigured()
	}
	health := s.ln.Health()
	if len(health) == 0 {
		return s.noLightningConfigured()
	}

	now := s.now().Unix()
	var stale []string
	for _, h := range health {
		if h.Stale(now, LNStaleAfter) {
			stale = append(stale, h.Name)
		}
	}
	if len(stale) == 0 {
		return ReadinessItem{
			ID: CheckLNConnected, OK: true,
			Label: "Reading your channels",
			Why: "Forktower can see your Lightning node, so it knows which of your " +
				"channels would be exposed on the other chain.",
		}
	}
	return ReadinessItem{
		ID: CheckLNConnected, OK: false,
		Label: "Cannot read your Lightning node",
		Why: "Forktower is still watching the chains, and it is still watching the " +
			"channels it already knew about — but it cannot see " + humanList(stale) +
			", so a channel opened or closed since then may be missing.",
	}
}

// noLightningConfigured is the "nothing to do here" answer, marked informational
// so that a feature the user has not set up does not turn the dashboard red. A
// user who learns that red means nothing will not look when red means something.
func (s *Server) noLightningConfigured() ReadinessItem {
	return ReadinessItem{
		ID: CheckLNConnected, OK: false, informational: true,
		Label: "No Lightning node connected",
		Why: "Forktower is watching the chains, which is what matters first. " +
			"Connect your Lightning node and it will also track which of your " +
			"channels are exposed.",
	}
}
