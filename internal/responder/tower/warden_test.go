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
)

type wardenHarness struct {
	t      *testing.T
	store  *store.Store
	bus    *bus.Bus
	warden *Warden
	events <-chan bus.Event
	clock  *atomic.Int64
	fake   *fakeTower
	client *fakeClient
}

func newWardenHarness(t *testing.T, tweak ...func(*WardenOptions)) *wardenHarness {
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

	fake := healthy()
	client := registeredWith(anchorSession(120))
	client.towers[0].Pubkey = fake.identity.Pubkey

	supervisor, err := New(Options{Kind: store.TowerLND, Reader: fake})
	if err != nil {
		t.Fatal(err)
	}

	opts := WardenOptions{
		Store: st, Bus: b, Supervisor: supervisor, Client: client,
		Kind: store.TowerLND, Managed: true, URI: "configured.onion:9911",
		Now: func() time.Time { return time.Unix(clock.Load(), 0) },
	}
	for _, fn := range tweak {
		fn(&opts)
	}

	w, err := NewWarden(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &wardenHarness{
		t: t, store: st, bus: b, warden: w, events: events,
		clock: clock, fake: fake, client: client,
	}
}

// pass runs one round, synchronously, which is what makes these tests
// deterministic rather than timing-dependent.
func (h *wardenHarness) pass() {
	h.t.Helper()
	h.warden.pass(context.Background())
}

// start runs the real loop until the test ends.
//
// Used where the warden has to take something *off the bus* first — a chain tip,
// which is what a subscription's expiry height is measured against. A test that
// reimplemented that select in the harness would pass against a warden that
// never subscribed at all, which is exactly the bug worth catching.
func (h *wardenHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.warden.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-done })
}

func (h *wardenHarness) tower() store.Tower {
	h.t.Helper()
	rows, err := h.store.ListTowers(context.Background(), store.TowerFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(rows) != 1 {
		h.t.Fatalf("got %d towers, want 1", len(rows))
	}
	return rows[0]
}

func (h *wardenHarness) drain() []bus.Event {
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

func (h *wardenHarness) addChannel(chanType store.ChanType, txid string) int64 {
	h.t.Helper()
	ctx := context.Background()
	const node = "02" + "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788"
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: node, Impl: store.ImplLND, LastSeenAt: 1_790_000_000,
	}); err != nil {
		h.t.Fatal(err)
	}
	id, _, err := h.store.UpsertChannel(ctx, store.Channel{
		LNNodeID: node, FundingTxID: txid, FundingVout: 0,
		CapacitySat: 1_000_000, ChanType: chanType, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// The first pass records the tower, so a restart does not start from nothing.
func TestTheFirstLookRecordsTheTower(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.pass()

	got := h.tower()
	if got.Pubkey != h.fake.identity.Pubkey {
		t.Errorf("pubkey = %q, want the one the tower reported", got.Pubkey)
	}
	if got.Status != store.TowerReachable {
		t.Errorf("status = %q, want %q", got.Status, store.TowerReachable)
	}
	if !got.Managed {
		t.Error("a tower this installation runs was not recorded as managed")
	}
}

// The tower knows where it actually ended up; a Tor address is published by the
// tower rather than written down in advance.
func TestTheAddressTheTowerReportsBeatsTheOneConfigured(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.pass()

	if got := h.tower().URI; got != h.fake.identity.URIs[0] {
		t.Errorf("uri = %q, want the tower's own %q", got, h.fake.identity.URIs[0])
	}

	// With nothing reported, the configured address is all there is.
	h2 := newWardenHarness(t)
	h2.fake.identity.URIs = nil
	h2.pass()
	if got := h2.tower().URI; got != "configured.onion:9911" {
		t.Errorf("uri = %q, want the configured one", got)
	}
}

// A tower is polled every minute. Announcing "still fine" each time would bury
// the one time it was not.
func TestOnlyAChangeIsAnnounced(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)

	h.pass()
	first := h.drain()
	if countKind(first, bus.KindTowerHealthChanged) != 1 {
		t.Fatalf("the first look announced %d health events, want 1",
			countKind(first, bus.KindTowerHealthChanged))
	}

	h.pass()
	h.pass()
	if got := countKind(h.drain(), bus.KindTowerHealthChanged); got != 0 {
		t.Errorf("an unchanged tower was announced %d more times", got)
	}

	// A change is news again, and carries what it was before.
	h.fake.identityErr = errors.New("connection refused")
	h.pass()
	changed := eventsOfKind(h.drain(), bus.KindTowerHealthChanged)
	if len(changed) != 1 {
		t.Fatalf("a tower that went down announced %d events", len(changed))
	}
	ev, ok := changed[0].(bus.TowerHealthChanged)
	if !ok {
		t.Fatalf("unexpected event type %T", changed[0])
	}
	if ev.Status != string(store.TowerUnreachable) {
		t.Errorf("status = %q", ev.Status)
	}
	if ev.Previous != string(store.TowerReachable) {
		t.Errorf("previous = %q, want %q — a subscriber should be able to tell "+
			"recovery from deterioration", ev.Previous, store.TowerReachable)
	}
}

// A standing problem repeated every minute is a problem nobody reads.
func TestAStandingProblemIsSaidOnceAndThenAgainWhenItReturns(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	// Registered now, so nothing is said yet: sessions are not negotiated
	// instantly and a check that cries wolf on every registration is ignored.
	h.pass()
	if got := countKind(h.drain(), bus.KindTowerConcern); got != 0 {
		t.Fatalf("a freshly registered tower was complained about %d times", got)
	}

	h.clock.Store(1_790_000_000 + GracePeriodSeconds*2)
	h.pass()
	first := countKind(h.drain(), bus.KindTowerConcern)
	if first == 0 {
		t.Fatal("an uncovered channel produced no concern at all")
	}

	h.pass()
	h.pass()
	if got := countKind(h.drain(), bus.KindTowerConcern); got != 0 {
		t.Errorf("the same problem was announced %d more times", got)
	}

	// Once it clears, it is news again if it comes back. Nothing is called by
	// hand here: the warden forgets resolved concerns itself at the end of every
	// pass, so backing up the channel is enough to clear it.
	h.client.towers[0].Sessions = append(h.client.towers[0].Sessions,
		Session{Policy: PolicyTaproot, NumBackups: 5, SweepSatPerVByte: 10})
	h.pass()
	h.drain()

	h.client.towers[0].Sessions = h.client.towers[0].Sessions[:1]
	h.pass()
	if got := countKind(h.drain(), bus.KindTowerConcern); got == 0 {
		t.Error("a problem that cleared and came back was never mentioned again")
	}
}

// The coverage verdict has to reach storage, or the dashboard has nothing to
// read and a restart loses what was known.
func TestWhatIsProtectedIsRecorded(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	anchors := h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	taproot := h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	h.pass()
	h.clock.Store(1_790_000_000 + GracePeriodSeconds*2)
	h.pass()

	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d coverage rows for two channels", len(rows))
	}
	byChannel := map[int64]store.Coverage{}
	for _, r := range rows {
		byChannel[r.ChannelID] = r
	}
	if !byChannel[anchors].Coverable {
		t.Errorf("the anchor channel was not covered: %q", byChannel[anchors].Reason)
	}
	if byChannel[taproot].Coverable {
		t.Error("the taproot channel was covered by an anchor session")
	}
	if byChannel[taproot].Reason == "" {
		t.Error("an uncovered channel was recorded with no reason")
	}
}

// A tower that has not answered yet has no identity to key on. That is the
// ordinary state while it starts up, not an error, and not a row.
func TestATowerThatHasNotAnsweredYetIsNotRecorded(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.fake.identityErr = errors.New("connection refused")

	h.pass()

	rows, err := h.store.ListTowers(context.Background(), store.TowerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a tower that never answered was recorded anyway: %+v", rows)
	}
	if h.warden.TowerID() != 0 {
		t.Errorf("a tower id was assigned before the tower answered: %d", h.warden.TowerID())
	}
}

// Storage filling up is worth saying while there is still time to act.
func TestStorageFillingUpIsRaised(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t, func(o *WardenOptions) {
		supervisor, err := New(Options{
			Kind: store.TowerLND, Reader: healthy(),
			DataDir: "/anywhere", LimitMB: 100,
			Usage: func(string) (int64, error) { return 90 << 20, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Supervisor = supervisor
	})

	h.pass()

	var found bool
	for _, e := range eventsOfKind(h.drain(), bus.KindTowerConcern) {
		c, ok := e.(bus.TowerConcern)
		if ok && c.Concern == string(ConcernDiskFilling) {
			found = true
			if !strings.Contains(c.Message, "90 MB") {
				t.Errorf("the message does not say how full it is: %q", c.Message)
			}
		}
	}
	if !found {
		t.Error("a tower at 90% of its storage limit said nothing")
	}
}

// Without a Lightning node there is no evidence about coverage — but the tower
// is still running, still listening, and still costing disk.
func TestATowerIsStillWatchedWithNoLightningNode(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t, func(o *WardenOptions) { o.Client = nil })

	h.pass()

	if h.tower().Status != store.TowerReachable {
		t.Error("the tower was not watched without a Lightning node to check against")
	}
	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("coverage was claimed with no node to read it from: %+v", rows)
	}
}

// A node that cannot be read is a reason to say so, not a reason to stop
// watching the tower.
func TestANodeThatCannotBeReadDoesNotStopTheWatch(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.client.towersErr = errors.New("connection refused")

	h.pass()

	if h.tower().Status != store.TowerReachable {
		t.Error("the tower's own condition was lost because the node was unreadable")
	}
}

// Run must stop when told to, and take one look immediately rather than making
// somebody wait a minute to learn their tower is down.
func TestRunLooksImmediatelyAndStopsWhenAsked(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.warden.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for h.warden.TowerID() == 0 {
		select {
		case <-deadline:
			t.Fatal("the warden did not look at the tower on starting")
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
		t.Fatal("the warden did not stop when asked")
	}
}

func TestAWardenNeedsItsParts(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := bus.New(nil)
	t.Cleanup(b.Close)
	supervisor, err := New(Options{Kind: store.TowerLND, Reader: healthy()})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts WardenOptions
	}{
		{"no store", WardenOptions{Bus: b, Supervisor: supervisor, Kind: store.TowerLND}},
		{"no bus", WardenOptions{Store: st, Supervisor: supervisor, Kind: store.TowerLND}},
		{"no supervisor", WardenOptions{Store: st, Bus: b, Kind: store.TowerLND}},
		{"no kind", WardenOptions{Store: st, Bus: b, Supervisor: supervisor}},
	} {
		if _, err := NewWarden(tc.opts); err == nil {
			t.Errorf("%s: a warden was built anyway", tc.name)
		}
	}
}

func countKind(events []bus.Event, kind string) int {
	return len(eventsOfKind(events, kind))
}

func eventsOfKind(events []bus.Event, kind string) []bus.Event {
	var out []bus.Event
	for _, e := range events {
		if e.Kind() == kind {
			out = append(out, e)
		}
	}
	return out
}

// **A restart must not restart the grace period.** Taking "now" as the moment
// the tower was first seen would suppress every real problem for ten minutes
// each time the daemon comes back — which is exactly when somebody is most
// likely to be looking at it.
func TestRestartingDoesNotResetTheGracePeriod(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	h.pass()
	h.drain()

	// Time passes, well beyond the grace period, and the daemon restarts: a
	// second warden over the same database, with no memory of the first.
	h.clock.Store(1_790_000_000 + GracePeriodSeconds*3)

	supervisor, err := New(Options{Kind: store.TowerLND, Reader: h.fake})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewWarden(WardenOptions{
		Store: h.store, Bus: h.bus, Supervisor: supervisor, Client: h.client,
		Kind: store.TowerLND, Managed: true,
		Now: func() time.Time { return time.Unix(h.clock.Load(), 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.pass(context.Background())

	if got := countKind(h.drain(), bus.KindTowerConcern); got == 0 {
		t.Error("after a restart, a long-standing uncovered channel was treated as " +
			"a fresh registration and said nothing")
	}
}

// **A configured tower that has never answered must still be reported.**
//
// It has no identity to key a row on — the row is keyed by the pubkey, and it
// has not told us one — so there is nothing to file. Reporting only what could
// be filed would make the one case where protection was never there at all the
// one case nobody hears about.
func TestATowerThatNeverAnsweredIsStillAnnounced(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.fake.identityErr = errors.New("connection refused")

	h.pass()

	events := eventsOfKind(h.drain(), bus.KindTowerHealthChanged)
	if len(events) != 1 {
		t.Fatalf("a tower that never answered produced %d health events", len(events))
	}
	ev, ok := events[0].(bus.TowerHealthChanged)
	if !ok {
		t.Fatalf("unexpected event type %T", events[0])
	}
	if ev.Status != string(store.TowerUnreachable) {
		t.Errorf("status = %q, want %q", ev.Status, store.TowerUnreachable)
	}
	if ev.TowerID != 0 {
		t.Errorf("tower id = %d, want zero — there is no row to point at", ev.TowerID)
	}
	if ev.Detail == "" {
		t.Error("nothing was said about why it is not answering")
	}
}

// The address a user pastes has to be one that keeps working.
//
// LND resolves `watchtower.externalip` at startup and advertises the result, so
// a tower configured with a hostname reports back an address. On a container
// platform that address comes from a pool and changes when the container is
// rebuilt — measured on StartOS: configured `forktower.startos:9911`,
// advertised `10.0.3.76:9911`. A user who pasted the second would have a
// registration that silently stopped working, with the tower still healthy and
// nothing saying why.
func TestAResolvedAddressLosesToTheNameItWasResolvedFrom(t *testing.T) {
	t.Parallel()
	const pubkey = "021089ec2bfcec440e12d9cfc64f8815191c1d0ff1b6a2f97e3eb6580ce8f87809"

	w := &Warden{managed: true, uri: "forktower.startos:9911"}
	got := w.uriOf(Observation{Identity: Identity{
		Pubkey: pubkey,
		URIs:   []string{pubkey + "@10.0.3.76:9911"},
	}})
	if want := pubkey + "@forktower.startos:9911"; got != want {
		t.Errorf("uriOf = %q, want %q — an address from a container pool stops "+
			"working the next time the container is rebuilt", got, want)
	}
}

// An onion still wins, because the tower creates it and nobody could have
// written it down in advance. That is the ordinary case and this must not have
// broken it.
func TestAPublishedOnionStillBeatsTheConfiguredAddress(t *testing.T) {
	t.Parallel()
	const pubkey = "03aaaa"

	w := &Warden{managed: true, uri: "forktower.startos:9911"}
	got := w.uriOf(Observation{Identity: Identity{
		Pubkey: pubkey,
		URIs:   []string{pubkey + "@abcdefg.onion:9911"},
	}})
	if want := pubkey + "@abcdefg.onion:9911"; got != want {
		t.Errorf("uriOf = %q, want the published onion %q", got, want)
	}
}

func TestNameOrNumberIsToldApart(t *testing.T) {
	t.Parallel()
	for uri, want := range map[string]bool{
		"03aa@10.0.3.76:9911":         true,
		"10.0.3.76:9911":              true,
		"03aa@[2001:db8::1]:9911":     true,
		"03aa@forktower.startos:9911": false,
		"03aa@abcdefg.onion:9911":     false,
		"forktower.startos:9911":      false,
		"":                            false,
	} {
		if got := isBareAddress(uri); got != want {
			t.Errorf("isBareAddress(%q) = %v, want %v", uri, got, want)
		}
	}
}

// A concern that clears is said to have cleared.
//
// **Reported by a tester.** They turned their node's watchtower client on,
// pasted in the address, came back, and found the same warning sitting there —
// the warden had noticed the concern go away and forgotten it in silence, so the
// one action a user takes here had no visible outcome.
func TestAConcernThatClearsIsAnnouncedAsCleared(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	// The first pass registers the tower; nothing is said inside the grace
	// period, so the clock moves past it before anything is expected.
	h.pass()
	h.drain()
	h.clock.Store(1_790_000_000 + GracePeriodSeconds*2)
	h.pass()
	if countKind(h.drain(), bus.KindTowerConcern) == 0 {
		t.Fatal("an uncovered channel produced no concern at all")
	}

	h.client.towers[0].Sessions = append(h.client.towers[0].Sessions,
		Session{Policy: PolicyTaproot, NumBackups: 5, SweepSatPerVByte: 10})
	h.pass()

	var cleared int
	for _, ev := range h.drain() {
		if c, ok := ev.(bus.TowerConcern); ok && c.Cleared {
			cleared++
			if c.Concern == "" {
				t.Error("a concern cleared without saying which one, so nothing " +
					"downstream can match it to the warning it closes")
			}
		}
	}
	if cleared == 0 {
		t.Error("the channel became covered and nothing said so, leaving the " +
			"warning on the dashboard reading as current")
	}
}

// Not knowing is not good news.
//
// **A gap in the change above, caught before it shipped.** The coverage check
// gives up and returns an empty list for several transient reasons — the most
// common being that the tower's own RPC is still starting up, which is what a
// tester saw in their log. Announcing a clear for every standing concern each
// time that happens would tell somebody their watchtower client had been
// switched on, then warn them it was off again a minute later, on a loop.
func TestATowerThatCannotBeReadClearsNothing(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t)
	h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

	// The first pass registers the tower; nothing is said inside the grace
	// period, so the clock moves past it before anything is expected.
	h.pass()
	h.drain()
	h.clock.Store(1_790_000_000 + GracePeriodSeconds*2)
	h.pass()
	if countKind(h.drain(), bus.KindTowerConcern) == 0 {
		t.Fatal("an uncovered channel produced no concern at all")
	}

	// The tower stops answering, so the check cannot tell what is covered.
	h.client.towersErr = errors.New("the RPC server is in the process of starting up")
	h.pass()

	for _, ev := range h.drain() {
		if c, ok := ev.(bus.TowerConcern); ok && c.Cleared {
			t.Errorf("a tower that could not be read was reported as having "+
				"fixed %q", c.Concern)
		}
	}
}
