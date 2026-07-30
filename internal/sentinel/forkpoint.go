package sentinel

import (
	"container/list"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// ErrForkTooDeep means the two chains could not be traced back to a common
// ancestor within the permitted number of steps.
//
// Treated as "cannot tell yet" rather than as a failure. It happens when a view
// has pruned the headers needed, or when the separation is older than the search
// allows — and in both cases claiming a separation point that has not been found
// would be worse than admitting the search came up short, because that point is
// the anchor for everything downstream.
var ErrForkTooDeep = errors.New("sentinel: the chains do not share a recent common ancestor")

// HeaderFetcher retrieves a block header, whether or not it is on the view's
// active chain.
//
// Injected rather than taken as a view, so the search itself performs no I/O of
// its own and can be tested exhaustively against scripted chains. Off-chain
// headers are essential: the walk back from two tips crosses blocks one side has
// rejected.
type HeaderFetcher func(ctx context.Context, h chainhash.Hash) (chainview.BlockMeta, error)

// DefaultMaxAncestorWalk bounds the search. Two chains that have diverged by more
// than this are past the point where the daemon can usefully reconstruct the
// separation; a bound also stops a malformed chain sending the walk on forever.
const DefaultMaxAncestorWalk = 20000

// FindForkPoint walks back from two tips to the last block they share.
//
// The taller side is brought down to the other's height first, then both step
// back together until they meet. Returns ErrForkTooDeep if they do not meet within
// maxWalk steps, or if a header needed along the way cannot be fetched — a pruned
// view is indistinguishable from an impossibly deep separation, and both mean the
// same thing to the caller.
func FindForkPoint(
	ctx context.Context,
	sf, sq HeaderFetcher,
	sfTip, sqTip chainview.BlockMeta,
	maxWalk int,
	cache *HeaderCache,
) (chainview.BlockRef, error) {
	if maxWalk <= 0 {
		maxWalk = DefaultMaxAncestorWalk
	}

	a, b := sfTip, sqTip
	steps := 0

	// Bring the taller side down. Counted against the same budget, since a large
	// height difference is itself a form of depth.
	for a.Height > b.Height {
		var err error
		if a, err = fetchHeader(ctx, sf, cache, chainview.BranchSF, a.PrevHash); err != nil {
			return chainview.BlockRef{}, err
		}
		steps++
		if steps > maxWalk {
			return chainview.BlockRef{}, fmt.Errorf(
				"walked back %d blocks on the chain your node follows: %w", steps, ErrForkTooDeep)
		}
	}
	for b.Height > a.Height {
		var err error
		if b, err = fetchHeader(ctx, sq, cache, chainview.BranchSQ, b.PrevHash); err != nil {
			return chainview.BlockRef{}, err
		}
		steps++
		if steps > maxWalk {
			return chainview.BlockRef{}, fmt.Errorf(
				"walked back %d blocks on the other chain: %w", steps, ErrForkTooDeep)
		}
	}

	for a.Hash != b.Hash {
		if a.Height == 0 || b.Height == 0 {
			// Reached the start of the chain without meeting, which means the two views
			// are not on the same network at all. A distinct problem from a deep
			// separation, and one the startup checks are meant to have caught already.
			return chainview.BlockRef{}, fmt.Errorf(
				"the two views share no common ancestor at all, so they are not the same "+
					"network: %w", ErrForkTooDeep)
		}

		var err error
		if a, err = fetchHeader(ctx, sf, cache, chainview.BranchSF, a.PrevHash); err != nil {
			return chainview.BlockRef{}, err
		}
		if b, err = fetchHeader(ctx, sq, cache, chainview.BranchSQ, b.PrevHash); err != nil {
			return chainview.BlockRef{}, err
		}
		steps++
		if steps > maxWalk {
			return chainview.BlockRef{}, fmt.Errorf(
				"walked back %d blocks on both chains without meeting: %w", steps, ErrForkTooDeep)
		}
	}

	return chainview.BlockRef{Hash: a.Hash, Height: a.Height}, nil
}

func fetchHeader(
	ctx context.Context,
	fetch HeaderFetcher,
	cache *HeaderCache,
	branch chainview.Branch,
	h chainhash.Hash,
) (chainview.BlockMeta, error) {
	if cache != nil {
		if meta, ok := cache.Get(branch, h); ok {
			return meta, nil
		}
	}
	meta, err := fetch(ctx, h)
	if err != nil {
		if errors.Is(err, chainview.ErrNotFound) {
			// A header the view no longer has. Reported as a search that came up short
			// rather than as a fault, because that is what the caller can act on.
			return chainview.BlockMeta{}, fmt.Errorf(
				"header %s is no longer available: %w", h, ErrForkTooDeep)
		}
		return chainview.BlockMeta{}, fmt.Errorf("fetching header %s: %w", h, err)
	}
	if cache != nil {
		cache.Put(branch, meta)
	}
	return meta, nil
}

// DefaultHeaderCacheSize is how many headers are remembered across searches.
//
// The search runs on every tick while the chains disagree and would otherwise
// re-fetch the same ancestors each time. Ten thousand headers is a few megabytes
// and covers a separation far deeper than the daemon will ever usefully handle.
const DefaultHeaderCacheSize = 10000

// HeaderCache remembers headers, discarding the least recently used first.
//
// Keyed by branch as well as hash. Not strictly required — a hash identifies a
// block globally — but it keeps one view's answers from being served for the
// other, so a misconfiguration where both views point at the same node cannot be
// masked by the cache.
type HeaderCache struct {
	limit int
	order *list.List
	items map[cacheKey]*list.Element
}

type cacheKey struct {
	branch chainview.Branch
	hash   chainhash.Hash
}

type cacheEntry struct {
	key  cacheKey
	meta chainview.BlockMeta
}

// NewHeaderCache creates a cache holding up to limit headers. A limit of zero or
// less uses DefaultHeaderCacheSize.
//
// Not safe for concurrent use: it belongs to the single goroutine that runs the
// search, which is where all of this decision-making lives.
func NewHeaderCache(limit int) *HeaderCache {
	if limit <= 0 {
		limit = DefaultHeaderCacheSize
	}
	return &HeaderCache{
		limit: limit,
		order: list.New(),
		items: make(map[cacheKey]*list.Element, limit),
	}
}

// Get returns a remembered header.
func (c *HeaderCache) Get(branch chainview.Branch, h chainhash.Hash) (chainview.BlockMeta, bool) {
	el, ok := c.items[cacheKey{branch, h}]
	if !ok {
		return chainview.BlockMeta{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).meta, true
}

// Put remembers a header, evicting the least recently used if full.
func (c *HeaderCache) Put(branch chainview.Branch, meta chainview.BlockMeta) {
	key := cacheKey{branch, meta.Hash}
	if el, ok := c.items[key]; ok {
		el.Value.(*cacheEntry).meta = meta
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, meta: meta})
	c.items[key] = el

	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).key)
	}
}

// Len reports how many headers are held.
func (c *HeaderCache) Len() int { return c.order.Len() }
