// Package deadline turns a confirmed commitment into a countdown, and a
// countdown into escalating warnings and, if nobody acts, a recorded loss.
//
// It is the part of Forktower that answers "how long have I got", which is the
// only question a user in trouble actually has.
package deadline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// SubscriberName identifies this engine's bus subscription in drop diagnostics.
const SubscriberName = "deadline"

const (
	// ReorgPatienceBlocks is how long a spend that left the chain is given to
	// come back before its countdown is retired.
	//
	// Generous, because a spend disappearing is not the same as a spend that
	// never happened: a counterparty replacing a breach with a higher fee looks
	// exactly like this, and the replacement lands within a block or two. A
	// hundred blocks of patience costs nothing and stopping too early would mean
	// dropping a clock that was about to matter again.
	ReorgPatienceBlocks = 100
	// writeTimeout bounds a storage write that outlives its trigger.
	writeTimeout = 5 * time.Second
)

// Status is what the dashboard needs to know about the clocks.
type Status struct {
	// Counting is how many deadlines are running.
	Counting int
	// Assumed is how many of those had to use a floor because an input was
	// missing. Surfaced *before* anything goes wrong, which is the only time it
	// can still be fixed.
	Assumed int
	// AssumedChannels names them, so the readiness check can say which.
	AssumedChannels []int64
	// EarliestHeight is the soonest deadline still counting, or zero.
	EarliestHeight int32
	// TipHeight is where the watched chain had got to when this was worked out.
	TipHeight int32
}

// InputsKnown reports whether every running countdown is built on real numbers.
func (s Status) InputsKnown() bool { return s.Assumed == 0 }

// Engine keeps the countdowns.
//
// Single-goroutine, like the watcher and for the same reason: it is the only
// writer of the deadline rows, and the escalation state depends on having seen
// every block once and in order.
type Engine struct {
	store  *store.Store
	bus    *bus.Bus
	branch store.Branch
	now    func() time.Time
	log    *slog.Logger

	// events is subscribed at construction rather than when Run starts, because
	// the event that starts a countdown is the one that must never be missed.
	events <-chan bus.Event

	mu          sync.Mutex
	tipHeight   int32
	avgInterval float64
	status      Status
}

// New builds the engine. A nil logger discards and a nil clock reads the real
// one.
func New(
	st *store.Store,
	b *bus.Bus,
	branch store.Branch,
	log *slog.Logger,
	now func() time.Time,
) (*Engine, error) {
	if st == nil {
		return nil, errors.New("deadline: a store is required")
	}
	if b == nil {
		return nil, errors.New("deadline: an event bus is required")
	}
	if !branch.Valid() {
		return nil, fmt.Errorf("deadline: %q is not a branch", branch)
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	return &Engine{
		store:  st,
		bus:    b,
		branch: branch,
		now:    now,
		log:    log,
		events: b.Subscribe(SubscriberName,
			bus.KindFundingSpent, bus.KindSecondOrderSpent, bus.KindSpendReorgedOut,
			bus.KindSplitBranchExtended),
	}, nil
}

// Status reports the countdowns, for the dashboard and its readiness check.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Run keeps the countdowns current until the context ends.
func (e *Engine) Run(ctx context.Context) error {
	e.refreshStatus(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-e.events:
			if !ok {
				return nil
			}
			e.handle(ctx, ev)
		}
	}
}

func (e *Engine) handle(ctx context.Context, ev bus.Event) {
	switch v := ev.(type) {
	case bus.FundingSpent:
		e.onFundingSpent(ctx, v)
	case bus.SplitBranchExtended:
		e.onBranchExtended(ctx, v)
	case bus.SecondOrderSpent:
		e.onSecondOrderSpent(ctx, v)
	case bus.SpendReorgedOut:
		e.onSpendReorgedOut(v)
	}
}

// onFundingSpent starts the clocks a confirmed commitment sets running.
func (e *Engine) onFundingSpent(ctx context.Context, ev bus.FundingSpent) {
	if store.Branch(ev.Branch) != e.branch {
		// The user's own chain is their node's business. This engine counts the
		// chain nobody else is watching.
		return
	}
	if store.SpendStatus(ev.Status) != store.SpendConfirmed {
		// An unconfirmed sighting is early warning, not a clock. The delay it
		// would start is measured from a block it is not in yet.
		return
	}
	if !startsAClock(store.SpendShape(ev.Shape)) {
		return
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	spend, err := e.store.GetSpend(wctx, ev.SpendEventID)
	if err != nil {
		e.log.Error("could not read the close a countdown should start from",
			slog.Int64("spend_id", ev.SpendEventID), slog.String("error", err.Error()))
		return
	}

	inputs := Inputs{ConfirmHeight: spend.BlockHeight, Shape: spend.Shape}
	channel, found := e.channel(wctx, ev.ChannelID)
	if found {
		inputs.CSVDelayLocal = channel.CSVDelayLocal
		inputs.CSVDelayRemote = channel.CSVDelayRemote
		if htlcs, htlcErr := e.store.ListHTLCs(wctx, channel.ID); htlcErr == nil {
			inputs.HTLCs = htlcs
		}
	}

	for _, computed := range Compute(inputs) {
		e.start(wctx, ev, computed)
	}
	e.refreshStatus(ctx)
}

// startsAClock reports whether a spend of a funding output puts the user on a
// timer.
//
// A commitment does, whoever published it. A cooperative close does not: it pays
// both sides directly and there is nothing to wait for and nothing to contest.
func startsAClock(shape store.SpendShape) bool {
	switch shape {
	case store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked:
		return true
	case store.ShapeMutualClose, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown:
		return false
	default:
		return false
	}
}

// start records one countdown and says so immediately.
//
// The first escalation happens on detection rather than on some fraction of the
// window, because the moment a commitment confirms is when the user has the most
// time and therefore the most options. Waiting for a threshold before saying
// anything would spend the part of the window that was worth the most.
func (e *Engine) start(ctx context.Context, ev bus.FundingSpent, computed Computed) {
	id, changed, err := e.store.UpsertDeadline(ctx, store.Deadline{
		SpendEventID:   ev.SpendEventID,
		Kind:           computed.Kind,
		DeadlineHeight: computed.Height,
		State:          store.DeadlineCounting,
		Escalation:     LevelDetected,
		Assumed:        computed.Assumed,
		UpdatedAt:      e.now().Unix(),
	})
	if err != nil {
		e.log.Error("could not start a countdown",
			slog.Int64("spend_id", ev.SpendEventID), slog.String("error", err.Error()))
		return
	}
	if !changed {
		// Already running. Re-recording it on a re-scan is expected and is not
		// something to announce twice.
		return
	}

	if computed.Assumed {
		e.log.Warn("counting down on an assumed delay, because your Lightning node "+
			"did not say what the real one is — the countdown is a floor, so it may "+
			"be shorter than the truth",
			slog.Int64("channel_id", ev.ChannelID),
			slog.String("kind", string(computed.Kind)))
	}

	e.escalateTo(ctx, id, ev.ChannelID, computed.Height, LevelDetected)
}

// onBranchExtended is where the countdowns actually count.
func (e *Engine) onBranchExtended(ctx context.Context, ev bus.SplitBranchExtended) {
	if store.Branch(ev.Branch) != e.branch {
		return
	}

	e.mu.Lock()
	e.tipHeight = ev.Block.Height
	if ev.AvgIntervalSecs > 0 {
		e.avgInterval = ev.AvgIntervalSecs
	}
	e.mu.Unlock()

	e.advance(ctx)
	e.refreshStatus(ctx)
}

// advance re-reads every running countdown against the new tip.
func (e *Engine) advance(ctx context.Context) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	running, err := e.store.ListDeadlines(wctx, store.DeadlineCounting)
	if err != nil {
		e.log.Error("could not read the running countdowns",
			slog.String("error", err.Error()))
		return
	}

	e.mu.Lock()
	tip := e.tipHeight
	e.mu.Unlock()

	for _, d := range running {
		spend, spendErr := e.store.GetSpend(wctx, d.SpendEventID)
		if spendErr != nil {
			e.log.Warn("could not read the close behind a countdown",
				slog.Int64("deadline_id", d.ID), slog.String("error", spendErr.Error()))
			continue
		}

		// A close that left the chain stops counting once it has stayed away long
		// enough to believe it is not coming back.
		if spend.Status == store.SpendReorgedOut {
			if tip-spend.BlockHeight > ReorgPatienceBlocks {
				e.retire(wctx, d, spend)
			}
			continue
		}

		remaining := Remaining(d.DeadlineHeight, tip)
		if remaining == 0 {
			e.expire(wctx, d, spend)
			continue
		}

		window := d.DeadlineHeight - spend.BlockHeight
		if level := Level(remaining, window); level > d.Escalation {
			e.escalateTo(wctx, d.ID, spend.ChannelID, d.DeadlineHeight, level)
		}
	}
}

// escalateTo records a tier and announces it.
func (e *Engine) escalateTo(ctx context.Context, id, channelID int64, height, level int32) {
	if err := e.store.SetDeadlineEscalation(ctx, id, level, e.now().Unix()); err != nil {
		e.log.Error("could not record how loudly a countdown has been raised",
			slog.Int64("deadline_id", id), slog.String("error", err.Error()))
		return
	}

	e.mu.Lock()
	tip, interval := e.tipHeight, e.avgInterval
	e.mu.Unlock()

	remaining := Remaining(height, tip)
	// A block count on its own is not an answer. A minority chain can take half
	// an hour a block, so the same number of blocks can be far more human time
	// than instinct says — and saying nothing about time is better than letting
	// someone assume ten minutes.
	estimate := ""
	if projected, ok := Project(remaining, interval); ok {
		estimate = HumanDuration(projected)
	}

	e.bus.Publish(bus.DeadlineEscalated{
		DeadlineID:      id,
		ChannelID:       channelID,
		Level:           int(level),
		RemainingBlocks: remaining,
		EstWallClock:    estimate,
	})
}

// expire records a countdown that ran out.
func (e *Engine) expire(ctx context.Context, d store.Deadline, spend store.Spend) {
	if err := e.store.SetDeadlineState(ctx, d.ID, store.DeadlineExpired, "",
		e.now().Unix()); err != nil {
		e.log.Error("could not record that a countdown ran out",
			slog.Int64("deadline_id", d.ID), slog.String("error", err.Error()))
		return
	}

	// Whose commitment it was decides what running out *means*, and the spec did
	// not draw this distinction. A commitment the peer published leaves their
	// output waiting on a delay, and the end of that delay is when they take the
	// money: a loss. Our own commitment leaves *our* output waiting, and the end
	// of that delay is when we can claim it: the opposite. Announcing a loss
	// there would tell somebody they had been robbed at the moment their funds
	// became spendable.
	if spend.Shape == store.ShapeCommitmentOurs {
		e.log.Info("the wait on your own channel close has finished, so those funds "+
			"can now be claimed on the other chain",
			slog.Int64("channel_id", spend.ChannelID))
		return
	}

	channel, found := e.channel(ctx, spend.ChannelID)
	var amount int64
	if found {
		// The channel's whole capacity. An upper bound rather than a
		// measurement — what a revoked commitment takes is everything in the
		// channel, and the balance at that moment is not something this daemon
		// can know.
		amount = channel.CapacitySat
	}

	e.log.Error("a countdown ran out with nobody having answered it",
		slog.Int64("channel_id", spend.ChannelID),
		slog.Int64("deadline_id", d.ID))

	e.bus.Publish(bus.DeadlineExpiredLoss{
		DeadlineID: d.ID,
		ChannelID:  spend.ChannelID,
		AmountSat:  amount,
	})
}

// retire stops a countdown whose close is no longer on the chain.
//
// Recorded as resolved rather than expired, deliberately. The schema's three
// states are counting, resolved and expired, and "the commitment that started
// this clock is no longer on the chain" means there is nothing left to lose —
// which is what resolved says to anyone reading it. Marking it expired would put
// the word for the bad outcome next to an event that was harmless, and the
// dashboard would have no way to tell the two apart.
func (e *Engine) retire(ctx context.Context, d store.Deadline, spend store.Spend) {
	if err := e.store.SetDeadlineState(ctx, d.ID, store.DeadlineResolved, "",
		e.now().Unix()); err != nil {
		e.log.Error("could not retire a countdown",
			slog.Int64("deadline_id", d.ID), slog.String("error", err.Error()))
		return
	}
	e.log.Info("a countdown has stopped: the close it was counting from is no longer "+
		"on the other chain, and has not come back",
		slog.Int64("deadline_id", d.ID), slog.Int64("channel_id", spend.ChannelID))

	e.bus.Publish(bus.DeadlineResolved{DeadlineID: d.ID})
}

// onSecondOrderSpent stops a countdown that somebody answered.
func (e *Engine) onSecondOrderSpent(ctx context.Context, ev bus.SecondOrderSpent) {
	if store.SpendShape(ev.Shape) != store.ShapeJustice {
		// Only a justice transaction settles a contested output in the user's
		// favour. A delayed sweep after the deadline is the other outcome, and the
		// countdown running out is what records that.
		return
	}
	if ev.SourceSpendEventID == 0 {
		return
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	running, err := e.store.ListDeadlines(wctx, store.DeadlineCounting)
	if err != nil {
		e.log.Error("could not read the running countdowns",
			slog.String("error", err.Error()))
		return
	}

	txid := txidOf(wctx, e.store, ev.SpendEventID)
	for _, d := range running {
		if d.SpendEventID != ev.SourceSpendEventID {
			continue
		}
		if err := e.store.SetDeadlineState(wctx, d.ID, store.DeadlineResolved,
			txid, e.now().Unix()); err != nil {
			e.log.Error("could not record that a countdown was answered",
				slog.Int64("deadline_id", d.ID), slog.String("error", err.Error()))
			continue
		}
		e.log.Info("a countdown was answered before it ran out",
			slog.Int64("deadline_id", d.ID))
		e.bus.Publish(bus.DeadlineResolved{DeadlineID: d.ID, ByTxid: txid})
	}
	e.refreshStatus(ctx)
}

// onSpendReorgedOut notes that a close has left the chain.
//
// Nothing is stopped here. The spend may confirm again on the new branch within
// a block or two — a counterparty replacing a breach with a higher fee looks
// exactly like this — and a countdown dropped at the first sign of a
// reorganisation would be a countdown that stopped precisely when it mattered.
// The sweep in advance retires it if it stays away.
func (e *Engine) onSpendReorgedOut(ev bus.SpendReorgedOut) {
	if store.Branch(ev.Branch) != e.branch {
		return
	}
	e.log.Info("a close a countdown was measuring from has left the other chain; "+
		"the countdown keeps running in case it comes back",
		slog.Int64("spend_id", ev.SpendEventID))
}

// refreshStatus recomputes what the dashboard is told.
func (e *Engine) refreshStatus(ctx context.Context) {
	running, err := e.store.ListDeadlines(ctx, store.DeadlineCounting)
	if err != nil {
		e.log.Warn("could not read the running countdowns",
			slog.String("error", err.Error()))
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	status := Status{Counting: len(running), TipHeight: e.tipHeight}
	seen := map[int64]bool{}
	for _, d := range running {
		if status.EarliestHeight == 0 || d.DeadlineHeight < status.EarliestHeight {
			status.EarliestHeight = d.DeadlineHeight
		}
		if !d.Assumed {
			continue
		}
		status.Assumed++
		channelID := e.channelOf(ctx, d.SpendEventID)
		if channelID != 0 && !seen[channelID] {
			seen[channelID] = true
			status.AssumedChannels = append(status.AssumedChannels, channelID)
		}
	}
	e.status = status
}

// channel reads a channel, or reports that it could not.
func (e *Engine) channel(ctx context.Context, id int64) (store.Channel, bool) {
	if id == 0 {
		return store.Channel{}, false
	}
	channels, err := e.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		e.log.Warn("could not read your channels while working out a countdown",
			slog.String("error", err.Error()))
		return store.Channel{}, false
	}
	for _, c := range channels {
		if c.ID == id {
			return c, true
		}
	}
	return store.Channel{}, false
}

// channelOf finds which channel a spend belongs to.
func (e *Engine) channelOf(ctx context.Context, spendID int64) int64 {
	spend, err := e.store.GetSpend(ctx, spendID)
	if err != nil {
		return 0
	}
	return spend.ChannelID
}

// txidOf reads the transaction that did something, for the record of what
// answered a countdown.
func txidOf(ctx context.Context, st *store.Store, spendID int64) string {
	spend, err := st.GetSpend(ctx, spendID)
	if err != nil {
		return ""
	}
	return spend.SpendTxID
}

// writeCtx detaches a storage write from the shutdown that may be cancelling its
// trigger.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}
