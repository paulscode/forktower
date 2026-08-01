package bitcoindview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/chainview"
)

// mutableTip lets a test move the node's tip between polls.
type mutableTip struct {
	mu     sync.Mutex
	hash   string
	prev   string
	height int32
}

func (m *mutableTip) set(hash, prev string, height int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hash, m.prev, m.height = hash, prev, height
}

func (m *mutableTip) get() (hash, prev string, height int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hash, m.prev, m.height
}

// pollingNode is a fake node whose tip a test can move.
func pollingNode(t *testing.T, tip *mutableTip) *fakeNode {
	t.Helper()
	node := newFakeNode(t)
	node.on("getbestblockhash", func([]json.RawMessage) (any, *rpcError) {
		h, _, _ := tip.get()
		return h, nil
	})
	node.on("getblockheader", func(p []json.RawMessage) (any, *rpcError) {
		h, prev, height := tip.get()
		var asked string
		_ = json.Unmarshal(p[0], &asked)
		if asked != h {
			return nil, &rpcError{Code: codeInvalidAddressOrKey, Message: "Block not found"}
		}
		return headerFixture(h, prev, height, 1790000000), nil
	})
	return node
}

func hashOf(n int) string { return strings.Repeat("0", 63) + string(rune('0'+n)) }

// A subscriber should learn where the chain already is without waiting a whole
// interval — otherwise a freshly started daemon looks stalled for five seconds.
func TestSubscribeTipEmitsTheCurrentTipImmediately(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	v := newFastPollView(t, pollingNode(t, tip))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-ch:
		if got.Height != 100 {
			t.Errorf("height = %d, want 100", got.Height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no tip delivered on subscribing; a consumer would sit blind until the next poll")
	}
}

// Detection is by hash, not height. A reorganisation replaces the tip without
// advancing it — sometimes lowering it — and a height comparison would miss the
// one event that matters most.
func TestPollingDetectsAReorgAtTheSameHeight(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	v := newFastPollView(t, pollingNode(t, tip))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := recvTip(t, ch)
	if first.Hash.String() != hashOf(1) {
		t.Fatalf("first tip = %s", first.Hash)
	}

	// Same height, different block: the shape of a reorganisation.
	tip.set(hashOf(2), hashOf(0), 100)

	second := recvTipWithin(t, ch, 3*testPollInterval, "reorg at the same height")
	if second.Hash.String() != hashOf(2) {
		t.Errorf("second tip = %s, want %s", second.Hash, hashOf(2))
	}
	if second.Height != 100 {
		t.Errorf("height = %d, want 100 — the tip changed without advancing", second.Height)
	}
}

func TestPollingDoesNotRepeatAnUnchangedTip(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	v := newFastPollView(t, pollingNode(t, tip))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recvTip(t, ch)

	// Two intervals with nothing changing should produce nothing: a consumer that
	// had to filter duplicates would end up reimplementing this comparison.
	select {
	case got := <-ch:
		t.Errorf("an unchanged tip was re-delivered: %s at %d", got.Hash, got.Height)
	case <-time.After(2*testPollInterval + time.Second):
	}
}

// A subscription must survive a node that is briefly unreachable. Closing the
// channel is how a consumer learns to stop, so a hiccup must not look like a
// shutdown.
func TestPollingSurvivesANodeOutage(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	node := pollingNode(t, tip)
	v := newFastPollView(t, node)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recvTip(t, ch)

	// The node starts failing every request, for long enough to span several polls.
	node.fail("getbestblockhash", -28, "Loading block index...")
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("the subscription closed when the node became unreachable; a consumer " +
				"cannot tell that from a shutdown")
		}
		t.Fatal("a tip arrived while the node was failing every request")
	case <-time.After(4 * testPollInterval):
	}

	// Recovery: the node answers again and the new tip arrives.
	node.on("getbestblockhash", func([]json.RawMessage) (any, *rpcError) {
		h, _, _ := tip.get()
		return h, nil
	})
	tip.set(hashOf(3), hashOf(1), 101)

	got := recvTipWithin(t, ch, 4*testPollInterval, "tip after the node recovered")
	if got.Height != 101 {
		t.Errorf("height = %d, want 101", got.Height)
	}
}

func TestSubscribeTipClosesWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	v := newFastPollView(t, pollingNode(t, tip))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recvTip(t, ch)
	cancel()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed, as it should be
			}
		case <-deadline:
			t.Fatal("cancelling the context did not close the subscription")
		}
	}
}

// A consumer that stops reading is stuck, not busy. Dropping is the right
// response — blocking here would stall the notification reader and turn one slow
// consumer into a blind daemon — but it must be reported, because a dropped
// notification and a quiet chain look identical from outside.
func TestAStalledConsumerIsReportedInHealth(t *testing.T) {
	t.Parallel()

	tip := &mutableTip{}
	tip.set(hashOf(1), hashOf(0), 100)
	node := pollingNode(t, tip)
	node.reply("getblockchaininfo", map[string]any{
		"chain": "main", "blocks": 100, "headers": 100,
		"bestblockhash": hashOf(1), "verificationprogress": 1.0,
	})
	node.reply("getnetworkinfo", map[string]any{"connections": 8})
	v := newFastPollView(t, node)

	before, err := v.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.State != chainview.HealthOK {
		t.Fatalf("health started at %q, want OK", before.State)
	}

	// Fill the buffer and one more, without ever reading.
	out := make(chan chainview.BlockMeta, subscriberBuffer)
	for i := range subscriberBuffer + 1 {
		v.emitTip(out, chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Height: int32(i)},
		})
	}

	after, err := v.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.State != chainview.HealthDegraded {
		t.Errorf("health = %q, want DEGRADED after dropping a notification", after.State)
	}
	if !strings.Contains(after.Detail, "keeping up") {
		t.Errorf("detail = %q, want it to say a consumer fell behind", after.Detail)
	}
}

// There is no way to poll for unconfirmed transactions, so saying so is better
// than pretending. The cost is early warning, not detection.
func TestSubscribeMempoolTxUnsupportedWithoutNotifications(t *testing.T) {
	t.Parallel()

	_, err := newTestView(t, newFakeNode(t)).SubscribeMempoolTx(context.Background())
	if !errors.Is(err, chainview.ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported when the node publishes nothing", err)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	d := reconnectMin
	seen := []time.Duration{d}
	for range 12 {
		d = nextBackoff(d)
		seen = append(seen, d)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("backoff went backwards: %v then %v", seen[i-1], seen[i])
		}
		if seen[i] > reconnectMax {
			t.Fatalf("backoff %v exceeded the ceiling %v", seen[i], reconnectMax)
		}
	}
	if seen[len(seen)-1] != reconnectMax {
		t.Errorf("backoff settled at %v, want the ceiling %v", seen[len(seen)-1], reconnectMax)
	}
}

func TestSleepCtxReturnsFalseOnCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx reported a completed wait on a cancelled context")
	}
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Error("sleepCtx reported an interrupted wait when it completed")
	}
}

func TestZMQEndpointValidation(t *testing.T) {
	t.Parallel()

	base := Options{RPCURL: "http://h:1", User: "u", Pass: "p"}
	for _, bad := range []string{"127.0.0.1:28332", "http://h:1", "nonsense"} {
		o := base
		o.ZMQRawBlock = bad
		if err := o.Validate(); err == nil {
			t.Errorf("accepted %q as a notification endpoint", bad)
		}
	}
	o := base
	o.ZMQRawBlock = "tcp://127.0.0.1:28332"
	o.ZMQRawTx = "tcp://127.0.0.1:28333"
	if err := o.Validate(); err != nil {
		t.Errorf("rejected usable endpoints: %v", err)
	}
}

// recvTip waits for the tip a subscription delivers on subscribing.
func recvTip(t *testing.T, ch <-chan chainview.BlockMeta) chainview.BlockMeta {
	t.Helper()
	return recvTipWithin(t, ch, 5*time.Second, "the tip on subscribing")
}

func recvTipWithin(t *testing.T, ch <-chan chainview.BlockMeta, d time.Duration, what string) chainview.BlockMeta {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("subscription closed while waiting for %s", what)
		}
		return got
	case <-time.After(d):
		t.Fatalf("timed out after %v waiting for %s", d, what)
		return chainview.BlockMeta{}
	}
}

// testPollInterval keeps the polling tests quick. Real deployments use the
// default; a test that waits real seconds is slow now and flaky later.
const testPollInterval = 40 * time.Millisecond

// newFastPollView is newTestView with a short polling interval.
func newFastPollView(t *testing.T, node *fakeNode) *View {
	t.Helper()
	srv := httptest.NewServer(node)
	t.Cleanup(srv.Close)

	v, err := New(Options{
		RPCURL:       srv.URL,
		User:         "u",
		Pass:         "p",
		PollInterval: testPollInterval,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// A stalled consumer is worth one line, not one line per dropped item.
//
// The first run against a real node during its initial sync produced 577,371
// error lines about dropped transactions — a node replaying its memory pool
// faster than anything downstream could read it. That is not a report of a
// problem; it is the destruction of the log that would have shown one, at the
// moment Forktower exists to be read.
func TestAStalledConsumerIsReportedOnceNotPerDrop(t *testing.T) {
	t.Parallel()

	clock := time.Unix(1_790_000_000, 0)
	var s stallState

	said := 0
	for range 100_000 {
		s.note()
		if say, _ := s.shouldSay(clock); say {
			said++
		}
	}
	if said != 1 {
		t.Errorf("100,000 drops in one instant produced %d log lines, want 1", said)
	}

	// The condition still has to be visible while it continues: a consumer that
	// is stuck after the chain is caught up is a daemon that has stopped
	// watching, and silence there would be the worse failure.
	clock = clock.Add(stallReportInterval + time.Second)
	s.note()
	say, since := s.shouldSay(clock)
	if !say {
		t.Fatal("a stall that is still going was never mentioned again")
	}
	// And it says what was lost in between, not only the running total.
	if since != 100_000 {
		t.Errorf("reported %d dropped since the last line, want 100000", since)
	}
	if total := s.dropped.Load(); total != 100_001 {
		t.Errorf("dropped total = %d, want 100001", total)
	}
}
