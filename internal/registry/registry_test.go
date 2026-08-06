package registry

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// The backfill asks a chain backend for a block, and a real chain backend must
// be able to answer without an adapter in between.
var _ BlockSource = chainview.ChainView(nil)

const (
	fundingA = "1111111111111111111111111111111111111111111111111111111111111111"
	fundingB = "2222222222222222222222222222222222222222222222222222222222222222"
	nodeID   = "02deadbeef"
)

// fakeNode is a Lightning node under the test's control.
type fakeNode struct {
	mu    sync.Mutex
	snap  Snapshot
	err   error
	polls int

	notify   func()
	watchErr error
}

func (f *fakeNode) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.err != nil {
		return Snapshot{}, f.err
	}
	// A copy, because the test mutates its own snapshot while the registry is
	// reading the one it was handed — which a real client, building fresh
	// records from a fresh HTTP response, never does.
	out := f.snap
	out.Channels = append([]ChannelRecord(nil), f.snap.Channels...)
	return out, nil
}

func (f *fakeNode) set(mutate func(*Snapshot, *error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(&f.snap, &f.err)
}

func (f *fakeNode) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// pushingNode also implements Notifier, as LND does.
type pushingNode struct {
	fakeNode
	watching chan struct{}
	once     sync.Once
}

func (p *pushingNode) Watch(ctx context.Context, notify func()) error {
	p.mu.Lock()
	p.notify = notify
	err := p.watchErr
	p.mu.Unlock()

	p.once.Do(func() { close(p.watching) })
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (p *pushingNode) push(t *testing.T) {
	t.Helper()
	select {
	case <-p.watching:
	case <-time.After(2 * time.Second):
		t.Fatal("the node was never asked for notifications")
	}
	p.mu.Lock()
	notify := p.notify
	p.mu.Unlock()
	notify()
}

type harness struct {
	t     *testing.T
	store *store.Store
	bus   *bus.Bus
	reg   *Registry
	clock *atomic.Int64
}

func newHarness(t *testing.T, sources []Source, blocks []BlockSource) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	b := bus.New(nil)
	t.Cleanup(b.Close)

	clock := &atomic.Int64{}
	clock.Store(1_790_000_000)

	reg, err := New(st, b, sources, blocks,
		Config{PollInterval: 20 * time.Millisecond, BackfillInterval: 20 * time.Millisecond},
		nil, func() time.Time { return time.Unix(clock.Load(), 0) })
	if err != nil {
		t.Fatalf("building the registry: %v", err)
	}
	return &harness{t: t, store: st, bus: b, reg: reg, clock: clock}
}

// run starts the registry and stops it when the test ends.
func (h *harness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.reg.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			h.t.Error("the registry did not stop")
		}
	})
}

// waitFor polls until cond holds. Storage writes happen on another goroutine, so
// reading once and asserting would be a race dressed up as a test.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // polling a real goroutine
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) channels() []store.Channel {
	h.t.Helper()
	got, err := h.store.ListChannels(context.Background(), store.ChannelFilter{})
	if err != nil {
		h.t.Fatalf("reading channels: %v", err)
	}
	return got
}

// find is the tolerant lookup, for use inside a waitFor condition where "not
// there yet" is the ordinary case rather than a failure.
func (h *harness) find(txid string) (store.Channel, bool) {
	h.t.Helper()
	for _, c := range h.channels() {
		if c.FundingTxID == txid {
			return c, true
		}
	}
	return store.Channel{}, false
}

func (h *harness) channel(txid string) store.Channel {
	h.t.Helper()
	c, ok := h.find(txid)
	if !ok {
		h.t.Fatalf("channel %s is not in the store", txid)
	}
	return c
}

func snapshotWith(records ...ChannelRecord) Snapshot {
	return Snapshot{
		Node:     NodeInfo{Pubkey: nodeID, Alias: "test node", Impl: store.ImplLND},
		Channels: records,
	}
}

func record(txid string, mutate func(*ChannelRecord)) ChannelRecord {
	rec := ChannelRecord{
		FundingTxID: txid,
		FundingVout: 0,
		CapacitySat: 1_000_000,
		ChanType:    store.ChanAnchors,
		PeerPubkey:  "03peer",
		OpenHeight:  900,
		CloseState:  store.CloseOpen,
	}
	if mutate != nil {
		mutate(&rec)
	}
	return rec
}

func TestChannelsAreReadIntoTheStore(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil), record(fundingB, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	events := h.bus.Subscribe("test", bus.KindChannelUpserted)
	h.run()

	h.waitFor("both channels to be stored", func() bool { return len(h.channels()) == 2 })

	got := h.channel(fundingA)
	if got.CapacitySat != 1_000_000 || got.ChanType != store.ChanAnchors {
		t.Errorf("stored %+v", got)
	}
	// No split yet, so everything is provisionally watched rather than unknown.
	if got.Relevance != store.Relevant || got.RelevanceReason != ReasonProvisional {
		t.Errorf("relevance = %q (%q)", got.Relevance, got.RelevanceReason)
	}

	// Both arrivals are announced, and announced as new.
	for range 2 {
		select {
		case e := <-events:
			up, ok := e.(bus.ChannelUpserted)
			if !ok {
				t.Fatalf("got %T on the channel feed", e)
			}
			if !up.New {
				t.Error("a channel seen for the first time was not announced as new")
			}
			if up.Channel.RelevanceReason == "" {
				t.Error("a channel was announced without saying why it is being watched")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a channel was stored but never announced")
		}
	}
}

// A poll that finds nothing new must stay quiet. With a poll a minute, an event
// per channel per poll would bury the one that meant something.
func TestAnUnchangedPollSaysNothing(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	events := h.bus.Subscribe("test", bus.KindChannelUpserted)
	h.run()

	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sighting was never announced")
	}

	// Several more polls with nothing changed.
	before := node.pollCount()
	h.waitFor("more polls", func() bool { return node.pollCount() >= before+3 })

	select {
	case e := <-events:
		t.Fatalf("an unchanged channel was announced again: %+v", e)
	default:
	}
}

func TestAChangedChannelIsAnnouncedAgain(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	events := h.bus.Subscribe("test", bus.KindChannelUpserted)
	h.run()

	select {
	case <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sighting was never announced")
	}

	node.set(func(s *Snapshot, _ *error) {
		s.Channels[0].PeerAlias = "the counterparty"
	})

	select {
	case e := <-events:
		up, _ := e.(bus.ChannelUpserted)
		if up.New {
			t.Error("a channel that changed was announced as new")
		}
		if up.Channel.PeerAlias != "the counterparty" {
			t.Errorf("announced alias %q", up.Channel.PeerAlias)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a changed channel was never announced")
	}
}

// The node knows it is closing before the chain does, which is earlier news and
// worth having.
func TestAClosingChannelIsRecordedAndAnnounced(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	closes := h.bus.Subscribe("test", bus.KindChannelClosedSF)
	h.run()

	h.waitFor("the channel to be stored", func() bool { return len(h.channels()) == 1 })

	node.set(func(s *Snapshot, _ *error) {
		s.Channels[0].CloseState = store.ClosePending
		s.Channels[0].CloseTxID = "abc123"
	})

	select {
	case e := <-closes:
		c, ok := e.(bus.ChannelClosedSF)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if c.State != string(store.ClosePending) || c.CloseTxid != "abc123" {
			t.Errorf("announced %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a closing channel was never announced")
	}

	h.waitFor("the close to be stored", func() bool {
		c, ok := h.find(fundingA)
		return ok && c.CloseState == store.ClosePending
	})
	// The block that confirms it is the watcher's to record, not ours.
	if got := h.channel(fundingA).CloseHeight; got != 0 {
		t.Errorf("the registry invented a close height: %d", got)
	}
}

// The chain's answer outranks the node's belief, and a node that restarts and
// briefly forgets must not be able to erase it.
func TestTheChainsAnswerIsNotOverwrittenByTheNode(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("the channel to be stored", func() bool { return len(h.channels()) == 1 })
	id := h.channel(fundingA).ID

	// The watcher sees the close confirm on chain.
	if err := h.store.SetChannelCloseSF(context.Background(), id,
		store.CloseForce, "chaintxid", 1200, 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	// The node now reports it as open again, as one does after a restore.
	node.set(func(s *Snapshot, _ *error) {
		s.Channels[0].CloseState = store.CloseOpen
		s.Channels[0].PeerAlias = "nudge" // force a write so a poll definitely lands
	})

	before := node.pollCount()
	h.waitFor("further polls", func() bool { return node.pollCount() >= before+3 })

	got := h.channel(fundingA)
	if got.CloseState != store.CloseForce || got.CloseTxID != "chaintxid" || got.CloseHeight != 1200 {
		t.Errorf("a Lightning poll undid what the chain recorded: %+v", got)
	}
}

func TestCloseStateOnlyEverMovesForward(t *testing.T) {
	t.Parallel()

	order := []store.CloseState{store.CloseOpen, store.ClosePending, store.CloseForce}
	for i, from := range order {
		for j, to := range order {
			if closeRank(to) > closeRank(from) != (j > i) {
				t.Errorf("%q -> %q is not ordered as expected", from, to)
			}
		}
	}
	// A value the schema does not know must not outrank a real close.
	if closeRank(store.CloseState("nonsense")) >= closeRank(store.CloseCoop) {
		t.Error("an unrecognised close state outranked a real one")
	}
	// The three terminal states rank together: which one it was matters to the
	// user, but not to whether it may be overwritten.
	if closeRank(store.CloseCoop) != closeRank(store.CloseBreach) ||
		closeRank(store.CloseCoop) != closeRank(store.CloseForce) {
		t.Error("the terminal close states do not rank together")
	}
}

// A node that stops answering must not take the inventory down with it. The
// stored channels keep being served, and the failure is visible rather than
// silent.
func TestAnUnreachableNodeDegradesRatherThanForgets(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("the channel to be stored", func() bool { return len(h.channels()) == 1 })

	node.set(func(_ *Snapshot, err *error) { *err = errors.New("connection refused") })

	h.waitFor("the failure to be visible", func() bool {
		hh := h.reg.Health()
		return len(hh) == 1 && hh[0].LastError != ""
	})

	if len(h.channels()) != 1 {
		t.Error("an unreachable node cost us the inventory we already had")
	}
	got := h.reg.Health()[0]
	if got.LastSuccessAt == 0 {
		t.Error("the earlier success was forgotten")
	}
	if !got.Stale(got.LastSuccessAt+120, time.Minute) {
		t.Error("an inventory two minutes old was not reported as stale")
	}
	if got.Stale(got.LastSuccessAt+10, time.Minute) {
		t.Error("a ten-second-old inventory was reported as stale")
	}
}

// A node that has never answered is stale, not fresh: zero is not a timestamp.
func TestANodeThatNeverAnsweredIsStale(t *testing.T) {
	t.Parallel()

	if !(SourceHealth{Name: "lnd"}).Stale(1_790_000_000, time.Minute) {
		t.Error("a node that has never been read was reported as current")
	}
}

// Everything was classified before there was a split to classify against. When
// one arrives, every one of those answers is re-decided.
func TestASplitReclassifiesEverything(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(
		record(fundingA, func(r *ChannelRecord) { r.OpenHeight = 900 }),  // before the fork
		record(fundingB, func(r *ChannelRecord) { r.OpenHeight = 1500 }), // after it
	)}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	// Both stored *and* classified: a row exists for a moment before its
	// relevance is written, and in that moment it holds the schema's default of
	// `unknown` — which is watched, so it is safe, but it is not the answer.
	h.waitFor("both channels to be classified", func() bool {
		got := h.channels()
		if len(got) != 2 {
			return false
		}
		for _, c := range got {
			if c.RelevanceReason == "" {
				return false
			}
		}
		return true
	})
	for _, c := range h.channels() {
		if c.Relevance != store.Relevant || c.RelevanceReason != ReasonProvisional {
			t.Fatalf("%s was not provisional before the split: %q", c.FundingTxID, c.Relevance)
		}
	}

	if err := h.store.SaveSplitState(context.Background(), store.Split{
		State: store.StateSplit, ForkHeight: 1000, ForkHash: "aa", DetectedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})

	h.waitFor("the post-fork channel to be dismissed", func() bool {
		c, ok := h.find(fundingB)
		return ok && c.Relevance == store.Irrelevant
	})
	if got := h.channel(fundingA); got.Relevance != store.Relevant ||
		got.RelevanceReason != ReasonFundedBeforeFork {
		t.Errorf("the pre-fork channel was reclassified as %q (%q)",
			got.Relevance, got.RelevanceReason)
	}
	if got := h.channel(fundingB).RelevanceReason; got != ReasonFundedAfterFork {
		t.Errorf("the post-fork channel was dismissed with the reason %q", got)
	}
}

// The watcher may find that a channel funded after the fork was mirrored onto
// the other chain anyway. The next poll must not quietly undo that.
func TestTheWatchersUpgradeSurvivesTheNextPoll(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(
		record(fundingA, func(r *ChannelRecord) { r.OpenHeight = 1500 }),
	)}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	if err := h.store.SaveSplitState(context.Background(), store.Split{
		State: store.StateSplit, ForkHeight: 1000, ForkHash: "aa", DetectedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("the channel to be dismissed", func() bool {
		c, ok := h.find(fundingA)
		return ok && c.Relevance == store.Irrelevant
	})

	// The watcher finds the funding transaction on the other chain too.
	id := h.channel(fundingA).ID
	if err := h.store.SetChannelRelevance(context.Background(), id,
		store.Relevant, ReasonFundedAfterForkButMirrored, 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	before := node.pollCount()
	h.waitFor("further polls", func() bool { return node.pollCount() >= before+3 })

	if got := h.channel(fundingA); got.Relevance != store.Relevant {
		t.Errorf("a poll undid the watcher's finding: %q (%q)", got.Relevance, got.RelevanceReason)
	}
}

func TestAPushTriggersAPollWithoutWaiting(t *testing.T) {
	t.Parallel()

	node := &pushingNode{
		fakeNode: fakeNode{snap: snapshotWith(record(fundingA, nil))},
		watching: make(chan struct{}),
	}
	// A poll interval long enough that a timer cannot be what makes this pass.
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.reg.cfg.PollInterval = time.Hour
	h.run()

	h.waitFor("the first poll", func() bool { return node.pollCount() >= 1 })
	before := node.pollCount()

	node.push(t)
	h.waitFor("a poll caused by the push", func() bool { return node.pollCount() > before })
}

// A push that arrives while a poll is already queued collapses into one: a
// hundred callers asking at once want the same single re-read.
func TestRefreshDoesNotQueueUp(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)

	for range 1000 {
		h.reg.Refresh() // must never block, with nothing running to drain it
	}
}

// A subscription that fails is not fatal: the poll is what guarantees progress.
func TestALostSubscriptionKeepsPolling(t *testing.T) {
	t.Parallel()

	node := &pushingNode{
		fakeNode: fakeNode{snap: snapshotWith(record(fundingA, nil))},
		watching: make(chan struct{}),
	}
	node.watchErr = errors.New("stream closed")

	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	before := node.pollCount()
	h.waitFor("polling to continue despite the lost subscription", func() bool {
		return node.pollCount() >= before+3
	})
}

func TestTwoNodesAreKeptApart(t *testing.T) {
	t.Parallel()

	one := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	two := &fakeNode{snap: Snapshot{
		Node:     NodeInfo{Pubkey: "03other", Alias: "second", Impl: store.ImplCLN},
		Channels: []ChannelRecord{record(fundingB, nil)},
	}}

	h := newHarness(t, []Source{{Name: "lnd", Client: one}, {Name: "cln", Client: two}}, nil)
	h.run()

	h.waitFor("both nodes' channels", func() bool { return len(h.channels()) == 2 })
	if got := h.channel(fundingA).LNNodeID; got != nodeID {
		t.Errorf("channel A belongs to %q", got)
	}
	if got := h.channel(fundingB).LNNodeID; got != "03other" {
		t.Errorf("channel B belongs to %q", got)
	}
	if len(h.reg.Health()) != 2 {
		t.Error("health is not reported per node")
	}
}

// One node failing must not stop the other being read.
func TestOneFailingNodeDoesNotStopTheOther(t *testing.T) {
	t.Parallel()

	broken := &fakeNode{err: errors.New("connection refused")}
	fine := &fakeNode{snap: Snapshot{
		Node:     NodeInfo{Pubkey: "03other", Impl: store.ImplCLN},
		Channels: []ChannelRecord{record(fundingB, nil)},
	}}

	h := newHarness(t, []Source{{Name: "broken", Client: broken}, {Name: "fine", Client: fine}}, nil)
	h.run()

	h.waitFor("the working node's channel", func() bool { return len(h.channels()) == 1 })
}

// A node that answers but does not say who it is cannot have its channels
// attributed to anything, and guessing would merge two users' channels together.
func TestANamelessNodeIsRefused(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: Snapshot{Channels: []ChannelRecord{record(fundingA, nil)}}}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("several polls", func() bool { return node.pollCount() >= 3 })
	if len(h.channels()) != 0 {
		t.Error("channels were stored for a node that did not identify itself")
	}
	if h.reg.Health()[0].LastError == "" {
		t.Error("a node that did not identify itself was reported as healthy")
	}
}

// One channel the adapter could not store must not lose the others.
func TestOneBadChannelDoesNotLoseTheRest(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(
		record(fundingA, func(r *ChannelRecord) { r.ChanType = "not a type" }),
		record(fundingB, nil),
	)}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("the good channel", func() bool { return len(h.channels()) == 1 })
	if h.channels()[0].FundingTxID != fundingB {
		t.Errorf("stored the wrong channel: %s", h.channels()[0].FundingTxID)
	}
}

func TestHTLCsAreSnapshottedAndReplaced(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, func(r *ChannelRecord) {
		r.HTLCs = []store.HTLCSnapshot{
			{Direction: "incoming", AmountMsat: 1000, CLTVExpiry: 800},
			{Direction: "outgoing", AmountMsat: 2000, CLTVExpiry: 810},
		}
	}))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("both HTLCs", func() bool {
		c, ok := h.find(fundingA)
		if !ok {
			return false
		}
		got, err := h.store.ListHTLCs(context.Background(), c.ID)
		return err == nil && len(got) == 2
	})

	// One settles. The picture is replaced, not accumulated: a settled HTLC must
	// stop producing a deadline.
	node.set(func(s *Snapshot, _ *error) {
		s.Channels[0].HTLCs = s.Channels[0].HTLCs[:1]
		s.Channels[0].PeerAlias = "nudge"
	})
	h.waitFor("the settled HTLC to disappear", func() bool {
		c, ok := h.find(fundingA)
		if !ok {
			return false
		}
		got, err := h.store.ListHTLCs(context.Background(), c.ID)
		return err == nil && len(got) == 1
	})
}

// fakeChain answers block lookups, or refuses to, as a pruned node does.
type fakeChain struct {
	mu     sync.Mutex
	blocks map[int32]*wire.MsgBlock
	err    error
	calls  int
}

func (f *fakeChain) BlockHashByHeight(_ context.Context, height int32) (chainhash.Hash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return chainhash.Hash{}, f.err
	}
	blk, ok := f.blocks[height]
	if !ok {
		return chainhash.Hash{}, errors.New("no block at that height")
	}
	return blk.BlockHash(), nil
}

func (f *fakeChain) Block(_ context.Context, h chainhash.Hash) (*wire.MsgBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	for _, blk := range f.blocks {
		if blk.BlockHash() == h {
			return blk, nil
		}
	}
	return nil, errors.New("no such block")
}

func (f *fakeChain) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// blockWithFunding builds a block containing a transaction whose output 0 pays
// the given script, and returns the block and that transaction's id.
func blockWithFunding(t *testing.T, script []byte) (blk *wire.MsgBlock, txid string) {
	t.Helper()

	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(wire.NewTxOut(1_000_000, script))

	blk = wire.NewMsgBlock(&wire.BlockHeader{Version: 1})
	if err := blk.AddTransaction(tx); err != nil {
		t.Fatalf("building a block: %v", err)
	}
	return blk, tx.TxHash().String()
}

func TestTheFundingScriptIsFilledInFromTheChain(t *testing.T) {
	t.Parallel()

	script := []byte{0x00, 0x20, 0xab, 0xcd}
	blk, txid := blockWithFunding(t, script)
	chain := &fakeChain{blocks: map[int32]*wire.MsgBlock{900: blk}}

	node := &fakeNode{snap: snapshotWith(record(txid, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})
	h.run()

	h.waitFor("the funding script", func() bool {
		c, ok := h.find(txid)
		return ok && c.FundingScriptHex == hex.EncodeToString(script)
	})

	// And a later poll, which carries no script, must not wipe it: the node never
	// reports one, so an empty value means "not said", not "gone".
	node.set(func(s *Snapshot, _ *error) { s.Channels[0].PeerAlias = "nudge" })
	before := node.pollCount()
	h.waitFor("further polls", func() bool { return node.pollCount() >= before+3 })

	if got := h.channel(txid).FundingScriptHex; got != hex.EncodeToString(script) {
		t.Errorf("a poll erased the funding script: %q", got)
	}
}

// The common case on both target platforms: a pruned node that simply cannot
// answer. It must not stop anything, and it must not be retried forever at full
// volume.
func TestAPrunedNodeCostsNothing(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{err: errors.New("block not available, pruned data")}
	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})
	h.run()

	h.waitFor("the backfill to have tried", func() bool { return chain.callCount() >= 2 })

	got := h.channel(fundingA)
	if got.FundingScriptHex != "" {
		t.Errorf("a script appeared from a node that has no blocks: %q", got.FundingScriptHex)
	}
	// The channel is still watched. Nothing about the missing script changes that
	// — on the tier this daemon runs on, outpoints are what get matched.
	if got.Relevance != store.Relevant {
		t.Errorf("a missing funding script cost a channel its protection: %q", got.Relevance)
	}
}

// The other chain's backend is a real fallback: before the fork both chains hold
// the same block, so the one the user's node pruned may still be over there.
func TestTheOtherChainIsTriedWhenTheFirstCannotAnswer(t *testing.T) {
	t.Parallel()

	script := []byte{0x51}
	blk, txid := blockWithFunding(t, script)

	pruned := &fakeChain{err: errors.New("pruned")}
	other := &fakeChain{blocks: map[int32]*wire.MsgBlock{900: blk}}

	node := &fakeNode{snap: snapshotWith(record(txid, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}},
		[]BlockSource{pruned, other})
	h.run()

	h.waitFor("the funding script from the fallback", func() bool {
		c, ok := h.find(txid)
		return ok && c.FundingScriptHex == hex.EncodeToString(script)
	})
	if pruned.callCount() == 0 {
		t.Error("the user's own node was not asked first")
	}
}

// A channel whose funding transaction is not in the block the node named is a
// disagreement worth not papering over: no script is better than a wrong one.
func TestAScriptIsNotInventedWhenTheBlockDisagrees(t *testing.T) {
	t.Parallel()

	blk, _ := blockWithFunding(t, []byte{0x51})
	chain := &fakeChain{blocks: map[int32]*wire.MsgBlock{900: blk}}

	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})
	h.run()

	h.waitFor("the backfill to have tried", func() bool { return chain.callCount() >= 2 })
	if got := h.channel(fundingA).FundingScriptHex; got != "" {
		t.Errorf("a script was taken from the wrong transaction: %q", got)
	}
}

// An output index the transaction does not have must be refused, not read past.
func TestAnOutputThatDoesNotExistIsRefused(t *testing.T) {
	t.Parallel()

	blk, txid := blockWithFunding(t, []byte{0x51})
	chain := &fakeChain{blocks: map[int32]*wire.MsgBlock{900: blk}}

	for _, vout := range []int32{-1, 1, 99} {
		_, err := scriptFromBlock(context.Background(), chain, 900, txid, vout)
		if err == nil {
			t.Errorf("output %d was accepted from a transaction with one output", vout)
		}
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()

	b := bus.New(nil)
	t.Cleanup(b.Close)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := New(nil, b, nil, nil, Config{}, nil, nil); err == nil {
		t.Error("a registry with no store was accepted")
	}
	if _, err := New(st, nil, nil, nil, Config{}, nil, nil); err == nil {
		t.Error("a registry with no bus was accepted")
	}
	if _, err := New(st, b, []Source{{Name: "lnd"}}, nil, Config{}, nil, nil); err == nil {
		t.Error("a source with no client was accepted")
	}
	if _, err := New(st, b, []Source{{Client: &fakeNode{}}}, nil, Config{}, nil, nil); err == nil {
		t.Error("a source with no name was accepted")
	}
	// No Lightning node at all is a supported configuration: split detection is
	// useful on its own, and the user may not have connected one yet.
	if _, err := New(st, b, nil, nil, Config{}, nil, nil); err != nil {
		t.Errorf("a registry with no Lightning node was refused: %v", err)
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	t.Parallel()

	got := Config{}.withDefaults()
	if got.PollInterval != DefaultPollInterval ||
		got.BackfillInterval != DefaultBackfillInterval ||
		got.SnapshotTimeout != DefaultSnapshotTimeout {
		t.Errorf("got %+v", got)
	}

	set := Config{PollInterval: time.Second, BackfillInterval: 2 * time.Second,
		SnapshotTimeout: 3 * time.Second}
	if set.withDefaults() != set {
		t.Error("explicit settings were overwritten by the defaults")
	}
}

// Shutdown must be prompt and must not leave a goroutine behind.
func TestItStopsWhenAsked(t *testing.T) {
	t.Parallel()

	node := &pushingNode{
		fakeNode: fakeNode{snap: snapshotWith(record(fundingA, nil))},
		watching: make(chan struct{}),
	}
	chain := &fakeChain{err: errors.New("pruned")}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.reg.Run(ctx) }()

	h.waitFor("the first poll", func() bool { return node.pollCount() >= 1 })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutting down reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registry did not stop when its context ended")
	}
}

// A funding script the adapter already knows is used as it stands. Nothing does
// this today — neither node reports one — but the field exists on the record and
// a future source could fill it, so the engine must not ignore what it is given.
func TestAScriptFromTheAdapterIsKept(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, func(r *ChannelRecord) {
		r.FundingScriptHex = "0020beef"
	}))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("the script from the adapter", func() bool {
		c, ok := h.find(fundingA)
		return ok && c.FundingScriptHex == "0020beef"
	})
}

// Shutdown closes the store while the engine is still running, and every one of
// these paths hits a database that is no longer there. None of them may panic,
// and the daemon must still stop cleanly — the M1 lesson about a store that
// nils out its own handle, kept honest here too.
func TestTheStoreClosingUnderneathIsSurvived(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{err: errors.New("pruned")}
	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.reg.Run(ctx) }()

	h.waitFor("the channel to be stored", func() bool {
		_, ok := h.find(fundingA)
		return ok
	})

	if err := h.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	// Everything the engine can be asked to do, against a store that has gone.
	h.bus.Publish(bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"})
	h.reg.Refresh()

	before := node.pollCount()
	deadline := time.Now().Add(2 * time.Second)
	for node.pollCount() < before+3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond) //nolint:forbidigo // waiting on a real goroutine
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutting down reported an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the registry did not stop after its store went away")
	}
}

// With no chain backend at all the backfill has nothing to ask, and must say so
// rather than reporting a success with an empty script.
func TestWithNoChainBackendTheLookupFailsHonestly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, nil)
	_, err := h.reg.findScript(context.Background(), store.Channel{OpenHeight: 900})
	if err == nil {
		t.Fatal("a lookup with no backend to ask reported success")
	}
}

// Nothing is looked up for a channel whose funding height nobody knows: there is
// no block to read, and asking for height zero would be asking the wrong
// question rather than getting no answer.
func TestAChannelWithNoKnownHeightIsNotLookedUp(t *testing.T) {
	t.Parallel()

	chain := &fakeChain{blocks: map[int32]*wire.MsgBlock{}}
	node := &fakeNode{snap: snapshotWith(record(fundingA, func(r *ChannelRecord) {
		r.OpenHeight = 0
	}))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, []BlockSource{chain})
	h.run()

	h.waitFor("the channel to be stored", func() bool {
		_, ok := h.find(fundingA)
		return ok
	})
	before := node.pollCount()
	h.waitFor("several backfill passes", func() bool { return node.pollCount() >= before+5 })

	if chain.callCount() != 0 {
		t.Errorf("the chain was asked about a channel with no known funding height (%d times)",
			chain.callCount())
	}
	// And it is watched, because not knowing is an instruction to keep looking.
	if got := h.channel(fundingA).Relevance; got != store.Relevant {
		t.Errorf("a channel with no known funding height was classified %q", got)
	}
}

// A Lightning node describes a channel that is closing with far fewer fields
// than one that is open. Taking that silence at face value cost the channel the
// delay that decides its deadline and the height that decides whether it is
// exposed — at exactly the moment it started closing, which is when those
// answers matter most.
func TestAThinnerPollDoesNotEraseWhatWasAlreadyKnown(t *testing.T) {
	t.Parallel()

	node := &fakeNode{snap: snapshotWith(record(fundingA, func(r *ChannelRecord) {
		r.CSVDelayLocal = ptr32(144)
		r.CSVDelayRemote = ptr32(2016)
		r.SCID = "961632x7x0"
		r.OpenHeight = 961_632
		r.CapacitySat = 2_100_000
		r.PeerAlias = "ACINQ"
	}))}
	h := newHarness(t, []Source{{Name: "lnd", Client: node}}, nil)
	h.run()

	h.waitFor("the channel to be recorded in full", func() bool {
		c, ok := h.find(fundingA)
		return ok && c.OpenHeight == 961_632 && c.CSVDelayRemote != nil
	})

	// The channel starts closing, and the node now says much less about it.
	node.set(func(s *Snapshot, _ *error) {
		s.Channels[0] = ChannelRecord{
			FundingTxID: fundingA,
			FundingVout: 0,
			ChanType:    store.ChanTypeUnknown,
			PeerPubkey:  "03peer",
			CloseState:  store.ClosePending,
			CloseTxID:   "closing",
		}
	})

	h.waitFor("the close to be recorded", func() bool {
		c, ok := h.find(fundingA)
		return ok && c.CloseState == store.ClosePending
	})

	got := h.channel(fundingA)
	if got.OpenHeight != 961_632 {
		t.Errorf("the funding height was erased when the channel started closing: %d",
			got.OpenHeight)
	}
	if got.SCID != "961632x7x0" {
		t.Errorf("the short channel id was erased: %q", got.SCID)
	}
	if got.CapacitySat != 2_100_000 {
		t.Errorf("the capacity was erased: %d", got.CapacitySat)
	}
	if got.PeerAlias != "ACINQ" {
		t.Errorf("the counterparty's name was erased: %q", got.PeerAlias)
	}
	if got.CSVDelayRemote == nil || *got.CSVDelayRemote != 2016 {
		t.Errorf("the delay that decides the deadline was erased: %v", got.CSVDelayRemote)
	}
	if got.ChanType != store.ChanAnchors {
		t.Errorf("a channel that was recognised became unrecognisable: %q", got.ChanType)
	}
}

// A value the node does say always wins: a node revising something it can see
// knows better than a stored copy.
func TestAValueThatIsActuallyThereWins(t *testing.T) {
	t.Parallel()

	prior := store.Channel{
		CapacitySat: 1000, OpenHeight: 100, SCID: "old", PeerAlias: "old",
		CSVDelayLocal: ptr32(10), CSVDelayRemote: ptr32(20),
		ChanType: store.ChanAnchors,
	}
	got := merge(prior, ChannelRecord{
		FundingTxID: fundingA, CapacitySat: 2000, OpenHeight: 200, SCID: "new",
		PeerAlias: "new", CSVDelayLocal: ptr32(30), CSVDelayRemote: ptr32(40),
		ChanType: store.ChanTaproot,
	})

	if got.CapacitySat != 2000 || got.OpenHeight != 200 || got.SCID != "new" ||
		got.PeerAlias != "new" || got.ChanType != store.ChanTaproot {
		t.Errorf("a stored copy overrode what the node said: %+v", got)
	}
	if *got.CSVDelayLocal != 30 || *got.CSVDelayRemote != 40 {
		t.Errorf("stored delays overrode reported ones: %v %v",
			got.CSVDelayLocal, got.CSVDelayRemote)
	}
}

// A delay of zero is not a delay. It would put a deadline in the block the
// commitment confirmed in, which reads as already lost.
func TestAZeroDelayIsNotTreatedAsAnAnswer(t *testing.T) {
	t.Parallel()

	got := merge(
		store.Channel{CSVDelayRemote: ptr32(2016)},
		ChannelRecord{FundingTxID: fundingA, CSVDelayRemote: ptr32(0)},
	)
	if got.CSVDelayRemote == nil || *got.CSVDelayRemote != 2016 {
		t.Errorf("a zero delay replaced a real one: %v", got.CSVDelayRemote)
	}

	// And when nobody has ever said, it stays unsaid rather than becoming zero.
	none := merge(store.Channel{}, ChannelRecord{FundingTxID: fundingA})
	if none.CSVDelayRemote != nil {
		t.Errorf("a delay nobody reported became %v", *none.CSVDelayRemote)
	}
}

func ptr32(v int32) *int32 { return &v }

// A node reports its name before it has ever been read.
//
// **Found sweeping for the "not yet" mistake.** Health builds one entry per
// configured node from a map that is empty until the first poll finishes, and
// the zero value carried no name — so a caller listing the nodes it could not
// see wrote a sentence with a hole where the name belonged. The dashboard did
// exactly that, on every fresh start.
func TestAnUnpolledNodeStillReportsItsName(t *testing.T) {
	t.Parallel()
	node := &fakeNode{snap: snapshotWith(record(fundingA, nil))}
	h := newHarness(t, []Source{{Name: "lnd-1", Client: node}}, nil)

	// Deliberately not run: this is the state before the first poll returns.
	got := h.reg.Health()
	if len(got) == 0 {
		t.Fatal("a configured node reported no health entry at all")
	}
	for _, hh := range got {
		if hh.Name == "" {
			t.Error("a node that has not been polled yet reports no name, so " +
				"anything naming it has a blank where the name should be")
		}
		if hh.LastSuccessAt != 0 {
			t.Error("an unpolled node claims to have been read successfully")
		}
	}
}
