package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
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
		CheckSFEnforcing, CheckAlertTransports, CheckLNConnected,
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
