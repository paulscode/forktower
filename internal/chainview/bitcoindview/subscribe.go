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
var _ chainview.ChainView = (*View)(nil)

// Subscription tuning.
const (
	// subscriberBuffer is how many events are held for a consumer that has fallen
	// behind. Blocks arrive minutes apart, so a consumer that fills this is stuck
	// rather than busy — and the point is to make that visible, not to absorb it.
	subscriberBuffer = 64

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
}

func (s *stallState) note() {
	s.stalled.Store(true)
	s.dropped.Add(1)
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
	out := make(chan *wire.MsgTx, subscriberBuffer)
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
		v.log().Error("tip consumer is not keeping up; dropped a notification",
			slog.String("hash", meta.Hash.String()),
			slog.Int("height", int(meta.Height)),
			slog.Int64("dropped_total", v.stall.dropped.Load()))
	}
}

func (v *View) emitTx(out chan<- *wire.MsgTx, tx *wire.MsgTx) {
	select {
	case out <- tx:
	default:
		v.stall.note()
		v.log().Error("mempool consumer is not keeping up; dropped a transaction",
			slog.String("txid", tx.TxHash().String()),
			slog.Int64("dropped_total", v.stall.dropped.Load()))
	}
}

// log returns the configured logger, or one that discards.
func (v *View) log() *slog.Logger {
	if v.c.opts.Logger != nil {
		return v.c.opts.Logger
	}
	return slog.New(discardHandler{})
}
