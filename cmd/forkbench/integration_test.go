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

// The scenario this whole project exists for, staged end to end: a channel, a
// counterparty rolled back to a state it had already revoked, and the commitment
// it then publishes landing on one chain and not the other.
//
// Slow — several minutes, two Bitcoin nodes and two Lightning nodes — and worth
// every second of it. Everything else about the watcher is checked against
// transactions this project builds; this is the only thing that checks the world
// itself behaves the way the daemon assumes.
func TestABreachOnOneChainOnly(t *testing.T) {
	ctx := testContext(t)

	if err := commandUp(ctx); err != nil {
		t.Fatalf("bringing the world up: %v", err)
	}
	t.Cleanup(func() {
		if err := commandDown(context.Background()); err != nil {
			t.Errorf("removing the world: %v", err)
		}
	})

	if err := commandLNUp(ctx); err != nil {
		t.Fatalf("bringing the Lightning layer up: %v", err)
	}

	user := lnNodes()[0]
	channels, err := user.channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("opened %d channels, want 1", len(channels))
	}
	fundingTxid, fundingVout, err := channels[0].fundingOutpoint()
	if err != nil {
		t.Fatal(err)
	}

	// Payments before the snapshot and after it. The ones after are what make the
	// saved state a *revoked* one rather than merely an old one.
	if err := commandPay(ctx, 2); err != nil {
		t.Fatalf("paying: %v", err)
	}
	if err := commandSnapshotMallory(ctx); err != nil {
		t.Fatalf("saving the counterparty's state: %v", err)
	}
	if err := commandPay(ctx, 2); err != nil {
		t.Fatalf("paying again: %v", err)
	}

	if err := commandSplit(ctx); err != nil {
		t.Fatalf("splitting: %v", err)
	}
	if err := commandBreach(ctx, nodeSQ, ""); err != nil {
		t.Fatalf("staging the breach: %v", err)
	}

	// The assertion that matters: the funding output is spent on one chain and
	// untouched on the other. That asymmetry is the entire threat model.
	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)

	spentOnSQ, err := outpointSpentInABlock(ctx, sq, fundingTxid, fundingVout)
	if err != nil {
		t.Fatal(err)
	}
	if !spentOnSQ {
		t.Error("the commitment did not confirm on the chain it was published to")
	}

	spentOnSF, err := outpointSpentInABlock(ctx, sf, fundingTxid, fundingVout)
	if err != nil {
		t.Fatal(err)
	}
	if spentOnSF {
		t.Error("the commitment confirmed on the user's own chain too, which is not " +
			"the scenario: the whole point is that their node sees nothing")
	}
}

// outpointSpentInABlock reports whether a node has a *confirmed* spend of an
// outpoint. Confirmed specifically: a transaction sitting in a memory pool is
// not a fact about the chain, and treating it as one would make this test pass
// for the wrong reason.
func outpointSpentInABlock(
	ctx context.Context, c *client, txid string, vout uint32,
) (bool, error) {
	var out *struct {
		Confirmations int64 `json:"confirmations"`
	}
	// A null answer means the output is unspent as far as this node knows;
	// gettxout ignores the memory pool when asked to.
	if err := c.call(ctx, "gettxout", []any{txid, vout, false}, &out); err != nil {
		return false, err
	}
	return out == nil, nil
}
