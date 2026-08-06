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
	"github.com/paulscode/forktower/internal/words"
)

// Readiness check ids. Machine-readable and stable; the label is what a user
// sees, and it never contains one of these.
const (
	CheckSQBackendDistinct   = "sq_backend_distinct"
	CheckSQSynced            = "sq_synced"
	CheckSQOnBranch          = "sq_on_branch"
	CheckSFEnforcing         = "sf_enforcing"
	CheckAlertTransports     = "alert_transports"
	CheckLNConnected         = "ln_connected"
	CheckWatchingActive      = "watching_active"
	CheckWatcherProgressing  = "watcher_progressing"
	CheckChannelsInventoried = "channels_inventoried"
	CheckDeadlineInputs      = "deadline_inputs_known"
	CheckTowerProtection     = "tower_protection"
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

	// settling marks something that is on its way and needs nobody.
	//
	// **This distinction has been missing four times and invented differently
	// each time.** A watchtower that has not started yet was reported as no
	// watchtower at all; one whose chain backend was catching up as one that had
	// stopped answering; lnd opening its listener as a tower that was down. Every
	// one of them told a user that protection was absent at the noisiest moment
	// in an installation's life, which is exactly when they are deciding whether
	// to believe the thing.
	//
	// "Not yet" and "not at all" are different, and a check that cannot say which
	// will keep guessing wrong. This is that difference, named once.
	settling bool
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
		s.checkWatchingActive(),
		s.checkWatcherProgressing(),
		s.checkLNConnected(),
		s.checkChannelsInventoried(ctx),
		s.checkDeadlineInputs(),
		s.checkTowerProtection(ctx),
	}
}

// checkTowerProtection says whether anything would answer a breach.
//
// **The item exists so that having no watchtower at all is visible.** The tower
// card renders from what is stored, and a user who never set one up has nothing
// stored — so without this the most common state of all, and the one where the
// response arm can do nothing, is the state nothing anywhere mentions. That is
// the failure this project is about, arrived at through its own dashboard.
//
// Informational rather than blocking. Somebody may have decided against a
// watchtower, or may be relying on their own node being online, and colouring
// the whole page red over a considered choice would teach them that red means
// nothing.
func (s *Server) checkTowerProtection(ctx context.Context) ReadinessItem {
	towers, err := s.store.ListTowers(ctx, store.TowerFilter{})
	if err != nil {
		return ReadinessItem{
			ID: CheckTowerProtection, OK: false, informational: true,
			Label: "Cannot tell whether a watchtower is protecting you",
			Why:   "Forktower could not read its own record of them.",
		}
	}

	if len(towers) == 0 {
		// **A tower this installation runs has not appeared in the record yet,
		// which is not the same as there being none.** It is registered the
		// first time it answers, and lnd takes a while to open its listener — so
		// a fresh install spent that time being told nothing was protecting it,
		// while the thing protecting it was starting up underneath.
		if s.cfg.RunsOwnWatchtower {
			return ReadinessItem{
				ID: CheckTowerProtection, OK: false,
				informational: true, settling: true,
				Label: "Your watchtower is still starting",
				Why: "Forktower runs one here, to watch " + words.OtherChain +
					" for you. It is not ready yet, and there is nothing for you to " +
					"do until it is — this finishes on its own.",
			}
		}
		return ReadinessItem{
			ID: CheckTowerProtection, OK: false, informational: true,
			Label: "No watchtower is protecting your channels",
			Why: "If somebody publishes an old channel state on " + words.OtherChain +
				", nothing would answer it unless your own node happened to be " +
				"watching that chain — and it is not.",
			Action: actionSetUpTower(),
		}
	}

	// **Somebody else's tower must not satisfy this check where we run one.**
	// A user with a third-party tower registered had every branch below pass —
	// their tower was reachable, their channels covered — so the setup step that
	// would have walked them through registering Forktower's address was marked
	// done, and nothing ever asked. An external tower watches whichever chain its
	// operator's node follows, which cannot be seen from here; the tower here is
	// the one with a known view of the chain their own node cannot see. Counting
	// the two as interchangeable is what left the gap open silently.
	if s.cfg.RunsOwnWatchtower {
		var ours, oursWorking int
		for _, t := range towers {
			if !t.Managed {
				continue
			}
			ours++
			if t.Status == store.TowerReachable {
				oursWorking++
			}
		}
		switch {
		case ours == 0:
			// Starting up, exactly as in the empty case above. Not a task.
			return ReadinessItem{
				ID: CheckTowerProtection, OK: false,
				informational: true, settling: true,
				Label: "Your watchtower is still starting",
				Why: "Forktower runs one here, to watch " + words.OtherChain +
					" for you. It is not ready yet, and there is nothing for you to " +
					"do until it is — this finishes on its own.",
			}
		case oursWorking > 0 && s.notUsingOurs(ctx, towers):
			return ReadinessItem{
				ID: CheckTowerProtection, OK: false, informational: true,
				Label: "Your node is not registered with Forktower's watchtower",
				Why: "Whatever else you have registered watches whichever chain " +
					"its operator's node follows, which cannot be seen from here. " +
					"The tower Forktower runs is the one with a known view of " +
					words.OtherChain + " — the chain your own node cannot see.",
				Action: actionSetUpTower(),
			}
		}
	}

	uncovered, err := s.store.ListCoverage(ctx, store.CoverageFilter{UncoverableOnly: true})
	if err == nil && len(uncovered) > 0 {
		return ReadinessItem{
			ID: CheckTowerProtection, OK: false, informational: true,
			Label:  "Some channels are not covered by a watchtower",
			Why:    "A breach against those channels would not be answered.",
			Detail: uncovered[0].Reason,
		}
	}

	working, settling := 0, 0
	for _, t := range towers {
		switch t.Status {
		case store.TowerReachable:
			working++
		case store.TowerTemporarilyUnreachable:
			settling++
		case store.TowerUnreachable, store.TowerSubscriptionError,
			store.TowerMisbehaving, store.TowerStatusUnknown:
			// Genuinely not protecting anything, and the sentence below says so.
			// Listed rather than defaulted so that a status added later has to be
			// classified here on purpose.
		}
	}
	// **"Not answering" and "answering, but blind" are different things, and
	// saying the first when the second is true is a lie about a healthy
	// component.** A tower whose chain backend is still catching up answers
	// every request perfectly and cannot see a breach yet; the warden already
	// says exactly that in its detail, and the label used to flatten it into a
	// fault the user would go looking for.
	if working == 0 && settling > 0 {
		return ReadinessItem{
			ID: CheckTowerProtection, OK: false,
			informational: true, settling: true,
			Label: "Your watchtower is not watching yet",
			Why: "It is running and answering. It cannot see " + words.OtherChain +
				" yet, so it could not act on a breach there. This finishes on " +
				"its own.",
			Detail: towers[0].StatusDetail,
		}
	}
	if working == 0 {
		return ReadinessItem{
			ID: CheckTowerProtection, OK: false, informational: true,
			Label: "Your watchtower is not answering",
			Why: "While it is down, a breach on " + words.OtherChain +
				" would not be answered.",
			Detail: towers[0].StatusDetail,
		}
	}
	return ReadinessItem{
		ID: CheckTowerProtection, OK: true,
		Label: "A watchtower is ready to answer a breach",
	}
}

// notUsingOurs reports whether the node is demonstrably not backing up to a
// tower this installation runs.
//
// **Absence of a verdict is not a verdict.** The warden writes a coverage row
// per channel once it has looked, so no rows at all means it has not looked yet
// — the seconds after the tower opens its listener — and reading that as "not
// registered" would put a task in front of somebody during a window where there
// is nothing to do and it would clear itself. Only a verdict that exists and
// says no counts.
func (s *Server) notUsingOurs(ctx context.Context, towers []store.Tower) bool {
	rows, err := s.store.ListCoverage(ctx, store.CoverageFilter{})
	if err != nil || len(rows) == 0 {
		return false
	}
	ours := make(map[int64]bool, len(towers))
	for _, t := range towers {
		if t.Managed {
			ours[t.ID] = true
		}
	}
	var judged, covered int
	for _, c := range rows {
		if !ours[c.TowerID] {
			continue
		}
		judged++
		if c.Coverable {
			covered++
		}
	}
	return judged > 0 && covered == 0
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
		item := ReadinessItem{
			ID: CheckSQSynced, OK: false,
			Label:  "Still catching up with the other chain",
			Why:    "Forktower cannot see the whole picture yet. This usually finishes on its own.",
			Detail: syncDetail(v.SyncProgress),
		}
		// **The one place where "still syncing" stops being something to wait
		// out.** Three days of catching up is three days with no protection at
		// all, and if the shortcut is available then this is the moment it is
		// worth taking — which is also the only moment the user is looking at
		// this line. An offer made later would be an offer made after the wait it
		// would have saved.
		if s.bootstrapOffered() {
			item.Why = "Forktower cannot see the other chain until this finishes, " +
				"which takes about three days on its own. There is a faster way."
			item.Action = &Action{
				Label:    "Start watching sooner",
				Endpoint: PathBootstrapStart,
			}
		}
		return item

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

// checkWatchingActive reports whether the other chain is being watched at all.
//
// Listed first among the M2 checks and stated plainly, because standing down is
// the one condition where everything else on this page can be green while
// nothing is being watched. Somebody who stood down last month and forgot must
// be able to see that from the top of the page rather than by noticing an
// absence.
func (s *Server) checkWatchingActive() ReadinessItem {
	if s.standDown == nil || s.standDown.Active() {
		return ReadinessItem{
			ID: CheckWatchingActive, OK: true,
			Label: "Watching the other chain",
		}
	}
	return ReadinessItem{
		ID: CheckWatchingActive, OK: false,
		Label: "You have turned off watching the other chain",
		Why: "Nothing on the other chain is being checked. Forktower is still " +
			"running, so everything else here will look normal.",
		Action: &Action{Label: "Start watching again", Endpoint: PathResume},
	}
}

// checkWatcherProgressing reports whether reading the other chain is getting
// anywhere.
//
// The failure this exists for is a quiet one. The mark that says how far has
// been read only moves after a block has been fully processed, so a block that
// fails every attempt freezes it — while the daemon stays up, the backend
// answers, and every other indicator stays green. Nobody would notice, which is
// exactly why it is worth a line of its own.
func (s *Server) checkWatcherProgressing() ReadinessItem {
	if s.watcher == nil {
		return ReadinessItem{
			ID: CheckWatcherProgressing, OK: false, informational: true,
			Label: "Not reading the other chain yet",
		}
	}

	progress := s.watcher.Progress()
	switch {
	case progress.Stalled:
		return ReadinessItem{
			ID: CheckWatcherProgressing, OK: false,
			Label: "Stopped reading the other chain",
			Why: "Forktower cannot get past a block on the other chain, so nothing " +
				"new there is being checked. It is still running, which is why this " +
				"needs saying.",
			Detail: progress.Why,
		}
	case progress.Height == 0:
		return ReadinessItem{
			ID: CheckWatcherProgressing, OK: false, informational: true,
			Label: "Has not read the other chain yet",
			Why:   "Forktower is waiting for its first block from the other chain.",
		}
	case progress.Rescanning():
		return ReadinessItem{
			ID: CheckWatcherProgressing, OK: true,
			Label: "Catching up on the other chain",
			Why: "Forktower is re-reading earlier blocks. It is watching for new " +
				"ones at the same time.",
		}
	default:
		return ReadinessItem{
			ID: CheckWatcherProgressing, OK: true,
			Label: "Reading the other chain",
		}
	}
}

// checkChannelsInventoried reports whether Forktower knows which channels it is
// protecting.
//
// Distinct from being able to reach the Lightning node, and the difference
// matters: a node that answers but has been asked nothing yet, or one whose
// channels all came back unreadable, leaves the watchset empty — and an empty
// watchset scans every block cleanly and finds nothing, which looks exactly like
// safety.
func (s *Server) checkChannelsInventoried(ctx context.Context) ReadinessItem {
	channels, err := s.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		return ReadinessItem{
			ID: CheckChannelsInventoried, OK: false,
			Label: "Cannot tell which channels you have",
			Why: "Forktower could not read its own record of your channels. " +
				"Open Details for what it said.",
			Detail: err.Error(),
		}
	}
	if len(channels) == 0 {
		// Not a fault. Someone may have no channels, or may not have connected a
		// node — and the check above already says which.
		return ReadinessItem{
			ID: CheckChannelsInventoried, OK: false, informational: true,
			Label: "No channels found yet",
			Why: "Forktower has not seen any channels to protect. If you have some, " +
				"check that your Lightning node is connected.",
		}
	}

	var watched int
	for _, c := range channels {
		if c.Relevance != store.Irrelevant {
			watched++
		}
	}
	if watched == 0 {
		return ReadinessItem{
			ID: CheckChannelsInventoried, OK: true,
			Label: "None of your channels are exposed",
			Why: "Forktower knows about " + countPhrase(len(channels), "channel") +
				", and none of them exist on the other chain.",
		}
	}
	return ReadinessItem{
		ID: CheckChannelsInventoried, OK: true,
		Label: "Watching your channels",
		Why: "Forktower is watching " + countPhrase(watched, "channel") +
			" on the other chain.",
	}
}

// countPhrase says a number of things in words a sentence can contain.
func countPhrase(n int, noun string) string {
	if n == 1 {
		return "one " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// checkDeadlineInputs reports whether the countdowns are built on real numbers.
//
// This check exists to be read *before* anything goes wrong, which is the only
// time the answer can still be changed. Once a commitment has confirmed, a
// countdown running on a conservative floor is what the user has, and finding
// out then that their Lightning node never reported the real delay is finding
// out too late to do anything about it.
//
// Never a hard failure. A countdown on a floor is worth far more than no
// countdown, and turning this red would say the opposite.
func (s *Server) checkDeadlineInputs() ReadinessItem {
	if s.deadlines == nil {
		return ReadinessItem{
			ID: CheckDeadlineInputs, OK: false, informational: true,
			Label: "Not tracking any countdowns yet",
			Why: "Once a channel of yours is closed on the other chain, Forktower " +
				"works out how long you have to respond.",
		}
	}

	status := s.deadlines.Status()
	if status.Counting == 0 {
		return ReadinessItem{
			ID: CheckDeadlineInputs, OK: true,
			Label: "No countdown is running",
			Why:   "Nothing has closed any of your channels on the other chain.",
		}
	}
	if status.InputsKnown() {
		return ReadinessItem{
			ID: CheckDeadlineInputs, OK: true,
			Label: "Counting down on real numbers",
			Why: "Forktower knows the actual delay on every channel it is counting, " +
				"so the time it shows you is the time you have.",
		}
	}
	return ReadinessItem{
		ID: CheckDeadlineInputs, OK: false, informational: true,
		Label: "Counting down on an assumed delay",
		Why: "Your Lightning node did not say how long the delay on " +
			plural(len(status.AssumedChannels), "one of your channels", "some of your channels") +
			" is, so Forktower is counting from a cautious floor instead. The " +
			"countdown is real; it may simply be shorter than the truth. " +
			"Reconnecting your Lightning node usually fixes it.",
	}
}

// plural picks the wording for one thing or several.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
