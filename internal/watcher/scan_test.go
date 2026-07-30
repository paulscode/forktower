package watcher

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// outpoint builds a recognisable outpoint from a small number, so a failure
// message names something a person can find in the test rather than 64 hex
// characters they have to compare by eye.
func outpoint(t *testing.T, n byte, vout uint32) wire.OutPoint {
	t.Helper()
	var h chainhash.Hash
	for i := range h {
		h[i] = n
	}
	return wire.OutPoint{Hash: h, Index: vout}
}

// spend builds a transaction spending the given outpoints, plus one output so
// it is not degenerate.
func spend(prevouts ...wire.OutPoint) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, op := range prevouts {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op, Sequence: wire.MaxTxInSequenceNum})
	}
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	return tx
}

// coinbase builds a block's reward transaction, whose input names no outpoint.
func coinbase() *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{}, Index: ^uint32(0)},
		SignatureScript:  []byte{0x03, 0x01, 0x02, 0x03},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(wire.NewTxOut(625_000_000, []byte{0x51}))
	return tx
}

func block(t *testing.T, txs ...*wire.MsgTx) *wire.MsgBlock {
	t.Helper()
	blk := wire.NewMsgBlock(&wire.BlockHeader{Version: 1})
	for _, tx := range txs {
		if err := blk.AddTransaction(tx); err != nil {
			t.Fatalf("building a block: %v", err)
		}
	}
	return blk
}

func funding(op wire.OutPoint, channelID int64) Target {
	return Target{Outpoint: op, Kind: KindFunding, ChannelID: channelID}
}

func TestABlockWithNothingWatchedInItMatchesNothing(t *testing.T) {
	t.Parallel()

	watched := outpoint(t, 0x11, 0)
	other := outpoint(t, 0x22, 0)

	got := ScanBlock(block(t, coinbase(), spend(other)), NewWatchSet(funding(watched, 1)))
	if len(got) != 0 {
		t.Errorf("matched %d things in a block that spends nothing we watch", len(got))
	}
}

func TestAFundingSpendIsFound(t *testing.T) {
	t.Parallel()

	watched := outpoint(t, 0x11, 0)
	tx := spend(outpoint(t, 0x99, 3), watched)

	got := ScanBlock(block(t, coinbase(), spend(outpoint(t, 0x22, 0)), tx),
		NewWatchSet(funding(watched, 7)))
	if len(got) != 1 {
		t.Fatalf("found %d matches, want 1", len(got))
	}
	m := got[0]
	if m.Target.ChannelID != 7 || m.Target.Kind != KindFunding {
		t.Errorf("matched the wrong target: %+v", m.Target)
	}
	if m.TxID != tx.TxHash() {
		t.Errorf("named the wrong transaction")
	}
	if m.TxIndex != 2 {
		t.Errorf("transaction index = %d, want 2", m.TxIndex)
	}
	// The second input is the watched one, and pointing at the first would
	// misdescribe the event to anyone reading it afterwards.
	if m.InputIndex != 1 {
		t.Errorf("input index = %d, want 1", m.InputIndex)
	}
}

func TestSeveralSpendsInOneBlockAreAllFound(t *testing.T) {
	t.Parallel()

	a, b, c := outpoint(t, 0x11, 0), outpoint(t, 0x22, 1), outpoint(t, 0x33, 2)
	first := spend(a)
	second := spend(outpoint(t, 0x44, 0), b)
	third := spend(c)

	ws := NewWatchSet(funding(a, 1), funding(b, 2), funding(c, 3))
	got := ScanBlock(block(t, coinbase(), first, second, third), ws)
	if len(got) != 3 {
		t.Fatalf("found %d matches, want 3", len(got))
	}
	// Block order, not map order. These become timeline entries, and an order
	// that varied between runs would make one event look like several.
	for i, want := range []int64{1, 2, 3} {
		if got[i].Target.ChannelID != want {
			t.Errorf("match %d is channel %d, want %d", i, got[i].Target.ChannelID, want)
		}
	}
}

// One transaction can spend two watched outpoints — a sweep taking several
// commitment outputs at once. Both must be reported: they are two things that
// happened, even though one transaction did them.
func TestOneTransactionSpendingTwoWatchedOutpointsReportsBoth(t *testing.T) {
	t.Parallel()

	a, b := outpoint(t, 0x11, 0), outpoint(t, 0x11, 1)
	tx := spend(a, outpoint(t, 0x99, 0), b)

	ws := NewWatchSet(
		Target{Outpoint: a, Kind: KindCommitmentOutput, Role: store.RoleToLocal},
		Target{Outpoint: b, Kind: KindCommitmentOutput, Role: store.RoleHTLC},
	)
	got := ScanBlock(block(t, coinbase(), tx), ws)
	if len(got) != 2 {
		t.Fatalf("found %d matches, want 2", len(got))
	}
	if got[0].Target.Role != store.RoleToLocal || got[1].Target.Role != store.RoleHTLC {
		t.Errorf("roles came back as %q and %q", got[0].Target.Role, got[1].Target.Role)
	}
	if got[0].InputIndex != 0 || got[1].InputIndex != 2 {
		t.Errorf("input indexes %d and %d", got[0].InputIndex, got[1].InputIndex)
	}
}

// The case that needs two passes: a commitment confirms and something spends one
// of its outputs in the very same block. The first pass cannot know about the
// output, because it did not exist until the first transaction was read. Scanning
// again with the larger set finds it, which is what the live loop must do.
func TestASpendInTheSameBlockIsFoundOnTheSecondPass(t *testing.T) {
	t.Parallel()

	fundingOP := outpoint(t, 0x11, 0)
	commitment := spend(fundingOP)
	commitmentID := commitment.TxHash()
	toLocal := wire.OutPoint{Hash: commitmentID, Index: 0}
	justice := spend(toLocal)

	blk := block(t, coinbase(), commitment, justice)

	first := ScanBlock(blk, NewWatchSet(funding(fundingOP, 4)))
	if len(first) != 1 || first[0].Target.Kind != KindFunding {
		t.Fatalf("the first pass found %d matches, want the funding spend", len(first))
	}

	// What the live loop does next: record the commitment, learn its outputs, and
	// look again.
	second := ScanBlock(blk, NewWatchSet(
		funding(fundingOP, 4),
		Target{Outpoint: toLocal, Kind: KindCommitmentOutput, Role: store.RoleToLocal},
	))
	if len(second) != 2 {
		t.Fatalf("the second pass found %d matches, want 2", len(second))
	}
	if second[1].TxID != justice.TxHash() {
		t.Error("the second pass did not find the spend of the commitment's output")
	}
	// And the funding spend is reported identically both times, so re-scanning
	// cannot turn one event into two different-looking ones.
	if first[0].TxID != second[0].TxID || first[0].InputIndex != second[0].InputIndex {
		t.Error("re-scanning the same block described the same spend differently")
	}
}

// A coinbase names the empty outpoint. If that ever got into a watchset — from a
// corrupt row, say — every block ever scanned would look like a spend of a
// channel, which is the loudest possible false alarm.
func TestACoinbaseIsNeverAMatch(t *testing.T) {
	t.Parallel()

	empty := wire.OutPoint{Hash: chainhash.Hash{}, Index: ^uint32(0)}
	ws := NewWatchSet(funding(empty, 1))

	if got := ScanBlock(block(t, coinbase()), ws); len(got) != 0 {
		t.Errorf("a coinbase matched %d watched outpoints", len(got))
	}

	// And it is identified by its input rather than by being first, so a block
	// that puts one elsewhere is still read correctly.
	if got := ScanBlock(block(t, spend(outpoint(t, 0x11, 0)), coinbase()), ws); len(got) != 0 {
		t.Errorf("a coinbase in second position matched %d watched outpoints", len(got))
	}
}

func TestNothingToScanIsNotAnError(t *testing.T) {
	t.Parallel()

	ws := NewWatchSet(funding(outpoint(t, 0x11, 0), 1))
	if got := ScanBlock(nil, ws); got != nil {
		t.Errorf("scanning no block returned %v", got)
	}
	if got := ScanBlock(block(t, coinbase()), WatchSet{}); got != nil {
		t.Errorf("scanning with an empty watchset returned %v", got)
	}
	if got := ScanBlock(block(t), ws); got != nil {
		t.Errorf("scanning an empty block returned %v", got)
	}
}

// The same outpoint index on different transactions, and the same transaction
// with different indexes, must not be confused. This is the mistake that would
// make Forktower report the wrong channel.
func TestOutpointsAreDistinguishedByBothHalves(t *testing.T) {
	t.Parallel()

	sameTx0 := outpoint(t, 0x11, 0)
	sameTx1 := outpoint(t, 0x11, 1)
	otherTx0 := outpoint(t, 0x22, 0)

	ws := NewWatchSet(funding(sameTx1, 42))

	if got := ScanBlock(block(t, coinbase(), spend(sameTx0)), ws); len(got) != 0 {
		t.Error("a different output of the same transaction matched")
	}
	if got := ScanBlock(block(t, coinbase(), spend(otherTx0)), ws); len(got) != 0 {
		t.Error("the same output index of a different transaction matched")
	}
	if got := ScanBlock(block(t, coinbase(), spend(sameTx1)), ws); len(got) != 1 {
		t.Error("the outpoint being watched did not match")
	}
}

// Scanning must not depend on how many blocks came before, so the same inputs
// always give the same answer.
func TestScanningIsRepeatable(t *testing.T) {
	t.Parallel()

	a, b := outpoint(t, 0x11, 0), outpoint(t, 0x22, 0)
	blk := block(t, coinbase(), spend(a), spend(outpoint(t, 0x33, 0)), spend(b))
	ws := NewWatchSet(funding(a, 1), funding(b, 2))

	want := fmt.Sprint(ScanBlock(blk, ws))
	for range 20 {
		if got := fmt.Sprint(ScanBlock(blk, ws)); got != want {
			t.Fatalf("scanning the same block twice gave different answers:\n%s\n%s", want, got)
		}
	}
}
