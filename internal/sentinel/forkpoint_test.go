package sentinel

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// scriptedChain is a chain of headers a test can hand to the search, so the walk
// is exercised against known shapes rather than against a live node.
type scriptedChain struct {
	byHash map[chainhash.Hash]chainview.BlockMeta
	// missing marks headers the view no longer has, standing in for a pruned node.
	missing map[chainhash.Hash]bool
	fetches int
}

func newScriptedChain() *scriptedChain {
	return &scriptedChain{
		byHash:  map[chainhash.Hash]chainview.BlockMeta{},
		missing: map[chainhash.Hash]bool{},
	}
}

// build lays down a chain of `n` blocks from a starting height, hashing each block
// as tag|height so a test can reason about which chain a hash belongs to.
func (c *scriptedChain) build(tag byte, from, n int32, prev chainhash.Hash) chainview.BlockMeta {
	last := chainview.BlockMeta{}
	for h := from; h < from+n; h++ {
		meta := chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: taggedHash(tag, h), Height: h},
			PrevHash: prev,
			Time:     time.Unix(int64(h)*600, 0),
		}
		c.byHash[meta.Hash] = meta
		prev = meta.Hash
		last = meta
	}
	return last
}

func (c *scriptedChain) fetcher() HeaderFetcher {
	return func(_ context.Context, h chainhash.Hash) (chainview.BlockMeta, error) {
		c.fetches++
		if c.missing[h] {
			return chainview.BlockMeta{}, fmt.Errorf("pruned: %w", chainview.ErrNotFound)
		}
		meta, ok := c.byHash[h]
		if !ok {
			return chainview.BlockMeta{}, fmt.Errorf("unknown %s: %w", h, chainview.ErrNotFound)
		}
		return meta, nil
	}
}

func taggedHash(tag byte, height int32) chainhash.Hash {
	var h chainhash.Hash
	h[0] = tag
	h[1] = byte(height)
	h[2] = byte(height >> 8)
	return h
}

// sharedThenSplit builds a shared history and then a private continuation on each
// side, returning both tips and the block they last agreed on.
func sharedThenSplit(t *testing.T, sharedLen, sfLen, sqLen int32) (
	sf, sq *scriptedChain, sfTip, sqTip chainview.BlockMeta, forkHeight int32,
) {
	t.Helper()

	sf, sq = newScriptedChain(), newScriptedChain()

	var prev chainhash.Hash
	shared := chainview.BlockMeta{}
	for h := int32(0); h < sharedLen; h++ {
		meta := chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: taggedHash('s', h), Height: h},
			PrevHash: prev,
			Time:     time.Unix(int64(h)*600, 0),
		}
		sf.byHash[meta.Hash] = meta
		sq.byHash[meta.Hash] = meta
		prev = meta.Hash
		shared = meta
	}

	sfTip = sf.build('f', sharedLen, sfLen, shared.Hash)
	sqTip = sq.build('q', sharedLen, sqLen, shared.Hash)
	return sf, sq, sfTip, sqTip, shared.Height
}

func TestFindForkPointOnEqualLengthBranches(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, forkHeight := sharedThenSplit(t, 100, 5, 5)

	got, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, nil)
	if err != nil {
		t.Fatalf("FindForkPoint: %v", err)
	}
	if got.Height != forkHeight {
		t.Errorf("separation at height %d, want %d", got.Height, forkHeight)
	}
	if got.Hash != taggedHash('s', forkHeight) {
		t.Errorf("separation hash = %s, want the last shared block", got.Hash)
	}
}

// One chain far ahead of the other is the expected shape during a split, since
// the two need not advance at the same rate at all.
func TestFindForkPointWithVeryUnevenBranches(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, forkHeight := sharedThenSplit(t, 100, 50, 2)

	got, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, nil)
	if err != nil {
		t.Fatalf("FindForkPoint: %v", err)
	}
	if got.Height != forkHeight {
		t.Errorf("separation at height %d, want %d", got.Height, forkHeight)
	}

	// And the same the other way round, so neither side is privileged.
	got, err = FindForkPoint(context.Background(), sq.fetcher(), sf.fetcher(),
		sqTip, sfTip, 0, nil)
	if err != nil {
		t.Fatalf("FindForkPoint reversed: %v", err)
	}
	if got.Height != forkHeight {
		t.Errorf("reversed: separation at height %d, want %d", got.Height, forkHeight)
	}
}

func TestFindForkPointOnIdenticalTips(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, _, _ := sharedThenSplit(t, 100, 1, 1)

	got, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sfTip, 0, nil)
	if err != nil {
		t.Fatalf("FindForkPoint: %v", err)
	}
	if got.Hash != sfTip.Hash {
		t.Errorf("with identical tips the separation should be the tip itself, got %s", got.Hash)
	}
	_ = sq
}

// A separation only a few blocks above genesis exercises the guard that stops the
// walk at the start of the chain, which is otherwise only reached by two views on
// different networks.
func TestFindForkPointNearGenesis(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, forkHeight := sharedThenSplit(t, 3, 4, 4)

	got, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, nil)
	if err != nil {
		t.Fatalf("FindForkPoint: %v", err)
	}
	if got.Height != forkHeight {
		t.Errorf("separation at height %d, want %d", got.Height, forkHeight)
	}
}

func TestFindForkPointRefusesToWalkTooFar(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, _ := sharedThenSplit(t, 100, 40, 40)

	_, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 5, nil)
	if !errors.Is(err, ErrForkTooDeep) {
		t.Errorf("got %v, want ErrForkTooDeep", err)
	}
}

// A pruned view is indistinguishable from an impossibly deep separation, and both
// mean the same thing: the separation point is not known, so nothing downstream may
// assume one.
func TestFindForkPointTreatsAPrunedHeaderAsTooDeep(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, _ := sharedThenSplit(t, 100, 5, 5)
	sf.missing[taggedHash('s', 99)] = true

	_, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, nil)
	if !errors.Is(err, ErrForkTooDeep) {
		t.Errorf("got %v, want ErrForkTooDeep for a header the view no longer has", err)
	}
}

// Two views on different networks share nothing at all. Distinct from a deep
// separation, and worth its own message: the startup checks are meant to have
// caught it, so reaching here means something is badly misconfigured.
func TestFindForkPointOnChainsWithNoCommonAncestor(t *testing.T) {
	t.Parallel()

	sf, sq := newScriptedChain(), newScriptedChain()
	sfTip := sf.build('f', 0, 10, chainhash.Hash{})
	sqTip := sq.build('q', 0, 10, chainhash.Hash{})

	_, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, nil)
	if !errors.Is(err, ErrForkTooDeep) {
		t.Fatalf("got %v, want ErrForkTooDeep", err)
	}
	if err == nil || !contains(err.Error(), "not the same") {
		t.Errorf("error does not explain that the networks differ: %v", err)
	}
}

// The search runs on every tick while the chains disagree, so re-fetching the same
// ancestors each time would be wasteful against a real node.
func TestFindForkPointUsesTheCacheOnRepeatedSearches(t *testing.T) {
	t.Parallel()

	sf, sq, sfTip, sqTip, _ := sharedThenSplit(t, 100, 20, 20)
	cache := NewHeaderCache(0)

	if _, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, cache); err != nil {
		t.Fatal(err)
	}
	first := sf.fetches + sq.fetches
	if first == 0 {
		t.Fatal("the first search fetched nothing")
	}

	if _, err := FindForkPoint(context.Background(), sf.fetcher(), sq.fetcher(),
		sfTip, sqTip, 0, cache); err != nil {
		t.Fatal(err)
	}
	second := (sf.fetches + sq.fetches) - first
	if second != 0 {
		t.Errorf("the second search fetched %d headers; the cache should have covered it", second)
	}
}

func TestHeaderCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	cache := NewHeaderCache(2)
	a := chainview.BlockMeta{BlockRef: chainview.BlockRef{Hash: taggedHash('a', 1), Height: 1}}
	b := chainview.BlockMeta{BlockRef: chainview.BlockRef{Hash: taggedHash('b', 2), Height: 2}}
	c := chainview.BlockMeta{BlockRef: chainview.BlockRef{Hash: taggedHash('c', 3), Height: 3}}

	cache.Put(chainview.BranchSF, a)
	cache.Put(chainview.BranchSF, b)

	// Touching a makes b the least recently used.
	if _, ok := cache.Get(chainview.BranchSF, a.Hash); !ok {
		t.Fatal("a was not cached")
	}
	cache.Put(chainview.BranchSF, c)

	if _, ok := cache.Get(chainview.BranchSF, b.Hash); ok {
		t.Error("b survived; the least recently used should have been evicted")
	}
	if _, ok := cache.Get(chainview.BranchSF, a.Hash); !ok {
		t.Error("a was evicted despite being used more recently than b")
	}
	if cache.Len() != 2 {
		t.Errorf("cache holds %d, want its limit of 2", cache.Len())
	}
}

// Keyed by chain as well as hash, so a misconfiguration where both views point at
// the same node cannot be masked by one view's answers being served for the other.
func TestHeaderCacheKeepsTheChainsApart(t *testing.T) {
	t.Parallel()

	cache := NewHeaderCache(0)
	meta := chainview.BlockMeta{BlockRef: chainview.BlockRef{Hash: taggedHash('x', 1), Height: 1}}
	cache.Put(chainview.BranchSF, meta)

	if _, ok := cache.Get(chainview.BranchSQ, meta.Hash); ok {
		t.Error("a header cached for one chain was served for the other")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
