package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

type towersPayload struct {
	Towers   []Tower  `json:"towers"`
	Guidance []string `json:"guidance"`
}

func towers(t *testing.T, h *harness) []Tower {
	t.Helper()
	return decode[towersPayload](t, h.do(t, http.MethodGet, "/api/v1/towers", "")).Towers
}

const towerKey = "03f3660d3209930439f5c975615c4653460ab7d466a97338a133663ac1e4150890"

func addTower(t *testing.T, h *harness, status store.TowerStatus, detail string) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: towerKey,
		URI:     towerKey + "@abcdef.onion:9911",
		Managed: true, FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTowerStatus(ctx, id,
		store.TowerHealth{Status: status, Detail: detail}, 1_790_000_100); err != nil {
		t.Fatal(err)
	}
	return id
}

func addCoverage(t *testing.T, h *harness, channelID, towerID int64, ok bool, reason string) {
	t.Helper()
	feeRate := int32(2500)
	err := h.store.UpsertCoverage(context.Background(), store.Coverage{
		ChannelID: channelID, TowerID: towerID, Coverable: ok, Reason: reason,
		NumBackups: 120, LastBackupAt: 1_790_000_100,
		SweepFeeSatPerKW: &feeRate, CheckedAt: 1_790_000_100,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// **The whole point of the card.** A watchtower that is up, answering, and
// holding no sessions looks perfectly healthy from every angle except the one
// that matters, so it must not read as fine.
func TestAReachableTowerHoldingNothingIsNotDescribedAsProtecting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	towerID := addTower(t, h, store.TowerReachable, "")
	channelID := addChannel(t, h, fundingA, nil)
	addCoverage(t, h, channelID, towerID, false,
		"both ends support anchor sessions but the node has not negotiated one")

	got := towers(t, h)
	if len(got) != 1 {
		t.Fatalf("got %d towers, want 1", len(got))
	}
	if got[0].Display.State == "protecting" {
		t.Errorf("a tower protecting nothing was shown as protecting: %+v", got[0].Display)
	}
	if got[0].Display.Uncovered != 1 {
		t.Errorf("uncovered = %d, want 1", got[0].Display.Uncovered)
	}
	// And the reason survives to the page, because "not protected" without one
	// is an accusation nobody can act on.
	if len(got[0].Coverage) != 1 || !strings.Contains(got[0].Coverage[0].Reason, "negotiated") {
		t.Errorf("the reason did not reach the dashboard: %+v", got[0].Coverage)
	}
}

func TestATowerCoveringEverythingSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	towerID := addTower(t, h, store.TowerReachable, "")
	channelID := addChannel(t, h, fundingA, nil)
	addCoverage(t, h, channelID, towerID, true, "the node holds an anchor session")

	got := towers(t, h)
	if got[0].Display.State != "protecting" {
		t.Errorf("state = %q, want protecting: %s", got[0].Display.State, got[0].Display.Summary)
	}
	if got[0].Display.Covered != 1 {
		t.Errorf("covered = %d, want 1", got[0].Display.Covered)
	}
}

// A tower with nothing registered yet is not a failure. It is what every
// installation looks like before the user has run the command.
func TestATowerWithNothingRegisteredYetPointsAtTheSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerReachable, "")

	got := towers(t, h)
	if got[0].Display.State == "not protecting" {
		t.Errorf("a freshly started tower was shown as a failure: %+v", got[0].Display)
	}
	if !strings.Contains(got[0].Display.Summary, "steps below") {
		t.Errorf("the summary does not point at what to do: %q", got[0].Display.Summary)
	}
}

// The address is the single most important string on the page: without it there
// is nothing to paste.
func TestTheAddressAUserPastesIsServed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerReachable, "")

	got := towers(t, h)
	if !strings.Contains(got[0].URI, "@") || !strings.Contains(got[0].URI, ".onion") {
		t.Errorf("the registration address is not usable: %q", got[0].URI)
	}
}

// Each unhealthy status gets its own sentence, because the remedies differ and a
// single "something is wrong" sends people to the wrong place.
func TestEachWayATowerCanFailReadsDifferently(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status  store.TowerStatus
		detail  string
		state   string
		mustSay string
	}{
		{store.TowerUnreachable, "the tower did not answer: connection refused",
			"not protecting", "connection refused"},
		{store.TowerTemporarilyUnreachable, "its node is still catching up",
			"settling", "catching up"},
		{store.TowerSubscriptionError, "", "not protecting", "run out"},
		{store.TowerMisbehaving, "", "not protecting", "does not check out"},
		{store.TowerStatusUnknown, "", "unknown", "not managed to ask"},
	} {
		h := newHarness(t, nil)
		addTower(t, h, tc.status, tc.detail)

		got := towers(t, h)
		if got[0].Display.State != tc.state {
			t.Errorf("%s: state = %q, want %q", tc.status, got[0].Display.State, tc.state)
		}
		if !strings.Contains(got[0].Display.Summary, tc.mustSay) {
			t.Errorf("%s: summary %q does not mention %q",
				tc.status, got[0].Display.Summary, tc.mustSay)
		}
	}
}

// No internal vocabulary reaches the page. The same rule the rest of the
// dashboard follows.
func TestATowerCardSaysNothingInTheProgramsOwnWords(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	towerID := addTower(t, h, store.TowerReachable, "")
	channelID := addChannel(t, h, fundingA, nil)
	addCoverage(t, h, channelID, towerID, false, "the tower is running v0.17.5")

	for _, tower := range towers(t, h) {
		text := tower.Display.Summary + " " + tower.Display.State
		for _, jargon := range []string{
			"sf", "sq", "blob", "wtclient", "coverable", "upsert", "nil",
		} {
			if strings.Contains(strings.ToLower(text), " "+jargon+" ") {
				t.Errorf("the tower card used %q: %q", jargon, text)
			}
		}
	}
}

// The fee is stored in sat/kW because that is the unit the policy uses. A user
// reads sat/vB.
func TestTheFeeRateIsShownInTheUnitAPersonReads(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	towerID := addTower(t, h, store.TowerReachable, "")
	channelID := addChannel(t, h, fundingA, nil)
	addCoverage(t, h, channelID, towerID, true, "the node holds an anchor session")

	got := towers(t, h)
	rate := got[0].Coverage[0].SweepFeeSatPerVByte
	if rate == nil || *rate != 10 {
		t.Errorf("fee rate = %v, want 10 sat/vB from the stored 2500 sat/kW", rate)
	}
}

// An LND tower has no subscription, so the fields must be absent rather than
// zero — a zero expiry height would read as "expired long ago".
func TestSubscriptionFieldsAreAbsentForAnLNDTower(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerReachable, "")

	got := towers(t, h)
	if got[0].SubscriptionExpiryHeight != nil || got[0].SubscriptionSlotsRemaining != nil {
		t.Errorf("an LND tower reported a subscription: %+v", got[0])
	}
}

// No towers is the ordinary case for most installations and is not an error.
func TestNoTowersIsAnEmptyListAndNotAFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.do(t, http.MethodGet, "/api/v1/towers", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := decode[towersPayload](t, resp).Towers; len(got) != 0 {
		t.Errorf("got %d towers, want none", len(got))
	}
}

// A tower Forktower does not run cannot be restarted from here, and saying so
// is the difference between an instruction and a dead end.
func TestAnExternalTowerIsNotDescribedAsSomethingWeCanFix(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	ctx := context.Background()
	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerTeos, Pubkey: "02" + strings.Repeat("cc", 32),
		URI: "somebody-elses.onion:9814", Managed: false,
		FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTowerStatus(ctx, id,
		store.TowerHealth{Status: store.TowerUnreachable}, 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	got := towers(t, h)
	if got[0].Managed {
		t.Error("somebody else's tower was reported as one we run")
	}
	if !strings.Contains(got[0].Display.Summary, "not one Forktower runs") {
		t.Errorf("the summary implies we could fix it: %q", got[0].Display.Summary)
	}
}

// A managed tower that is simply not answering gets the plainer sentence.
func TestAManagedTowerThatIsSilentSaysWhatThatMeans(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerUnreachable, "")

	got := towers(t, h)
	if !strings.Contains(got[0].Display.Summary, "nothing is being backed up to it") {
		t.Errorf("summary = %q", got[0].Display.Summary)
	}
}

// A tower still starting up with no detail still says something.
func TestATowerStartingUpWithNoDetailStillReads(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerTemporarilyUnreachable, "")

	got := towers(t, h)
	if got[0].Display.Summary == "" {
		t.Error("a tower with no detail was shown with no summary at all")
	}
}

// Some channels covered and some not is the case that most needs pointing
// somewhere, because the card cannot fit every reason.
func TestAPartiallyCoveringTowerPointsAtTheDetail(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	towerID := addTower(t, h, store.TowerReachable, "")
	covered := addChannel(t, h, fundingA, nil)
	uncovered := addChannel(t, h, fundingB, nil)
	addCoverage(t, h, covered, towerID, true, "the node holds an anchor session")
	addCoverage(t, h, uncovered, towerID, false, "no taproot session")

	got := towers(t, h)
	if got[0].Display.State != "not protecting" {
		t.Errorf("state = %q — a partially covered tower is not fully protecting",
			got[0].Display.State)
	}
	if !strings.Contains(got[0].Display.Summary, "details below") {
		t.Errorf("the summary does not point anywhere: %q", got[0].Display.Summary)
	}
	if got[0].Display.Covered != 1 || got[0].Display.Uncovered != 1 {
		t.Errorf("counts = %d covered, %d not", got[0].Display.Covered, got[0].Display.Uncovered)
	}
}

// A database that has gone away must be an error, not an empty list that reads
// as "you have no towers".
func TestAFailedReadIsNotAnEmptyTowerList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addTower(t, h, store.TowerReachable, "")
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodGet, "/api/v1/towers", "")
	if resp.StatusCode == http.StatusOK {
		t.Error("a store that had gone away reported no towers rather than an error")
	}
}

// The towers payload carries the steps for this platform.
//
// The card used to say only that the setup list names the right ones — true,
// and on a different part of the page. Somebody reading the watchtower card
// wants them there.
func TestTheTowersPayloadCarriesThePlatformSteps(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Platform = config.PlatformStartOS035 })

	got := decode[towersPayload](t, h.do(t, http.MethodGet, "/api/v1/towers", ""))
	if len(got.Guidance) == 0 {
		t.Fatal("no steps were served with the towers")
	}
	joined := strings.Join(got.Guidance, " ")
	if !strings.Contains(joined, "Watchtower Client Enabled") {
		t.Errorf("the steps are not this platform's: %q", joined)
	}
}
