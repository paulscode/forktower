package chainview

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ctxT keeps the compile-time interface check below readable.
type ctxT = context.Context

func TestBranchValidityAndOther(t *testing.T) {
	t.Parallel()

	for _, ok := range []Branch{BranchSF, BranchSQ} {
		if !ok.Valid() {
			t.Errorf("branch %q should be valid", ok)
		}
	}
	for _, bad := range []Branch{"", "SF", "main", "other"} {
		if bad.Valid() {
			t.Errorf("branch %q should not be valid", bad)
		}
	}

	if got := BranchSF.Other(); got != BranchSQ {
		t.Errorf("BranchSF.Other() = %q, want %q", got, BranchSQ)
	}
	if got := BranchSQ.Other(); got != BranchSF {
		t.Errorf("BranchSQ.Other() = %q, want %q", got, BranchSF)
	}
	// An unknown branch has no opposite; returning one of the two would be a
	// guess, and guessing which chain to watch is the worst possible default.
	if got := Branch("elsewhere").Other(); got != "" {
		t.Errorf("Other() on an unknown branch = %q, want empty", got)
	}
}

func TestWhichHealthStatesCanBeComparedAgainst(t *testing.T) {
	t.Parallel()

	// A tip is worth comparing when the node's account of its own chain is its
	// own, even if it is behind.
	for _, h := range []HealthState{HealthOK, HealthDegraded} {
		if !h.Usable() {
			t.Errorf("%q should be usable: the node still knows its own chain", h)
		}
	}

	// Degraded specifically. A node with no peers, or one whose notifications
	// were dropped, is behind on its own chain rather than telling us about
	// someone else's — and refusing to compare would stop detection at exactly
	// the moment a view became unhealthy, while the dashboard said only "having
	// trouble seeing".
	if !HealthDegraded.Usable() {
		t.Error("a degraded view is still the node's own account of its own chain")
	}

	// The rest cannot be relied on, each for its own reason: mid-download a tip
	// is wherever the node has got to; an eclipsed view may be a fabrication
	// shown to this node alone; a wrong-branch view produces a clean report about
	// a chain nobody is on; and a node that is down has no tip at all.
	for _, h := range []HealthState{
		HealthSyncing, HealthEclipseSuspect, HealthWrongBranch, HealthDown,
	} {
		if h.Usable() {
			t.Errorf("%q should not count as usable for detection", h)
		}
	}
	if HealthState("SOMETHING_NEW").Usable() {
		t.Error("an unrecognised state should not be trusted for detection")
	}

	for _, h := range []HealthState{
		HealthSyncing, HealthOK, HealthDegraded,
		HealthEclipseSuspect, HealthWrongBranch, HealthDown,
	} {
		if !h.Valid() {
			t.Errorf("%q should be a valid state", h)
		}
	}
	for _, bad := range []HealthState{"", "ok", "FINE"} {
		if bad.Valid() {
			t.Errorf("%q should not be a valid state", bad)
		}
	}
}

func TestWatchSet(t *testing.T) {
	t.Parallel()

	var hash chainhash.Hash
	hash[0] = 0xab
	op := wire.OutPoint{Hash: hash, Index: 1}

	empty := WatchSet{}
	if !empty.Empty() {
		t.Error("a zero WatchSet should report empty")
	}
	if empty.HasOutpoint(op) {
		t.Error("an empty WatchSet matched an outpoint")
	}

	ws := WatchSet{
		Outpoints: map[wire.OutPoint]struct{}{op: {}},
		Scripts:   [][]byte{{0x00, 0x20}},
	}
	if ws.Empty() {
		t.Error("a populated WatchSet reported empty")
	}
	if !ws.HasOutpoint(op) {
		t.Error("HasOutpoint missed a watched outpoint")
	}
	if ws.HasOutpoint(wire.OutPoint{Hash: hash, Index: 2}) {
		t.Error("HasOutpoint matched a different index on the same transaction")
	}

	// Scripts alone is non-empty: a light backend has only the scripts, and
	// treating that as nothing to watch would silently disable it.
	if (WatchSet{Scripts: [][]byte{{0x51}}}).Empty() {
		t.Error("a WatchSet with only scripts reported empty")
	}
}

func TestChainTipRejected(t *testing.T) {
	t.Parallel()

	// A node that fetched a block and refused it is the strongest evidence of a
	// rule disagreement available, and it needs no agreement from any peer. Merely
	// not having pursued a branch says nothing.
	if !(ChainTip{Status: TipInvalid}).Rejected() {
		t.Error("an invalid tip should count as rejected")
	}
	for _, s := range []string{TipActive, TipHeadersOnly, TipValidHeaders, TipValidFork, TipUnknown, ""} {
		if (ChainTip{Status: s}).Rejected() {
			t.Errorf("status %q should not count as rejected", s)
		}
	}
}

// The interface is the contract every backend must satisfy, so it is worth a
// compile-time check that it can actually be implemented as written — a method
// set that cannot be satisfied would otherwise surface only when the first
// backend is attempted. These stubs return usable zero values rather than nil
// pairs, so the type is a legitimate implementation even though nothing calls it.
type staticCheck struct{}

var _ ChainView = staticCheck{}

func (staticCheck) BestBlock(_ ctxT) (BlockMeta, error) { return BlockMeta{}, nil }
func (staticCheck) BlockHeaderByHash(_ ctxT, _ chainhash.Hash) (BlockMeta, error) {
	return BlockMeta{}, nil
}
func (staticCheck) BlockHashByHeight(_ ctxT, _ int32) (chainhash.Hash, error) {
	return chainhash.Hash{}, nil
}
func (staticCheck) Block(_ ctxT, _ chainhash.Hash) (*wire.MsgBlock, error) {
	return &wire.MsgBlock{}, nil
}
func (staticCheck) MatchBlock(_ ctxT, _ chainhash.Hash, _ WatchSet) (bool, error) {
	return true, nil
}
func (staticCheck) SubscribeTip(_ ctxT) (<-chan BlockMeta, error) {
	ch := make(chan BlockMeta)
	close(ch)
	return ch, nil
}
func (staticCheck) SubscribeMempoolTx(_ ctxT) (<-chan *wire.MsgTx, error) { return nil, ErrUnsupported }
func (staticCheck) Broadcast(_ ctxT, _ *wire.MsgTx) error                 { return nil }
func (staticCheck) Health(_ ctxT) (BackendHealth, error)                  { return BackendHealth{}, nil }
