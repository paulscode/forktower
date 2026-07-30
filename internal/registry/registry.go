package registry

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"golang.org/x/sync/errgroup"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// SubscriberName identifies the registry's bus subscription in drop diagnostics.
const SubscriberName = "registry"

// Defaults for anything the caller leaves unset.
const (
	// DefaultPollInterval is how often each Lightning node is re-read. Channels
	// change rarely; this is a floor on how stale the picture can be, not a
	// latency target. LND additionally pushes, which is what makes the common
	// case fast.
	DefaultPollInterval = time.Minute
	// DefaultBackfillInterval is how often the funding scripts nobody could find
	// are looked for again. Deliberately slow: on a pruned node without an
	// address index the answer will not change, and retrying every minute would
	// be a lot of load for a lookup that is optional anyway.
	DefaultBackfillInterval = 10 * time.Minute
	// DefaultSnapshotTimeout bounds one read of one Lightning node.
	DefaultSnapshotTimeout = 20 * time.Second
	// backfillTimeout bounds one attempt to find one funding script. Short, and
	// short on purpose: this is best-effort work that must never hold anything up.
	backfillTimeout = 15 * time.Second
	// writeTimeout bounds a storage write that outlives its trigger.
	writeTimeout = 5 * time.Second
)

// Client is what an adapter must provide: one read of one node.
//
// Both the LND and the Core Lightning adapters satisfy this, and nothing else
// about them is required — which is the point, because it means a third
// implementation is an adapter and not an engine change.
type Client interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Notifier is the optional half: a node that can say "something changed"
// instead of waiting to be asked.
//
// Optional because Core Lightning has no equivalent. A source that does not
// implement this is polled and nothing else, which is why the poll — not the
// push — is what guarantees progress.
type Notifier interface {
	// Watch calls notify whenever the node reports a channel change, and returns
	// when the context ends or the subscription fails.
	Watch(ctx context.Context, notify func()) error
}

// Source is one configured Lightning node.
type Source struct {
	// Name identifies it in logs and health. Supplied by the caller because two
	// nodes of the same implementation are a supported configuration and "lnd"
	// twice would be no help to anyone reading a diagnostic.
	Name   string
	Client Client
}

// BlockSource is the slice of a chain backend the funding-script backfill needs.
//
// Narrow on purpose: this engine has no business with tips, health, or
// broadcasting, and an interface that offered them would invite it to grow
// some. chainview.ChainView satisfies this.
type BlockSource interface {
	BlockHashByHeight(ctx context.Context, height int32) (chainhash.Hash, error)
	Block(ctx context.Context, h chainhash.Hash) (*wire.MsgBlock, error)
}

// Config tunes the registry. Zero values take the defaults above.
type Config struct {
	PollInterval     time.Duration
	BackfillInterval time.Duration
	SnapshotTimeout  time.Duration
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BackfillInterval <= 0 {
		c.BackfillInterval = DefaultBackfillInterval
	}
	if c.SnapshotTimeout <= 0 {
		c.SnapshotTimeout = DefaultSnapshotTimeout
	}
	return c
}

// SourceHealth is how a single Lightning node is doing.
type SourceHealth struct {
	Name string
	// LastSuccessAt is when this node was last read successfully. Zero means it
	// never has been.
	LastSuccessAt int64
	// LastError is why the most recent attempt failed, empty if it did not.
	// Already safe to show: it comes from an adapter that reports addresses and
	// status codes, never credentials.
	LastError string
	// Channels is how many channels the last successful read found.
	Channels int
}

// Stale reports whether this node's inventory is older than the given age.
func (h SourceHealth) Stale(now int64, maxAge time.Duration) bool {
	if h.LastSuccessAt == 0 {
		return true
	}
	return now-h.LastSuccessAt > int64(maxAge/time.Second)
}

// Registry keeps the stored channel inventory in step with the user's nodes,
// and decides which channels are exposed on the chain those nodes cannot see.
//
// It is the only writer of the channel inventory, and it never writes the two
// things it does not know: what the chain says about a close, which is the
// watcher's, and what a spend was, which is the deadline engine's.
type Registry struct {
	store   *store.Store
	bus     *bus.Bus
	sources []Source
	// blocks are tried in order for the funding-script backfill: the user's own
	// node first, then the other chain's backend. Either may be pruned, which is
	// why failing is not an error.
	blocks []BlockSource
	cfg    Config
	now    func() time.Time
	log    *slog.Logger

	// events is subscribed at construction rather than when Run starts, for the
	// same reason the alerter is: a split detected in the gap between wiring the
	// daemon up and this goroutine being scheduled would never reach the
	// classifier, and every channel would keep its provisional answer.
	events  <-chan bus.Event
	trigger chan struct{}

	mu     sync.Mutex
	health map[string]SourceHealth
	// scriptTried remembers which channels the backfill has already failed to
	// find a script for, so the first failure is reported loudly and the rest
	// quietly. Not persisted: on restart it is worth one more look.
	scriptTried map[int64]bool
}

// New builds a registry. A nil logger discards and a nil clock reads the real
// one, so a caller that cares about neither can pass nothing.
func New(
	st *store.Store,
	b *bus.Bus,
	sources []Source,
	blocks []BlockSource,
	cfg Config,
	log *slog.Logger,
	now func() time.Time,
) (*Registry, error) {
	if st == nil {
		return nil, errors.New("registry: a store is required")
	}
	if b == nil {
		return nil, errors.New("registry: an event bus is required")
	}
	for _, s := range sources {
		if s.Client == nil {
			return nil, errors.New("registry: a source has no client")
		}
		if s.Name == "" {
			return nil, errors.New("registry: a source has no name")
		}
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	r := &Registry{
		store:       st,
		bus:         b,
		sources:     sources,
		blocks:      blocks,
		cfg:         cfg.withDefaults(),
		now:         now,
		log:         log,
		events:      b.Subscribe(SubscriberName, bus.KindSplitStateChanged),
		trigger:     make(chan struct{}, 1),
		health:      make(map[string]SourceHealth, len(sources)),
		scriptTried: make(map[int64]bool),
	}
	if len(sources) == 0 {
		// Not an error. Split detection is useful on its own, and a user may not
		// have connected a Lightning node yet. But protection of channels is what
		// the product is for, so it is said out loud once.
		r.log.Warn("no Lightning node is configured, so no channels are being watched")
	}
	return r, nil
}

// Refresh asks for a poll as soon as one can run.
//
// Never blocks and never queues more than one: a hundred callers asking at once
// want the same single re-read, and the answer they get is the state after it.
func (r *Registry) Refresh() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Health reports how each configured node is doing, so the dashboard can say
// that an inventory is stale rather than silently showing an old one.
func (r *Registry) Health() []SourceHealth {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]SourceHealth, 0, len(r.sources))
	for _, s := range r.sources {
		out = append(out, r.health[s.Name])
	}
	return out
}

// Run keeps the inventory current until the context ends.
//
// Three kinds of goroutine: the poll loop, one watcher per node that can push,
// and the funding-script backfill. They are separate because a node that has
// stopped answering must not stop the others, and because the backfill talks to
// a chain backend that may be pruned, slow, or both.
func (r *Registry) Run(ctx context.Context) error {
	var g errgroup.Group
	g.Go(func() error { return r.pollLoop(ctx) })
	g.Go(func() error { return r.backfillLoop(ctx) })

	for _, s := range r.sources {
		n, ok := s.Client.(Notifier)
		if !ok {
			continue
		}
		g.Go(func() error { return r.watchSource(ctx, s.Name, n) })
	}
	return g.Wait()
}

// watchSource turns a node's push notifications into poll triggers.
//
// A failed subscription is not fatal and is not retried aggressively: the poll
// is what guarantees progress, so losing the push costs latency and nothing
// else. Exactly the arrangement the chain views use, for the same reason.
func (r *Registry) watchSource(ctx context.Context, name string, n Notifier) error {
	for ctx.Err() == nil {
		// A subscription that ends because we are shutting down is not news, so
		// the context is checked before the error is believed.
		if err := n.Watch(ctx, r.Refresh); err != nil && ctx.Err() == nil {
			r.log.Info("lost the channel notification stream, will keep polling",
				slog.String("source", name), slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.cfg.PollInterval):
		}
	}
	return nil
}

func (r *Registry) pollLoop(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	// Poll once immediately. A daemon that has just started knows nothing, and
	// waiting a full interval before finding out would leave the dashboard empty
	// for a minute at exactly the moment a user is looking at it.
	r.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			r.poll(ctx)

		case <-r.trigger:
			r.poll(ctx)

		case e, ok := <-r.events:
			if !ok {
				r.events = nil
				continue
			}
			if _, isSplit := e.(bus.SplitStateChanged); isSplit {
				// The fork point moved, or arrived. Every provisional answer was
				// given in the absence of this, so every one of them is re-decided.
				r.reclassifyAll(ctx)
			}
		}
	}
}

// poll re-reads every node and brings the stored inventory into line.
func (r *Registry) poll(ctx context.Context) {
	split, err := r.store.GetSplitState(ctx)
	if err != nil {
		r.log.Warn("could not read the split state, so channels keep their "+
			"current classification", slog.String("error", err.Error()))
		// Continue anyway: an inventory that is current but unclassified is far
		// more useful than no inventory at all, and Classify's answer without a
		// fork point is the same conservative "watch it" the store already holds.
	}

	for _, s := range r.sources {
		r.pollSource(ctx, s, split)
	}
}

func (r *Registry) pollSource(ctx context.Context, s Source, split store.Split) {
	sctx, cancel := context.WithTimeout(ctx, r.cfg.SnapshotTimeout)
	snap, err := s.Client.Snapshot(sctx)
	cancel()

	if err != nil {
		r.recordFailure(s.Name, err)
		// Degraded, not stopped. The stored inventory is still served, the
		// watchset it produced is still watched, and towers keep running: a
		// Lightning node restarting must not take the protection down with it.
		r.log.Warn("could not read your Lightning node, so its channels are being "+
			"served from the last successful read",
			slog.String("source", s.Name), slog.String("error", err.Error()))
		return
	}
	if snap.Node.Pubkey == "" {
		r.recordFailure(s.Name, errors.New("the node did not say who it is"))
		r.log.Warn("a Lightning node did not identify itself, so its channels "+
			"cannot be attributed", slog.String("source", s.Name))
		return
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	now := r.now().Unix()
	if err := r.store.UpsertLNNode(wctx, store.LNNode{
		ID:         snap.Node.Pubkey,
		Impl:       snap.Node.Impl,
		Alias:      snap.Node.Alias,
		LastSeenAt: now,
	}); err != nil {
		r.recordFailure(s.Name, err)
		r.log.Error("could not record your Lightning node",
			slog.String("source", s.Name), slog.String("error", err.Error()))
		return
	}

	prior, err := r.priorChannels(wctx, snap.Node.Pubkey)
	if err != nil {
		r.recordFailure(s.Name, err)
		r.log.Error("could not read the stored channels",
			slog.String("source", s.Name), slog.String("error", err.Error()))
		return
	}

	for _, rec := range snap.Channels {
		r.apply(wctx, snap.Node.Pubkey, rec, prior[outpointOf(rec)], split, now)
	}
	r.recordSuccess(s.Name, now, len(snap.Channels))
}

// outpoint is a channel's identity: the only thing about it that cannot change.
type outpoint struct {
	txid string
	vout int32
}

func outpointOf(rec ChannelRecord) outpoint {
	return outpoint{txid: rec.FundingTxID, vout: rec.FundingVout}
}

func (r *Registry) priorChannels(
	ctx context.Context, nodeID string,
) (map[outpoint]store.Channel, error) {
	rows, err := r.store.ListChannels(ctx, store.ChannelFilter{LNNodeID: nodeID})
	if err != nil {
		return nil, err
	}
	out := make(map[outpoint]store.Channel, len(rows))
	for _, c := range rows {
		out[outpoint{txid: c.FundingTxID, vout: c.FundingVout}] = c
	}
	return out, nil
}

// apply brings one channel into line with what the node just said.
func (r *Registry) apply(
	ctx context.Context,
	nodeID string,
	rec ChannelRecord,
	prior store.Channel,
	split store.Split,
	now int64,
) {
	isNew := prior.ID == 0

	ch := store.Channel{
		LNNodeID:    nodeID,
		FundingTxID: rec.FundingTxID,
		FundingVout: rec.FundingVout,
		// Never cleared by a poll. The node does not know the funding script and
		// never reports one, so an empty value here means "not said", not "gone" —
		// and overwriting a script the backfill found would mean finding it again
		// every minute.
		FundingScriptHex: firstNonEmpty(rec.FundingScriptHex, prior.FundingScriptHex),
		CapacitySat:      rec.CapacitySat,
		ChanType:         rec.ChanType,
		CSVDelayLocal:    rec.CSVDelayLocal,
		CSVDelayRemote:   rec.CSVDelayRemote,
		PeerPubkey:       rec.PeerPubkey,
		PeerAlias:        rec.PeerAlias,
		OpenHeight:       rec.OpenHeight,
		SCID:             rec.SCID,
		UpdatedAt:        now,
	}

	id, changed, err := r.store.UpsertChannel(ctx, ch)
	if err != nil {
		r.log.Error("could not record one of your channels",
			slog.String("channel", rec.FundingTxID), slog.String("error", err.Error()))
		return
	}

	if err := r.store.ReplaceHTLCSnapshot(ctx, id, now, rec.HTLCs); err != nil {
		// Not fatal to the channel: a stale HTLC picture makes the deadline
		// engine early rather than wrong, which is the direction to be wrong in.
		r.log.Warn("could not record the payments in flight on one of your channels",
			slog.String("channel", rec.FundingTxID), slog.String("error", err.Error()))
	}

	closeState := r.advanceClose(ctx, id, rec, prior, now)
	rel, reason := r.classify(ctx, id, rec, prior, closeState, split, now)

	if changed {
		r.bus.Publish(bus.ChannelUpserted{
			New: isNew,
			Channel: bus.ChannelJSON{
				ID:              id,
				FundingTxID:     rec.FundingTxID,
				FundingVout:     rec.FundingVout,
				CapacitySat:     rec.CapacitySat,
				ChanType:        string(rec.ChanType),
				PeerPubkey:      rec.PeerPubkey,
				PeerAlias:       rec.PeerAlias,
				OpenHeight:      rec.OpenHeight,
				SCID:            rec.SCID,
				CloseState:      string(closeState),
				Relevance:       string(rel),
				RelevanceReason: reason,
			},
		})
	}
}

// closeRank orders the close states so that the registry can move a channel
// forward without ever moving it back.
//
// This matters because two things write the close state from different
// evidence. The node knows it has begun closing before the chain shows
// anything, which is genuinely earlier news and worth having. The watcher knows
// what actually confirmed, which is the stronger claim. A node that restarts and
// briefly reports a channel as open again must not be able to erase a close the
// chain has already recorded.
func closeRank(c store.CloseState) int {
	switch c {
	case store.CloseOpen:
		return 0
	case store.ClosePending:
		return 1
	case store.CloseCoop, store.CloseForce, store.CloseBreach:
		return 2
	default:
		return 0
	}
}

// advanceClose records a close the node has told us about, if it is news, and
// returns the close state now in force.
func (r *Registry) advanceClose(
	ctx context.Context, id int64, rec ChannelRecord, prior store.Channel, now int64,
) store.CloseState {
	current := prior.CloseState
	if current == "" {
		current = store.CloseOpen
	}
	if closeRank(rec.CloseState) <= closeRank(current) {
		return current
	}

	// Height stays unset: the node is reporting its own belief, and the block
	// that confirmed the close is the watcher's to fill in.
	if err := r.store.SetChannelCloseSF(ctx, id, rec.CloseState, rec.CloseTxID, 0, now); err != nil {
		r.log.Error("could not record that one of your channels is closing",
			slog.String("channel", rec.FundingTxID), slog.String("error", err.Error()))
		return current
	}

	r.bus.Publish(bus.ChannelClosedSF{
		ChannelID: id,
		CloseTxid: rec.CloseTxID,
		State:     string(rec.CloseState),
	})
	return rec.CloseState
}

// classify decides and stores this channel's exposure on the other chain.
func (r *Registry) classify(
	ctx context.Context,
	id int64,
	rec ChannelRecord,
	prior store.Channel,
	closeState store.CloseState,
	split store.Split,
	now int64,
) (rel store.Relevance, reason string) {
	rel, reason = Classify(Facts{
		ForkKnown:       split.ForkKnown(),
		ForkHeight:      split.ForkHeight,
		OpenHeight:      rec.OpenHeight,
		CloseState:      closeState,
		CloseHeight:     prior.CloseHeight,
		FundingSeenOnSQ: fundingSeenOnSQ(prior),
	})
	if rel == prior.Relevance && reason == prior.RelevanceReason {
		return rel, reason
	}
	if err := r.store.SetChannelRelevance(ctx, id, rel, reason, now); err != nil {
		r.log.Error("could not record whether one of your channels is exposed",
			slog.String("channel", rec.FundingTxID), slog.String("error", err.Error()))
		return prior.Relevance, prior.RelevanceReason
	}
	return rel, reason
}

// fundingSeenOnSQ recovers a finding that only the watcher can make: that a
// channel funded after the separation had its funding transaction reach the
// other chain anyway.
//
// Read back out of the reason the watcher wrote, because that reason is where
// the finding was recorded and a second column holding the same fact would be a
// second thing to keep in step. The coupling is real, so both sides go through
// the same constant.
func fundingSeenOnSQ(prior store.Channel) bool {
	return prior.Relevance == store.Relevant &&
		prior.RelevanceReason == ReasonFundedAfterForkButMirrored
}

// reclassifyAll re-decides every channel's exposure, which is what a change in
// the fork point demands: every provisional answer was given without it.
func (r *Registry) reclassifyAll(ctx context.Context) {
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	split, err := r.store.GetSplitState(wctx)
	if err != nil {
		r.log.Error("could not read the split state, so channels keep their "+
			"current classification", slog.String("error", err.Error()))
		return
	}

	channels, err := r.store.ListChannels(wctx, store.ChannelFilter{})
	if err != nil {
		r.log.Error("could not read your channels to re-check which are exposed",
			slog.String("error", err.Error()))
		return
	}

	now := r.now().Unix()
	var moved int
	for _, c := range channels {
		rel, reason := Classify(Facts{
			ForkKnown:       split.ForkKnown(),
			ForkHeight:      split.ForkHeight,
			OpenHeight:      c.OpenHeight,
			CloseState:      c.CloseState,
			CloseHeight:     c.CloseHeight,
			FundingSeenOnSQ: fundingSeenOnSQ(c),
		})
		if rel == c.Relevance && reason == c.RelevanceReason {
			continue
		}
		if err := r.store.SetChannelRelevance(wctx, c.ID, rel, reason, now); err != nil {
			r.log.Error("could not record whether one of your channels is exposed",
				slog.Int64("channel_id", c.ID), slog.String("error", err.Error()))
			continue
		}
		moved++
	}
	if moved > 0 {
		r.log.Info("re-checked which of your channels are exposed on the other chain",
			slog.Int("channels", len(channels)), slog.Int("changed", moved))
	}
}

// backfillLoop looks for the funding scripts nobody has found yet.
//
// Best-effort and non-blocking. Both target platforms commonly run a
// pruned Bitcoin node without a transaction index, where reading a block from
// the height a channel was funded at will simply fail. That costs nothing on the
// tier this daemon actually runs on, which matches outpoints rather than
// scripts; it matters only on the light-client tier, where the readiness check
// says so plainly instead of this loop trying harder.
func (r *Registry) backfillLoop(ctx context.Context) error {
	if len(r.blocks) == 0 {
		return nil
	}
	ticker := time.NewTicker(r.cfg.BackfillInterval)
	defer ticker.Stop()

	r.backfill(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.backfill(ctx)
		}
	}
}

func (r *Registry) backfill(ctx context.Context) {
	channels, err := r.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		r.log.Warn("could not read your channels to look up their funding scripts",
			slog.String("error", err.Error()))
		return
	}

	for _, c := range channels {
		if ctx.Err() != nil {
			return
		}
		if c.FundingScriptHex != "" || c.OpenHeight <= 0 {
			continue
		}
		script, err := r.findScript(ctx, c)
		if err != nil {
			r.noteScriptMiss(c, err)
			continue
		}
		c.FundingScriptHex = script
		c.UpdatedAt = r.now().Unix()

		wctx, cancel := writeCtx(ctx)
		if _, _, err := r.store.UpsertChannel(wctx, c); err != nil {
			r.log.Warn("could not record a funding script",
				slog.Int64("channel_id", c.ID), slog.String("error", err.Error()))
		}
		cancel()

		r.mu.Lock()
		delete(r.scriptTried, c.ID)
		r.mu.Unlock()
	}
}

// noteScriptMiss reports a failed lookup once at info and quietly thereafter.
// A pruned node fails the same way every ten minutes forever, and a log that
// repeats that is a log nobody reads.
func (r *Registry) noteScriptMiss(c store.Channel, err error) {
	r.mu.Lock()
	first := !r.scriptTried[c.ID]
	r.scriptTried[c.ID] = true
	r.mu.Unlock()

	msg := "could not find the funding script for one of your channels, which is " +
		"harmless unless you are using a light client"
	if first {
		r.log.Info(msg, slog.Int64("channel_id", c.ID), slog.String("error", err.Error()))
		return
	}
	r.log.Debug(msg, slog.Int64("channel_id", c.ID), slog.String("error", err.Error()))
}

// findScript reads the funding output's script from whichever backend still has
// the block. The user's own node is asked first; the other chain's backend is a
// fallback because before the fork the two chains hold the same block.
func (r *Registry) findScript(ctx context.Context, c store.Channel) (string, error) {
	var lastErr error
	for _, src := range r.blocks {
		bctx, cancel := context.WithTimeout(ctx, backfillTimeout)
		script, err := scriptFromBlock(bctx, src, c.OpenHeight, c.FundingTxID, c.FundingVout)
		cancel()

		if err == nil {
			return script, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no chain backend is configured")
	}
	return "", lastErr
}

func scriptFromBlock(
	ctx context.Context, src BlockSource, height int32, txid string, vout int32,
) (string, error) {
	hash, err := src.BlockHashByHeight(ctx, height)
	if err != nil {
		return "", fmt.Errorf("reading block %d: %w", height, err)
	}
	blk, err := src.Block(ctx, hash)
	if err != nil {
		return "", fmt.Errorf("reading block %d: %w", height, err)
	}
	if blk == nil {
		return "", fmt.Errorf("block %d came back empty", height)
	}

	for _, tx := range blk.Transactions {
		if tx.TxHash().String() != txid {
			continue
		}
		if vout < 0 || int(vout) >= len(tx.TxOut) {
			return "", fmt.Errorf("transaction %s has no output %d", txid, vout)
		}
		return hex.EncodeToString(tx.TxOut[vout].PkScript), nil
	}
	return "", fmt.Errorf("transaction %s is not in block %d", txid, height)
}

func (r *Registry) recordSuccess(name string, at int64, channels int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[name] = SourceHealth{Name: name, LastSuccessAt: at, Channels: channels}
}

func (r *Registry) recordFailure(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health[name]
	h.Name = name
	h.LastError = err.Error()
	r.health[name] = h
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// writeCtx detaches a storage write from the shutdown that may be cancelling
// its trigger. Five seconds, then it really does give up.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}
