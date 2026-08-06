package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/paulscode/forktower/internal/bootstrap"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
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
