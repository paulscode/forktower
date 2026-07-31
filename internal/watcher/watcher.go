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
	BlockAttempts int
	RetryDelay    time.Duration
}

func (c Config) withDefaults() Config {
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
}

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

	mu       sync.Mutex
	ws       WatchSet
	last     blockRef
	progress Progress
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

	for {
		select {
		case <-ctx.Done():
			return nil

		case _, ok := <-w.events:
			if !ok {
				w.events = nil
				continue
			}
			// Any of these can change what must be watched: a new channel, a close,
			// or a split that re-classified every one of them. Rebuilding is cheap
			// and idempotent, so it is not worth working out which.
			w.rebuild(ctx)

		case tip, ok := <-tips:
			if !ok {
				// The subscription ended without the context doing so. The backend
				// reconnects rather than closing, so this is a real stop.
				return nil
			}
			w.handleTip(ctx, tip)
		}
	}
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
	w.mu.Lock()
	w.pausedNoted = false
	last := w.last
	w.mu.Unlock()

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

	if tip.PrevHash == last.Hash {
		w.processTo(ctx, []chainview.BlockMeta{tip})
		return
	}

	// Either the chain grew while nobody was looking, or it was replaced. Both
	// are answered the same way: walk back until we recognise something.
	w.catchUp(ctx, tip, last)
}
