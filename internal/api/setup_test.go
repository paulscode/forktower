package api

import (
	"net/http"

	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"

	"github.com/paulscode/forktower/internal/config"
)

func setupOf(t *testing.T, h *harness) SetupState {
	t.Helper()
	return decode[SetupState](t, h.do(t, http.MethodGet, "/api/v1/setup", ""))
}

// **One thing at a time.** Eleven red items is a wall, and a wall is where
// people stop.
func TestSetupShowsOneStepAtATime(t *testing.T) {
	h := newHarness(t, nil)

	got := setupOf(t, h)
	if got.Step == nil {
		t.Fatal("a fresh install reported nothing to do")
	}
	if got.Total == 0 {
		t.Error("no total; there is no sense of progress")
	}
	if got.Complete {
		t.Error("a fresh install reported itself complete")
	}
}

// A chain that has not finished syncing is not a task.
//
// Presenting it as one leaves somebody clicking at a thing they cannot affect
// and eventually deciding the software is broken. It counts as done for the
// purpose of getting on, and is listed separately as something being waited for.
func TestSyncingIsWaitedForRatherThanAskedOf(t *testing.T) {
	for _, id := range []string{
		CheckSQSynced, CheckWatcherProgressing, CheckChannelsInventoried,
	} {
		if !waitingOn(id) {
			t.Errorf("%s is presented as a task, but the user cannot do anything about it", id)
		}
	}
	// And the things that genuinely need a person are not swallowed by it.
	for _, id := range []string{CheckTowerProtection, CheckAlertTransports, CheckLNConnected} {
		if waitingOn(id) {
			t.Errorf("%s is treated as something to wait for, so nobody is ever asked to do it", id)
		}
	}
}

// **Three platforms, three genuinely different answers**, and on one of them the
// thing is not a toggle at all. Naming a switch that is not on somebody's screen
// wastes more of their time than saying nothing.
func TestWatchtowerDirectionsMatchThePlatform(t *testing.T) {
	for _, tc := range []struct {
		platform config.Platform
		mustSay  string
		mustNot  string
	}{
		{
			platform: config.PlatformStartOS04,
			mustSay:  "Actions → Watchtower",
			// On 0.4.x the client turns itself on once a tower is listed; telling
			// somebody to find wtclient.active sends them hunting for nothing.
			mustNot: "wtclient.active",
		},
		{
			platform: config.PlatformStartOS035,
			mustSay:  "wtclient.active",
			mustNot:  "Advanced Settings",
		},
		{
			platform: config.PlatformUmbrel,
			mustSay:  "Advanced Settings",
			mustNot:  "StartOS",
		},
	} {
		joined := strings.Join(watchtowerGuidance(tc.platform), " ")
		if !strings.Contains(joined, tc.mustSay) {
			t.Errorf("%s: directions do not mention %q\n%s", tc.platform, tc.mustSay, joined)
		}
		if strings.Contains(joined, tc.mustNot) {
			t.Errorf("%s: directions mention %q, which is another platform's screen",
				tc.platform, tc.mustNot)
		}
	}
}

// An unknown platform says nothing rather than guessing.
//
// A self-hoster knows where their own lnd.conf is. Inventing a menu path for
// them would be worse than the silence.
func TestAnUnknownPlatformGivesNoDirections(t *testing.T) {
	if got := watchtowerGuidance(config.PlatformUnknown); got != nil {
		t.Errorf("guessed at directions for an unknown platform: %v", got)
	}
}

// The steps a user may reasonably decline say what declining costs.
//
// A setup somebody cannot finish is a setup they abandon, and an abandoned
// install protects nobody — so the way out is documented rather than hidden.
func TestTheSkippableStepsSayWhatSkippingCosts(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Platform = config.PlatformUmbrel })

	for _, id := range []string{CheckTowerProtection, CheckAlertTransports} {
		step := h.srv.stepFor(ReadinessItem{ID: id})
		if !step.Skippable {
			t.Errorf("%s cannot be skipped, so somebody without one is stuck", id)
			continue
		}
		if step.SkipCost == "" {
			t.Errorf("%s can be skipped with no statement of what it costs", id)
		}
	}

	// And the ones that are not optional are not offered as optional.
	if step := h.srv.stepFor(ReadinessItem{ID: CheckSQBackendDistinct}); step.Skippable {
		t.Error("watching a second chain at all was offered as optional")
	}
}

// The watchtower step carries the platform's directions with it.
func TestTheWatchtowerStepCarriesDirections(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Platform = config.PlatformStartOS04 })

	step := h.srv.stepFor(ReadinessItem{ID: CheckTowerProtection})
	if len(step.Guidance) == 0 {
		t.Fatal("the one step Forktower cannot do for the user comes with no directions")
	}
}

// Setup is derived from the readiness list rather than keeping its own idea of
// progress, so the two can never disagree about whether something is done.
func TestSetupCountsTheSameChecksTheDashboardShows(t *testing.T) {
	h := newHarness(t, nil)

	got := setupOf(t, h)
	blocking := 0
	for _, item := range h.srv.Readiness(t.Context()) {
		if !item.informational || item.ID == CheckTowerProtection {
			blocking++
		}
	}
	if got.Total != blocking {
		t.Errorf("setup counts %d steps, the readiness list has %d blocking checks",
			got.Total, blocking)
	}
}

// What is being waited for is named, so a user who cannot finish knows they are
// waiting rather than stuck.
func TestWhatIsBeingWaitedForIsNamed(t *testing.T) {
	h := newHarness(t, nil)

	// The second chain still catching up: real, nobody's fault, and the reason
	// this distinction exists at all.
	h.sen.mu.Lock()
	h.sen.sqView = chainview.BackendHealth{State: chainview.HealthSyncing}
	h.sen.mu.Unlock()

	got := setupOf(t, h)
	if got.Waiting == nil {
		t.Fatal("waiting is null rather than a list")
	}
	if len(got.Waiting) == 0 {
		t.Fatal("a syncing second chain was not reported as something being waited for")
	}
	for _, w := range got.Waiting {
		if w == "" {
			t.Error("something is being waited for with no name to show")
		}
	}
	// It is not offered as a task, because there is nothing to do about it.
	if got.Step != nil && got.Step.ID == CheckSQSynced {
		t.Error("the user was asked to do something about a chain that is syncing")
	}
	// And it counts towards progress: counting it as outstanding would say the
	// install is less finished than it is.
	if got.Done < len(got.Waiting) {
		t.Errorf("done = %d but %d things are being waited for", got.Done, len(got.Waiting))
	}
}

// A platform Forktower has never heard of gets no directions rather than
// somebody else's.
func TestAnUnrecognisedPlatformGetsNoDirections(t *testing.T) {
	if got := watchtowerGuidance(config.Platform("something-new")); got != nil {
		t.Errorf("invented directions for an unknown platform: %v", got)
	}
}
