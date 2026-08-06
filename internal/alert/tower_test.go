package alert

import (
	"context"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/responder/tower"
	"github.com/paulscode/forktower/internal/store"
)

// A tower that has stopped answering is protection that has gone, and the
// message has to say what that means rather than only what happened.
func TestATowerGoingDownSaysWhatItCosts(t *testing.T) {
	t.Parallel()

	got, ok := MapEventToAlert(bus.TowerHealthChanged{
		TowerID: 1, TowerKind: "lnd", Status: string(store.TowerUnreachable),
		Detail: "the tower did not answer: connection refused", Previous: "reachable",
	})
	if !ok {
		t.Fatal("a tower going down raised nothing")
	}
	if got.Tier != store.TierWarning {
		t.Errorf("tier = %q", got.Tier)
	}
	if !strings.Contains(got.Message, "would not be answered") {
		t.Errorf("the message does not say what is lost: %q", got.Message)
	}
	if !strings.Contains(got.Message, "connection refused") {
		t.Errorf("what was actually seen was dropped: %q", got.Message)
	}
}

// Somebody told their protection had gone is owed the sentence saying it came
// back. Otherwise the only way to find out is to go and look, which is the
// behaviour this project exists to replace.
func TestRecoveryIsAnnouncedAndIsNotAlarming(t *testing.T) {
	t.Parallel()

	got, ok := MapEventToAlert(bus.TowerHealthChanged{
		TowerID: 1, Status: string(store.TowerReachable),
		Previous: string(store.TowerUnreachable),
	})
	if !ok {
		t.Fatal("a tower coming back raised nothing")
	}
	if got.Tier != store.TierResolved {
		t.Errorf("tier = %q, want it not to wake anybody", got.Tier)
	}
	if tierRank(got.Tier) != tierRank(store.TierInfo) {
		t.Error("recovery would reach somebody who asked for warnings only")
	}
	// It clears the alert that said it was down.
	down, _ := MapEventToAlert(bus.TowerHealthChanged{
		TowerID: 1, Status: string(store.TowerUnreachable), Previous: "reachable",
	})
	if got.DedupKey != down.DedupKey {
		t.Errorf("recovery keyed %q and the failure %q — they must be the same "+
			"thread or the dashboard shows both at once", got.DedupKey, down.DedupKey)
	}
}

// A tower coming up for the first time has not "recovered", and saying so would
// confuse somebody who has just started it.
func TestATowerStartingUpIsNotAnnouncedAsRecovery(t *testing.T) {
	t.Parallel()

	for _, previous := range []string{"", string(store.TowerStatusUnknown)} {
		if _, ok := MapEventToAlert(bus.TowerHealthChanged{
			TowerID: 1, Status: string(store.TowerReachable), Previous: previous,
		}); ok {
			t.Errorf("a tower whose previous state was %q was announced as recovered",
				previous)
		}
	}
}

// Still starting up is not news, and alerting on it would fire on every restart.
func TestATowerStillStartingSaysNothing(t *testing.T) {
	t.Parallel()

	for _, status := range []store.TowerStatus{
		store.TowerTemporarilyUnreachable, store.TowerStatusUnknown,
	} {
		if _, ok := MapEventToAlert(bus.TowerHealthChanged{
			TowerID: 1, Status: string(status),
		}); ok {
			t.Errorf("%q raised an alert", status)
		}
	}
}

// Misbehaviour is the one signal in this system backed by proof, and it is the
// only tower condition worth waking somebody for.
func TestMisbehaviourIsTheOneTowerAlertThatWakesYou(t *testing.T) {
	t.Parallel()

	got, ok := MapEventToAlert(bus.TowerHealthChanged{
		TowerID: 1, Status: string(store.TowerMisbehaving),
	})
	if !ok {
		t.Fatal("a misbehaving tower raised nothing")
	}
	if got.Tier != store.TierCritical {
		t.Errorf("tier = %q, want critical", got.Tier)
	}
	if !strings.Contains(got.Message, "proof rather than suspicion") {
		t.Errorf("the message reads as a guess: %q", got.Message)
	}
	if !strings.Contains(got.Message, "another") {
		t.Errorf("the message does not say what to do: %q", got.Message)
	}
}

// Every concern reaches somebody, including one this build has never heard of.
// Silence is the only outcome worse than an unfamiliar subject line.
func TestEveryConcernReachesSomebody(t *testing.T) {
	t.Parallel()

	kinds := []tower.ConcernKind{
		tower.ConcernClientOff, tower.ConcernPluginMissing, tower.ConcernNotRegistered,
		tower.ConcernChannelUncovered, tower.ConcernBackupsStalled,
		tower.ConcernSubscriptionExpiring, tower.ConcernSlotsLow,
		tower.ConcernAppointmentsUndelivered, tower.ConcernAppointmentsInvalid,
		tower.ConcernTowerMisbehaving, tower.ConcernFeeRateFixed,
		tower.ConcernSessionsExhausted, tower.ConcernDiskFilling,
		tower.ConcernKind("something_nobody_has_written_yet"),
	}

	for _, kind := range kinds {
		got, ok := MapEventToAlert(bus.TowerConcern{
			TowerID: 1, ChannelID: 2, Concern: string(kind),
			Message: "the sentence the monitor wrote.",
		})
		if !ok {
			t.Errorf("%q raised nothing at all", kind)
			continue
		}
		if got.Subject == "" || got.Message == "" {
			t.Errorf("%q raised an empty alert: %+v", kind, got)
		}
		if got.DedupKey == "" {
			t.Errorf("%q raised an alert with no dedup key, so it would repeat", kind)
		}
		// The monitor's own sentence survives to the user. It is the one written
		// with the specifics in it.
		if !strings.Contains(got.Message, "the sentence the monitor wrote") {
			t.Errorf("%q lost the monitor's own wording: %q", kind, got.Message)
		}
	}
}

// The tiers have to differ, or the loud ones train people to ignore the lot.
func TestTheOrdinaryConcernsDoNotWakeAnybody(t *testing.T) {
	t.Parallel()

	for _, kind := range []tower.ConcernKind{
		tower.ConcernFeeRateFixed, tower.ConcernSessionsExhausted, tower.ConcernDiskFilling,
	} {
		got, _ := MapEventToAlert(bus.TowerConcern{TowerID: 1, Concern: string(kind), Message: "x."})
		if got.Tier != store.TierInfo {
			t.Errorf("%q is tier %q — none of these is protection failing", kind, got.Tier)
		}
	}

	// And the ones that mean protection is not happening do carry weight.
	for _, kind := range []tower.ConcernKind{
		tower.ConcernClientOff, tower.ConcernChannelUncovered, tower.ConcernBackupsStalled,
	} {
		got, _ := MapEventToAlert(bus.TowerConcern{TowerID: 1, Concern: string(kind), Message: "x."})
		if tierRank(got.Tier) < tierRank(store.TierWarning) {
			t.Errorf("%q is only tier %q", kind, got.Tier)
		}
	}
}

// Per-channel concerns must key per channel, or one uncovered channel would hide
// the next.
func TestAPerChannelConcernIsKeyedPerChannel(t *testing.T) {
	t.Parallel()

	first, _ := MapEventToAlert(bus.TowerConcern{
		TowerID: 1, ChannelID: 7, Concern: string(tower.ConcernChannelUncovered),
		Message: "channel 7 is not protected.",
	})
	second, _ := MapEventToAlert(bus.TowerConcern{
		TowerID: 1, ChannelID: 8, Concern: string(tower.ConcernChannelUncovered),
		Message: "channel 8 is not protected.",
	})
	if first.DedupKey == second.DedupKey {
		t.Errorf("two uncovered channels share the key %q, so only one would be "+
			"shown", first.DedupKey)
	}
}

// --- Reconciliation: the answer to an alert that can be missed permanently ---

// **A tower that is down gets its alert from stored state, with no event at
// all.** That is the property: an event is delivered once, and if it was dropped
// or the daemon was stopped, nothing else would ever say so.
func TestATowerThatIsDownIsAlertedFromStoredStateAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: "03" + strings.Repeat("ab", 32),
		Managed: true, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTowerStatus(ctx, id, store.TowerHealth{
		Status: store.TowerUnreachable, Detail: "no answer for an hour",
	}, 2); err != nil {
		t.Fatal(err)
	}

	// No event is published. Only the sweep runs.
	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(alerts, KindTowerDown) {
		t.Fatalf("a tower down in the database raised nothing: %+v", alerts)
	}
}

// The same for a channel nothing is protecting, which is the failure with no
// other symptom.
func TestAnUncoveredChannelIsAlertedFromStoredStateAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	towerID, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: "03" + strings.Repeat("cd", 32),
		Managed: true, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	channelID := h.seedChannel()
	if err := h.store.UpsertCoverage(ctx, store.Coverage{
		ChannelID: channelID, TowerID: towerID, Coverable: false,
		Reason:    "the tower is running v0.17.5 and accepts no taproot sessions",
		CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range alerts {
		if a.Kind == KindTowerNotProtecting {
			found = true
			if !strings.Contains(a.Message, "taproot") {
				t.Errorf("the reason did not reach the user: %q", a.Message)
			}
		}
	}
	if !found {
		t.Fatalf("an uncovered channel raised nothing: %+v", alerts)
	}
}

// Running twice must not produce two alerts: the sweep runs on a timer, and a
// condition that persists would otherwise fill the list with itself.
func TestReconcilingRepeatedlyDoesNotRepeatTheAlert(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: "03" + strings.Repeat("ef", 32),
		Managed: true, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTowerStatus(ctx, id,
		store.TowerHealth{Status: store.TowerUnreachable}, 2); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		h.al.reconcile(ctx)
	}

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, a := range alerts {
		if a.Kind == KindTowerDown {
			count++
		}
	}
	if count != 1 {
		t.Errorf("three sweeps produced %d alerts about one tower", count)
	}
}

// A healthy tower produces nothing, which is what makes the sweep safe to run
// every minute.
func TestReconcilingAHealthyTowerSaysNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, nil)
	ctx := context.Background()

	id, _, err := h.store.UpsertTower(ctx, store.Tower{
		Kind: store.TowerLND, Pubkey: "03" + strings.Repeat("11", 32),
		Managed: true, FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	channelID := h.seedChannel()
	if err := h.store.SetTowerStatus(ctx, id,
		store.TowerHealth{Status: store.TowerReachable}, 2); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCoverage(ctx, store.Coverage{
		ChannelID: channelID, TowerID: id, Coverable: true,
		Reason: "the node holds an anchor session", CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	h.al.reconcile(ctx)

	alerts, err := h.store.ListAlerts(ctx, store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Kind == KindTowerDown || a.Kind == KindTowerNotProtecting {
			t.Errorf("a healthy tower raised %q: %s", a.Kind, a.Message)
		}
	}
}

func hasKind(alerts []store.Alert, kind string) bool {
	for _, a := range alerts {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// seedChannel registers a channel so that coverage can be recorded against it.
func (h *harness) seedChannel() int64 {
	ctx := context.Background()
	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		panic(err)
	}
	id, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: node, FundingTxID: "aa" + strings.Repeat("0", 62),
		CapacitySat: 1_000_000, ChanType: store.ChanTaproot, UpdatedAt: 1,
	})
	if err != nil {
		panic(err)
	}
	return id
}

// A tower that has never come up has no protection to lose.
//
// **Seen on a fresh install**: lnd took a few seconds to open its listener, this
// fired, and "your watchtower is not answering" then sat on the dashboard for
// days. It is the mirror of the rule that coming up for the first time is not
// recovery — going down for the first time, having never been up, is not a loss.
func TestATowerStartingUpIsNotReportedAsLost(t *testing.T) {
	t.Parallel()
	for _, previous := range []string{"", string(store.TowerStatusUnknown)} {
		_, raised := mapTowerHealth(bus.TowerHealthChanged{
			TowerID:  1,
			Status:   string(store.TowerUnreachable),
			Previous: previous,
			Detail:   "connection refused",
		})
		if raised {
			t.Errorf("a tower whose previous status was %q raised a warning while "+
				"it was still starting", previous)
		}
	}
}

// A tower that really was working and then stopped is still reported.
func TestATowerThatWasWorkingAndStoppedIsStillReported(t *testing.T) {
	t.Parallel()
	got, raised := mapTowerHealth(bus.TowerHealthChanged{
		TowerID:  1,
		Status:   string(store.TowerUnreachable),
		Previous: string(store.TowerReachable),
	})
	if !raised {
		t.Fatal("a tower that stopped answering raised nothing")
	}
	if got.Kind != KindTowerDown || got.Tier != store.TierWarning {
		t.Errorf("got %s at %s", got.Kind, got.Tier)
	}
}

// Coming back as "settling" closes the warning.
//
// The resolve used to fire only on fully reachable, which a tower cannot reach
// until its own chain backend has caught up — days, on a node syncing from
// nothing. So the warning stayed up, true for the seconds it described and false
// for every one after.
func TestATowerBackButStillCatchingUpClosesTheWarning(t *testing.T) {
	t.Parallel()
	got, raised := mapTowerHealth(bus.TowerHealthChanged{
		TowerID:  1,
		Status:   string(store.TowerTemporarilyUnreachable),
		Previous: string(store.TowerUnreachable),
		Detail:   "its node is still catching up with the chain",
	})
	if !raised {
		t.Fatal("a tower that came back left its warning standing")
	}
	if got.Tier != store.TierResolved {
		t.Errorf("tier = %s, want %s", got.Tier, store.TierResolved)
	}
	// Same key as the warning, or it closes nothing.
	if got.DedupKey != "tower_down:1" {
		t.Errorf("dedup key = %q, which does not match the warning it should close",
			got.DedupKey)
	}
}

// And an ordinary settle from nothing still says nothing.
func TestATowerSettlingFromStartupSaysNothing(t *testing.T) {
	t.Parallel()
	if _, raised := mapTowerHealth(bus.TowerHealthChanged{
		TowerID: 1,
		Status:  string(store.TowerTemporarilyUnreachable),
	}); raised {
		t.Error("a tower settling on first sight woke somebody")
	}
}

// A machine's answer is trimmed to the part a person can act on.
//
// What reached the dashboard was a sentence carrying an HTTP status, a JSON body
// and a gRPC code, wrapped in prose about a broken promise. All true, almost
// none of it useful: the reader wants to know whether their protection is gone
// and is handed `{"code":2, ...}` to interpret.
func TestTheDetailIsTrimmedToWhatAPersonCanUse(t *testing.T) {
	t.Parallel()

	// Verbatim from a StartOS install.
	raw := `the tower did not answer: answered 500 Internal Server Error for ` +
		`/v2/watchtower/server: {"code":2, "message":"the RPC server is in the ` +
		`process of starting up, but not yet ready to accept calls", "details":[]}`

	got := detailSentence(raw)
	if strings.Contains(got, `"code"`) || strings.Contains(got, "500") ||
		strings.Contains(got, "details") {
		t.Errorf("the reader is still being handed transport bookkeeping: %q", got)
	}
	if !strings.Contains(got, "starting up") {
		t.Errorf("the part that explains what is happening was lost: %q", got)
	}
}

// A plain sentence is left alone rather than mangled.
func TestAPlainDetailIsLeftAsItIs(t *testing.T) {
	t.Parallel()
	got := detailSentence("the tower is running but its node is still catching up")
	if !strings.Contains(got, "still catching up") {
		t.Errorf("a readable detail was damaged: %q", got)
	}
}

func TestNoDetailProducesNoSentence(t *testing.T) {
	t.Parallel()
	if got := detailSentence(""); got != "" {
		t.Errorf("an empty detail produced %q", got)
	}
}

// Fixing the one thing Forktower cannot fix is worth saying out loud.
//
// **Reported by a tester.** They turned on their node's watchtower client,
// pasted in the address, came back — and found the same "your watchtower client
// is switched off" warning sitting there, with nothing to say whether it was
// current or a record of something already dealt with. The warden had noticed
// the concern go away and forgotten it in silence.
func TestFixingTheWatchtowerClientIsAnnounced(t *testing.T) {
	t.Parallel()
	got, raised := mapTowerConcern(bus.TowerConcern{
		TowerID: 1,
		Concern: string(tower.ConcernClientOff),
		Cleared: true,
	})
	if !raised {
		t.Fatal("the client being switched on produced nothing at all")
	}
	if got.Tier != store.TierResolved {
		t.Errorf("tier = %s, want %s", got.Tier, store.TierResolved)
	}
	// **Its own key.** Raising a resolution under the warning's key would find
	// the existing row and bump it, leaving the warning's own words on screen.
	warning, _ := mapTowerConcern(bus.TowerConcern{
		TowerID: 1,
		Concern: string(tower.ConcernClientOff),
	})
	if got.DedupKey == warning.DedupKey {
		t.Errorf("the resolution reuses the warning's key %q, so it would edit "+
			"the warning rather than appear beside it", got.DedupKey)
	}
}

// Not every concern ending is news. A channel becoming coverable because it
// closed is not something anybody was waiting to hear, and an entry for each
// would bury the two that a person actually acted on.
func TestOnlyConcernsAPersonActedOnAreAnnouncedAsFixed(t *testing.T) {
	t.Parallel()
	for _, kind := range []tower.ConcernKind{
		tower.ConcernChannelUncovered,
		tower.ConcernBackupsStalled,
		tower.ConcernFeeRateFixed,
		tower.ConcernSessionsExhausted,
	} {
		if _, raised := mapTowerConcern(bus.TowerConcern{
			TowerID: 1, Concern: string(kind), Cleared: true,
		}); raised {
			t.Errorf("%s ending produced an announcement nobody was waiting for", kind)
		}
	}
}
