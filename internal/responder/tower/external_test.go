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
