//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests drive real Bitcoin nodes in containers, so they are behind a build
// tag and run by `make integration`. They are the only thing that proves the
// world this tool builds actually behaves the way the daemon needs it to.

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// The whole point of the tool: two nodes that agree, then do not, and stay that
// way.
func TestUpThenSplit(t *testing.T) {
	ctx := testContext(t)

	if err := commandUp(ctx); err != nil {
		t.Fatalf("bringing the world up: %v", err)
	}
	t.Cleanup(func() {
		if err := commandDown(context.Background()); err != nil {
			t.Errorf("removing the world: %v", err)
		}
	})

	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)

	before, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqBefore, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Best != sqBefore.Best {
		t.Fatalf("the world came up already split: %s vs %s", before.Best, sqBefore.Best)
	}
	if before.Blocks < initialBlocks {
		t.Errorf("height %d, want at least %d so coinbases have matured",
			before.Blocks, initialBlocks)
	}

	if err := commandSplit(ctx); err != nil {
		t.Fatalf("splitting: %v", err)
	}

	sfAfter, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqAfter, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sfAfter.Best == sqAfter.Best {
		t.Fatal("the two nodes still agree after a split")
	}

	// The part that makes this a split rather than two chains that have merely
	// not met: one node has *rejected* the other's block. Two chains that have
	// only lost sight of each other merge the moment they reconnect, which would
	// make this world lie about what it demonstrates.
	tips, err := sf.chainTips(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rejected bool
	for _, tip := range tips {
		if tip.Status == "invalid" {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("no branch was rejected, so the split would heal on reconnection: %+v", tips)
	}

	// And the daemon's own view of it: the rejected branch is what its detection
	// reads as direct, local evidence of a rule disagreement.
	if got := describeBranches(tips); !strings.Contains(got, "rejected") {
		t.Errorf("status would not show the rejection: %q", got)
	}
}

// Every command is written to be run again, because the first thing anyone does
// with a tool like this is run it twice.
func TestSplittingTwiceChangesNothing(t *testing.T) {
	ctx := testContext(t)

	if err := commandUp(ctx); err != nil {
		t.Fatalf("bringing the world up: %v", err)
	}
	t.Cleanup(func() { _ = commandDown(context.Background()) })

	if err := commandSplit(ctx); err != nil {
		t.Fatalf("splitting: %v", err)
	}

	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)
	sfFirst, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqFirst, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := commandSplit(ctx); err != nil {
		t.Fatalf("splitting a second time: %v", err)
	}

	sfSecond, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqSecond, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sfFirst.Best != sfSecond.Best || sqFirst.Best != sqSecond.Best {
		t.Error("splitting an already-split world moved the chains")
	}

	// And `up` against a split world leaves it split rather than quietly
	// reconnecting the nodes underneath whoever is using it.
	if err := commandUp(ctx); err != nil {
		t.Fatalf("bringing an already-split world up: %v", err)
	}
	sfThird, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqThird, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sfThird.Best == sqThird.Best {
		t.Error("`up` healed a split someone may have been in the middle of demonstrating")
	}
}

// Mining is how a split is made to grow, which is what turns a difference of one
// block into something the daemon will believe.
func TestMiningGrowsOneChainOnly(t *testing.T) {
	ctx := testContext(t)

	if err := commandUp(ctx); err != nil {
		t.Fatalf("bringing the world up: %v", err)
	}
	t.Cleanup(func() { _ = commandDown(context.Background()) })
	if err := commandSplit(ctx); err != nil {
		t.Fatalf("splitting: %v", err)
	}

	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)
	sqBefore, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := commandMine(ctx, nodeSF, 5); err != nil {
		t.Fatalf("mining: %v", err)
	}

	sqAfter, err := sq.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sqAfter.Best != sqBefore.Best {
		t.Error("mining on one chain moved the other")
	}

	sfAfter, err := sf.chainInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sfAfter.Blocks != sqBefore.Blocks+5 {
		t.Errorf("height %d after mining 5, want %d", sfAfter.Blocks, sqBefore.Blocks+5)
	}

	if err := commandMine(ctx, "nowhere", 1); err == nil {
		t.Error("mining on a node that does not exist was accepted")
	}
	if err := commandMine(ctx, nodeSQ, 0); err == nil {
		t.Error("mining zero blocks was accepted")
	}
}
