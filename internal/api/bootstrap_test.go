package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bootstrap"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"

	"github.com/paulscode/forktower/internal/alert"
)

// fakeBootstrap is a scripted snapshot shortcut.
type fakeBootstrap struct {
	mu      sync.Mutex
	state   bootstrap.State
	started int
	stopped int
	startEr error
}

func newFakeBootstrap(phase bootstrap.Phase) *fakeBootstrap {
	return &fakeBootstrap{state: bootstrap.State{
		Phase:    phase,
		Snapshot: bootstrap.MainnetHeight935000,
	}}
}

func (f *fakeBootstrap) State() bootstrap.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeBootstrap) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	return f.startEr
}

func (f *fakeBootstrap) Cancel(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	return nil
}

func (f *fakeBootstrap) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

// An installation without the shortcut says so plainly, and its endpoints do not
// pretend to work.
func TestWithoutTheShortcutTheDashboardIsToldThereIsNothingToShow(t *testing.T) {
	h := newHarness(t, nil)

	view := h.srv.bootstrapView()
	if view.Available {
		t.Error("a server with no snapshot shortcut reported one as available")
	}

	resp := h.do(t, "POST", PathBootstrapStart, "{}")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 404 {
		t.Errorf("starting a shortcut that does not exist returned %d, want 404",
			resp.StatusCode)
	}
}

// The offer states its costs, and the download is the first of them.
//
// Putting the benefit first and the costs in a footnote is how consent forms are
// written by people who do not want them read. This is the one place Forktower
// reaches out to a machine the user does not own, and the size, the source and
// the fact that it is the only such request all have to be on the screen before
// anybody presses the button.
func TestTheOfferSaysWhatItCostsBeforeItIsAccepted(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.MountBootstrap(newFakeBootstrap(bootstrap.PhaseOffered))

	view := h.srv.bootstrapView()
	if !view.Available {
		t.Fatal("the offer was not shown")
	}
	if len(view.Why) == 0 {
		t.Fatal("the offer lists nothing about what it costs")
	}
	if !strings.Contains(view.Why[0], "GB") {
		t.Errorf("the first thing said about the offer is %q, which does not "+
			"mention how much it downloads", view.Why[0])
	}

	all := strings.Join(view.Why, " ")
	for _, expected := range []string{"deleted", "Bitcoin Core", "only thing"} {
		if !strings.Contains(all, expected) {
			t.Errorf("the offer never mentions %q: %v", expected, view.Why)
		}
	}
	if view.Action == nil || view.Action.Endpoint != PathBootstrapStart {
		t.Error("the offer has no button that starts it")
	}
}

// None of the words a user reads are the words the implementation uses.
//
// "assumeutxo", "UTXO set" and "chainstate" are the correct terms and the wrong
// ones. The person reading installed an app to protect their Lightning channels.
func TestTheCardAvoidsTheImplementationsVocabulary(t *testing.T) {
	h := newHarness(t, nil)

	for _, phase := range []bootstrap.Phase{
		bootstrap.PhaseOffered, bootstrap.PhaseDownloading,
		bootstrap.PhaseLoading, bootstrap.PhaseDone, bootstrap.PhaseFailed,
	} {
		h.srv.MountBootstrap(newFakeBootstrap(phase))
		view := h.srv.bootstrapView()
		prose := strings.ToLower(view.Title + " " + view.Detail + " " +
			strings.Join(view.Why, " "))

		for _, jargon := range []string{
			"assumeutxo", "utxo", "chainstate", "loadtxoutset", "txoutset",
			"snapshot", "ibd", "initial block download",
		} {
			if strings.Contains(prose, jargon) {
				t.Errorf("in phase %q the card says %q: %q", phase, jargon, prose)
			}
		}
	}
}

func TestStartingAndStoppingReachTheRunner(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseOffered)
	h.srv.MountBootstrap(fake)

	resp := h.do(t, "POST", PathBootstrapStart, "{}")
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("starting returned %d", resp.StatusCode)
	}
	if fake.starts() != 1 {
		t.Errorf("the runner was started %d times", fake.starts())
	}

	resp = h.do(t, "POST", PathBootstrapCancel, "{}")
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("stopping returned %d", resp.StatusCode)
	}
	fake.mu.Lock()
	stopped := fake.stopped
	fake.mu.Unlock()
	if stopped != 1 {
		t.Errorf("the runner was stopped %d times", stopped)
	}
}

func TestARefusedStartIsReportedRatherThanSwallowed(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseOffered)
	fake.startEr = errors.New("the shortcut is switched off")
	h.srv.MountBootstrap(fake)

	resp := h.do(t, "POST", PathBootstrapStart, "{}")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 409 {
		t.Errorf("a refused start returned %d, want 409", resp.StatusCode)
	}
}

// Both endpoints change something, so both are behind the same origin check as
// everything else that does.
func TestTheShortcutCannotBeStartedFromAnotherSite(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseOffered)
	h.srv.MountBootstrap(fake)

	for _, path := range []string{PathBootstrapStart, PathBootstrapCancel} {
		resp := h.doWith(t, "POST", path, "{}", func(r *http.Request) {
			r.Header.Set("Origin", "https://somewhere-else.invalid")
		})
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s accepted a request from another site (%d)", path, resp.StatusCode)
		}
	}
	if fake.starts() != 0 {
		t.Error("a cross-site request reached the runner")
	}
}

// The moment the offer matters is the moment the user is looking at "still
// catching up".
//
// Three days of syncing is three days with no protection, and this is the only
// window in which the shortcut helps. An offer surfaced anywhere else would be
// found after the wait it exists to prevent.
func TestTheSyncCheckOffersTheShortcutWhileItWouldStillHelp(t *testing.T) {
	h := newHarness(t, nil)
	h.sen.mu.Lock()
	h.sen.sqView = chainview.BackendHealth{
		State: chainview.HealthSyncing, SyncProgress: 0.2,
	}
	h.sen.mu.Unlock()

	// Without the shortcut, catching up is something to wait out.
	item := findCheck(t, h.srv.Readiness(context.Background()), CheckSQSynced)
	if item.Action != nil {
		t.Error("catching up offered an action with no shortcut available")
	}
	if !waitingOn(item) {
		t.Error("catching up is not presented as something to wait for")
	}

	// With it, there is something to do, and it is no longer presented as a wait.
	h.srv.MountBootstrap(newFakeBootstrap(bootstrap.PhaseOffered))
	item = findCheck(t, h.srv.Readiness(context.Background()), CheckSQSynced)
	if item.Action == nil || item.Action.Endpoint != PathBootstrapStart {
		t.Fatal("the shortcut was not offered while the second node was catching up")
	}
	if waitingOn(item) {
		t.Error("the offer is buried in the list of things to wait for")
	}
	if !strings.Contains(item.Why, "faster") {
		t.Errorf("the reason given is %q, which does not mention a faster way", item.Why)
	}
}

// A shortcut already running, or already taken, is not something to put a button
// in front of somebody for.
func TestTheOfferIsNotRepeatedOnceItHasBeenAccepted(t *testing.T) {
	h := newHarness(t, nil)
	h.sen.mu.Lock()
	h.sen.sqView = chainview.BackendHealth{State: chainview.HealthSyncing}
	h.sen.mu.Unlock()

	for _, phase := range []bootstrap.Phase{
		bootstrap.PhaseDownloading, bootstrap.PhaseLoading,
		bootstrap.PhaseDone, bootstrap.PhaseUnavailable,
	} {
		h.srv.MountBootstrap(newFakeBootstrap(phase))
		item := findCheck(t, h.srv.Readiness(context.Background()), CheckSQSynced)
		if item.Action != nil {
			t.Errorf("in phase %q the readiness list still offers to start it", phase)
		}
	}
}

// Progress is a sentence as well as a bar, because a bar tells a screen reader
// nothing.
func TestProgressIsAvailableAsWords(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseDownloading)
	fake.state.Progress = bootstrap.Progress{
		BytesDone:  4 << 30,
		BytesTotal: bootstrap.MainnetHeight935000.TotalBytes(),
		Part:       3,
		Parts:      5,
		Remaining:  2 * 60 * 60 * 1e9,
	}
	h.srv.MountBootstrap(fake)

	view := h.srv.bootstrapView()
	if view.Human == "" {
		t.Fatal("a running transfer reports no progress in words")
	}
	for _, expected := range []string{"4.0 GB", "part 3 of 5", "2 hours"} {
		if !strings.Contains(view.Human, expected) {
			t.Errorf("the progress sentence %q does not mention %q", view.Human, expected)
		}
	}
	if view.Percent <= 0 || view.Percent >= 100 {
		t.Errorf("percent = %v for a part-finished transfer", view.Percent)
	}
}

// Somebody who stopped a transfer should be able to see that resuming would not
// start over.
func TestAStoppedTransferSaysHowMuchIsAlreadyFetched(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseFailed)
	fake.state.StagedBytes = 3 << 30
	h.srv.MountBootstrap(fake)

	view := h.srv.bootstrapView()
	if !strings.Contains(view.Human, "3.0 GB") {
		t.Errorf("a stopped transfer reports %q, which does not say what is "+
			"already on disk", view.Human)
	}
}

func findCheck(t *testing.T, items []ReadinessItem, id string) ReadinessItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("no readiness item with id %q", id)
	return ReadinessItem{}
}

// The card must not promise a time the default configuration cannot keep.
//
// The shortcut was measured at 48 minutes over a direct connection, and the
// download goes through the same Tor proxy the second node peers over unless
// somebody changed that. On real hardware over Tor it ran to several hours.
// Quoting the fast figure alone quotes the configuration almost nobody has.
func TestTheOfferDoesNotPromiseAnHourItCannotKeep(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.MountBootstrap(newFakeBootstrap(bootstrap.PhaseOffered))

	view := h.srv.bootstrapView()
	prose := strings.ToLower(view.Title + " " + view.Detail)

	if strings.Contains(prose, "within the hour") {
		t.Errorf("the card promises the hour: %q", prose)
	}
	// Where an hour is mentioned it has to be qualified by how you are connected.
	if strings.Contains(prose, "hour") && !strings.Contains(prose, "direct") {
		t.Errorf("the card quotes an hour without saying it needs a direct "+
			"connection: %q", prose)
	}
	if !strings.Contains(prose, "three days") {
		t.Errorf("the card no longer says what the alternative costs: %q", prose)
	}
}

// "Not answering" and "answering, but blind" are different things.
//
// A watchtower whose chain backend is still catching up answers every request
// perfectly and simply cannot see the other chain yet. Reported as "not
// answering" — which is what shipped — it sends the user looking for a fault in
// a component that is working, at the one moment they are least able to tell.
func TestATowerWhoseNodeIsBehindIsNotCalledUnresponsive(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind:   store.TowerLND,
		Pubkey: "02aa",
		URI:    "02aa@forktower.startos:9911",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Status is written by the warden through its own call, not by the upsert —
	// which is how the real one gets there.
	if err := h.store.SetTowerStatus(ctx, id, store.TowerHealth{
		Status: store.TowerTemporarilyUnreachable,
		Detail: "the tower is running but its node is still catching up with " +
			"the chain (height 938061), so it would not see a breach yet",
	}, h.clock.Load()); err != nil {
		t.Fatal(err)
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(strings.ToLower(item.Label), "not answering") {
		t.Errorf("a tower that is answering was labelled %q", item.Label)
	}
	if !strings.Contains(item.Why, "running") {
		t.Errorf("the reason %q does not say the tower is working", item.Why)
	}
	if !strings.Contains(strings.ToLower(item.Why), "on its own") {
		t.Errorf("the reason %q does not say this resolves without the user", item.Why)
	}
}

// The reading phase says how long it has been reading.
//
// **It is the one part of the shortcut with nothing to show.** The node answers
// nothing while it works, so there is no progress to poll — and a card that says
// "several minutes" and then says nothing else for half an hour is
// indistinguishable from one that has hung. Seen on an appliance, where the read
// took considerably longer than the fast machine the figure came from.
func TestTheReadingPhaseSaysHowLongItHasBeenReading(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseLoading)
	fake.state.LoadStartedAt = h.clock.Load() - int64(37*time.Minute/time.Second)
	h.srv.MountBootstrap(fake)

	view := h.srv.bootstrapView()
	if view.Human == "" {
		t.Fatal("the reading phase reports nothing at all about how long it has run")
	}
	if !strings.Contains(view.Human, "37 minutes") {
		t.Errorf("elapsed reads %q, which does not say how long", view.Human)
	}
	// And the figure quoted is no longer the fast machine's alone.
	if strings.Contains(view.Detail, "several minutes") {
		t.Errorf("the detail still promises several minutes: %q", view.Detail)
	}
	if !strings.Contains(view.Detail, "appliance") {
		t.Errorf("the detail does not say it is slower on small hardware: %q",
			view.Detail)
	}
}

// Just handed over is not "reading for 0 minutes".
func TestAReadThatHasJustBegunSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	fake := newFakeBootstrap(bootstrap.PhaseLoading)
	fake.state.LoadStartedAt = h.clock.Load()
	h.srv.MountBootstrap(fake)

	if got := h.srv.bootstrapView().Human; got != "Just started." {
		t.Errorf("Human = %q at the moment of handover", got)
	}
}

// A phase that never recorded a start says nothing rather than inventing one.
func TestNoStartTimeProducesNoElapsedClaim(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.MountBootstrap(newFakeBootstrap(bootstrap.PhaseLoading))
	if got := h.srv.bootstrapView().Human; got != "" {
		t.Errorf("Human = %q with no recorded start", got)
	}
}

// A watchtower this installation runs has not appeared yet — which is not the
// same as there being none.
//
// **Reported from a fresh install.** The tower is written into the record the
// first time it answers, and lnd takes a while to open its listener. For that
// whole window the dashboard said "No watchtower is protecting your channels",
// which is exactly the sentence a new user should be able to trust, about the
// component the app was starting for them underneath.
func TestATowerThisInstallRunsIsNotReportedAsAbsentWhileItStarts(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })

	item := findCheck(t, h.srv.Readiness(context.Background()), CheckTowerProtection)
	if strings.Contains(item.Label, "No watchtower") {
		t.Errorf("a tower that is starting was reported as absent: %q", item.Label)
	}
	if item.Action != nil {
		t.Error("the user was asked to go and find a tower the app is already starting")
	}
	if !waitingOn(item) {
		t.Error("a tower that is coming up on its own was presented as a task")
	}
}

// An installation that runs no tower of its own still says so plainly. That
// sentence is the whole reason this check exists.
func TestAnInstallWithNoTowerOfItsOwnStillSaysThereIsNone(t *testing.T) {
	h := newHarness(t, nil)

	item := findCheck(t, h.srv.Readiness(context.Background()), CheckTowerProtection)
	if !strings.Contains(item.Label, "No watchtower") {
		t.Errorf("label = %q; a user with no tower must be told so", item.Label)
	}
	if item.Action == nil {
		t.Error("no way offered to set one up")
	}
	if waitingOn(item) {
		t.Error("a tower nobody is bringing up was presented as something to wait for")
	}
}

// The directions have to be carryable out in the order given.
//
// **StartOS 0.3.5.1 refuses to save the watchtower client as enabled with an
// empty tower list.** The directions said to switch it on, save, restart, and
// register afterwards — which that platform rejects at the first save, so
// somebody following them got stuck at step two with no indication that the
// sequence itself was impossible.
//
// Getting the address is therefore the first step, not the last, and both
// settings go in one edit.
func TestTheWatchtowerDirectionsCanBeFollowedInOrder(t *testing.T) {
	t.Parallel()

	for _, platform := range []config.Platform{
		config.PlatformStartOS04,
		config.PlatformStartOS035,
		config.PlatformUmbrel,
	} {
		steps := watchtowerGuidance(platform)
		if len(steps) == 0 {
			t.Errorf("%s has no directions at all", platform)
			continue
		}
		joined := strings.ToLower(strings.Join(steps, " "))

		// Every platform needs the address, and it is Forktower that has it.
		if !strings.Contains(joined, "address") {
			t.Errorf("%s never mentions the address to register: %v", platform, steps)
		}

		// Where the address is needed, it must be fetched before it is used.
		copyAt, useAt := -1, -1
		for i, s := range steps {
			l := strings.ToLower(s)
			if copyAt < 0 && (strings.Contains(l, "copy the address") ||
				strings.Contains(l, "paste the address")) {
				copyAt = i
			}
			if useAt < 0 && strings.Contains(l, "wtclient.active") {
				useAt = i
			}
		}
		if copyAt >= 0 && useAt >= 0 && useAt < copyAt {
			t.Errorf("%s tells the user to enable the client at step %d and to get "+
				"the address at step %d; that order cannot be carried out",
				platform, useAt+1, copyAt+1)
		}
	}
}

// The platform that validates both together says so, rather than leaving
// somebody to discover it from a rejected save.
func TestTheOneSaveConstraintIsStated(t *testing.T) {
	t.Parallel()
	joined := strings.Join(watchtowerGuidance(config.PlatformStartOS035), " ")
	if !strings.Contains(joined, "same edit") && !strings.Contains(joined, "together") {
		t.Errorf("the directions do not say both settings must be saved at once: %q",
			joined)
	}
}

// Somebody else's watchtower must not mark this step done.
//
// **Reported by a user on StartOS 0.3.5.1.** They had a third-party tower
// registered, so every branch of this check passed — reachable tower, covered
// channels — and the setup step that would have walked them through registering
// Forktower's address was counted as complete. Nothing ever asked them, and the
// gap the whole program exists to close stayed open quietly.
func TestSomebodyElsesTowerDoesNotSatisfyTheCheckWhereWeRunOne(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	theirs := putTower(t, h, "02"+strings.Repeat("b", 62), false)
	ours := putTower(t, h, "03"+strings.Repeat("c", 62), true)
	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)

	// Their tower is protecting the channel; ours has been looked at and is not.
	for _, c := range []store.Coverage{
		{ChannelID: ch, TowerID: theirs, Coverable: true, Reason: "sessions exist"},
		{ChannelID: ch, TowerID: ours, Coverable: false, Reason: "nothing registered"},
	} {
		if err := h.store.UpsertCoverage(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	// The scout's own verdict, which is what says the node has not registered
	// with ours. Without it this is the *other* state — registered, no session
	// yet — which is not something to ask the user to act on.
	if _, err := h.store.UpsertAlert(ctx, store.Alert{
		Tier: store.TierWarning, Kind: alert.KindTowerNotProtecting,
		DedupKey:  alert.KindTowerNotProtecting + ":ours-not-registered",
		Subject:   "Forktower's watchtower is not registered with your node",
		Message:   "your node is backing up to a watchtower Forktower does not run",
		CreatedAt: 1, LastRaisedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if item.OK {
		t.Fatal("a third-party tower marked the watchtower step done, so the " +
			"user is never asked to register the one with a view of the other chain")
	}
	// **Naming it is the point.** This used to fall through to "Some channels
	// are not covered by a watchtower" — true, and no help: it does not say
	// which tower, does not say what to do, and offers nothing to click.
	if !strings.Contains(item.Label, "Forktower's watchtower") {
		t.Errorf("label = %q, which does not say which tower is missing", item.Label)
	}
	if item.Action == nil {
		t.Error("no way offered to register it")
	}
	if waitingOn(item) {
		t.Error("a registration only the user can make was presented as " +
			"something that resolves itself")
	}
}

// But not before the warden has looked.
//
// The seconds after our tower opens its listener have no coverage verdict at
// all, and reading that as "not registered" would put a task in front of
// somebody during a window where there is nothing to do.
func TestOurTowerIsNotCalledUnregisteredBeforeAnyoneHasLooked(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	putTower(t, h, "03"+strings.Repeat("c", 62), true)

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(item.Label, "not registered") {
		t.Errorf("asked for a registration before anything had checked whether "+
			"it was already done: %q", item.Label)
	}
}

// putTower records a tower, ours or somebody else's, reachable either way.
func putTower(t *testing.T, h *harness, pubkey string, managed bool) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: pubkey, URI: pubkey + "@somewhere.onion:9911",
		Managed: managed, FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTowerStatus(ctx, id,
		store.TowerHealth{Status: store.TowerReachable, Detail: "answering"},
		1_790_000_100); err != nil {
		t.Fatal(err)
	}
	return id
}

// On a platform with no settings screen, having nowhere to send alerts is a
// fact rather than an unfinishable task.
//
// **Reported from a live Umbrel install.** It was shown as "Step 9 of 9", with
// directions to edit the app's docker-compose.yml by hand and restart — to
// somebody who had installed from an app store. The step could not be completed,
// so the setup never read as finished, which is the thing SkipCost's own comment
// warns produces abandoned installs.
func TestNowhereToSendAlertsIsNotASetupStepOnAnAppStorePlatform(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Platform = config.PlatformUmbrel })
	h.alerter.setNames()
	ctx := context.Background()

	item := findCheck(t, h.srv.Readiness(ctx), CheckAlertTransports)
	if item.Action != nil {
		t.Error("a button was offered for something the user cannot do from any " +
			"screen they have")
	}
	if !item.informational {
		t.Error("a condition nobody on this platform can clear was counted as a " +
			"blocking failure, so the dashboard stays red for ever")
	}

	// And it must not hold up the wizard, which was the reported symptom.
	setup := setupOf(t, h)
	for _, w := range setup.Waiting {
		if strings.Contains(w, "reach you") || strings.Contains(w, "Alerts appear") {
			t.Error("presented as something to wait for, when nothing is coming")
		}
	}
	if setup.Step != nil && setup.Step.ID == CheckAlertTransports {
		t.Errorf("still the wizard's current step: %q", setup.Step.Label)
	}

	// But it is still said. Somebody who does not know this will assume that
	// software talking about urgent countdowns will come and find them.
	if item.OK {
		t.Error("a deployment that cannot reach its user was reported as fine")
	}
	if !strings.Contains(item.Label, "nowhere else") {
		t.Errorf("label = %q, which does not say alerts stay on this page", item.Label)
	}
}

// A self-hoster who wrote the compose file is a different audience: naming an
// environment variable is the natural instruction, and it stays a real step.
func TestASelfHostedDeploymentIsStillAskedToSetAlertsUp(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Platform = config.PlatformUnknown })
	h.alerter.setNames()

	item := findCheck(t, h.srv.Readiness(context.Background()), CheckAlertTransports)
	if item.informational {
		t.Error("a deployment whose operator edits its configuration by hand was " +
			"excused from setting up notifications")
	}
	if item.Action == nil {
		t.Error("no way offered to set them up")
	}
}

// Registered, and waiting for a session, is not the same as not registered.
//
// **Reported from a live install.** The user had registered Forktower's watchtower
// with their node — Forktower's own record showed the scout had seen it and
// withdrawn its concern — and the setup wizard still said "Your node is not
// registered with Forktower's watchtower", as step nine of nine, with
// instructions to go and do the thing they had just done. It was inferring "not
// registered" from "not covered", which are different states with different
// remedies, and only one of them has a remedy at all.
func TestRegisteredButNotYetCoveredIsNotReportedAsUnregistered(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	ours := putTower(t, h, "03"+strings.Repeat("c", 62), true)
	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)
	if err := h.store.UpsertCoverage(ctx, store.Coverage{
		ChannelID: ch, TowerID: ours, Coverable: false,
		Reason: "both ends support anchor sessions but the node has not negotiated one",
	}); err != nil {
		t.Fatal(err)
	}
	// No scout concern standing: the node *has* registered.

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(item.Label, "not registered") {
		t.Errorf("label = %q, sending somebody to redo a registration they have "+
			"already made", item.Label)
	}
	if item.Action != nil {
		t.Error("a button was offered for a registration that already exists")
	}
	if !waitingOn(item) {
		t.Error("a session the node agrees on its own was presented as a task")
	}
}

// A watchtower the node cannot reach, holding the coverage, is named and its
// removal spelled out.
//
// **Proven on hardware, not reasoned.** A node holding sessions with a
// watchtower it could no longer reach kept retrying that one and never
// negotiated with Forktower's: zero sessions for a day, then one within three
// minutes of the dead registration being removed. Throughout, Forktower said
// "nothing for you to do".
func TestAnUnreachableTowerHoldingTheCoverageIsNamed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	ours := putTower(t, h, "0315149f"+strings.Repeat("c", 56), true)
	stalePubkey := "021089ec" + strings.Repeat("b", 56)
	stale := putTower(t, h, stalePubkey, false)
	unreachable(t, h, stale)

	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)
	for _, c := range []store.Coverage{
		{ChannelID: ch, TowerID: ours, Coverable: false, Reason: "no session"},
		{ChannelID: ch, TowerID: stale, Coverable: true, Reason: "holds a session"},
	} {
		if err := h.store.UpsertCoverage(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(item.Why, "Nothing for you to do") {
		t.Error("told the user there was nothing to do about the one thing " +
			"stopping their channels being protected")
	}
	if waitingOn(item) {
		t.Error("presented as something that resolves itself; it never will")
	}
	// The key must be filled in. Somebody who needs this message will not go and
	// find their tower's public key.
	if !strings.Contains(item.Detail, stalePubkey) {
		t.Errorf("the command does not carry the key to remove: %q", item.Detail)
	}
	if !strings.Contains(item.Detail, "paulscode.com") {
		t.Error("no offer of help for somebody who cannot run a command")
	}
	// Without this line the user does the obvious thing and loses another day.
	if !strings.Contains(item.Why, "can only add") {
		t.Errorf("does not say the settings screen will not remove it: %q", item.Why)
	}
}

// But not for our own tower merely having no session yet.
//
// That is the ordinary state for the minutes after a fresh registration. Raising
// this for it would tell a healthy user to delete a working watchtower.
func TestNoSessionOnItsOwnDoesNotAccuseAnotherTower(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	ours := putTower(t, h, "0315149f"+strings.Repeat("c", 56), true)
	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)
	if err := h.store.UpsertCoverage(ctx, store.Coverage{
		ChannelID: ch, TowerID: ours, Coverable: false, Reason: "no session",
	}); err != nil {
		t.Fatal(err)
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(item.Label, "cannot reach") {
		t.Errorf("accused a tower that does not exist: %q", item.Label)
	}
	if !waitingOn(item) {
		t.Error("a session the node agrees on its own was presented as a task")
	}
}

// A reachable third-party tower holding the coverage is somebody's deliberate
// arrangement, not a fault.
func TestAReachableOtherTowerIsNotAccused(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunsOwnWatchtower = true })
	ctx := context.Background()

	ours := putTower(t, h, "0315149f"+strings.Repeat("c", 56), true)
	theirs := putTower(t, h, "021089ec"+strings.Repeat("b", 56), false)
	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)
	for _, c := range []store.Coverage{
		{ChannelID: ch, TowerID: ours, Coverable: false, Reason: "no session"},
		{ChannelID: ch, TowerID: theirs, Coverable: true, Reason: "holds a session"},
	} {
		if err := h.store.UpsertCoverage(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if strings.Contains(item.Label, "cannot reach") {
		t.Errorf("a working third-party watchtower was reported as blocking: %q",
			item.Label)
	}
}

// unreachable marks a tower as one the node can no longer reach.
func unreachable(t *testing.T, h *harness, id int64) {
	t.Helper()
	if err := h.store.SetTowerStatus(context.Background(), id,
		store.TowerHealth{Status: store.TowerUnreachable, Detail: "no answer"},
		1_790_000_200); err != nil {
		t.Fatal(err)
	}
}

// The command has to be the one that works on the platform the user is on.
//
// **A command that fails when pasted is worse than none**, and the generic form
// fails on all three packaged platforms: lnd is inside a container on each, and
// on StartOS 0.4.x its TLS certificate does not match localhost. The two StartOS
// forms below were run on real hardware.
func TestTheRemovalCommandFitsThePlatform(t *testing.T) {
	t.Parallel()
	const key = "021089ecbbbb"

	for _, tc := range []struct {
		platform config.Platform
		wants    []string
	}{
		{config.PlatformStartOS04, []string{
			"start-cli package attach lnd", "--rpcserver=127.0.0.1:10009",
		}},
		{config.PlatformStartOS035, []string{
			"podman exec lnd.embassy", "--rpcserver=lnd.embassy:10009",
		}},
		{config.PlatformUmbrel, []string{"docker exec"}},
		{config.PlatformUnknown, []string{"lncli wtclient remove"}},
	} {
		got := removeTowerLine(tc.platform, key)
		for _, want := range tc.wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: command %q does not contain %q", tc.platform, got, want)
			}
		}
		if !strings.HasSuffix(got, key) {
			t.Errorf("%s: the key is not in the command: %q", tc.platform, got)
		}
	}
}

// And the platform's own form reaches the user, not the generic one.
func TestTheMessageCarriesThePlatformsCommand(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.RunsOwnWatchtower = true
		c.Platform = config.PlatformStartOS04
	})
	ctx := context.Background()

	ours := putTower(t, h, "0315149f"+strings.Repeat("c", 56), true)
	stale := putTower(t, h, "021089ec"+strings.Repeat("b", 56), false)
	unreachable(t, h, stale)
	ch := addChannel(t, h, "aa"+strings.Repeat("0", 62), nil)
	for _, c := range []store.Coverage{
		{ChannelID: ch, TowerID: ours, Coverable: false, Reason: "no session"},
		{ChannelID: ch, TowerID: stale, Coverable: true, Reason: "holds a session"},
	} {
		if err := h.store.UpsertCoverage(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	item := findCheck(t, h.srv.Readiness(ctx), CheckTowerProtection)
	if !strings.Contains(item.Detail, "start-cli package attach lnd") {
		t.Errorf("the user is given a command that will not run on their "+
			"platform: %q", item.Detail)
	}
}
