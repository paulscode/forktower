package tower

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

type scoutHarness struct {
	t      *testing.T
	store  *store.Store
	bus    *bus.Bus
	client *fakeClient
	scout  *Scout
	clock  *atomic.Int64
	events <-chan bus.Event
}

func newScoutHarness(t *testing.T, tweak ...func(*ScoutOptions)) *scoutHarness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)
	events := b.Subscribe("test", bus.KindTowerHealthChanged, bus.KindTowerConcern)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	client := registeredWith(anchorSession(120))
	opts := ScoutOptions{
		Store: st, Client: client, Bus: b,
		Now: func() time.Time { return time.Unix(clock.Load(), 0) },
	}
	for _, fn := range tweak {
		fn(&opts)
	}
	sc, err := NewScout(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &scoutHarness{
		t: t, store: st, bus: b, client: client, scout: sc,
		clock: clock, events: events,
	}
}

// recordOurTower puts a managed tower in the store, standing in for the
// companion tower having come up and reported its identity.
func (h *scoutHarness) recordOurTower() {
	h.t.Helper()
	if _, _, err := h.store.UpsertTower(context.Background(), store.Tower{
		Kind: store.TowerLND, Pubkey: ourTower, Managed: true,
		URI: ourTower + "@abcdef.onion:9911", FirstSeenAt: h.clock.Load(),
		UpdatedAt: h.clock.Load(),
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *scoutHarness) pass() {
	h.t.Helper()
	h.scout.pass(context.Background())
}

func (h *scoutHarness) addChannel(chanType store.ChanType, txid string) int64 {
	h.t.Helper()
	ctx := context.Background()
	const node = "02aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		h.t.Fatal(err)
	}
	id, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: node, FundingTxID: txid, CapacitySat: 1_000_000,
		ChanType: chanType, UpdatedAt: 1,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// ourTowerIsRecorded files the tower this installation runs, the way the warden
// would have.
//
// Through the store rather than through configuration, because that is how the
// scout finds out: a tower's identity comes from the tower, so nothing could
// have told the scout in advance.
func (h *scoutHarness) ourTowerIsRecorded() {
	h.t.Helper()
	if _, _, err := h.store.UpsertTower(context.Background(), store.Tower{
		Kind: store.TowerLND, Pubkey: ourTower, Managed: true,
		FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *scoutHarness) towers() []store.Tower {
	h.t.Helper()
	rows, err := h.store.ListTowers(context.Background(), store.TowerFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *scoutHarness) drain() []bus.Event {
	h.t.Helper()
	var out []bus.Event
	for {
		select {
		case e := <-h.events:
			out = append(out, e)
		case <-time.After(50 * time.Millisecond): //nolint:forbidigo // draining a channel
			return out
		}
	}
}

// **Discovered rather than configured.** The node already knows which towers it
// backs up to; asking the user to type the same list into Forktower would be
// asking twice and getting it wrong once.
func TestATowerTheUserRegisteredWithIsFoundWithoutBeingConfigured(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))

	h.pass()

	rows := h.towers()
	if len(rows) != 1 {
		t.Fatalf("got %d towers, want the one the node uses", len(rows))
	}
	if rows[0].Managed {
		t.Error("a tower the user registered with was recorded as one we run")
	}
	if rows[0].Pubkey != ourTower {
		t.Errorf("pubkey = %q", rows[0].Pubkey)
	}
	if rows[0].URI == "" {
		t.Error("no address was recorded, so the dashboard cannot show one")
	}
}

// The tower this installation runs is the warden's business. Recording it twice
// would put it on the page twice.
func TestOurOwnTowerIsNotRecordedAgain(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.ourTowerIsRecorded()

	h.pass()

	rows := h.towers()
	if len(rows) != 1 {
		t.Fatalf("got %d towers, want only the managed one", len(rows))
	}
	// **And it is still ours.** Recording it as somebody else's would overwrite
	// the flag the page uses to decide whether it can honestly offer to restart
	// it.
	if !rows[0].Managed {
		t.Error("the scout demoted the tower this installation runs to somebody else's")
	}
}

// Coverage rests on the session existing, which is evidence. An external tower's
// version cannot be read at all — there is no interface to ask — and that is the
// better basis anyway.
func TestCoverageForAnExternalTowerRestsOnTheSessionsItHolds(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)
	anchors := h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	taproot := h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))
	h.clock.Store(1_790_000_000)

	h.pass()
	h.clock.Add(GracePeriodSeconds * 2)
	h.pass()

	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byChannel := map[int64]store.Coverage{}
	for _, r := range rows {
		byChannel[r.ChannelID] = r
	}
	if !byChannel[anchors].Coverable {
		t.Errorf("the anchor channel was not covered: %q", byChannel[anchors].Reason)
	}
	if byChannel[taproot].Coverable {
		t.Error("a taproot channel was covered by an anchor session")
	}
	if byChannel[anchors].NumBackups != 120 {
		t.Errorf("backups = %d, want the session's 120", byChannel[anchors].NumBackups)
	}
}

// A tower registered with but holding no sessions is not yet protecting
// anything, and must not read as though it were.
func TestATowerWithNoSessionsYetIsNotCalledWorking(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		bare := registeredWith()
		bare.towers[0].Pubkey = ourTower
		o.Client = bare
	})

	h.pass()

	rows := h.towers()
	if len(rows) != 1 {
		t.Fatalf("got %d towers", len(rows))
	}
	if rows[0].Status == store.TowerReachable {
		t.Error("a tower holding no sessions was reported as working")
	}
	if !strings.Contains(rows[0].StatusDetail, "not agreed a session") {
		t.Errorf("the detail does not say what is missing: %q", rows[0].StatusDetail)
	}
}

// **Said once, because it describes the deployment rather than a fault with it.**
// It matters because it changes what can be done when one stops: there is no
// process here to restart and no settings here to correct.
func TestBeingProtectedOnlyBySomebodyElsesTowerIsSaidOnce(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))

	h.pass()
	first := h.drain()

	var said *bus.TowerConcern
	for _, e := range first {
		if c, ok := e.(bus.TowerConcern); ok && c.Concern == string(ConcernExternalOnly) {
			said = &c
		}
	}
	if said == nil {
		t.Fatalf("nothing was said about being protected only by somebody else's tower: %+v", first)
	}
	if !strings.Contains(said.Message, "register with another") {
		t.Errorf("the message does not say what the remedy would be: %q", said.Message)
	}
	// **It used to open with "That works", and that was the bug.** It works for
	// what a watchtower ordinarily does; it cannot be relied on for the thing
	// this program is about, because the chain a tower somebody else runs is
	// watching is neither visible from here nor fixed. Leading with reassurance
	// talked a user out of the step that closes the gap.
	if strings.Contains(said.Message, "That works") {
		t.Errorf("the message reassures about a guarantee it cannot make: %q", said.Message)
	}
	if !strings.Contains(said.Message, "whichever chain their node follows") {
		t.Errorf("the message does not say what an external tower cannot promise: %q",
			said.Message)
	}

	h.pass()
	h.pass()
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok && c.Concern == string(ConcernExternalOnly) {
			t.Error("a description of the deployment was repeated on every pass")
		}
	}
}

// With a tower of our own, this is not worth saying at all.
func TestWithOurOwnTowerNothingIsSaidAboutExternalOnes(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.ourTowerIsRecorded()

	h.pass()

	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok && c.Concern == string(ConcernExternalOnly) {
			t.Errorf("said the user has only external towers while running one: %q", c.Message)
		}
	}
}

// A node with its watchtower client switched off is the warden's news. Two
// notifications for one fact is one too many.
func TestASwitchedOffClientIsLeftToTheWardenToReport(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.Client = &fakeClient{towersErr: ErrClientNotActive}
	})

	h.pass()

	if rows := h.towers(); len(rows) != 0 {
		t.Errorf("towers were recorded from a node that is backing up to nothing: %+v", rows)
	}
	if events := h.drain(); len(events) != 0 {
		t.Errorf("the scout announced something the warden already says: %+v", events)
	}
}

// A restart must not restart the grace period, for the same reason it must not
// for the tower we run: it would suppress real problems exactly when somebody is
// most likely to be looking.
func TestARestartDoesNotResetAnExternalTowersGracePeriod(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		bare := registeredWith()
		bare.towers[0].Pubkey = ourTower
		o.Client = bare
	})
	h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	h.pass()
	h.clock.Add(GracePeriodSeconds * 3)

	// A second scout over the same database, with no memory of the first.
	restarted, err := NewScout(ScoutOptions{
		Store: h.store, Client: h.scout.client, Bus: h.bus,
		Now: func() time.Time { return time.Unix(h.clock.Load(), 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.pass(context.Background())

	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d coverage rows", len(rows))
	}
	if strings.Contains(rows[0].Reason, "registered less than") {
		t.Errorf("after a restart a long-registered tower was treated as fresh: %q",
			rows[0].Reason)
	}
}

// A read that failed must not be reported as a node using no towers.
func TestANodeThatCannotBeReadRecordsNothing(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.Client = &fakeClient{towersErr: errors.New("connection refused")}
	})

	h.pass()

	if rows := h.towers(); len(rows) != 0 {
		t.Errorf("a failed read produced %d towers", len(rows))
	}
}

func TestAScoutNeedsItsParts(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := bus.New(nil)
	t.Cleanup(b.Close)

	for _, tc := range []struct {
		name string
		opts ScoutOptions
	}{
		{"no storage", ScoutOptions{Client: &fakeClient{}, Bus: b}},
		{"no bus", ScoutOptions{Store: st, Client: &fakeClient{}}},
		{"no node to read", ScoutOptions{Store: st, Bus: b}},
	} {
		if _, err := NewScout(tc.opts); err == nil {
			t.Errorf("%s: a scout was built anyway", tc.name)
		}
	}
}

// Run must look immediately and stop when asked.
func TestTheScoutLooksImmediatelyAndStopsWhenAsked(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.scout.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for len(h.towers()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the scout did not look on starting")
		case <-time.After(10 * time.Millisecond): //nolint:forbidigo // waiting for a first pass
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopping reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scout did not stop when asked")
	}
}

// A tower this installation runs that has not answered yet is not an absent one.
//
// **Reported by a user on StartOS 0.3.5.1, where the companion tower is on by
// default.** They had a third-party tower registered, and within seconds of
// installing were told their arrangement was external-only and that it worked —
// said once and never withdrawn — while the tower they should have registered
// was still opening its listener. It is not recorded until it reports its own
// pubkey, and reading that gap as "this installation runs no tower" is the whole
// of the bug.
func TestATowerOfOursThatHasNotAnsweredYetIsNotAnAbsentOne(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) { o.RunsOwnWatchtower = true })
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))

	// The node backs up to somebody else's tower. Ours has not answered, so
	// nothing has been recorded for it.
	h.client.towers = []RegisteredTower{{
		Pubkey:    "02" + strings.Repeat("b", 62),
		Addresses: []string{"somebody-else.onion:9911"},
		Sessions:  []Session{anchorSession(40)},
	}}

	h.pass()
	h.pass()

	for _, e := range h.drain() {
		c, ok := e.(bus.TowerConcern)
		if !ok {
			continue
		}
		if c.Concern == string(ConcernExternalOnly) {
			t.Errorf("an installation that runs its own tower was described as "+
				"external-only while that tower was still starting: %q", c.Message)
		}
		if c.Concern == string(ConcernOursNotRegistered) {
			t.Errorf("the user was asked to register a tower that has not said "+
				"who it is yet: %q", c.Message)
		}
	}
}

// Once ours is up and the node is not using it, say so — and say what it is for.
func TestATowerOfOursThatIsUpAndUnregisteredIsAskedFor(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) { o.RunsOwnWatchtower = true })
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.recordOurTower()

	h.client.towers = []RegisteredTower{{
		Pubkey:    "02" + strings.Repeat("b", 62),
		Addresses: []string{"somebody-else.onion:9911"},
		Sessions:  []Session{anchorSession(40)},
	}}

	h.pass()

	var said *bus.TowerConcern
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok && c.Concern == string(ConcernOursNotRegistered) {
			said = &c
		}
	}
	if said == nil {
		t.Fatal("a third-party tower and an unregistered tower of our own " +
			"produced no request to register it")
	}
	// The reason has to be in the message. "Register this too" without saying
	// what it buys reads as housekeeping, and housekeeping gets postponed.
	if !strings.Contains(said.Message, "whichever chain their node follows") {
		t.Errorf("the message does not say what an external tower cannot "+
			"promise: %q", said.Message)
	}
	if !strings.Contains(said.Message, words.OtherChain) {
		t.Errorf("the message does not say which chain this is about: %q", said.Message)
	}
}

// And when they go and do it, it is withdrawn.
func TestRegisteringOurTowerWithdrawsTheRequest(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) { o.RunsOwnWatchtower = true })
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.recordOurTower()

	h.client.towers = []RegisteredTower{{
		Pubkey:    "02" + strings.Repeat("b", 62),
		Addresses: []string{"somebody-else.onion:9911"},
		Sessions:  []Session{anchorSession(40)},
	}}
	h.pass()
	h.drain()

	// They register ours alongside the one they had.
	h.client.towers = append(h.client.towers, RegisteredTower{
		Pubkey:    ourTower,
		Addresses: []string{"abcdef.onion:9911"},
		Sessions:  []Session{anchorSession(3)},
	})
	h.pass()

	var cleared bool
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernOursNotRegistered) && c.Cleared {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the user registered the tower and the request for it was never " +
			"withdrawn, so the dashboard still asks for something already done")
	}
}

// ── A registration that points at nothing ────────────────────────────────────

// ourTowerAt records our tower advertising a particular address.
func (h *scoutHarness) ourTowerAt(uri string) {
	h.t.Helper()
	if _, _, err := h.store.UpsertTower(context.Background(), store.Tower{
		Kind: store.TowerLND, Pubkey: ourTower, Managed: true, URI: uri,
		FirstSeenAt: h.clock.Load(), UpdatedAt: h.clock.Load(),
	}); err != nil {
		h.t.Fatal(err)
	}
}

// staleConcern picks the stale-registration concern out of what was published.
func (h *scoutHarness) staleConcern() *bus.TowerConcern {
	h.t.Helper()
	var found *bus.TowerConcern
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && !c.Cleared {
			found = &c
		}
	}
	return found
}

// The defect this was written for. Measured on StartOS 0.4.0.1: the node held two
// container addresses, both of them previous incarnations of the Forktower
// container, while the tower itself was alive on a third. Every session request
// dialled a dead address and failed silently for eighty-seven minutes.
func TestARegistrationPointingAtADeadAddressIsReported(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"10.0.3.233:9911", "10.0.3.93:9911"},
	}}

	h.pass()

	said := h.staleConcern()
	if said == nil {
		t.Fatal("the node held only addresses that no longer reach the tower and " +
			"nothing was said, which is the whole defect: a registration that " +
			"looks correct and protects nothing")
	}
	// Both dead addresses, so the user can see which registration is meant.
	for _, dead := range []string{"10.0.3.233:9911", "10.0.3.93:9911"} {
		if !strings.Contains(said.Message, dead) {
			t.Errorf("the message does not name the stale address %s: %q", dead, said.Message)
		}
	}
	// And where it actually is, or there is nothing to act on.
	if !strings.Contains(said.Message, "forktower.startos:9911") {
		t.Errorf("the message does not say where the tower is now: %q", said.Message)
	}
	// It has to agree that they already registered. Somebody who is told to
	// register a tower they can see listed on their own node concludes Forktower
	// is mistaken, and that is the last time they read one of these.
	if !strings.Contains(said.Message, "was correct when you made it") {
		t.Errorf("the message does not acknowledge that their registration was "+
			"right at the time, so it reads as a false alarm: %q", said.Message)
	}
	// And say what changed, or re-running the command they ran before is the
	// obvious move and it fails identically.
	if !strings.Contains(said.Message, "moved") {
		t.Errorf("the message does not say the address changed underneath them: %q",
			said.Message)
	}
	// The remedy, in the syntax lncli actually takes. `--pubkey=`/`--address=`
	// are not flags it has; `remove` takes pubkey or pubkey@address positionally.
	if !strings.Contains(said.Message, "wtclient remove "+ourTower) {
		t.Errorf("the message does not give a command that would work: %q", said.Message)
	}
	if strings.Contains(said.Message, "--pubkey") || strings.Contains(said.Message, "--address") {
		t.Errorf("the message gives lncli flags that do not exist: %q", said.Message)
	}
}

// **The false alarm this must never raise.** lnd does not store the name it was
// given — it resolves it when the tower is added and keeps the number. Measured:
// a tower added as `forktower.startos:9911` reads back as `10.0.3.52:9911`. A
// string comparison would call that perfectly good registration stale, every
// pass, and teach the user to ignore the one concern that matters.
func TestAResolvedAddressIsNotAStaleOne(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"10.0.3.52:9911"},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a registration holding the address our name resolves to was "+
			"called stale: %q", said.Message)
	}
}

// One live address among dead ones still reaches the tower. Saying so would be
// noise about a wasted retry, not a warning about lost protection.
func TestOneReachableAddressIsEnough(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"10.0.3.233:9911", "10.0.3.52:9911"},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a registration that can still reach the tower was reported as "+
			"stale: %q", said.Message)
	}
}

// An onion is stored verbatim, so it compares directly and needs no resolver.
// Confirmed on hardware: registered as an onion, read back as that onion, and a
// session followed.
func TestAnOnionRegistrationIsComparedDirectly(t *testing.T) {
	t.Parallel()
	const onion = "bnmsdrpzycm3pzkuj4q4lbupa7ngmiuj43u37uo6ucxkgaiibdionxid.onion:9911"
	resolved := false
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			resolved = true
			return nil, errors.New("an onion must not be sent to the resolver")
		}
	})
	h.ourTowerAt(ourTower + "@" + onion)

	h.client.towers = []RegisteredTower{{Pubkey: ourTower, Addresses: []string{onion}}}
	h.pass()

	if resolved {
		t.Error("an onion address was handed to the system resolver, which cannot " +
			"answer for it and would fail the comparison open")
	}
	if said := h.staleConcern(); said != nil {
		t.Errorf("a correct onion registration was called stale: %q", said.Message)
	}
}

// A stale onion is still stale — a rebuilt tower gets a new one.
func TestAnOldOnionIsStale(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) { o.RunsOwnWatchtower = true })
	h.ourTowerAt(ourTower + "@newonion" + strings.Repeat("a", 48) + ".onion:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"oldonion" + strings.Repeat("b", 48) + ".onion:9911"},
	}}
	h.pass()

	if said := h.staleConcern(); said == nil {
		t.Error("the tower was rebuilt onto a new onion and the node's old one " +
			"was not reported")
	}
}

// **A name that will not resolve is not evidence of anything.** Silence beats a
// guess: the resolver being down says nothing about whether the registration is
// good, and a concern raised on that basis is the kind that gets ignored.
func TestAnUnresolvableNameSaysNothing(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"10.0.3.233:9911"},
	}}
	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a lookup failure was reported as a stale registration: %q", said.Message)
	}
}

// **And a lookup failure must not withdraw one either.** Caught on hardware: a
// genuinely stale registration was reported, then retracted a pass later while
// it was still stale, because "could not tell" and "it is fixed" came out of the
// check as the same answer. The user is left with a resolved notification and no
// protection, which is worse than never having said anything.
func TestALookupFailureDoesNotWithdrawAStandingWarning(t *testing.T) {
	t.Parallel()
	resolves := true
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			if !resolves {
				return nil, errors.New("no such host")
			}
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}

	h.pass()
	if h.staleConcern() == nil {
		t.Fatal("the stale registration was never reported")
	}

	// The resolver goes away. Nothing about the registration has changed.
	resolves = false
	h.pass()
	h.pass()

	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && c.Cleared {
			t.Fatal("a resolver failure withdrew a standing stale-registration " +
				"warning, telling the user their protection was restored when it " +
				"had not been checked at all")
		}
	}
}

// **Evidence beats the string comparison.** A node holding an address Forktower
// no longer advertises may still be reaching the tower on it — seen on hardware
// after an onion was detached while Tor went on serving it. A live session
// settles the question, and the rest of this package already prefers a session
// over any inference drawn from configuration.
func TestALiveSessionMeansTheRegistrationIsNotStale(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"oldonion" + strings.Repeat("c", 48) + ".onion:9911"},
		Sessions:  []Session{anchorSession(12)},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a registration with a live session was called stale on the "+
			"strength of an address comparison: %q", said.Message)
	}
}

// A tower that has not said where it is yet cannot contradict anything.
func TestATowerWithNoAddressYetSaysNothing(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) { o.RunsOwnWatchtower = true })
	h.ourTowerAt("")

	h.client.towers = []RegisteredTower{{
		Pubkey:    ourTower,
		Addresses: []string{"10.0.3.233:9911"},
	}}
	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a tower that has not reported an address produced a stale "+
			"registration warning: %q", said.Message)
	}
}

// And when the user corrects it, the concern is withdrawn — the same complaint
// that produced half of 0.6.2 applies here: they have to act on their own node,
// and coming back to the same sentence tells them nothing took.
func TestCorrectingTheRegistrationWithdrawsTheConcern(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")

	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}
	h.pass()
	if h.staleConcern() == nil {
		t.Fatal("no concern to withdraw")
	}

	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.52:9911"},
	}}
	h.pass()

	var cleared bool
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && c.Cleared {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the registration was corrected and the warning was never withdrawn")
	}
}

// Said once while it stands, not once a pass.
func TestAStandingStaleRegistrationIsNotRepeated(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.52"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}

	h.pass()
	h.drain()
	h.pass()
	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a standing stale registration was announced again: %q", said.Message)
	}
}

// ── The upgrade case: a registration made before the tower had an onion ──────

// **The false positive that would have scared everybody who upgraded.** Before
// the onion existed, users registered against the sibling hostname, and lnd
// stored the address that resolved to. Giving the tower an onion changes what
// Forktower advertises and changes nothing about whether that older registration
// works — it still reaches the tower, and on a node that dials directly it goes
// on backing up exactly as before.
//
// Checking against the advertised address alone would have told every one of
// those users to go and redo something that is not wrong, which is the worst
// possible place to be wrong: the true version of this warning means their
// channels are unprotected, and one false alarm is all it takes for the next one
// to be ignored.
func TestARegistrationFromBeforeTheOnionIsNotStale(t *testing.T) {
	t.Parallel()
	const onion = "33t6ppdplzzs3633rgbfgnjbbnjy25rb3m74k73xuzfwfwdm5ivdykad.onion:9911"
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.AlsoReachableAt = []string{"forktower.startos:9911"}
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.7"}, nil
		}
	})
	// The tower now advertises its onion.
	h.ourTowerAt(ourTower + "@" + onion)

	// The node still holds what it was given a year ago: the address the sibling
	// hostname resolved to at the time, and no session yet this pass.
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.7:9911"},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a registration made before the tower had an onion — and still "+
			"reaching it — was reported as pointing at a dead address: %q",
			said.Message)
	}
}

// The same, where the node stored the name rather than a number.
func TestTheSiblingHostnameItselfIsNotStale(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.AlsoReachableAt = []string{"forktower.startos:9911"}
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.7"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@abcdef" + strings.Repeat("a", 50) + ".onion:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"forktower.startos:9911"},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("a registration holding the sibling hostname was called stale "+
			"while the tower still answers there: %q", said.Message)
	}
}

// And an address that is neither the onion nor any address the tower answers on
// is still reported — the check has not been defanged.
func TestAnAddressTheTowerDoesNotAnswerOnIsStillStale(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.AlsoReachableAt = []string{"forktower.startos:9911"}
		o.Resolve = func(context.Context, string) ([]string, error) {
			return []string{"10.0.3.7"}, nil
		}
	})
	h.ourTowerAt(ourTower + "@abcdef" + strings.Repeat("a", 50) + ".onion:9911")
	// A container address from two rebuilds ago.
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}

	h.pass()

	if h.staleConcern() == nil {
		t.Error("an address the tower answers on none of its addresses was not " +
			"reported, so the check no longer catches what it exists for")
	}
}

// A resolver failure while checking *any* of the alternates leaves the whole
// answer unknown. A half-built set of reachable addresses is exactly how a good
// registration gets called stale.
func TestAPartiallyResolvedAddressSetSaysNothing(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.AlsoReachableAt = []string{"forktower.startos:9911"}
		o.Resolve = func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		}
	})
	h.ourTowerAt(ourTower + "@abcdef" + strings.Repeat("a", 50) + ".onion:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}

	h.pass()

	if said := h.staleConcern(); said != nil {
		t.Errorf("an unresolvable alternate address produced a stale-registration "+
			"warning on a half-built set: %q", said.Message)
	}
}

// **A restart between the fault and its repair must not strand the warning.**
// The memory of having raised it dies with the process; if withdrawal depends on
// that memory, a user who fixes their registration while the daemon is down is
// left with a warning nothing can ever retire. Caught by review rather than by
// hardware, which is why it is a test.
func TestARestartStillWithdrawsACorrectedRegistration(t *testing.T) {
	t.Parallel()
	resolve := func(context.Context, string) ([]string, error) {
		return []string{"10.0.3.52"}, nil
	}
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = resolve
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}
	h.pass()
	if h.staleConcern() == nil {
		t.Fatal("the stale registration was never reported")
	}

	// The daemon restarts: a new Scout, empty memory, same storage.
	fresh, err := NewScout(ScoutOptions{
		Store: h.store, Client: h.client, Bus: h.bus, Now: h.scout.now,
		RunsOwnWatchtower: true, Resolve: resolve,
	})
	if err != nil {
		t.Fatal(err)
	}
	// They corrected it while it was down.
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.52:9911"},
	}}
	fresh.pass(context.Background())

	var cleared int
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && c.Cleared {
			cleared++
		}
	}
	if cleared == 0 {
		t.Fatal("after a restart the corrected registration was never withdrawn, " +
			"so the warning stands on the dashboard with nothing able to retire it")
	}

	// And it is said once, not once a minute.
	fresh.pass(context.Background())
	fresh.pass(context.Background())
	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && c.Cleared {
			t.Error("the withdrawal was repeated on a later pass, which is a bus " +
				"event a minute saying nothing has happened")
		}
	}
}

// A pass that could not reach a verdict must not withdraw it either — the same
// rule as the raise side, and the reason "settled" is not the same as "absent".
func TestARestartWithAnUnjudgeableRegistrationWithdrawsNothing(t *testing.T) {
	t.Parallel()
	h := newScoutHarness(t, func(o *ScoutOptions) {
		o.RunsOwnWatchtower = true
		o.Resolve = func(context.Context, string) ([]string, error) {
			return nil, errors.New("no such host")
		}
	})
	h.ourTowerAt(ourTower + "@forktower.startos:9911")
	h.client.towers = []RegisteredTower{{
		Pubkey: ourTower, Addresses: []string{"10.0.3.233:9911"},
	}}
	h.pass()

	for _, e := range h.drain() {
		if c, ok := e.(bus.TowerConcern); ok &&
			c.Concern == string(ConcernRegistrationStale) && c.Cleared {
			t.Error("a fresh process that could not judge the registration " +
				"withdrew a warning it had not checked")
		}
	}
}
