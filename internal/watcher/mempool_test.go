package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// The early warning, and the whole reason this mode exists. Seeing a commitment
// before it confirms buys the user a block of notice they would not otherwise
// have had — which on a chain with slow blocks can be a great deal of time.
func TestACloseIsNoticedBeforeAnyBlockAcceptsIt(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	id := addChannel(t, h.store, fundingA, store.Relevant)
	sightings := h.bus.Subscribe("test", bus.KindMempoolSighting)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	h.view.InjectMempoolTx(commitment)

	select {
	case e := <-sightings:
		got, ok := e.(bus.MempoolSighting)
		if !ok {
			t.Fatalf("got %T", e)
		}
		if got.ChannelID != id {
			t.Errorf("announced channel %d, want %d", got.ChannelID, id)
		}
		if got.SpendTxid != commitment.TxHash().String() {
			t.Error("announced the wrong transaction")
		}
		// The shape is worked out the same way as for a confirmed spend, so the
		// warning says what is coming rather than only that something is.
		if got.Shape != string(store.ShapeCommitmentUnknown) {
			t.Errorf("announced shape %q", got.Shape)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unconfirmed close of a channel was never announced")
	}

	h.waitFor("the sighting to be recorded", func() bool { return len(h.spends()) == 1 })
	sp := h.spends()[0]
	if sp.Status != store.SpendMempool {
		t.Errorf("recorded with status %q, want it marked as not yet confirmed", sp.Status)
	}
	// It is a sighting, not a fact about the chain, so it names no block.
	if sp.BlockHash != "" || sp.BlockHeight != 0 {
		t.Errorf("an unconfirmed sighting was given a block: %+v", sp)
	}
}

// The block that confirms a sighting updates that same record. One thing
// happened; the timeline should not say two did.
func TestConfirmingASightingUpdatesTheSameRecord(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	h.view.InjectMempoolTx(commitment)
	h.waitFor("the sighting", func() bool { return len(h.spends()) == 1 })
	first := h.spends()[0].ID

	meta := h.view.ExtendWith("confirmed", coinbase(), commitment)

	h.waitFor("the sighting to be confirmed", func() bool {
		got := h.spends()
		return len(got) == 1 && got[0].Status == store.SpendConfirmed
	})

	got := h.spends()
	if len(got) != 1 {
		t.Fatalf("one close became %d records", len(got))
	}
	if got[0].ID != first {
		t.Error("confirming a sighting replaced the record rather than updating it")
	}
	if got[0].BlockHeight != meta.Height {
		t.Errorf("confirmed at height %d, want %d", got[0].BlockHeight, meta.Height)
	}
}

// A transaction that spends nothing of ours is not our business, however
// interesting it looks.
func TestAnUnrelatedTransactionIsIgnored(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.view.InjectMempoolTx(commitmentSpending(fundingOutpoint(t, fundingB, 1)))

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if len(h.spends()) != 0 {
		t.Error("a transaction spending nothing of ours was recorded")
	}
}

// Seeing the same transaction twice — which happens, because a memory pool
// re-announces — is one sighting.
func TestTheSameSightingTwiceIsOneRecord(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	for range 4 {
		h.view.InjectMempoolTx(commitment)
	}

	h.waitFor("the sighting", func() bool { return len(h.spends()) == 1 })
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // letting the repeats land
	if got := h.spends(); len(got) != 1 {
		t.Errorf("four announcements of one transaction produced %d records", len(got))
	}
}

// A sighting from a chain the daemon is unsure of is worse than none, for the
// same reason a confirmed spend from one would be.
func TestNoSightingsWhileTheDaemonIsUnsureOfTheChain(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.guard.paused.Store(true)
	h.view.InjectMempoolTx(commitmentSpending(fundingOutpoint(t, fundingA, 1)))

	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // proving an absence
	if len(h.spends()) != 0 {
		t.Error("an unconfirmed sighting was taken from a chain the daemon was unsure of")
	}
}

// Nothing is followed up from an unconfirmed commitment: its outputs do not
// exist yet, and might never.
func TestAnUnconfirmedCommitmentLeavesNothingToWatchYet(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	h.view.InjectMempoolTx(commitmentSpending(fundingOutpoint(t, fundingA, 1)))
	h.waitFor("the sighting", func() bool { return len(h.spends()) == 1 })

	watched, err := h.store.ListWatchOutpoints(context.Background(), store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != 0 {
		t.Errorf("started watching %d outputs that do not exist yet", len(watched))
	}
}

// A backend with no view of a memory pool is an absence, not a failure. A light
// client genuinely cannot see one, and refusing to start over it would be
// refusing to do the part of the job that does not need it.
func TestABackendWithNoMemoryPoolStillWatchesBlocks(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.view.Fail("SubscribeMempoolTx", chainview.ErrUnsupported)
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	h.plant("spent", fundingOutpoint(t, fundingA, 1))
	h.waitFor("the confirmed spend", func() bool { return len(h.spends()) == 1 })
}

// A subscription that fails for some other reason costs the early warning and
// nothing else.
func TestAFailedMemoryPoolSubscriptionCostsOnlyTheEarlyWarning(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.view.Fail("SubscribeMempoolTx", errors.New("the node is not answering"))
	h.run()

	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })
	h.plant("spent", fundingOutpoint(t, fundingA, 1))
	h.waitFor("the confirmed spend", func() bool { return len(h.spends()) == 1 })
}

// The loose-transaction scan and the block scan must agree about what was spent,
// which is why they are the same code.
func TestScanningATransactionAgreesWithScanningTheBlockItLandsIn(t *testing.T) {
	t.Parallel()

	watched := outpoint(t, 0x11, 0)
	tx := spend(outpoint(t, 0x99, 0), watched)
	ws := NewWatchSet(funding(watched, 7))

	loose := ScanTx(tx, ws)
	inBlock := ScanBlock(block(t, coinbase(), tx), ws)

	if len(loose) != 1 || len(inBlock) != 1 {
		t.Fatalf("found %d loose and %d in a block", len(loose), len(inBlock))
	}
	if loose[0].TxID != inBlock[0].TxID ||
		loose[0].InputIndex != inBlock[0].InputIndex ||
		loose[0].Target.ChannelID != inBlock[0].Target.ChannelID {
		t.Error("the same spend was read differently loose and in a block")
	}
}

func TestScanningNothingFindsNothing(t *testing.T) {
	t.Parallel()

	ws := NewWatchSet(funding(outpoint(t, 0x11, 0), 1))
	if got := ScanTx(nil, ws); got != nil {
		t.Errorf("scanning no transaction returned %v", got)
	}
	if got := ScanTx(spend(outpoint(t, 0x11, 0)), WatchSet{}); got != nil {
		t.Errorf("scanning with an empty watchset returned %v", got)
	}
	if got := ScanTx(coinbase(), NewWatchSet(funding(
		wire.OutPoint{Index: ^uint32(0)}, 1))); got != nil {
		t.Errorf("a coinbase matched: %v", got)
	}
}

// A commitment noticed before it confirmed must still be followed up once it
// does. On a full node this is the ordinary path, not the unusual one — the
// memory pool sees almost everything first — so getting it wrong means the
// outcome of an attack is never reported at all.
func TestACommitmentSeenEarlyIsStillFollowedUp(t *testing.T) {
	t.Parallel()
	h := newLiveHarness(t, nil)
	addChannel(t, h.store, fundingA, store.Relevant)
	h.run()
	h.waitFor("the starting point", func() bool { return h.w.Progress().Height > 0 })

	commitment := commitmentSpending(fundingOutpoint(t, fundingA, 1))
	h.view.InjectMempoolTx(commitment)
	h.waitFor("the sighting", func() bool { return len(h.spends()) == 1 })

	// Nothing yet: an unconfirmed commitment has no outputs to watch.
	watched, err := h.store.ListWatchOutpoints(context.Background(), store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if len(watched) != 0 {
		t.Fatalf("started watching %d outputs before they existed", len(watched))
	}

	h.view.ExtendWith("confirmed", coinbase(), commitment)

	h.waitFor("the commitment's outputs to be watched", func() bool {
		got, listErr := h.store.ListWatchOutpoints(context.Background(), store.BranchSQ)
		return listErr == nil && len(got) == len(commitment.TxOut)
	})

	// And the answer to it is then found, which is the thing that would have been
	// lost entirely.
	justice := justiceSpending(wire.OutPoint{Hash: commitment.TxHash(), Index: 0})
	h.view.ExtendWith("justice", coinbase(), justice)

	h.waitFor("the justice transaction", func() bool {
		for _, sp := range h.spends() {
			if sp.SpendTxID == justice.TxHash().String() {
				return sp.Shape == store.ShapeJustice
			}
		}
		return false
	})
}
