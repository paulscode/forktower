package watcher

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// Match is one watched outpoint, spent.
type Match struct {
	// Target is what was being watched, carried whole so the caller knows which
	// channel it belongs to without a second lookup.
	Target Target
	// Tx is the transaction that spent it.
	Tx *wire.MsgTx
	// TxID is that transaction's id, computed once here because every caller
	// wants it and computing it is not free.
	TxID chainhash.Hash
	// TxIndex is where the transaction sits in the block, and InputIndex which of
	// its inputs did the spending. Both are needed to point at the exact thing
	// that happened when explaining it afterwards.
	TxIndex    int
	InputIndex int
}

// ScanBlock finds every watched outpoint spent by a block.
//
// Pure by construction: no storage, no network, no clock, no logging. It is
// given a block and a set and returns what it found, which is what makes the
// scenarios that matter testable against crafted blocks rather than against a
// running chain.
//
// Results come back in block order — transactions as they appear, inputs by
// index — so the same block and the same set always produce the same list. That
// is not cosmetic: these become timeline entries a person reads afterwards, and
// an order that varied between runs would make two records of one event look
// like two events.
//
// One pass over a fixed set. If a transaction in this block spends an output
// created by another transaction in the same block, the second spend is only
// found once the set contains that output — which the caller cannot know until
// it has seen the first match. Scanning is cheap and this function is
// idempotent, so the answer is to scan again with the larger set; a caller that
// does so gets both, and a caller that does not gets the second one on the
// rescan rather than never.
func ScanBlock(blk *wire.MsgBlock, ws WatchSet) []Match {
	if blk == nil || ws.Empty() {
		return nil
	}

	var out []Match
	for txIndex, tx := range blk.Transactions {
		out = append(out, scanTx(tx, ws, txIndex)...)
	}
	return out
}

// ScanTx finds every watched outpoint one transaction spends.
//
// The same reading as ScanBlock, for a transaction that is not in a block yet.
// Sharing the matching means an unconfirmed sighting and the confirmation that
// follows it cannot disagree about what was spent — which they would eventually,
// if this were written twice.
func ScanTx(tx *wire.MsgTx, ws WatchSet) []Match {
	if ws.Empty() {
		return nil
	}
	return scanTx(tx, ws, 0)
}

func scanTx(tx *wire.MsgTx, ws WatchSet, txIndex int) []Match {
	// A coinbase spends nothing: its single input names the empty outpoint.
	// Skipped explicitly rather than relied upon not to match, because a watchset
	// that ever contained a zero outpoint would otherwise match the coinbase of
	// every block ever scanned.
	if tx == nil || isCoinbase(tx) {
		return nil
	}

	var out []Match
	for inputIndex, in := range tx.TxIn {
		target, watched := ws.Lookup(in.PreviousOutPoint)
		if !watched {
			continue
		}
		out = append(out, Match{
			Target:     target,
			Tx:         tx,
			TxID:       tx.TxHash(),
			TxIndex:    txIndex,
			InputIndex: inputIndex,
		})
	}
	return out
}

// isCoinbase reports whether a transaction is a block's reward transaction.
//
// Identified by its input rather than by its position, so a malformed block that
// puts one elsewhere — or none first — is still read correctly. A coinbase has
// exactly one input, whose previous outpoint is the empty hash with an index of
// all ones.
func isCoinbase(tx *wire.MsgTx) bool {
	if tx == nil || len(tx.TxIn) != 1 {
		return false
	}
	prev := tx.TxIn[0].PreviousOutPoint
	return prev.Index == maxUint32 && prev.Hash == (chainhash.Hash{})
}

const maxUint32 = ^uint32(0)
