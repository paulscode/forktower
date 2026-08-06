package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/deadline"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// Rules that hold for every readiness item in every situation. Each of these is
// a sentence a worried person reads, so the shape matters as much as the logic:
// a red mark with no explanation and nothing to do is anxiety, not information.
func assertItemIsFitToShow(t *testing.T, item ReadinessItem) {
	t.Helper()

	if item.Label == "" {
		t.Errorf("%s: no label, so the user sees a blank row", item.ID)
	}
	// Every one of these is rendered on the page, so every one of them has to be
	// fit to read. An earlier version of this checked only the label, and two
	// leaks — a milestone name and a raw node version string — reached a real
	// screen through `detail` before anyone noticed.
	visible := map[string]string{
		"label": item.Label, "why": item.Why, "detail": item.Detail,
	}
	if item.Action != nil {
		visible["action label"] = item.Action.Label
	}

	// The id is machine-readable and stable; it must never appear in what a user
	// reads, and neither must an internal state name, a milestone, or a raw
	// string from a Bitcoin node.
	leaks := []string{
		item.ID, "sq_", "sf_", "ln_", "SYNCING", "DEGRADED", "DOWN",
		"WRONG_BRANCH", "ECLIPSE_SUSPECT", "M1", "M2", "M3", "M4",
		"/Satoshi", "Satoshi:", "subversion", "RPC", "ZMQ", "outpoint", "reorg",
	}
	for field, text := range visible {
		for _, leak := range leaks {
			if leak != "" && strings.Contains(text, leak) {
				t.Errorf("%s: the %s contains %q, which means nothing to a user: %q",
					item.ID, field, leak, text)
			}
		}
	}
	if !item.OK && item.Why == "" {
		t.Errorf("%s: fails without saying what it means for the user", item.ID)
	}
	if item.Action != nil {
		if item.Action.Label == "" {
			t.Errorf("%s: an action with no label", item.ID)
		}
		if (item.Action.Endpoint == "") == (item.Action.Href == "") {
			t.Errorf("%s: an action needs exactly one destination: %+v", item.ID, item.Action)
		}
	}
	// A check that passes has nothing to ask of anyone.
	if item.OK && item.Action != nil {
		t.Errorf("%s: passes but still offers something to do: %+v", item.ID, item.Action)
	}
}

func itemByID(t *testing.T, items []ReadinessItem, id string) ReadinessItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("no readiness check with id %q", id)
	return ReadinessItem{}
}

// The whole list, in every state each check can reach. Table-driven because the
// interesting thing is that no combination produces a row that reads badly.
func TestEveryReadinessCheckReadsWell(t *testing.T) {
	t.Parallel()

	healthStates := []chainview.HealthState{
		chainview.HealthOK, chainview.HealthSyncing, chainview.HealthDegraded,
		chainview.HealthEclipseSuspect, chainview.HealthWrongBranch, chainview.HealthDown,
		chainview.HealthState("SOMETHING_NEW"),
	}
	checkStates := []sentinel.Checks{
		{DistinctNodes: true, DistinctVerified: true, OnExpectedBranch: true, BranchVerifiedAt: 1},
		{DistinctNodes: false, DistinctVerified: false, Detail: "could not confirm"},
		{DistinctNodes: false, DistinctVerified: true},
		{DistinctVerified: true, DistinctNodes: true, OnExpectedBranch: false, BranchVerifiedAt: 0},
		{DistinctVerified: true, DistinctNodes: true, OnExpectedBranch: false, BranchVerifiedAt: 5},
	}

	for _, health := range healthStates {
		for i, checks := range checkStates {
			h := newHarness(t, nil)
			h.sen.set(func(f *fakeSentinel) {
				f.checks = checks
				f.sqView = chainview.BackendHealth{State: health, SyncProgress: 0.42}
			})

			for _, item := range h.srv.Readiness(context.Background()) {
				t.Run(string(health)+"/"+itoa(int64(i))+"/"+item.ID, func(t *testing.T) {
					t.Parallel()
					assertItemIsFitToShow(t, item)
				})
			}
		}
	}
}

// Two configurations reaching one node produce views that agree by construction,
// so every other check passes forever while nothing is watched. Proven the same
// is a fault; unable to tell is a gap. They read differently because the user can
// act on one and not the other.
func TestTheDistinctNodeCheckSeparatesProvenFromUnknown(t *testing.T) {
	t.Parallel()

	sameNode := readinessFor(t, sentinel.Checks{DistinctNodes: false, DistinctVerified: true})
	item := itemByID(t, sameNode, CheckSQBackendDistinct)
	if item.OK {
		t.Error("two views of one node were reported as fine")
	}
	if item.Action == nil {
		t.Error("a fixable misconfiguration was reported with nothing to do about it")
	}

	unknown := readinessFor(t, sentinel.Checks{DistinctVerified: false, Detail: "could not confirm"})
	item = itemByID(t, unknown, CheckSQBackendDistinct)
	if item.OK {
		t.Error("a check that could not be run was reported as passed")
	}
	if item.Action != nil {
		t.Errorf("an unknown is not something the user can fix: %+v", item.Action)
	}

	fine := readinessFor(t, sentinel.Checks{DistinctNodes: true, DistinctVerified: true})
	if !itemByID(t, fine, CheckSQBackendDistinct).OK {
		t.Error("two proven-separate nodes were reported as a problem")
	}
}

// Not yet checked and checked-and-wrong are different things. The first is
// expected until the chains actually differ; the second means watching stopped.
func TestTheBranchCheckSeparatesNotYetFromWrong(t *testing.T) {
	t.Parallel()

	notYet := itemByID(t, readinessFor(t, sentinel.Checks{
		DistinctNodes: true, DistinctVerified: true, BranchVerifiedAt: 0,
	}), CheckSQOnBranch)
	if notYet.Action != nil {
		t.Errorf("a check that has not run yet asks the user to do something: %+v", notYet.Action)
	}
	if !strings.Contains(notYet.Why, "Nothing for you to do") {
		t.Errorf("why = %q, want it to say plainly there is nothing to do", notYet.Why)
	}

	wrong := itemByID(t, readinessFor(t, sentinel.Checks{
		DistinctNodes: true, DistinctVerified: true, BranchVerifiedAt: 5, Detail: "wrong chain",
	}), CheckSQOnBranch)
	if wrong.Action == nil {
		t.Error("a broken setup was reported with nothing to do about it")
	}
}

func TestSyncProgressIsReportedInWordsAUserCanRead(t *testing.T) {
	t.Parallel()

	cases := map[float64]string{
		0.42: "42%",
		0:    "",
		1:    "",
	}
	for progress, want := range cases {
		h := newHarness(t, nil)
		h.sen.set(func(f *fakeSentinel) {
			f.sqView = chainview.BackendHealth{
				State: chainview.HealthSyncing, SyncProgress: progress,
			}
		})
		item := itemByID(t, h.srv.Readiness(context.Background()), CheckSQSynced)
		if want == "" {
			if item.Detail != "" {
				t.Errorf("progress %v produced %q, want nothing rather than a meaningless number",
					progress, item.Detail)
			}
			continue
		}
		if !strings.Contains(item.Detail, want) {
			t.Errorf("progress %v produced %q, want it to mention %q", progress, item.Detail, want)
		}
	}
}

// Several broken transports read as a sentence, not as a slice printed into one.
func TestFailingTransportsAreListedReadably(t *testing.T) {
	t.Parallel()

	cases := map[int]string{1: "my-phone", 2: "and", 3: ", "}
	for count, want := range cases {
		ctx := context.Background()
		h := newHarness(t, nil)

		names := []string{"my-phone", "my-email", "my-webhook"}[:count]
		h.alerter.mu.Lock()
		h.alerter.names = names
		h.alerter.mu.Unlock()

		for _, name := range names {
			if _, err := h.store.UpsertAlert(ctx, store.Alert{
				Tier: store.TierWarning, Kind: "transport_failing",
				DedupKey: "transport_failing:" + name, Subject: name, Message: "broken",
			}); err != nil {
				t.Fatal(err)
			}
		}

		item := itemByID(t, h.srv.Readiness(ctx), CheckAlertTransports)
		if !strings.Contains(item.Detail, want) {
			t.Errorf("with %d failing transports the detail is %q, want it to contain %q",
				count, item.Detail, want)
		}
	}
}

// A list ordered by how much protection depends on it, because the headline shows
// the first failing one and the dashboard renders them in this order.
func TestReadinessOrderIsStable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	want := []string{
		CheckSQBackendDistinct, CheckSQSynced, CheckSQOnBranch,
		CheckSFEnforcing, CheckAlertTransports,
		CheckWatchingActive, CheckWatcherProgressing, CheckLNConnected,
		CheckChannelsInventoried, CheckDeadlineInputs,
		// Last, and informational: whether a watchtower would answer a breach is
		// the response arm's question, and somebody may have decided against one.
		CheckTowerProtection,
	}
	got := h.srv.Readiness(context.Background())
	if len(got) != len(want) {
		t.Fatalf("got %d checks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("check %d is %q, want %q", i, got[i].ID, want[i])
		}
	}
}

// "Not built yet" must not turn the dashboard red: a user who learns that red
// means nothing will not look at it when red means something.
func TestNotYetBuiltDoesNotLookLikeAFault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	items := h.srv.Readiness(context.Background())
	ln := itemByID(t, items, CheckLNConnected)
	if ln.OK {
		t.Error("a connection that does not exist was reported as working")
	}
	if len(blockingFailures(items)) != 0 {
		t.Errorf("an unbuilt feature is dragging the headline down: %+v", blockingFailures(items))
	}

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State != StateProtected {
		t.Errorf("headline = %q, want protected", got.Headline.State)
	}
}

func readinessFor(t *testing.T, checks sentinel.Checks) []ReadinessItem {
	t.Helper()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.checks = checks })
	return h.srv.Readiness(context.Background())
}

// Neither StartOS nor Umbrel exposes its notification system to an app
// container — verified on both. So on those platforms the wrapper reads this
// API and raises the notification itself, and a daemon with no transports
// configured is in its normal, correct state. Reporting that as a problem would
// send people hunting for a setting that is supposed to stay empty.
func TestPlatformNotificationsAreNotAMissingAlarm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, func(c *Config) { c.PlatformNotifications = true })
	h.alerter.mu.Lock()
	h.alerter.names = nil
	h.alerter.mu.Unlock()

	item := itemByID(t, h.srv.Readiness(ctx), CheckAlertTransports)
	if !item.OK {
		t.Errorf("a platform install with no transports was reported as unreachable: %+v", item)
	}
	if item.Action != nil {
		t.Errorf("the user was asked to fix something that is already right: %+v", item.Action)
	}
	assertItemIsFitToShow(t, item)

	// And the headline follows it, rather than telling them to set up something
	// the platform already does.
	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State == StateActionNeeded {
		t.Errorf("headline = %q, want it not to demand notifications setup", got.Headline.State)
	}

	// Without the platform flag, the same daemon says it cannot reach anyone.
	plain := newHarness(t, nil)
	plain.alerter.mu.Lock()
	plain.alerter.names = nil
	plain.alerter.mu.Unlock()
	if itemByID(t, plain.srv.Readiness(ctx), CheckAlertTransports).OK {
		t.Error("a daemon with no way to reach anyone reported that it could")
	}
}

// A platform install that also configures its own transport still hears about
// one that is broken: the flag says the platform helps, not that nothing can
// fail.
func TestPlatformNotificationsDoNotHideABrokenTransport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, func(c *Config) { c.PlatformNotifications = true })
	if _, err := h.store.UpsertAlert(ctx, store.Alert{
		Tier:     store.TierWarning,
		Kind:     alert.KindTransportFailing,
		DedupKey: alert.KindTransportFailing + ":my-phone",
		Subject:  "my-phone",
		Message:  "could not deliver",
	}); err != nil {
		t.Fatal(err)
	}

	item := itemByID(t, h.srv.Readiness(ctx), CheckAlertTransports)
	if item.OK {
		t.Error("a broken transport was hidden by the platform flag")
	}
}

// The Lightning check went from a placeholder to a real reading of the
// registry's health, and each of its three answers means something different to
// a user.
func TestTheLightningCheckReportsWhatIsActuallyHappening(t *testing.T) {
	t.Parallel()

	t.Run("no node configured is not a fault", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.ln.set(nil)

		items := h.srv.Readiness(context.Background())
		ln := itemByID(t, items, CheckLNConnected)
		if ln.OK {
			t.Error("a connection that does not exist was reported as working")
		}
		if len(blockingFailures(items)) != 0 {
			t.Errorf("not having connected a node is dragging the headline down: %+v",
				blockingFailures(items))
		}
		if strings.Contains(strings.ToLower(ln.Label+ln.Why), "later version") {
			t.Error("the dashboard still says channel reading is not built yet")
		}
	})

	t.Run("a node being read is reported as working", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.ln.set([]registry.SourceHealth{{Name: "lnd-1", LastSuccessAt: h.clock.Load()}})

		ln := itemByID(t, h.srv.Readiness(context.Background()), CheckLNConnected)
		if !ln.OK {
			t.Errorf("a node that was just read is reported as %q", ln.Label)
		}
	})

	t.Run("a node that cannot be read is a real failure", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.ln.set([]registry.SourceHealth{{
			Name:          "lnd-1",
			LastSuccessAt: h.clock.Load() - int64(2*LNStaleAfter/time.Second),
			LastError:     "connection refused",
		}})

		items := h.srv.Readiness(context.Background())
		ln := itemByID(t, items, CheckLNConnected)
		if ln.OK {
			t.Error("a node nobody has read for ten minutes was reported as working")
		}
		// Configured-but-unreachable is a genuine gap in protection, not an
		// unbuilt feature, so it does count against the headline.
		if len(blockingFailures(items)) == 0 {
			t.Error("an unreachable Lightning node was hidden from the headline")
		}
		if !strings.Contains(ln.Why, "lnd-1") {
			t.Errorf("the user is not told which node is unreachable: %q", ln.Why)
		}
	})

	t.Run("one healthy node does not excuse a broken one", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.ln.set([]registry.SourceHealth{
			{Name: "lnd-1", LastSuccessAt: h.clock.Load()},
			{Name: "cln-1"}, // never read at all
		})

		ln := itemByID(t, h.srv.Readiness(context.Background()), CheckLNConnected)
		if ln.OK {
			t.Error("a node that has never answered was covered up by one that did")
		}
		if !strings.Contains(ln.Why, "cln-1") || strings.Contains(ln.Why, "lnd-1") {
			t.Errorf("the wrong node is named: %q", ln.Why)
		}
	})
}

// The countdown check exists to be read *before* anything goes wrong, which is
// the only time the answer can still be changed.
func TestTheCountdownCheckSaysWhatIsBeingCountedOn(t *testing.T) {
	t.Parallel()

	t.Run("nothing running is not a problem", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		item := itemByID(t, h.srv.Readiness(context.Background()), CheckDeadlineInputs)
		if !item.OK {
			t.Errorf("having no countdown was reported as a fault: %q", item.Label)
		}
	})

	t.Run("counting on real numbers", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.dl.set(deadline.Status{Counting: 2, EarliestHeight: 900})

		item := itemByID(t, h.srv.Readiness(context.Background()), CheckDeadlineInputs)
		if !item.OK {
			t.Errorf("a countdown on real numbers was reported as a fault: %q", item.Label)
		}
	})

	t.Run("counting on a floor is flagged but not a failure", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.dl.set(deadline.Status{Counting: 2, Assumed: 1, AssumedChannels: []int64{7}})

		items := h.srv.Readiness(context.Background())
		item := itemByID(t, items, CheckDeadlineInputs)
		if item.OK {
			t.Error("a countdown on a guess was reported as fully known")
		}
		// A countdown on a floor is worth far more than no countdown, so this must
		// not drag the headline down as though protection had failed.
		for _, failing := range blockingFailures(items) {
			if failing.ID == CheckDeadlineInputs {
				t.Error("counting from a cautious floor was treated as a broken protection")
			}
		}
		if !strings.Contains(item.Why, "one of your channels") {
			t.Errorf("the wording does not say how many are affected: %q", item.Why)
		}
	})

	t.Run("several channels affected reads correctly", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.dl.set(deadline.Status{Counting: 3, Assumed: 2, AssumedChannels: []int64{7, 9}})

		item := itemByID(t, h.srv.Readiness(context.Background()), CheckDeadlineInputs)
		if !strings.Contains(item.Why, "some of your channels") {
			t.Errorf("the wording does not read for several: %q", item.Why)
		}
	})
}

// A Lightning node that has not been read yet is not one that cannot be read.
//
// **Found sweeping for the mistake that produced 0.6.2 and 0.6.3.** The registry
// hands back one health entry per configured node, zero-valued until the first
// poll finishes — and a zero LastSuccessAt reads as stale, which readiness
// reports as "Cannot read your Lightning node". Not informational: a blocking
// failure, red on the dashboard, from the moment the app starts until the first
// poll returns. On a fresh install where lnd is itself still coming up, that is
// the whole of the user's first impression.
//
// The zero value carries no name either, so the sentence naming what cannot be
// seen had a hole in it.
func TestALightningNodeNotYetReadIsNotOneThatCannotBeRead(t *testing.T) {
	h := newHarness(t, nil)

	// Configured, and the first poll has not come back. This is what the
	// registry returns in that window.
	h.ln.set([]registry.SourceHealth{{Name: "lnd-1"}})

	item := findCheck(t, h.srv.Readiness(context.Background()), CheckLNConnected)
	if !item.OK && !item.informational {
		t.Errorf("a node that has not been polled yet was reported as a blocking "+
			"failure: %q — %q", item.Label, item.Why)
	}
	if strings.Contains(item.Why, "see , ") || strings.Contains(item.Why, "see  ") {
		t.Errorf("the sentence names nothing where a node's name belongs: %q", item.Why)
	}
	if !waitingOn(item) {
		t.Error("a first poll that has not returned was presented as a task")
	}
}

// But one that has been tried and failed is a real problem, and says why.
func TestALightningNodeThatWasTriedAndFailedIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.ln.set([]registry.SourceHealth{{
		Name: "lnd-1", LastError: "connection refused",
	}})

	item := findCheck(t, h.srv.Readiness(context.Background()), CheckLNConnected)
	if item.OK {
		t.Fatal("a node that has never answered was reported as readable")
	}
	if waitingOn(item) {
		t.Error("a node that is refusing connections was presented as something " +
			"that resolves itself")
	}
	if !strings.Contains(item.Detail, "connection refused") {
		t.Errorf("the node's own account of the failure is not shown: %q", item.Detail)
	}
}
