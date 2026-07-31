package tower

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

type fakeClient struct {
	towers    []RegisteredTower
	towersErr error
	stats     ClientStats
	statsErr  error
	version   string
	versErr   error
}

func (f *fakeClient) Towers(context.Context) ([]RegisteredTower, error) {
	return f.towers, f.towersErr
}

func (f *fakeClient) Stats(context.Context) (ClientStats, error) {
	return f.stats, f.statsErr
}

func (f *fakeClient) Version(context.Context) (Version, error) {
	if f.versErr != nil {
		return Version{}, f.versErr
	}
	return ParseVersion(f.version), nil
}

const ourTower = "03f3660d3209930439f5c975615c4653460ab7d466a97338a133663ac1e4150890"

func registeredWith(sessions ...Session) *fakeClient {
	return &fakeClient{
		version: "0.18.5-beta",
		towers: []RegisteredTower{{
			Pubkey:    ourTower,
			Addresses: []string{"abcdef.onion:9911"},
			Sessions:  sessions,
		}},
	}
}

func anchorSession(backups uint32) Session {
	return Session{
		Policy: PolicyAnchor, NumBackups: backups,
		MaxBackups: 1024, SweepSatPerVByte: 10,
	}
}

func channelsOf(types ...store.ChanType) []store.Channel {
	out := make([]store.Channel, 0, len(types))
	for i, t := range types {
		out = append(out, store.Channel{
			ID:          int64(i + 1),
			ChanType:    t,
			FundingTxID: strings.Repeat("ab", 32),
			//nolint:gosec // a small loop counter
			FundingVout: int32(i),
		})
	}
	return out
}

func newMonitor(t *testing.T, c ClientReader, tweak ...func(*MonitorOptions)) *Monitor {
	t.Helper()
	o := MonitorOptions{
		Client: c, TowerID: 7, TowerPubkey: ourTower,
		TowerVersion:         ParseVersion("0.18.5-beta"),
		RegisteredForSeconds: GracePeriodSeconds * 2,
	}
	for _, fn := range tweak {
		fn(&o)
	}
	m, err := NewMonitor(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func concernsOf(p Pass, kind ConcernKind) []Concern {
	var out []Concern
	for _, c := range p.Concerns {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func TestAChannelWithItsSessionIsRecordedAsCovered(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(120)))

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1_790_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if !pass.Registered {
		t.Error("our tower was not found among the node's registered towers")
	}
	if len(pass.Coverage) != 1 || !pass.Coverage[0].Coverable {
		t.Fatalf("coverage = %+v", pass.Coverage)
	}
	if pass.Coverage[0].NumBackups != 120 {
		t.Errorf("backups = %d, want 120", pass.Coverage[0].NumBackups)
	}
	if got := pass.Coverage[0].SweepFeeSatPerKW; got == nil || *got != 2500 {
		t.Errorf("fee rate = %v, want 2500 sat/kW (10 sat/vB)", got)
	}
	if len(concernsOf(pass, ConcernChannelUncovered)) != 0 {
		t.Error("a covered channel produced a complaint")
	}
}

// The failure that has no other symptom: one channel type uncovered while the
// rest back up normally and the tower reports itself perfectly healthy.
func TestOneUncoveredChannelIsNamedWhileTheOthersAreFine(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(120)))

	channels := channelsOf(store.ChanAnchors, store.ChanTaproot, store.ChanAnchors)
	pass, err := m.Check(context.Background(), channels, nil, 1_790_000_000)
	if err != nil {
		t.Fatal(err)
	}

	uncovered := concernsOf(pass, ConcernChannelUncovered)
	if len(uncovered) != 1 {
		t.Fatalf("got %d uncovered-channel concerns, want exactly the taproot one: %+v",
			len(uncovered), uncovered)
	}
	if uncovered[0].ChannelID != 2 {
		t.Errorf("the wrong channel was named: %d", uncovered[0].ChannelID)
	}
	if !strings.Contains(uncovered[0].Message, "not protected") {
		t.Errorf("the message does not say what is wrong: %q", uncovered[0].Message)
	}
	// And the other two are still recorded as covered, because they are.
	covered := 0
	for _, c := range pass.Coverage {
		if c.Coverable {
			covered++
		}
	}
	if covered != 2 {
		t.Errorf("%d channels covered, want 2", covered)
	}
}

// A node backing up to nothing at all is a bigger problem than a tower being
// unreachable, and the remedy is a setting Forktower will not change for them.
func TestAWatchtowerClientSwitchedOffIsAnAnswerNotAnError(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, &fakeClient{towersErr: ErrClientNotActive})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatalf("a switched-off client was reported as a failure to read: %v", err)
	}
	if pass.ClientActive {
		t.Error("the client was reported active")
	}
	off := concernsOf(pass, ConcernClientOff)
	if len(off) != 1 {
		t.Fatalf("got %d concerns about the client being off", len(off))
	}
	if !strings.Contains(off[0].Message, "cannot change it for you") {
		t.Errorf("the message does not say who has to act: %q", off[0].Message)
	}
	// No per-channel verdicts: with the client off, "this channel is uncovered"
	// would be technically true and useless — the one thing to fix is upstream.
	if len(pass.Coverage) != 0 {
		t.Errorf("per-channel verdicts were produced anyway: %+v", pass.Coverage)
	}
}

// Our tower existing and the node never having heard of it is the ordinary
// state before the user has run the registration command.
func TestATowerTheNodeHasNeverHeardOfIsReported(t *testing.T) {
	t.Parallel()
	other := &fakeClient{
		version: "0.18.5-beta",
		towers: []RegisteredTower{{
			Pubkey:   "02" + strings.Repeat("cc", 32),
			Sessions: []Session{anchorSession(50)},
		}},
	}
	m := newMonitor(t, other)

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Registered {
		t.Error("a tower nobody registered with was reported as registered")
	}
	if len(concernsOf(pass, ConcernNotRegistered)) != 1 {
		t.Errorf("nothing was said about the tower not being registered: %+v", pass.Concerns)
	}
	// And somebody else's sessions do not cover our channels.
	if pass.Coverage[0].Coverable {
		t.Error("another tower's sessions were counted as coverage by ours")
	}
}

// Nothing is said during the grace window, because a healthy tower looks
// identical to a broken one while its sessions are being negotiated.
func TestNothingIsSaidWhileAFreshRegistrationSettles(t *testing.T) {
	t.Parallel()
	fresh := &fakeClient{
		version: "0.18.5-beta",
		towers:  []RegisteredTower{{Pubkey: ourTower}},
	}
	m := newMonitor(t, fresh, func(o *MonitorOptions) { o.RegisteredForSeconds = 60 })

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(concernsOf(pass, ConcernChannelUncovered)) != 0 {
		t.Error("a tower registered a minute ago was already being complained about")
	}
	// The verdict is still honestly "not covered" — it is just not shouted about.
	if pass.Coverage[0].Coverable {
		t.Error("a channel with no session was recorded as covered during the grace period")
	}

	settled := newMonitor(t, fresh)
	pass, err = settled.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(concernsOf(pass, ConcernChannelUncovered)) != 1 {
		t.Error("once the grace period passed, nothing was said about the missing session")
	}
}

// The classic failure: reachable, healthy-looking, and no longer receiving
// anything. Needs two observations plus the knowledge that the channel moved.
func TestBackupsThatStopWhileTheChannelMovesAreReported(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(120)))

	previous := []store.Coverage{{ChannelID: 1, TowerID: 7, Coverable: true, NumBackups: 120}}
	current := []store.Coverage{{ChannelID: 1, TowerID: 7, Coverable: true, NumBackups: 120}}

	stalled := m.StalledBackups(current, previous, map[int64]bool{1: true})
	if len(stalled) != 1 {
		t.Fatalf("a channel that moved with a flat backup count produced %d concerns", len(stalled))
	}
	if !strings.Contains(stalled[0].Message, "already has") {
		t.Errorf("the message does not say what the consequence is: %q", stalled[0].Message)
	}

	// A quiet channel with a flat count is working exactly as intended.
	if got := m.StalledBackups(current, previous, map[int64]bool{1: false}); len(got) != 0 {
		t.Errorf("a quiet channel was reported as stalled: %+v", got)
	}
	// And a count that moved is fine.
	moved := []store.Coverage{{ChannelID: 1, TowerID: 7, Coverable: true, NumBackups: 121}}
	if got := m.StalledBackups(moved, previous, map[int64]bool{1: true}); len(got) != 0 {
		t.Errorf("a channel whose backups advanced was reported as stalled: %+v", got)
	}
	// A channel with no previous observation cannot have stalled.
	if got := m.StalledBackups(current, nil, map[int64]bool{1: true}); len(got) != 0 {
		t.Errorf("a first observation was called a stall: %+v", got)
	}
}

// Nobody can raise the fee on a pre-signed justice transaction, so the only
// useful thing to do about it is say so plainly.
func TestALowBakedInFeeRateIsReportedWithTheOnlyRealRemedy(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(120)),
		func(o *MonitorOptions) { o.LowFeeSatPerVByte = 25 })

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	fee := concernsOf(pass, ConcernFeeRateFixed)
	if len(fee) != 1 {
		t.Fatalf("got %d fee concerns for a 10 sat/vB session against a 25 sat/vB floor", len(fee))
	}
	if !strings.Contains(fee[0].Message, "nobody can raise it") {
		t.Errorf("the message does not say the rate is fixed: %q", fee[0].Message)
	}
	if !strings.Contains(fee[0].Message, "Re-registering") {
		t.Errorf("the message does not give the only remedy: %q", fee[0].Message)
	}

	// A healthy rate says nothing, and neither does an unset floor.
	quiet := newMonitor(t, registeredWith(anchorSession(120)),
		func(o *MonitorOptions) { o.LowFeeSatPerVByte = 5 })
	pass, err = quiet.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(concernsOf(pass, ConcernFeeRateFixed)) != 0 {
		t.Error("a session paying above the floor was complained about")
	}
}

// Exhausted sessions are not a fault, and they are the only moment the fee rate
// on future justice transactions ever changes.
func TestExhaustedSessionsAreExplainedRatherThanAlarmedAbout(t *testing.T) {
	t.Parallel()
	c := registeredWith(anchorSession(1024))
	c.stats = ClientStats{NumBackups: 1024, NumSessionsAcq: 2, NumSessionsExh: 1}
	m := newMonitor(t, c)

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	exh := concernsOf(pass, ConcernSessionsExhausted)
	if len(exh) != 1 {
		t.Fatalf("got %d concerns about exhausted sessions", len(exh))
	}
	if !strings.Contains(exh[0].Message, "normal") {
		t.Errorf("an ordinary event was not described as one: %q", exh[0].Message)
	}
	if pass.Stats.NumBackups != 1024 {
		t.Errorf("the node's own summary was not carried through: %+v", pass.Stats)
	}
}

// Backups across sessions of one type add up: a session that filled and was
// replaced still holds the states it took.
func TestBackupsAccumulateAcrossReplacedSessions(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(1024), anchorSession(37)))

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Coverage[0].NumBackups != 1061 {
		t.Errorf("backups = %d, want 1061 across both sessions", pass.Coverage[0].NumBackups)
	}
}

// A session type LND reported in a form we do not recognise must not be counted
// as covering anything.
func TestAnUnrecognisedSessionTypeCoversNothing(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(Session{Policy: PolicyUnknown, NumBackups: 500}))

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Coverage[0].Coverable {
		t.Error("a session of an unrecognised type was counted as coverage")
	}
}

func TestPolicyNamesAreReadInEitherFormLNDSends(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wire string
		want PolicyType
	}{
		{"LEGACY", PolicyLegacy},
		{"ANCHOR", PolicyAnchor},
		{"TAPROOT", PolicyTaproot},
		{"legacy", PolicyLegacy},
		{"POLICY_TYPE_ANCHOR", PolicyAnchor},
		{"policy_type_taproot", PolicyTaproot},
		{"SOMETHING_NEW", PolicyUnknown},
		{"", PolicyUnknown},
	} {
		if got := policyFromWire(tc.wire); got != tc.want {
			t.Errorf("%q read as %q, want %q", tc.wire, got, tc.want)
		}
	}
}

// A read that genuinely failed must not be mistaken for a clean pass with
// nothing to report.
func TestAFailedReadIsAnErrorAndNotAnEmptyVerdict(t *testing.T) {
	t.Parallel()

	m := newMonitor(t, &fakeClient{towersErr: errors.New("connection refused")})
	if _, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1); err == nil {
		t.Error("a node that could not be read produced a clean pass")
	}

	noVersion := registeredWith(anchorSession(1))
	noVersion.versErr = errors.New("nope")
	m = newMonitor(t, noVersion)
	if _, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1); err == nil {
		t.Error("a node whose version could not be read produced a clean pass")
	}

	// Stats failing is survivable: it is a summary, not the answer.
	noStats := registeredWith(anchorSession(1))
	noStats.statsErr = errors.New("nope")
	m = newMonitor(t, noStats)
	if _, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1); err != nil {
		t.Errorf("a missing summary was treated as fatal: %v", err)
	}
}

func TestAMonitorNeedsANodeAndATower(t *testing.T) {
	t.Parallel()

	if _, err := NewMonitor(MonitorOptions{TowerPubkey: ourTower}); err == nil {
		t.Error("a monitor with no node to read was built")
	}
	if _, err := NewMonitor(MonitorOptions{Client: &fakeClient{}}); err == nil {
		t.Error("a monitor that does not know which tower it watches was built")
	}
}

// The last-backup time is what a user reads to know whether protection is
// current, so it must not be reset by a pass that learned nothing new.
func TestTheLastBackupTimeIsKeptWhenNothingNewArrives(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(120)))
	channels := channelsOf(store.ChanAnchors)

	first, err := m.Check(context.Background(), channels, nil, 1_790_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if first.Coverage[0].LastBackupAt != 1_790_000_000 {
		t.Fatalf("the first observation did not record a backup time: %+v", first.Coverage[0])
	}

	// Same count an hour later: nothing new arrived, so the time must not move.
	second, err := m.Check(context.Background(), channels, first.Coverage, 1_790_003_600)
	if err != nil {
		t.Fatal(err)
	}
	if second.Coverage[0].LastBackupAt != 1_790_000_000 {
		t.Errorf("the last-backup time moved to %d without a new backup",
			second.Coverage[0].LastBackupAt)
	}

	// A new backup does move it.
	m = newMonitor(t, registeredWith(anchorSession(121)))
	third, err := m.Check(context.Background(), channels, second.Coverage, 1_790_007_200)
	if err != nil {
		t.Fatal(err)
	}
	if third.Coverage[0].LastBackupAt != 1_790_007_200 {
		t.Errorf("a new backup did not update the time: %+v", third.Coverage[0])
	}
}
