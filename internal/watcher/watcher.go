package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// SubscriberName identifies the watcher's bus subscription in drop diagnostics.
const SubscriberName = "watcher"

// Defaults and bounds.
const (
	// DefaultMaxReorgDepth is how far back a reorganisation may reach before it
	// stops being a reorganisation and starts being a reason to doubt the
	// backend. A hundred blocks is about seventeen hours of the honest chain; a
	// chain that replaces that much is either under an attack this daemon cannot
	// help with, or is not the chain we think we are watching.
	DefaultMaxReorgDepth = 100

	// DefaultMaxForwardGap is how far the chain may run ahead of the reader
	// before the reader gives up following it block by block and re-anchors at
	// the tip.
	//
	// Generous enough that an ordinary outage — a restart, a slow minute — is
	// read properly rather than skipped, and small enough that a node catching
	// up from nothing does not have a quarter of a million blocks walked one at
	// a time before it can watch anything current.
	DefaultMaxForwardGap = 2000
	// DefaultBlockAttempts is how many times one block is retried before the
	// watcher declares itself stuck. Bounded on purpose: the high-water mark only
	// advances after a block commits, so a block that fails forever freezes
	// progress while everything else still reports fine.
	DefaultBlockAttempts = 3
	// DefaultRetryDelay is the pause between attempts at one block.
	DefaultRetryDelay = 5 * time.Second
	// maxSamePasses bounds the re-scan of a single block. A block is scanned
	// again whenever the pass before it recorded something new, because a later
	// stage may have added outpoints in response — a sweep of a commitment that
	// confirmed in the same block. Two passes is the real answer; the bound is
	// there so a bug cannot turn it into a loop.
	maxSamePasses = 8
	// writeTimeout bounds a storage write that outlives its trigger.
	writeTimeout = 5 * time.Second
)

// StalledAlertKind is the alert raised when the watcher stops making progress.
const StalledAlertKind = "watcher_stalled"

// DeepReorgAlertKind is the alert raised when the chain is replaced further back
// than any reorganisation should reach.
const DeepReorgAlertKind = "watcher_deep_reorg"

// Guard is the part of the detection engine the watcher must obey.
//
// Scanning a chain the second view is not actually on produces a clean report
// about a chain nobody needs watched, which is worse than producing nothing at
// all: the user is told they are covered while the exposure goes unseen. So when
// the sentinel pauses, this pauses.
type Guard interface {
	Paused() bool
}

// Config tunes the watcher. Zero values take the defaults above.
type Config struct {
	MaxReorgDepth int32
	// MaxForwardGap bounds how far behind the reader may fall and still catch up
	// block by block. Zero uses DefaultMaxForwardGap.
	MaxForwardGap int32
	BlockAttempts int
	RetryDelay    time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxForwardGap <= 0 {
		c.MaxForwardGap = DefaultMaxForwardGap
	}
	if c.MaxReorgDepth <= 0 {
		c.MaxReorgDepth = DefaultMaxReorgDepth
	}
	if c.BlockAttempts <= 0 {
		c.BlockAttempts = DefaultBlockAttempts
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = DefaultRetryDelay
	}
	return c
}

// Progress is how far the watcher has got, for the readiness check that notices
// when it has stopped getting anywhere.
type Progress struct {
	// Height and Hash are the last block fully processed and committed. Zero and
	// empty before anything has been.
	Height int32
	Hash   string
	// At is when that happened.
	At int64
	// Stalled reports that a block has failed every attempt, so the high-water
	// mark is frozen. The daemon is up and the backend may look healthy; nothing
	// is being scanned.
	Stalled bool
	// StalledAt is the height it is stuck on, and Why is safe to show.
	StalledAt int32
	Why       string

	// RescanNext and RescanTarget bound a catch-up sweep of blocks behind the
	// high-water mark. Both zero when there is nothing to catch up on.
	RescanNext   int32
	RescanTarget int32
}

// Rescanning reports whether there is history still being caught up on.
func (p Progress) Rescanning() bool { return p.RescanTarget > 0 && p.RescanNext <= p.RescanTarget }

// Progressing reports whether scanning is moving. False also before the first
// block is processed, which is honest: nothing has been scanned yet.
func (p Progress) Progressing() bool { return !p.Stalled && p.Height > 0 }

// Watcher follows one chain and notices when anything it was asked to watch is
// spent.
//
// Single-goroutine by design. Blocks arrive about ten minutes apart and the work
// per block is trivial, so there is nothing to gain from processing two at once
// and a great deal to lose: the high-water mark, the reorganisation walk and the
// spend records all assume they are the only writer.
type Watcher struct {
	store  *store.Store
	bus    *bus.Bus
	view   chainview.ChainView
	branch store.Branch
	guard  Guard
	cfg    Config
	now    func() time.Time
	log    *slog.Logger

	// events is subscribed at construction rather than when Run starts, so a
	// channel that arrives during startup is not lost between wiring up and being
	// scheduled.
	events <-chan bus.Event
	// nudge wakes the loop when something outside it queues work. Without it a
	// sweep asked for from the dashboard would sit until the next block, which on
	// this chain can be a long time.
	nudge chan struct{}

	mu       sync.Mutex
	ws       WatchSet
	last     blockRef
	progress Progress
	// replaying remembers whether the backend is still working through history,
	// with when it was last asked. Cached because during an initial sync a tip
	// notification arrives for every connected block, and an RPC per block would
	// make the check heavier than the scanning it exists to prevent.
	replaying     bool
	replayingAt   time.Time
	replayingSeen bool
	// activeBest is the height the backend itself calls its best block, from the
	// same cached read. Needed to tell a tip on the chain being watched from one
	// belonging to some other chainstate the same node happens to be building.
	activeBest int32
	strayNoted bool
	// sweep is the range of blocks behind the high-water mark still to be
	// looked at, and blockedSweep says it hit something it could not read. The
	// sweep is retried when the next block arrives rather than immediately, so a
	// backend that cannot answer is not asked a thousand times a second.
	sweep        sweep
	blockedSweep bool
	// pausedNoted stops the "watching is paused" line repeating on every tip.
	pausedNoted bool
}

// blockRef is a block by both halves. A height alone cannot tell ordinary growth
// from the chain being replaced up to that point.
type blockRef struct {
	Height int32
	Hash   chainhash.Hash
}

func (b blockRef) known() bool { return b.Hash != chainhash.Hash{} }

// New builds a watcher. A nil logger discards and a nil clock reads the real
// one. A nil guard means nothing can pause it, which is only right in a test.
func New(
	st *store.Store,
	b *bus.Bus,
	view chainview.ChainView,
	branch store.Branch,
	guard Guard,
	cfg Config,
	log *slog.Logger,
	now func() time.Time,
) (*Watcher, error) {
	if st == nil {
		return nil, errors.New("watcher: a store is required")
	}
	if b == nil {
		return nil, errors.New("watcher: an event bus is required")
	}
	if view == nil {
		return nil, errors.New("watcher: a chain view is required")
	}
	if !branch.Valid() {
		return nil, fmt.Errorf("watcher: %q is not a branch", branch)
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	return &Watcher{
		store:  st,
		bus:    b,
		view:   view,
		branch: branch,
		guard:  guard,
		cfg:    cfg.withDefaults(),
		now:    now,
		log:    log,
		nudge:  make(chan struct{}, 1),
		events: b.Subscribe(SubscriberName,
			bus.KindChannelUpserted, bus.KindChannelClosedSF, bus.KindSplitStateChanged),
	}, nil
}

// Progress reports how far scanning has got.
func (w *Watcher) Progress() Progress {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.progress
}

// WatchSet is what is currently being looked for, for diagnostics and for the
// dashboard.
func (w *Watcher) WatchSet() WatchSet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ws
}

// Run follows the chain until the context ends.
func (w *Watcher) Run(ctx context.Context) error {
	// A shutdown arriving during startup is a clean stop, not a failure. Every
	// read below goes through the context it was given, so a cancelled one
	// surfaces as an error meaning nothing more than "we are stopping" — checked
	// first so that it is never reported as a fault.
	loadErr := w.load(ctx)
	if stopping(ctx) {
		return nil
	}
	if loadErr != nil {
		return loadErr
	}

	tips, subErr := w.view.SubscribeTip(ctx)
	if stopping(ctx) {
		return nil
	}
	if subErr != nil {
		return fmt.Errorf("following the other chain: %w", subErr)
	}

	// Unconfirmed transactions arrive on the same loop as blocks, because they
	// write the same records. A nil channel here means this backend cannot see a
	// memory pool, and a nil channel in a select never fires.
	mempool := w.subscribeMempool(ctx)

	for {
		// Anything waiting is dealt with first. A new block matters more than
		// catching up on old ones, and an event may change what the catch-up is
		// even looking for.
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-w.events:
			w.onEvent(ctx, e, ok)
			continue
		case tip, ok := <-tips:
			if !ok {
				// The subscription ended without the context doing so. The backend
				// reconnects rather than closing, so this is a real stop.
				return nil
			}
			w.handleTip(ctx, tip)
			continue
		case tx, ok := <-mempool:
			if !ok {
				mempool = nil
				continue
			}
			mempool = w.drainMempool(ctx, mempool, tx)
			continue
		case <-w.nudge:
			continue
		default:
		}

		// Catching up happens here, in slices, on this same goroutine. One writer
		// of the spend records and one reader of the chain is the whole
		// concurrency design: blocks are ten minutes apart and there is nothing to
		// win by racing.
		if w.sweepSlice(ctx) {
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-w.events:
			w.onEvent(ctx, e, ok)
		case tip, ok := <-tips:
			if !ok {
				return nil
			}
			w.handleTip(ctx, tip)
		case tx, ok := <-mempool:
			if !ok {
				mempool = nil
				continue
			}
			// Drained here too. This is the blocking select — the loop sits in it
			// whenever there is no catch-up work — so it is where most
			// transactions actually arrive, and handling one per wake-up here
			// would leave the batching above doing nothing on an idle chain.
			mempool = w.drainMempool(ctx, mempool, tx)
		case <-w.nudge:
		}
	}
}

// wake asks the loop to look for work. Never blocks and never queues more than
// one: the loop re-reads everything anyway, so one wake-up is as good as ten.
func (w *Watcher) wake() {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// onEvent reacts to something else in the daemon changing.
func (w *Watcher) onEvent(ctx context.Context, e bus.Event, open bool) {
	if !open {
		w.events = nil
		return
	}

	// Any of these can change what must be watched: a new channel, a close, or a
	// split that re-classified every one of them. Rebuilding is cheap and
	// idempotent, so it is not worth working out which.
	w.rebuild(ctx)

	switch ev := e.(type) {
	case bus.SplitStateChanged:
		// The moment the separation point is known, everything since it needs
		// looking at — and until now there was no reason to have looked.
		if store.SplitState(ev.New) == store.StateSplit {
			_, _, _ = w.rescanFromFork(ctx, "the chains separated")
		}

	case bus.ChannelUpserted:
		// A channel seen for the first time has a history nobody has checked. Only
		// on first sighting: re-sweeping every time a counterparty changes its
		// alias would be a lot of reading for no new information.
		if ev.New {
			_, _, _ = w.rescanFromFork(ctx, "a channel Forktower had not seen before")
		}
	}
}

// replayingCheckEvery is how often the backend is asked whether it is still
// working through history.
//
// Not per tip: during an initial sync a notification arrives for every connected
// block, and asking each time would cost more than the scanning this prevents.
// Half a minute is far shorter than any sync and far longer than any block.
const replayingCheckEvery = 30 * time.Second

// backendReplaying reports whether the other chain's node is still validating
// blocks it already has headers for.
//
// **Fails open, deliberately.** If the backend cannot say, this answers "not
// replaying" and scanning proceeds — the failure to read health is itself
// reported elsewhere, and a watcher that stops watching because one status call
// did not come back would turn a transient RPC blip into silent blindness. The
// cost of being wrong the other way is some wasted scanning.
// backendState reads what the node says about itself, at most once every
// replayingCheckEvery.
//
// Returns whether it is still working through history, and the height it calls
// its own best block. Zero for that height means it could not be read, and every
// caller treats that as "no opinion" rather than as a low tip.
func (w *Watcher) backendState(ctx context.Context) (replaying bool, activeBest int32) {
	now := w.now()

	w.mu.Lock()
	fresh := !w.replayingAt.IsZero() && now.Sub(w.replayingAt) < replayingCheckEvery
	cachedReplaying, cachedBest := w.replaying, w.activeBest
	w.mu.Unlock()
	if fresh {
		return cachedReplaying, cachedBest
	}

	health, err := w.view.Health(ctx)
	if err != nil {
		return false, 0
	}

	w.mu.Lock()
	w.replaying = health.ReplayingHistory
	w.activeBest = health.Tip.Height
	w.replayingAt = now
	w.mu.Unlock()
	return health.ReplayingHistory, health.Tip.Height
}

// strayChainstate reports that a height cannot belong to the chain being
// watched, because the node itself puts its best block far above it.
//
// **A node can be building more than one chain at a time.** After the snapshot
// shortcut, Bitcoin Core keeps two chainstates: the one from the snapshot, at
// the tip, and a background one validating from genesis. Both publish block
// notifications on the same socket. Observed on real hardware: the watcher was
// handed heights climbing from 176 at roughly a thousand blocks every five
// seconds, interleaved with the real tip at 959,766 — and every alternation
// between them read as the chain being replaced far deeper than any
// reorganisation reaches. Fifteen thousand critical alerts, and scanning
// stopped.
//
// The rule is to trust the node's own account of its active chain over an
// unsolicited notification. Anything further below the best block than a
// reorganisation could reach is not a reorganisation; it is a different chain
// being built by the same process.
//
// Fails closed on no information: with no readable best height nothing is
// treated as stray, because discarding a real tip would be silent blindness and
// the cost the other way is one wasted comparison.
func (w *Watcher) strayChainstate(activeBest, height int32) bool {
	if activeBest <= 0 {
		return false
	}
	return height < activeBest-w.cfg.MaxReorgDepth
}

// mempoolDrainPerTurn is how many unconfirmed transactions are taken in one go
// before the loop attends to anything else.
//
// **One at a time was the old behaviour, and it is a block-shaped assumption.**
// Every other event this loop serves arrives minutes apart; a node at the tip of
// mainnet relays transactions by the dozen per second, so taking one per turn
// and then doing a slice of catch-up work let the queue grow faster than it
// drained. Bounded rather than "until empty", because a block that has just
// arrived matters more than the next thousand transactions, and an unbounded
// drain would let a busy mempool starve it.
const mempoolDrainPerTurn = 256

// drainMempool handles one transaction and as many more as are already waiting.
//
// Returns the channel, or nil once it has closed, so the caller stops selecting
// on it.
func (w *Watcher) drainMempool(
	ctx context.Context, mempool <-chan *wire.MsgTx, first *wire.MsgTx,
) <-chan *wire.MsgTx {
	w.handleMempoolTx(ctx, first)

	for range mempoolDrainPerTurn - 1 {
		select {
		case tx, ok := <-mempool:
			if !ok {
				return nil
			}
			w.handleMempoolTx(ctx, tx)
		default:
			return mempool
		}
	}
	return mempool
}

// stopping reports that the daemon is shutting down, which makes every failure
// below it uninteresting.
func stopping(ctx context.Context) bool { return ctx.Err() != nil }

// load reads the high-water mark and builds the initial watchset.
func (w *Watcher) load(ctx context.Context) error {
	height, err := w.store.GetMetaInt64(ctx, store.MetaLastScannedSQHeight, 0)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("reading how far the other chain has been scanned: %w", err)
	}
	hashStr, err := w.store.GetMeta(ctx, store.MetaLastScannedSQHash)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("reading how far the other chain has been scanned: %w", err)
	}

	var last blockRef
	if hashStr != "" && height > 0 {
		h, parseErr := chainhash.NewHashFromStr(hashStr)
		narrowed, inRange := toInt32(height)
		if parseErr != nil || len(hashStr) != chainhash.MaxHashStringSize || !inRange {
			// A mark we cannot read is treated as no mark at all rather than as a
			// height to trust. Starting again from the tip is recoverable; scanning
			// forward from a block that may not be on this chain is not.
			w.log.Warn("could not read where scanning had got to, so it starts again " +
				"from the current tip")
		} else {
			last = blockRef{Height: narrowed, Hash: *h}
		}
	}

	w.mu.Lock()
	w.last = last
	w.progress = Progress{Height: last.Height, Hash: hashOrEmpty(last)}
	w.mu.Unlock()

	w.rebuild(ctx)
	w.loadSweep(ctx)
	return nil
}

// toInt32 narrows a value the schema stores as a 32-bit integer, refusing
// anything that would wrap. A wrapped height is negative, and a negative height
// reads as a perfectly ordinary value everywhere downstream — which is how a
// bad number becomes a wrong answer rather than an error.
func toInt32(v int64) (int32, bool) {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, false
	}
	return int32(v), true
}

func hashOrEmpty(b blockRef) string {
	if !b.known() {
		return ""
	}
	return b.Hash.String()
}

// rebuild re-reads what must be watched.
func (w *Watcher) rebuild(ctx context.Context) {
	ws, err := Build(ctx, w.store, w.branch)
	if err != nil {
		// The previous set stays in force. An empty set scans clean, and a scan
		// that finds nothing looks exactly like a scan that had nothing to find.
		w.log.Error("could not work out what to watch, so the previous list stays "+
			"in use", slog.String("error", err.Error()))
		return
	}
	for _, s := range ws.Skipped {
		w.log.Warn("something could not be watched",
			slog.String("what", s.What), slog.String("why", s.Why))
	}

	w.mu.Lock()
	before := w.ws.Len()
	w.ws = ws
	w.mu.Unlock()

	if ws.Len() != before {
		w.log.Info("the list of what is being watched changed",
			slog.Int("outpoints", ws.Len()))
	}
}

// handleTip brings scanning up to the new tip.
func (w *Watcher) handleTip(ctx context.Context, tip chainview.BlockMeta) {
	if w.guard != nil && w.guard.Paused() {
		w.mu.Lock()
		noted := w.pausedNoted
		w.pausedNoted = true
		w.mu.Unlock()
		if !noted {
			w.log.Warn("not scanning the other chain, because the daemon is not sure " +
				"it is looking at the right one")
		}
		return
	}
	// **A node replaying history has no tip worth watching.** Its "tip" is a
	// historical block that climbs quickly, and following it means scanning
	// hundreds of thousands of blocks from years before any of the user's
	// channels existed — while competing for disk with the very sync being
	// chased. Observed on a fresh install: the mark reached height 311,611, in
	// 2014, and every read that hiccupped under that load raised a *critical*
	// "scanning has stopped" alert at a new height.
	//
	// Nothing is recorded either, so no mark is laid at a height that means
	// nothing. Once the backend is caught up, the first tip starts watching from
	// a real one.
	replaying, activeBest := w.backendState(ctx)
	if replaying {
		w.mu.Lock()
		noted := w.replayingSeen
		w.replayingSeen = true
		w.progress.Why = "the other chain's node is still catching up, so there is " +
			"nothing to watch yet"
		w.mu.Unlock()
		if !noted {
			w.log.Info("not watching the other chain yet, because its node is still " +
				"catching up with history")
		}
		return
	}
	// Not this chain's tip. Ignored rather than acted on, and *not* reported as a
	// reorganisation, which is what it used to look like.
	if w.strayChainstate(activeBest, tip.Height) {
		w.mu.Lock()
		noted := w.strayNoted
		w.strayNoted = true
		w.mu.Unlock()
		if !noted {
			w.log.Info("ignoring blocks from another chainstate the node is building",
				slog.Int("their_height", int(tip.Height)),
				slog.Int("best_height", int(activeBest)))
		}
		return
	}

	w.mu.Lock()
	if w.replayingSeen {
		w.replayingSeen = false
		w.progress.Why = ""
		w.log.Info("the other chain's node has caught up, so watching starts")
	}
	w.strayNoted = false
	w.pausedNoted = false
	last := w.last
	w.mu.Unlock()

	// **A mark that is not on the node's active chain is not a mark.** Installs
	// that took the snapshot shortcut before this was understood recorded one
	// from the background chainstate — height 11,173, from 2009, on a node whose
	// best block was 959,766. Left alone it drives a nine-hundred-thousand-block
	// catch-up through a chain nobody was watching.
	//
	// Discarding it is safe here in a way it is not in general: this is not "the
	// mark is old", which would mean skipping blocks that genuinely needed
	// scanning, but "the mark is on a different chain", where there is nothing to
	// skip. History is the rescan's business either way, and it is anchored on
	// the fork point rather than on wherever this happened to be.
	if last.known() && w.strayChainstate(activeBest, last.Height) {
		w.log.Warn("discarding a scan position recorded from another chainstate",
			slog.Int("was_at_height", int(last.Height)),
			slog.Int("best_height", int(activeBest)))
		last = blockRef{}
		w.mu.Lock()
		w.last = last
		w.mu.Unlock()
	}

	// Nothing scanned yet. Start from here rather than from the beginning of
	// time: the historical sweep is the rescan's job, and it knows where to start
	// from because it is anchored on the fork point rather than on whenever the
	// daemon happened to be installed.
	if !last.known() {
		w.commitMark(ctx, blockRef{Height: tip.Height, Hash: tip.Hash})
		w.log.Info("started watching the other chain",
			slog.Int("height", int(tip.Height)))
		return
	}

	if tip.Hash == last.Hash {
		return // already processed
	}

	// A new block is the cue to try again at whatever the catch-up could not
	// read: the backend has evidently started answering.
	w.mu.Lock()
	w.blockedSweep = false
	w.mu.Unlock()

	if tip.PrevHash == last.Hash {
		w.processTo(ctx, []chainview.BlockMeta{tip})
		return
	}

	// Either the chain grew while nobody was looking, or it was replaced. Both
	// are answered the same way: walk back until we recognise something.
	w.catchUp(ctx, tip, last)
}
