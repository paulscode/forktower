package store

import (
	"context"
	"strings"
	"testing"
)

const testTowerPubkey = "03f3660d3209930439f5c975615c4653460ab7d466a97338a133663ac1e4150890"

func seedTower(t *testing.T, s *Store, kind TowerKind, pubkey string) int64 {
	t.Helper()
	id, _, err := s.UpsertTower(context.Background(), Tower{
		Kind: kind, Pubkey: pubkey, URI: pubkey + "@tower.onion:9911",
		Managed: true, FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A tower is its pubkey. The address it answers at is not: a hidden service can
// be republished without becoming a different tower.
func TestATowerIsIdentifiedByItsKeyAndNotItsAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	id := seedTower(t, s, TowerLND, testTowerPubkey)

	again, changed, err := s.UpsertTower(ctx, Tower{
		Kind: TowerLND, Pubkey: testTowerPubkey,
		URI:     testTowerPubkey + "@somewhere-else.onion:9911",
		Managed: true, FirstSeenAt: 1_790_000_999, UpdatedAt: 1_790_000_999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("a tower that moved address got a second row: %d then %d", id, again)
	}
	if !changed {
		t.Error("a new address was reported as no change")
	}

	towers, err := s.ListTowers(ctx, TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 {
		t.Fatalf("got %d towers, want 1", len(towers))
	}
	if !strings.Contains(towers[0].URI, "somewhere-else") {
		t.Errorf("the new address was not recorded: %q", towers[0].URI)
	}
}

// Re-reading configuration must not be able to declare a tower healthy. Status
// comes from the monitor, on evidence a config reload does not have.
func TestReloadingConfigurationCannotMarkATowerHealthy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	id := seedTower(t, s, TowerLND, testTowerPubkey)
	if err := s.SetTowerStatus(ctx, id, TowerHealth{
		Status: TowerUnreachable, Detail: "no answer for an hour",
	}, 1_790_001_000); err != nil {
		t.Fatal(err)
	}

	// The config is read again — same tower, same everything.
	if _, _, err := s.UpsertTower(ctx, Tower{
		Kind: TowerLND, Pubkey: testTowerPubkey,
		URI:     testTowerPubkey + "@tower.onion:9911",
		Managed: true, FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_002_000,
	}); err != nil {
		t.Fatal(err)
	}

	towers, err := s.ListTowers(ctx, TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if towers[0].Status != TowerUnreachable {
		t.Errorf("status = %q after a config reload, want it left at %q",
			towers[0].Status, TowerUnreachable)
	}
	if towers[0].StatusDetail != "no answer for an hour" {
		t.Errorf("the detail was lost: %q", towers[0].StatusDetail)
	}
}

// "Answered once and has now stopped" is a different fact from "never answered",
// and the number a user needs is when it last worked — not when we last asked.
func TestTheLastGoodTimeSurvivesGoingQuiet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	id := seedTower(t, s, TowerLND, testTowerPubkey)

	towers, err := s.ListTowers(ctx, TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if towers[0].LastOKAt != 0 {
		t.Errorf("a tower that has never answered has a last-good time of %d", towers[0].LastOKAt)
	}

	const good = 1_790_001_000
	if err := s.SetTowerStatus(ctx, id, TowerHealth{Status: TowerReachable}, good); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTowerStatus(ctx, id, TowerHealth{
		Status: TowerUnreachable, Detail: "gone",
	}, good+3600); err != nil {
		t.Fatal(err)
	}

	towers, err = s.ListTowers(ctx, TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if towers[0].LastOKAt != good {
		t.Errorf("last-good = %d after going quiet, want %d — the user needs when it "+
			"last worked, not when we last asked", towers[0].LastOKAt, good)
	}
}

// teos subscriptions expire and LND sessions do not, so a zero on an LND tower
// would be a claim rather than an absence.
func TestSubscriptionFieldsStayAbsentForAnLNDTower(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	lnd := seedTower(t, s, TowerLND, testTowerPubkey)
	teos := seedTower(t, s, TowerTeos, "02"+strings.Repeat("bb", 32))

	if err := s.SetTowerStatus(ctx, lnd, TowerHealth{
		Status: TowerReachable, BlobTypes: `["legacy","anchor","taproot"]`,
	}, 1_790_001_000); err != nil {
		t.Fatal(err)
	}
	expiry, slots := int32(900_000), int32(9_412)
	if err := s.SetTowerStatus(ctx, teos, TowerHealth{
		Status: TowerReachable, ExpiryHeight: &expiry, SlotsLeft: &slots,
	}, 1_790_001_000); err != nil {
		t.Fatal(err)
	}

	towers, err := s.ListTowers(ctx, TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[TowerKind]Tower{}
	for _, tw := range towers {
		byKind[tw.Kind] = tw
	}

	if byKind[TowerLND].SubscriptionExpiryHeight != nil {
		t.Error("an LND tower reported a subscription expiry, which it does not have")
	}
	if byKind[TowerLND].BlobTypes == "" {
		t.Error("the LND tower's blob types were not recorded")
	}
	if byKind[TowerTeos].BlobTypes != "" {
		t.Error("a teos tower reported blob types, which it never sees")
	}
	if got := byKind[TowerTeos].SubscriptionExpiryHeight; got == nil || *got != expiry {
		t.Errorf("teos expiry height = %v, want %d", got, expiry)
	}
	if got := byKind[TowerTeos].SubscriptionSlotsRemaining; got == nil || *got != slots {
		t.Errorf("teos slots remaining = %v, want %d", got, slots)
	}
}

// Two of the statuses cannot exist on an LND tower. One would mean somebody
// invented evidence that cannot be produced.
func TestLNDCannotReachTheStatusesOnlyTeosCanProve(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status   TowerStatus
		lnd      bool
		teosOnly bool
	}{
		{TowerReachable, true, false},
		{TowerTemporarilyUnreachable, true, false},
		{TowerUnreachable, true, false},
		{TowerStatusUnknown, true, false},
		{TowerSubscriptionError, false, true},
		{TowerMisbehaving, false, true},
	} {
		if got := tc.status.PossibleFor(TowerLND); got != tc.lnd {
			t.Errorf("%q possible for lnd = %v, want %v", tc.status, got, tc.lnd)
		}
		if !tc.status.PossibleFor(TowerTeos) {
			t.Errorf("%q should be possible for teos", tc.status)
		}
	}
}

// A verdict with no reason is an accusation without evidence in one direction
// and an unauditable claim in the other.
func TestCoverageNeedsAReasonInBothDirections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}
	towerID := seedTower(t, s, TowerLND, testTowerPubkey)

	for _, coverable := range []bool{true, false} {
		err := s.UpsertCoverage(ctx, Coverage{
			ChannelID: channelID, TowerID: towerID,
			Coverable: coverable, Reason: "", CheckedAt: 1,
		})
		if err == nil {
			t.Errorf("a coverage verdict of %v was accepted with no reason", coverable)
		}
	}
}

// One row per (channel, tower), rewritten as the answer changes — a monitor
// running every minute must not grow the table.
func TestCoverageIsOneRowPerChannelAndTower(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}
	towerID := seedTower(t, s, TowerLND, testTowerPubkey)

	feeRate := int32(2500)
	for i := range 3 {
		if err := s.UpsertCoverage(ctx, Coverage{
			ChannelID: channelID, TowerID: towerID, Coverable: true,
			Reason: "anchor channel, tower accepts anchor sessions",
			//nolint:gosec // a small loop counter
			NumBackups: int64(i * 10), LastBackupAt: 1_790_000_000,
			SweepFeeSatPerKW: &feeRate, CheckedAt: 1_790_000_000,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ListCoverage(ctx, CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d coverage rows after three passes, want 1", len(rows))
	}
	if rows[0].NumBackups != 20 {
		t.Errorf("backups = %d, want the latest count 20", rows[0].NumBackups)
	}
	if got := rows[0].SweepFeeSatPerKW; got == nil || *got != 2500 {
		t.Errorf("negotiated fee rate = %v, want 2500 sat/kW", got)
	}
}

// The readiness UI leads with the channels nobody can protect, so that has to be
// a query rather than a scan.
func TestUncoverableChannelsCanBeAskedForDirectly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)
	towerID := seedTower(t, s, TowerLND, testTowerPubkey)

	covered := sampleChannel(node)
	coveredID, _, err := s.UpsertChannel(ctx, covered)
	if err != nil {
		t.Fatal(err)
	}
	orphan := sampleChannel(node)
	orphan.FundingTxID = "bb" + strings.Repeat("0", 62)
	orphan.ChanType = ChanTaproot
	orphanID, _, err := s.UpsertChannel(ctx, orphan)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertCoverage(ctx, Coverage{
		ChannelID: coveredID, TowerID: towerID, Coverable: true,
		Reason: "anchor channel, tower accepts anchor sessions", CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCoverage(ctx, Coverage{
		ChannelID: orphanID, TowerID: towerID, Coverable: false,
		Reason: "taproot channel, tower is v0.17.5 and accepts no taproot sessions",
		//nolint:mnd // a timestamp in a test
		CheckedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	uncovered, err := s.ListCoverage(ctx, CoverageFilter{UncoverableOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(uncovered) != 1 || uncovered[0].ChannelID != orphanID {
		t.Fatalf("uncoverable = %+v, want just the taproot channel %d", uncovered, orphanID)
	}
	if !strings.Contains(uncovered[0].Reason, "taproot") {
		t.Errorf("the reason does not say what is wrong: %q", uncovered[0].Reason)
	}
}

// Setting a status on a tower that does not exist must not be a silent no-op:
// that is how a caller comes to believe it recorded something it did not.
func TestSettingStatusOnAMissingTowerIsAnError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	err := s.SetTowerStatus(context.Background(), 4242,
		TowerHealth{Status: TowerReachable}, 1)
	if err == nil {
		t.Error("recording a status against a tower that does not exist was accepted")
	}
}

func TestATowerNeedsAKindAndAKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if _, _, err := s.UpsertTower(ctx, Tower{Kind: TowerLND}); err == nil {
		t.Error("a tower with no pubkey was accepted")
	}
	if _, _, err := s.UpsertTower(ctx, Tower{
		Kind: "nostr-tower", Pubkey: testTowerPubkey,
	}); err == nil {
		t.Error("a tower of an unknown kind was accepted")
	}
	if err := s.SetTowerStatus(ctx, 1, TowerHealth{Status: "vibes"}, 1); err == nil {
		t.Error("an invented status was accepted")
	}
}

// The readiness UI asks three different questions of this table, and each is a
// filter rather than a scan.
func TestTowersAndCoverageCanBeNarrowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	lnd := seedTower(t, s, TowerLND, testTowerPubkey)
	teos := seedTower(t, s, TowerTeos, "02"+strings.Repeat("bb", 32))
	external, _, err := s.UpsertTower(ctx, Tower{
		Kind: TowerTeos, Pubkey: "02" + strings.Repeat("cc", 32),
		URI: "somebody-elses.onion:9814", Managed: false,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	channelID, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}
	for _, towerID := range []int64{lnd, teos, external} {
		if err := s.UpsertCoverage(ctx, Coverage{
			ChannelID: channelID, TowerID: towerID, Coverable: true,
			Reason: "registered and backing up", CheckedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name string
		f    TowerFilter
		want int
	}{
		{"everything", TowerFilter{}, 3},
		{"only lnd", TowerFilter{Kind: TowerLND}, 1},
		{"only teos", TowerFilter{Kind: TowerTeos}, 2},
		{"only ours", TowerFilter{ManagedOnly: true}, 2},
		{"ours and teos", TowerFilter{Kind: TowerTeos, ManagedOnly: true}, 1},
	} {
		got, listErr := s.ListTowers(ctx, tc.f)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(got) != tc.want {
			t.Errorf("%s: got %d towers, want %d", tc.name, len(got), tc.want)
		}
	}

	byChannel, err := s.ListCoverage(ctx, CoverageFilter{ChannelID: channelID})
	if err != nil {
		t.Fatal(err)
	}
	if len(byChannel) != 3 {
		t.Errorf("got %d verdicts for one channel, want one per tower", len(byChannel))
	}
	byTower, err := s.ListCoverage(ctx, CoverageFilter{TowerID: lnd})
	if err != nil {
		t.Fatal(err)
	}
	if len(byTower) != 1 {
		t.Errorf("got %d verdicts for one tower, want 1", len(byTower))
	}
}

// A store that has gone away must report it, not return an empty answer that
// reads as "no towers, nothing wrong".
func TestReadingTowersFromAClosedStoreFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	seedTower(t, s, TowerLND, testTowerPubkey)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ListTowers(ctx, TowerFilter{}); err == nil {
		t.Error("listing towers from a closed store reported no towers rather than an error")
	}
	if _, err := s.ListCoverage(ctx, CoverageFilter{}); err == nil {
		t.Error("listing coverage from a closed store reported nothing rather than an error")
	}
	if _, _, err := s.UpsertTower(ctx, Tower{
		Kind: TowerLND, Pubkey: testTowerPubkey,
	}); err == nil {
		t.Error("recording a tower in a closed store was accepted")
	}
	if err := s.UpsertCoverage(ctx, Coverage{
		ChannelID: 1, TowerID: 1, Reason: "anything", CheckedAt: 1,
	}); err == nil {
		t.Error("recording coverage in a closed store was accepted")
	}
	if err := s.SetTowerStatus(ctx, 1, TowerHealth{Status: TowerReachable}, 1); err == nil {
		t.Error("recording a status in a closed store was accepted")
	}
}
