package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// RetryInterval is how often the queue of allowed transactions is worked
// through.
//
// The backoff decides when any one transaction is next tried; this only decides
// how often that question is asked. Half a minute is fine either way, and keeps
// the two numbers independent.
const RetryInterval = 30 * time.Second

// SubscriberName identifies this engine on the bus and in logs.
const SubscriberName = "mirror"

// Runner drives one direction of the mirror: watching one chain for the user's
// transactions, and offering the allowed ones to the other.
//
// Two of these run, one each way, because the rules differ sharply between the
// directions and a single engine serving both would need a flag at every
// decision.
type Runner struct {
	observer *Observer
	mirror   *Mirror
	bus      *bus.Bus
	log      *slog.Logger
	from     store.Branch
	events   <-chan bus.Event
	interval time.Duration
}

// RunnerOptions configures a Runner.
type RunnerOptions struct {
	Observer *Observer
	Mirror   *Mirror
	Bus      *bus.Bus
	Log      *slog.Logger
	// From is the chain being watched. Blocks on the other one are not this
	// runner's business.
	From     store.Branch
	Interval time.Duration
}

// NewRunner builds a Runner.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Observer == nil || opts.Mirror == nil {
		return nil, errors.New("mirror: a runner needs something to watch with and " +
			"something to send with")
	}
	if opts.Bus == nil {
		return nil, errors.New("mirror: a runner needs a bus")
	}
	if !opts.From.Valid() {
		return nil, fmt.Errorf("mirror: %q is not a chain", opts.From)
	}
	if opts.Log == nil {
		opts.Log = slog.New(discardHandler{})
	}
	if opts.Interval <= 0 {
		opts.Interval = RetryInterval
	}
	return &Runner{
		observer: opts.Observer, mirror: opts.Mirror, bus: opts.Bus,
		log: opts.Log, from: opts.From, interval: opts.Interval,
		events: opts.Bus.Subscribe(
			SubscriberName+":"+string(opts.From), bus.KindSplitBranchExtended),
	}, nil
}

// Run watches until the context is cancelled.
//
// Two things on two schedules. New blocks are read as they arrive, because a
// transaction that ought to be on the other chain should get there promptly. The
// queue is worked through on a timer, because whether the other chain will now
// accept something depends on that chain rather than on this one.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-r.events:
			if !ok {
				return nil
			}
			extended, isBlock := ev.(bus.SplitBranchExtended)
			if !isBlock || store.Branch(extended.Branch) != r.from {
				continue
			}
			r.scan(ctx, extended.Block.Height)

		case <-ticker.C:
			r.send(ctx)
		}
	}
}

// scan looks at one block for the user's transactions.
func (r *Runner) scan(ctx context.Context, height int32) {
	found, err := r.observer.ScanBlock(ctx, height, time.Now().Unix())
	if err != nil {
		r.log.Error("looking for your transactions to copy",
			slog.Int64("height", int64(height)), slog.String("error", err.Error()))
		return
	}
	for _, f := range found {
		if !f.New {
			continue
		}
		// Logged at debug for the refusals and info for the rest. Most of what
		// this sees it declines, and a log that announced every refusal at info
		// would drown the transactions that are actually going somewhere — but the
		// refusals still go in the database, which is where the user reads them.
		if f.Decision.State == store.MirrorDenied {
			r.log.Debug("not copying a transaction",
				slog.String("txid", f.Lifted.TxID),
				slog.String("why", f.Decision.Reason))
			continue
		}
		r.log.Info("a transaction of yours will be copied to the other chain",
			slog.String("txid", f.Lifted.TxID),
			slog.String("why", f.Decision.Reason))
	}

	// Anything new is worth offering straight away rather than waiting for the
	// timer: a close the user is waiting on should not sit for half a minute.
	if len(found) > 0 {
		r.send(ctx)
	}
}

// send works through the queue of allowed transactions.
func (r *Runner) send(ctx context.Context) {
	outcomes, err := r.mirror.Pass(ctx)
	if err != nil {
		r.log.Error("copying your transactions to the other chain",
			slog.String("error", err.Error()))
		return
	}
	for _, o := range outcomes {
		switch o.State {
		case store.MirrorAccepted:
			r.log.Info("a transaction of yours reached the other chain",
				slog.String("txid", o.TxID))
		case store.MirrorAbandoned:
			r.log.Warn("giving up on copying a transaction to the other chain",
				slog.String("txid", o.TxID), slog.String("why", o.Note))
		case store.MirrorRejected:
			r.log.Info("the other chain has not taken a transaction yet",
				slog.String("txid", o.TxID), slog.String("why", o.Note))
		case store.MirrorDenied, store.MirrorPending:
			// Not outcomes of an attempt.
		}
	}
}
