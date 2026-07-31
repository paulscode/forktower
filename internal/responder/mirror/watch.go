package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
)

// Observer finds the user's transactions on one chain so they can be offered to
// the other.
//
// **Not a second detection engine, and the difference matters.** The Watcher
// looks at the chain nobody else is watching, because a spend there is a threat
// nobody would otherwise see. This looks at the chain the user's *own* node
// follows, where every spend is already known to them and none of it is news —
// it is here only to be copied. So this publishes nothing, raises nothing, and
// starts no countdowns: the transactions it finds on the user's own chain are
// not events, they are material.
//
// The pieces it does reuse are the pure ones — the watchset, the block scan, and
// the classifier. A second implementation of "whose commitment is this" would be
// a second place for that answer to be wrong, and the two would disagree exactly
// when it mattered.
type Observer struct {
	store  Store
	view   chainview.ChainView
	log    *slog.Logger
	from   store.Branch
	to     store.Branch
	lastAt int32
}

// Store is the storage this reads and writes. An interface so a test can make a
// read fail without a broken database.
type Store interface {
	watcher.Source
	RecordMirrorDecision(ctx context.Context, d store.MirrorDecision) (int64, bool, error)
	ListSpends(ctx context.Context, f store.SpendFilter) ([]store.Spend, error)
}

// ObserverOptions configures an Observer.
type ObserverOptions struct {
	Store Store
	View  chainview.ChainView
	Log   *slog.Logger
	// From is the chain being read and To the chain things would be copied to.
	// Named rather than assumed, because the mirror runs in both directions and
	// the rules differ sharply between them.
	From, To store.Branch
}

// NewObserver builds an Observer.
func NewObserver(opts ObserverOptions) (*Observer, error) {
	if opts.Store == nil {
		return nil, errors.New("mirror: an observer needs storage")
	}
	if opts.View == nil {
		return nil, errors.New("mirror: an observer needs a chain to read")
	}
	if !opts.From.Valid() || !opts.To.Valid() {
		return nil, errors.New("mirror: an observer needs two chains to move between")
	}
	if opts.From == opts.To {
		return nil, fmt.Errorf("mirror: %s to itself is not a direction", opts.From)
	}
	if opts.Log == nil {
		opts.Log = slog.New(discardHandler{})
	}
	return &Observer{
		store: opts.Store, view: opts.View, log: opts.Log,
		from: opts.From, to: opts.To,
	}, nil
}

// Found is one transaction worth a decision.
type Found struct {
	Lifted   Lifted
	Decision store.MirrorDecision
	// New is false when this transaction had already been decided about, which is
	// the ordinary case: a block is scanned once but a pass may revisit one.
	New bool
}

// ScanBlock looks at one block and records what it decided about each of the
// user's transactions in it.
//
// Returns everything it considered, refusals included. **The refusals are the
// larger half and the more interesting one**: the allowlist declines most of
// what it sees, and a user asking "why was that not copied?" needs an answer
// that outlived the log.
func (o *Observer) ScanBlock(
	ctx context.Context, height int32, at int64,
) ([]Found, error) {
	ws, err := watcher.Build(ctx, o.store, o.from)
	if err != nil {
		return nil, err
	}
	for _, skipped := range ws.Skipped {
		// A channel missing from the set is a channel whose transactions will
		// never be copied. Not fatal — the others still work — but never silent.
		o.log.Warn("something could not be watched for copying",
			slog.String("what", skipped.What), slog.String("why", skipped.Why))
	}
	if ws.Empty() {
		return nil, nil
	}

	hash, err := o.view.BlockHashByHeight(ctx, height)
	if err != nil {
		return nil, fmt.Errorf("finding block %d to look for your transactions: %w", height, err)
	}
	// Asked whether the block could hold anything of ours before it is fetched.
	// On a full node this costs nothing; on a light backend it is the difference
	// between reading every block and reading the handful that matter.
	interesting, err := o.view.MatchBlock(ctx, hash, ws.ChainViewSet())
	if err != nil {
		return nil, fmt.Errorf("checking block %d for your transactions: %w", height, err)
	}
	if !interesting {
		o.lastAt = height
		return nil, nil
	}
	blk, err := o.view.Block(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("reading block %d to look for your transactions: %w", height, err)
	}

	matches := watcher.ScanBlock(blk, ws)
	if len(matches) == 0 {
		o.lastAt = height
		return nil, nil
	}

	out := make([]Found, 0, len(matches))
	for _, m := range matches {
		found, decideErr := o.consider(ctx, m, height, at)
		if decideErr != nil {
			// One transaction that could not be judged must not stop the rest.
			// Logged rather than dropped: a transaction nobody decided about is
			// one nobody can explain later.
			o.log.Error("could not decide about one of your transactions",
				slog.String("txid", m.TxID.String()),
				slog.String("error", decideErr.Error()))
			continue
		}
		out = append(out, found)
	}
	o.lastAt = height
	return out, nil
}

// consider lifts one match and records the verdict.
func (o *Observer) consider(
	ctx context.Context, m watcher.Match, height int32, at int64,
) (Found, error) {
	facts, err := o.factsFor(ctx, m, height)
	if err != nil {
		return Found{}, err
	}

	lifted, err := Lift(m, m.Tx, facts.Facts)
	if err != nil {
		return Found{}, err
	}

	decision := Decision(lifted, o.from, o.to, facts.fundingOptIn, at)
	_, existed, err := o.store.RecordMirrorDecision(ctx, decision)
	if err != nil {
		return Found{}, err
	}
	return Found{Lifted: lifted, Decision: decision, New: !existed}, nil
}

// liftFacts is Facts plus the one thing that comes from the user rather than
// from the chain.
type liftFacts struct {
	Facts
	fundingOptIn bool
}

// factsFor gathers what cannot be read off the transaction.
//
// The important one is the source shape: for a spend of a commitment's output,
// whose sweep or justice transaction this is turns entirely on what the
// commitment it follows was classified as.
func (o *Observer) factsFor(
	ctx context.Context, m watcher.Match, height int32,
) (liftFacts, error) {
	out := liftFacts{Facts: Facts{
		SpendHeight: height, Role: m.Target.Role, ChannelID: m.Target.ChannelID,
	}}

	// A second-order target is a commitment's *output*. The watchset records it
	// against the spend that produced it rather than against a channel, so both
	// the channel and — crucially — what that commitment was classified as have
	// to be found through the spend.
	//
	// The source shape is the whole reason this lookup exists: whose sweep or
	// justice transaction this is turns entirely on what it follows.
	if m.Target.SourceSpendEventID != 0 {
		spends, err := o.store.ListSpends(ctx, store.SpendFilter{
			Branch: o.from, Limit: store.MaxSpendLimit,
		})
		if err != nil {
			return liftFacts{}, fmt.Errorf("reading what this transaction follows: %w", err)
		}
		for _, sp := range spends {
			if sp.ID != m.Target.SourceSpendEventID {
				continue
			}
			out.SourceShape = sp.Shape
			out.ChannelID = sp.ChannelID
			break
		}
	}

	if out.ChannelID == 0 {
		return out, nil
	}

	channels, err := o.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		return liftFacts{}, fmt.Errorf("reading the channel this belongs to: %w", err)
	}
	for _, c := range channels {
		if c.ID != out.ChannelID {
			continue
		}
		out.OurCloseTxID = c.CloseTxID
		out.fundingOptIn = c.MirrorFundingOptIn
		break
	}
	return out, nil
}

// LastScanned is the highest block this observer has looked at.
func (o *Observer) LastScanned() int32 { return o.lastAt }

// discardHandler drops log records, for an observer built without a logger.
type discardHandler struct{ slog.Handler }

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
