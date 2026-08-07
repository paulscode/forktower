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

// ── Registered, serving, and still no session ────────────────────────────────

// registeredNoSessions is a node that knows the tower and has agreed nothing.
func registeredNoSessions() *fakeClient {
	return &fakeClient{
		version: "0.18.5-beta",
		towers: []RegisteredTower{{
			Pubkey:    ourTower,
			Addresses: []string{"forktower.startos:9911"},
		}},
	}
}

// The case this concern exists for, seen on real hardware. Everything looked
// right — registered, tower up, chain synced, address correct — and no session
// was ever agreed because the node's dial could not arrive. Nothing anywhere
// said so for eighty-seven minutes.
//
// The assertions are about what the message has to *achieve*, because the reader
// has already registered their node and can see the tower listed on it. A
// message that reads as "you forgot to register" gets dismissed by exactly the
// people who need it.
func TestARegisteredTowerThatNeverGetsASessionNamesTheCause(t *testing.T) {
	t.Parallel()
	const onion = "03aabb@33t6ppdplzzs3633rgbfgnjbbnjy25rb3m74k73xuzfwfwdm5ivdykad.onion:9911"
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
		o.TowerURI = onion
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	said := concernsOf(pass, ConcernUnreachableFromNode)
	if len(said) != 1 {
		t.Fatalf("a registered tower with no session past the grace period "+
			"produced %d concerns naming the cause, want 1", len(said))
	}
	msg := said[0].Message

	// It must agree that they are registered, or it reads as a mistake.
	if !strings.Contains(msg, "is registered") {
		t.Errorf("the message does not acknowledge that they already registered, "+
			"so it reads as a false alarm to the person who needs it: %q", msg)
	}
	// It must say nothing has *ever* arrived — the fact that makes somebody act.
	if !strings.Contains(msg, "ever reached it") {
		t.Errorf("the message does not say no backup has ever arrived: %q", msg)
	}
	// It must warn that their own node will corroborate nothing.
	if !strings.Contains(msg, "Nothing on your node will tell you") {
		t.Errorf("the message does not warn that the node stays silent: %q", msg)
	}
	// It must say the address changed, or re-running the old command is the
	// obvious next move and it fails the same way.
	if !strings.Contains(msg, "the old one will not start working") {
		t.Errorf("the message does not say the old address is dead: %q", msg)
	}
	// It must hand over the exact string to paste.
	if !strings.Contains(msg, "wtclient add "+onion) {
		t.Errorf("the message does not give the address to register: %q", msg)
	}
	// And reassure, or a cautious user does nothing rather than risk their setup.
	if !strings.Contains(msg, "Nothing is lost") {
		t.Errorf("the message does not say re-registering is safe: %q", msg)
	}
}

// **Do not tell somebody to re-register at an address that will fail too.** When
// the tower has no Tor address yet, re-registering reproduces the same failure
// exactly, and having followed instructions that did not work is how a user
// stops following them.
func TestWithNoTorAddressYetTheMessageAsksForOneFirst(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
		o.TowerURI = "03aabb@forktower.startos:9911"
		o.CanAttachOnion = true
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	said := concernsOf(pass, ConcernUnreachableFromNode)
	if len(said) != 1 {
		t.Fatalf("want 1 concern, got %d", len(said))
	}
	msg := said[0].Message

	if strings.Contains(msg, "wtclient add") {
		t.Errorf("the user was told to re-register at an address that cannot "+
			"work either, which is how somebody learns to ignore instructions: %q", msg)
	}
	if !strings.Contains(msg, "does not have a Tor address yet") {
		t.Errorf("the message does not say what is missing: %q", msg)
	}
	if !strings.Contains(msg, "asked it for one on your behalf") {
		t.Errorf("the message does not tell them a request is waiting for them, "+
			"which is the only thing they can act on: %q", msg)
	}
}

// Not while the tower itself cannot serve — there is already a better
// explanation, and two explanations for one symptom is one too many.
func TestATowerThatCannotServeYetIsNotBlamedOnTheNetwork(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = false
		o.TowerNotServingWhy = "its Bitcoin node is still catching up"
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := concernsOf(pass, ConcernUnreachableFromNode); len(got) != 0 {
		t.Errorf("a tower that is not serving yet was reported as unreachable "+
			"from the node: %q", got[0].Message)
	}
}

// Not during the grace period. Session negotiation takes minutes, and a check
// that fires on every fresh registration is one people learn to ignore.
func TestAFreshRegistrationIsNotCalledUnreachable(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
		o.RegisteredForSeconds = 60
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := concernsOf(pass, ConcernUnreachableFromNode); len(got) != 0 {
		t.Errorf("a registration a minute old was called unreachable: %q", got[0].Message)
	}
}

// Not when there is nothing to protect. A node with no channels has no reason to
// have negotiated anything, so the absence proves nothing.
func TestANodeWithNoChannelsIsNotCalledUnreachable(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
	})

	pass, err := m.Check(context.Background(), nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := concernsOf(pass, ConcernUnreachableFromNode); len(got) != 0 {
		t.Errorf("a node with no channels was told its tower was unreachable: %q",
			got[0].Message)
	}
}

// And once a session exists the question is settled — that is the evidence the
// path works, whatever else may be true.
func TestASessionSettlesTheReachabilityQuestion(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredWith(anchorSession(5)), func(o *MonitorOptions) {
		o.TowerServing = true
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := concernsOf(pass, ConcernUnreachableFromNode); len(got) != 0 {
		t.Errorf("a tower with a live session was reported unreachable: %q", got[0].Message)
	}
}

// An unregistered tower is a different fault with a different remedy, and gets
// its own concern rather than this one.
func TestAnUnregisteredTowerIsNotCalledUnreachable(t *testing.T) {
	t.Parallel()
	none := &fakeClient{version: "0.18.5-beta"}
	m := newMonitor(t, none, func(o *MonitorOptions) { o.TowerServing = true })

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := concernsOf(pass, ConcernUnreachableFromNode); len(got) != 0 {
		t.Errorf("a tower the node has never heard of was reported as unreachable "+
			"rather than unregistered: %q", got[0].Message)
	}
	if len(concernsOf(pass, ConcernNotRegistered)) != 1 {
		t.Error("the unregistered tower was not reported as unregistered")
	}
}

// **No bleed-over onto the packagings that cannot attach an onion.** StartOS
// 0.3.5.1 and Umbrel advertise the only address they have, their nodes dial local
// addresses directly, and Forktower never asks anything for a Tor address there.
// Telling those users that a request is waiting for their approval would send
// them looking for a screen that does not exist — worse than saying nothing,
// because it burns the credibility of the next message too.
func TestThePlatformsWithoutOnionsAreNotToldToApproveOne(t *testing.T) {
	t.Parallel()
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
		o.TowerURI = "03aabb@forktower.embassy:9911"
		o.CanAttachOnion = false
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	said := concernsOf(pass, ConcernUnreachableFromNode)
	if len(said) != 1 {
		t.Fatalf("want 1 concern, got %d", len(said))
	}
	msg := said[0].Message

	if strings.Contains(msg, "asked it for one on your behalf") {
		t.Errorf("a packaging that never asks for a Tor address told the user a "+
			"request was waiting for them: %q", msg)
	}
	if strings.Contains(msg, "Tor address yet") {
		t.Errorf("the user was pointed at an onion their packaging cannot "+
			"produce: %q", msg)
	}
	// It still has to be useful: name the real state and something checkable.
	if !strings.Contains(msg, "is where the tower is") {
		t.Errorf("the message does not say the address is correct: %q", msg)
	}
	if !strings.Contains(msg, "9911") {
		t.Errorf("the message names no port to check: %q", msg)
	}
}

// An onion, however it was obtained, is the same advice everywhere — so a
// packaging that cannot request one is still told to use the one it has.
func TestAnOnionIsTheSameAdviceOnEveryPlatform(t *testing.T) {
	t.Parallel()
	const onion = "03aabb@33t6ppdplzzs3633rgbfgnjbbnjy25rb3m74k73xuzfwfwdm5ivdykad.onion:9911"
	m := newMonitor(t, registeredNoSessions(), func(o *MonitorOptions) {
		o.TowerServing = true
		o.TowerURI = onion
		o.CanAttachOnion = false
	})

	pass, err := m.Check(context.Background(), channelsOf(store.ChanAnchors), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	said := concernsOf(pass, ConcernUnreachableFromNode)
	if len(said) != 1 {
		t.Fatalf("want 1 concern, got %d", len(said))
	}
	if !strings.Contains(said[0].Message, "wtclient add "+onion) {
		t.Errorf("a tower with an onion did not hand over the address to "+
			"register: %q", said[0].Message)
	}
}
