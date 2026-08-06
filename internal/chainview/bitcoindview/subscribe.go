package bitcoindview

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// Now that the subscription methods exist, the whole contract can be asserted.
// Compile-time proof that this backend satisfies every contract the daemon looks
// for.
//
// The optional ones matter more than they look. They are discovered with a type
// assertion at runtime, so a backend that stops satisfying one does not fail to
// build — it quietly loses a capability, and the check that depended on it
// reports itself as unavailable forever. That is how the distinct-node check —
// the worst silent failure in this design — came to have never once run against
// a real node.
var (
	_ chainview.ChainView    = (*View)(nil)
	_ chainview.Identifiable = (*View)(nil)
	_ chainview.ChainTipper  = (*View)(nil)
	_ chainview.Deployer     = (*View)(nil)
)

// Subscription tuning.
const (
	// subscriberBuffer is how many block notifications are held for a consumer
	// that has fallen behind. Blocks arrive minutes apart, so a consumer that
	// fills this is stuck rather than busy — and the point is to make that
	// visible, not to absorb it.
	subscriberBuffer = 64
	// mempoolBuffer is the same for unconfirmed transactions, and is far larger
	// because the reasoning above is about blocks and does not transfer.
	//
	// **A node at the tip of mainnet relays transactions by the dozen per
	// second.** Sharing the block buffer meant a healthy consumer overflowed it
	// continuously the moment a second node finished syncing: seven hundred
	// dropped in four minutes on the first machine to get there, climbing. That
	// is a busy consumer, not a stuck one, and the two want opposite treatment —
	// a block backlog should be reported, a transaction backlog absorbed.
	//
	// Large enough to ride out a sweep slice or a slow block, and still bounded:
	// this is early warning, and a consumer that falls behind by thousands has
	// lost the earliness that made it worth having.
	mempoolBuffer = 4096

	// reconnectMin and reconnectMax bound the backoff after a notification socket
	// fails. It starts fast because a node restart is brief and routine, and caps
	// low enough that recovery from a long outage is prompt rather than
	// apologetic.
	reconnectMin = 1 * time.Second
	reconnectMax = 60 * time.Second

	// zmqReceiveTimeout bounds a single read from the notification socket. Reads
	// time out and loop so that a cancelled context is noticed promptly rather
	// than only when the next block arrives.
	zmqReceiveTimeout = 5 * time.Second
)

// stallState records that a consumer stopped keeping up, so Health can report it.
//
// Surfaced rather than swallowed: a subscription that is silently dropping
// notifications looks exactly like a quiet chain, and telling those apart is the
// whole job.
type stallState struct {
	stalled atomic.Bool
	dropped atomic.Int64
	// lastSaid is when the condition was last logged, as a Unix nanosecond
	// count so it can be compared and swapped without a lock.
	lastSaid atomic.Int64
	// saidAt is the running total at that moment, so the next line can say how
	// many were lost in between rather than only how many in total.
	saidAt atomic.Int64
}

func (s *stallState) note() {
	s.stalled.Store(true)
	s.dropped.Add(1)
}

// stallReportInterval is how often a continuing stall is worth repeating.
//
// Chosen to be short enough that a stall during a split is visible within a
// block's worth of time, and long enough that it cannot be the reason a log
// becomes unreadable.
const stallReportInterval = 30 * time.Second

// shouldSay reports whether this drop is the one to log, and how many have been
// lost since the last time it was.
//
// **A dropped notification is worth one line, not one line each.** During an
// initial sync the node replays its memory pool faster than anything downstream
// can consume it, and the first run of this on real hardware produced 577,371
// error lines — which is not a report of a problem, it is the destruction of the
// log that would have shown one. The condition still has to be visible, because
// a consumer that is genuinely stuck after the chain is caught up is a daemon
// that has stopped watching; so it is said at once, then at intervals, with the
// count that accumulated in between.
func (s *stallState) shouldSay(now time.Time) (say bool, since int64) {
	total := s.dropped.Load()
	last := s.lastSaid.Load()
	if last != 0 && now.UnixNano()-last < int64(stallReportInterval) {
		return false, 0
	}
	if !s.lastSaid.CompareAndSwap(last, now.UnixNano()) {
		// Somebody else is reporting this round; one line is the point.
		return false, 0
	}
	previous := s.saidAt.Swap(total)
	return true, total - previous
}

// SubscribeTip delivers the node's tip whenever it changes.
//
// Uses the node's notification socket when one is configured and polls otherwise.
// Both paths report a change the same way — by hash, not by height — because a
// reorganisation replaces the tip without advancing it, and a height comparison
// would miss exactly the event that matters most.
func (v *View) SubscribeTip(ctx context.Context) (<-chan chainview.BlockMeta, error) {
	out := make(chan chainview.BlockMeta, subscriberBuffer)
	go v.runTip(ctx, out)
	return out, nil
}

// tipTracker serialises tip updates arriving from more than one source and
// suppresses repeats.
type tipTracker struct {
	mu   sync.Mutex
	last chainview.BlockMeta
}

// update reads the node's current tip and emits it if it has changed.
//
// Compared by hash, not height: a reorganisation replaces the tip without
// advancing it — sometimes lowering it — and a height check would silently miss
// precisely the event that matters most.
func (v *View) update(ctx context.Context, out chan<- chainview.BlockMeta, t *tipTracker) {
	tip, err := v.BestBlock(ctx)
	if err != nil {
		// Assumed transient: the node may be restarting. Health reports the outage;
		// this keeps trying rather than giving up, because closing the channel is how
		// a consumer learns to stop and a hiccup must not look like a shutdown.
		if ctx.Err() == nil {
			v.log().Debug("reading the tip failed", slog.String("error", firstLine(err.Error())))
		}
		return
	}

	t.mu.Lock()
	changed := tip.Hash != t.last.Hash
	if changed {
		t.last = tip
	}
	t.mu.Unlock()

	if changed {
		v.emitTip(out, tip)
	}
}

// runTip keeps a consumer supplied with the node's tip.
//
// A timer always runs, even when the node publishes notifications. That is not
// redundancy for its own sake: a publish socket can stop delivering without ever
// reporting an error — a restarted node leaves a connection that reads as merely
// idle — and a subscription that trusted it alone would go quietly blind, which is
// the exact failure this whole project exists to catch. With the timer present, a
// dead socket costs latency instead of sight.
func (v *View) runTip(ctx context.Context, out chan<- chainview.BlockMeta) {
	defer close(out)

	tracker := &tipTracker{}

	// Report where the chain already is, so a consumer does not wait an interval to
	// learn anything.
	v.update(ctx, out, tracker)

	var wg sync.WaitGroup

	if v.c.opts.ZMQRawBlock != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The socket is a doorbell, not a source of truth: on any notification the
			// tip is read back over RPC. A block announcement carries the block, and a
			// header does not contain its own height, so the announcement alone cannot
			// produce what a consumer needs — and reading back gives the right answer
			// during a reorganisation too, when the block just announced may already
			// not be the tip.
			v.runZMQLoop(ctx, v.c.opts.ZMQRawBlock, topicRawBlock, func([][]byte) {
				v.update(ctx, out, tracker)
			})
		}()
	}

	ticker := time.NewTicker(v.c.opts.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			v.update(ctx, out, tracker)
		}
	}
}

// SubscribeMempoolTx streams unconfirmed transactions.
//
// Returns ErrUnsupported when the node publishes none: there is no way to poll
// for this, and pretending otherwise would be worse than saying so. Callers lose
// early warning, not detection.
func (v *View) SubscribeMempoolTx(ctx context.Context) (<-chan *wire.MsgTx, error) {
	if v.c.opts.ZMQRawTx == "" {
		return nil, chainview.ErrUnsupported
	}
	out := make(chan *wire.MsgTx, mempoolBuffer)
	go v.runMempoolZMQ(ctx, out)
	return out, nil
}

// emitTip offers a tip to the consumer without blocking.
//
// A full buffer means the consumer is stuck. The notification is dropped and
// recorded, because blocking here would stall the reader of the notification
// socket and turn one slow consumer into a blind daemon.
func (v *View) emitTip(out chan<- chainview.BlockMeta, meta chainview.BlockMeta) {
	select {
	case out <- meta:
	default:
		v.stall.note()
		if say, since := v.stall.shouldSay(v.now()); say {
			v.log().Error("tip consumer is not keeping up; dropping notifications",
				slog.String("latest_hash", meta.Hash.String()),
				slog.Int("latest_height", int(meta.Height)),
				slog.Int64("dropped_since_last_report", since),
				slog.Int64("dropped_total", v.stall.dropped.Load()))
		}
	}
}

// emitTx offers an unconfirmed transaction to the consumer without blocking.
//
// **Counted separately from block notifications, and deliberately not part of
// the view's health.** Missing a transaction before it confirms costs early
// warning; missing a block means not seeing the chain. Reporting the first as
// the second put "Having trouble seeing the other chain — Forktower may not see
// everything happening there" permanently in front of every user whose second
// node had finished syncing, which is the healthy state.
//
// Still logged, because a consumer that cannot keep up is worth knowing about,
// and still at warning rather than error: it is a reduction in lead time, not a
// failure to watch.
func (v *View) emitTx(out chan<- *wire.MsgTx, tx *wire.MsgTx) {
	select {
	case out <- tx:
	default:
		v.mempoolStall.note()
		if say, since := v.mempoolStall.shouldSay(v.now()); say {
			v.log().Warn("falling behind on unconfirmed transactions, so a spend "+
				"may not be seen until it confirms",
				slog.String("latest_txid", tx.TxHash().String()),
				slog.Int64("dropped_since_last_report", since),
				slog.Int64("dropped_total", v.mempoolStall.dropped.Load()))
		}
	}
}

// log returns the configured logger, or one that discards.
func (v *View) log() *slog.Logger {
	if v.c.opts.Logger != nil {
		return v.c.opts.Logger
	}
	return slog.New(discardHandler{})
}
