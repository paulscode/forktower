// Package chainviewtest provides an in-memory chain view for tests.
//
// The behaviour that matters most in this codebase — a chain splitting, a
// reorganisation removing a spend, a backend quietly following the wrong chain —
// is impractical to provoke against real nodes on demand. This lets a test script
// those situations exactly and assert what the daemon does about them.
package chainviewtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// View is a scriptable chain view. Safe for concurrent use.
type View struct {
	mu sync.Mutex

	// blocks is the active chain, indexed by height.
	blocks []chainview.BlockMeta
	// known holds every header ever seen, including ones no longer on the active
	// chain, because the separation-point search walks through exactly those.
	known map[chainhash.Hash]chainview.BlockMeta

	health   chainview.BackendHealth
	identity chainview.Identity
	tips     []chainview.ChainTip
	deploys  map[string]chainview.Deployment

	// failures lets a test make a specific call fail, to exercise the paths that
	// only happen when a node misbehaves.
	failures map[string]error

	subscribers []chan chainview.BlockMeta
	mempool     []chan *wire.MsgTx
	closed      bool

	broadcast []*wire.MsgTx
}

// Compile-time proof that the fake satisfies the same contract as a real backend;
// a fake that had drifted from the interface would be worse than none.
var (
	_ chainview.ChainView    = (*View)(nil)
	_ chainview.Identifiable = (*View)(nil)
	_ chainview.ChainTipper  = (*View)(nil)
	_ chainview.Deployer     = (*View)(nil)
)

// New creates a view whose chain is a single genesis block, tagged so that two
// views built with different tags are on different networks.
func New(tag string) *View {
	v := &View{
		known:    map[chainhash.Hash]chainview.BlockMeta{},
		deploys:  map[string]chainview.Deployment{},
		failures: map[string]error{},
		health: chainview.BackendHealth{
			State:        chainview.HealthOK,
			SyncProgress: 1,
			PeerCount:    8,
		},
		identity: chainview.Identity{
			Endpoint:       "http://" + tag + ":8332",
			LocalAddresses: []string{tag + ".local:8333"},
			Subversion:     "/fake:1.0(" + tag + ")/",
		},
	}
	genesis := chainview.BlockMeta{
		BlockRef: chainview.BlockRef{Hash: TaggedHash("genesis-"+tag, 0), Height: 0},
		Time:     time.Unix(1231006505, 0),
	}
	v.blocks = []chainview.BlockMeta{genesis}
	v.known[genesis.Hash] = genesis
	return v
}

// NewSharedHistory builds two views on the same network, agreeing up to height
// `shared`, which is the starting point for any split scenario.
func NewSharedHistory(shared int32) (sf, sq *View) {
	sf, sq = New("shared"), New("shared")
	for h := int32(1); h <= shared; h++ {
		block := chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: TaggedHash("shared", h), Height: h},
			PrevHash: TaggedHash("shared", h-1),
			Time:     time.Unix(1231006505+int64(h)*600, 0),
		}
		if h == 1 {
			block.PrevHash = sf.blocks[0].Hash
		}
		sf.appendLocked(block)
		sq.appendLocked(block)
	}
	return sf, sq
}

// TaggedHash builds a deterministic hash from a label and a height, so a test can
// tell at a glance which chain a hash came from — a failure that prints
// "shared/100" against "theirs/100" explains itself, where two random hashes would
// not.
//
// Panics if the label and height do not fit, because a silent truncation would
// make two different blocks share a hash and turn a scripted split into a chain
// that agrees with itself.
func TaggedHash(tag string, height int32) chainhash.Hash {
	label := fmt.Sprintf("%s/%d", tag, height)
	if len(label) > chainhash.HashSize {
		panic("chainviewtest: block label " + label + " is too long to be a hash")
	}
	var h chainhash.Hash
	copy(h[:], label)
	return h
}

func (v *View) appendLocked(block chainview.BlockMeta) {
	v.blocks = append(v.blocks, block)
	v.known[block.Hash] = block
}

// Extend adds n blocks to the chain, tagged with label, and notifies subscribers.
func (v *View) Extend(label string, n int32) chainview.BlockMeta {
	v.mu.Lock()
	last := v.blocks[len(v.blocks)-1]
	for range n {
		next := chainview.BlockMeta{
			BlockRef: chainview.BlockRef{
				Hash:   TaggedHash(label, last.Height+1),
				Height: last.Height + 1,
			},
			PrevHash: last.Hash,
			Time:     last.Time.Add(10 * time.Minute),
		}
		v.appendLocked(next)
		last = next
	}
	subs := append([]chan chainview.BlockMeta(nil), v.subscribers...)
	v.mu.Unlock()

	notify(subs, last)
	return last
}

// Reorg discards blocks above height and rebuilds from there with a new label,
// which is how a test produces a tip that changes without advancing.
func (v *View) Reorg(from int32, label string, n int32) chainview.BlockMeta {
	v.mu.Lock()
	if int(from) < len(v.blocks) {
		// Headers stay in `known`: the search walks back through blocks that are no
		// longer on anyone's active chain.
		v.blocks = v.blocks[:from+1]
	}
	v.mu.Unlock()
	return v.Extend(label, n)
}

func notify(subs []chan chainview.BlockMeta, tip chainview.BlockMeta) {
	for _, ch := range subs {
		select {
		case ch <- tip:
		default:
			// A test that is not reading is not the fake's problem.
		}
	}
}

// SetHealth replaces what the view reports about itself.
func (v *View) SetHealth(h chainview.BackendHealth) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.health = h
}

// SetIdentity replaces the view's node identity. Passing the same identity to two
// views is how a test reproduces the misconfiguration where both point at one node.
func (v *View) SetIdentity(id chainview.Identity) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.identity = id
}

// SetChainTips replaces the branch tips the view reports.
func (v *View) SetChainTips(tips []chainview.ChainTip) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tips = tips
}

// SetDeployment records a soft fork the view will report.
func (v *View) SetDeployment(name string, d chainview.Deployment) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deploys[name] = d
}

// Fail makes the named method return err until cleared with a nil err.
func (v *View) Fail(method string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		delete(v.failures, method)
		return
	}
	v.failures[method] = err
}

func (v *View) failure(method string) error {
	if err, ok := v.failures[method]; ok {
		return err
	}
	return nil
}

// Broadcasts returns every transaction submitted to this view.
func (v *View) Broadcasts() []*wire.MsgTx {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]*wire.MsgTx(nil), v.broadcast...)
}

// Tip returns the current tip without going through the interface.
func (v *View) Tip() chainview.BlockMeta {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.blocks[len(v.blocks)-1]
}

// Ancestry returns up to n recent hashes, newest first.
func (v *View) Ancestry(n int) []chainhash.Hash {
	v.mu.Lock()
	defer v.mu.Unlock()

	out := make([]chainhash.Hash, 0, n)
	for i := len(v.blocks) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, v.blocks[i].Hash)
	}
	return out
}

// --- chainview.ChainView -----------------------------------------------------

// BestBlock returns the current tip.
func (v *View) BestBlock(context.Context) (chainview.BlockMeta, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("BestBlock"); err != nil {
		return chainview.BlockMeta{}, err
	}
	return v.blocks[len(v.blocks)-1], nil
}

// BlockHeaderByHash returns any header the view has ever seen, on the active chain
// or not.
func (v *View) BlockHeaderByHash(_ context.Context, h chainhash.Hash) (chainview.BlockMeta, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("BlockHeaderByHash"); err != nil {
		return chainview.BlockMeta{}, err
	}
	meta, ok := v.known[h]
	if !ok {
		return chainview.BlockMeta{}, fmt.Errorf("header %s: %w", h, chainview.ErrNotFound)
	}
	return meta, nil
}

// BlockHashByHeight returns the active chain's hash at a height.
func (v *View) BlockHashByHeight(_ context.Context, height int32) (chainhash.Hash, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("BlockHashByHeight"); err != nil {
		return chainhash.Hash{}, err
	}
	if height < 0 || int(height) >= len(v.blocks) {
		return chainhash.Hash{}, fmt.Errorf("height %d: %w", height, chainview.ErrNotFound)
	}
	return v.blocks[height].Hash, nil
}

// Block returns an empty block carrying the right header, which is enough for
// anything that does not inspect transactions.
func (v *View) Block(_ context.Context, h chainhash.Hash) (*wire.MsgBlock, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("Block"); err != nil {
		return nil, err
	}
	meta, ok := v.known[h]
	if !ok {
		return nil, fmt.Errorf("block %s: %w", h, chainview.ErrNotFound)
	}
	return &wire.MsgBlock{Header: wire.BlockHeader{Timestamp: meta.Time}}, nil
}

// MatchBlock always reports a possible match, as a full node does.
func (v *View) MatchBlock(context.Context, chainhash.Hash, chainview.WatchSet) (bool, error) {
	return true, nil
}

// SubscribeTip delivers the tip on subscribing and after every change.
func (v *View) SubscribeTip(ctx context.Context) (<-chan chainview.BlockMeta, error) {
	v.mu.Lock()
	if err := v.failure("SubscribeTip"); err != nil {
		v.mu.Unlock()
		return nil, err
	}
	ch := make(chan chainview.BlockMeta, 64)
	if v.closed {
		close(ch)
		v.mu.Unlock()
		return ch, nil
	}
	v.subscribers = append(v.subscribers, ch)
	tip := v.blocks[len(v.blocks)-1]
	v.mu.Unlock()

	ch <- tip

	go func() {
		<-ctx.Done()
		v.mu.Lock()
		defer v.mu.Unlock()
		for i, existing := range v.subscribers {
			if existing == ch {
				v.subscribers = append(v.subscribers[:i], v.subscribers[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch, nil
}

// SubscribeMempoolTx streams transactions a test injects.
func (v *View) SubscribeMempoolTx(ctx context.Context) (<-chan *wire.MsgTx, error) {
	v.mu.Lock()
	if err := v.failure("SubscribeMempoolTx"); err != nil {
		v.mu.Unlock()
		return nil, err
	}
	ch := make(chan *wire.MsgTx, 64)
	v.mempool = append(v.mempool, ch)
	v.mu.Unlock()

	go func() {
		<-ctx.Done()
		v.mu.Lock()
		defer v.mu.Unlock()
		for i, existing := range v.mempool {
			if existing == ch {
				v.mempool = append(v.mempool[:i], v.mempool[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch, nil
}

// InjectMempoolTx delivers a transaction to memory-pool subscribers.
func (v *View) InjectMempoolTx(tx *wire.MsgTx) {
	v.mu.Lock()
	subs := append([]chan *wire.MsgTx(nil), v.mempool...)
	v.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- tx:
		default:
		}
	}
}

// Broadcast records a submitted transaction.
func (v *View) Broadcast(_ context.Context, tx *wire.MsgTx) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("Broadcast"); err != nil {
		return err
	}
	v.broadcast = append(v.broadcast, tx)
	return nil
}

// Health reports what the test set, with the current tip filled in.
func (v *View) Health(context.Context) (chainview.BackendHealth, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("Health"); err != nil {
		return chainview.BackendHealth{}, err
	}
	h := v.health
	h.Tip = v.blocks[len(v.blocks)-1]
	return h, nil
}

// Identity reports the node behind this view.
func (v *View) Identity(context.Context) (chainview.Identity, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("Identity"); err != nil {
		return chainview.Identity{}, err
	}
	return v.identity, nil
}

// ChainTips reports the branch tips the test set.
func (v *View) ChainTips(context.Context) ([]chainview.ChainTip, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("ChainTips"); err != nil {
		return nil, err
	}
	tips := append([]chainview.ChainTip(nil), v.tips...)
	if len(tips) == 0 {
		tip := v.blocks[len(v.blocks)-1]
		tips = append(tips, chainview.ChainTip{
			Hash: tip.Hash, Height: tip.Height, Status: chainview.TipActive,
		})
	}
	return tips, nil
}

// Deployment reports a soft fork the test registered.
func (v *View) Deployment(_ context.Context, name string) (*chainview.Deployment, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failure("Deployment"); err != nil {
		return nil, err
	}
	d, ok := v.deploys[name]
	if !ok {
		return nil, fmt.Errorf("deployment %q: %w", name, chainview.ErrNotFound)
	}
	return &d, nil
}

// Close ends every subscription, standing in for a shut-down backend.
func (v *View) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	for _, ch := range v.subscribers {
		close(ch)
	}
	v.subscribers = nil
	for _, ch := range v.mempool {
		close(ch)
	}
	v.mempool = nil
}
