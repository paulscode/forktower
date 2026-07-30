package chainview

import (
	"context"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ChainView is a read-mostly window onto one chain.
//
// Backends supply blocks and hints; they do not interpret them. Outpoint
// matching, reorganisation bookkeeping and scan progress all live above this
// interface, so that logic exists once and behaves identically whether it is
// reading from a full node or a light client. A backend that started making those
// decisions itself would have to be trusted twice: once for the data and again
// for the conclusion.
//
// Every method must be safe for concurrent use, and must respect its context.
// Backends are otherwise passive: nothing here owns a goroutine except the
// subscriptions, whose lifetime is the context passed to them.
type ChainView interface {
	// BestBlock returns the backend's current tip.
	//
	// Called on every tick, so it must be cheap — under about 50ms for a local
	// node. A backend that cannot answer quickly should be reporting poor health
	// rather than making the caller wait.
	BestBlock(ctx context.Context) (BlockMeta, error)

	// BlockHeaderByHash returns a header the backend knows, whether or not it is
	// on the active chain.
	//
	// Off-chain headers matter: walking back from two tips to find where they
	// separated crosses blocks that one side has rejected, and a backend that
	// only answered for its own best chain could not support that search.
	// Returns ErrNotFound if the header is unknown.
	BlockHeaderByHash(ctx context.Context, h chainhash.Hash) (BlockMeta, error)

	// BlockHashByHeight returns the ACTIVE-chain hash at a height, or ErrNotFound
	// beyond the tip.
	//
	// Active-chain only, deliberately: comparing two views at the same height is
	// how the daemon decides whether they agree, and that comparison is only
	// meaningful against each view's own best chain.
	BlockHashByHeight(ctx context.Context, height int32) (chainhash.Hash, error)

	// Block returns a full block, which on a light backend may mean fetching it
	// from a peer.
	//
	// Returns ErrNotFound when the backend cannot supply it — a pruned node asked
	// for old history, most often. Callers must treat that as a health event and
	// keep going: it means this view has a blind spot, which the user needs to be
	// told about, not that the daemon should stop.
	Block(ctx context.Context, h chainhash.Hash) (*wire.MsgBlock, error)

	// MatchBlock reports whether a block MAY contain activity from ws.
	//
	// The asymmetry is deliberate and load-bearing. False is a promise: there is
	// definitely nothing here, so the caller may skip the block without fetching
	// it. True is only a hint, and the caller must fetch and check properly. A
	// full node backend returning a constant true is therefore correct — it has no
	// cheaper test than looking, and claiming otherwise would be a false promise
	// in the direction that loses money.
	MatchBlock(ctx context.Context, h chainhash.Hash, ws WatchSet) (bool, error)

	// SubscribeTip delivers the new tip after each change, including
	// reorganisations.
	//
	// A reorganisation is not announced as such: it simply arrives as a new tip
	// whose previous-hash does not follow the last one the consumer saw. Consumers
	// detect it from that discontinuity, which means they cannot be misled by a
	// backend's opinion about whether something counts as a reorganisation.
	//
	// The channel closes when ctx ends. On internal failure the implementation
	// reconnects with backoff rather than closing: a backend restart is routine
	// and must not look like a shutdown to its consumer.
	SubscribeTip(ctx context.Context) (<-chan BlockMeta, error)

	// SubscribeMempoolTx streams unconfirmed transactions, or returns
	// ErrUnsupported on a backend without a view of the memory pool.
	//
	// Unconfirmed sightings are early warning: seeing a channel-closing
	// transaction before it confirms buys the user time that a confirmation-only
	// view would not have given them. Optional because a light client genuinely
	// cannot offer it.
	SubscribeMempoolTx(ctx context.Context) (<-chan *wire.MsgTx, error)

	// Broadcast submits a raw transaction.
	//
	// Already-known and already-in-chain results map to nil: this is called on
	// retry paths and after restarts, and "it is already there" is the outcome the
	// caller wanted. Any other rejection returns an error carrying the backend's
	// own reason, which is usually the only clue about why a transaction will not
	// go through.
	Broadcast(ctx context.Context, tx *wire.MsgTx) error

	// Health performs a liveness and sync check, and must not block for more than
	// about five seconds.
	//
	// A backend reports only what it can see about itself. Whether it is on the
	// right chain, or being shown a fabricated one, cannot be determined from
	// inside a single view and is decided above.
	Health(ctx context.Context) (BackendHealth, error)
}

// Verification helpers, implemented alongside the sentinel that drives them.
//
// They are described here rather than declared, because a function body that
// returns "not implemented" is a trap: it compiles, satisfies every caller, and
// silently reports failure — or worse, success — if the real implementation is
// never written. Nothing calls these yet, so there is nothing to declare.
//
// VerifyNetwork(ctx, v, want) asserts that a view is on the expected network.
// Run for every view before any scanning, and again after a reconnection. A
// mismatch is fatal rather than degraded: a backend pointed at a test network
// answers everything correctly and diverges permanently, so nothing downstream
// could tell it apart from a genuine chain split. Better to refuse to start than
// to spend a week confidently watching the wrong network.
//
// Comparing the genesis hash is sufficient on its own, and better than comparing
// the network name: the name is a label a node reports, while the genesis hash is
// the chain's identity. That means this needs nothing beyond the interface above —
// BlockHashByHeight(0) — so it does not require reaching into a concrete backend.
//
// VerifyBranch(ctx, sf, sq, anchorHeight, fork) confirms that sq follows the
// chain the user's node does not. Two halves, both needed:
//
//   - Agreement below the anchor. At sampled heights up to anchorHeight the two
//     views must return identical hashes. History from before the chains could
//     possibly disagree is shared, so any difference there means the backend is on
//     another chain, another network, or being shown a fabrication. Sampled rather
//     than walked, because two hash lookups per height is cheap enough to repeat
//     often, and repetition is what catches a backend that drifts later.
//
//   - Divergence above the fork. Once a separation point is known, the two views
//     must actually differ somewhere above it, and sq must not contain the
//     user's-side block there. A backend that has quietly followed the same chain
//     as the user's node is the failure this catches, and it is worse than having
//     no backend at all: it produces a clean report about a chain nobody needs
//     watching, while the exposure it was installed to find goes unseen.
//
// anchorHeight is capped strictly below the height at which the two rule sets
// could first disagree. fork is absent before a separation point is known, in
// which case only the first half applies.
