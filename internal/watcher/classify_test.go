package watcher

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// Script shapes, spelled out once so the tests read as the thing they mean.
var (
	p2wpkh = append([]byte{0x00, 0x14}, make([]byte, 20)...)
	p2wsh  = append([]byte{0x00, 0x20}, make([]byte, 32)...)
	p2tr   = append([]byte{0x51, 0x20}, make([]byte, 32)...)
	p2pkh  = append([]byte{0x76, 0xa9, 0x14}, append(make([]byte, 20), 0x88, 0xac)...)
)

// fundingSpendWitness is what any spend of a channel's funding output carries:
// the two-of-two multisig satisfaction. Shared by a cooperative close and a
// commitment alike, which is exactly why it cannot tell them apart.
func fundingSpendWitness() wire.TxWitness {
	return wire.TxWitness{{}, {0x30, 0x44}, {0x30, 0x44}, {0x52, 0x21}}
}

// closeTx builds a transaction spending the funding output, which the callers
// then bend into the shape they are testing.
func closeTx(outputs ...*wire.TxOut) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Witness:          fundingSpendWitness(),
		Sequence:         wire.MaxTxInSequenceNum,
	})
	for _, o := range outputs {
		tx.AddTxOut(o)
	}
	return tx
}

func TestWhatAFundingSpendLooksLike(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func() *wire.MsgTx
		ours  string
		txid  string
		want  store.SpendShape
	}{
		{
			name: "a close both sides agreed to, paying each of them",
			build: func() *wire.MsgTx {
				return closeTx(wire.NewTxOut(500_000, p2wpkh), wire.NewTxOut(400_000, p2tr))
			},
			want: store.ShapeMutualClose,
		},
		{
			name: "a close both sides agreed to, paying only one of them",
			build: func() *wire.MsgTx {
				return closeTx(wire.NewTxOut(900_000, p2wpkh))
			},
			want: store.ShapeMutualClose,
		},
		{
			// The clearest mark a commitment leaves. Nothing else in a channel
			// close is 330 satoshis.
			name: "a commitment, given away by its fee-bumping outputs",
			build: func() *wire.MsgTx {
				return closeTx(
					wire.NewTxOut(500_000, p2wsh),
					wire.NewTxOut(AnchorValueSat, p2wsh),
				)
			},
			want: store.ShapeCommitmentUnknown,
		},
		{
			// A channel opened before anchors existed has none, and then the
			// commitment number hidden in the locktime is what gives it away.
			name: "a commitment with no anchors, given away by its locktime",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(500_000, p2wsh), wire.NewTxOut(400_000, p2wpkh))
				tx.LockTime = 0x20_12_34_56
				return tx
			},
			want: store.ShapeCommitmentUnknown,
		},
		{
			name: "a commitment given away by its input sequence",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(900_000, p2wsh))
				tx.TxIn[0].Sequence = 0x80_ab_cd_ef
				return tx
			},
			want: store.ShapeCommitmentUnknown,
		},
		{
			// The one case where "whose commitment is it" has an answer: the
			// user's own node said it broadcast this one.
			name: "our own commitment, because our node said so",
			build: func() *wire.MsgTx {
				return closeTx(
					wire.NewTxOut(500_000, p2wsh),
					wire.NewTxOut(AnchorValueSat, p2wsh),
				)
			},
			ours: "self",
			txid: "self",
			want: store.ShapeCommitmentOurs,
		},
		{
			name: "a commitment our node did not broadcast stays unattributed",
			build: func() *wire.MsgTx {
				return closeTx(
					wire.NewTxOut(500_000, p2wsh),
					wire.NewTxOut(AnchorValueSat, p2wsh),
				)
			},
			ours: "something-else",
			txid: "this-one",
			want: store.ShapeCommitmentUnknown,
		},
		{
			name: "three outputs is not a cooperative close",
			build: func() *wire.MsgTx {
				return closeTx(
					wire.NewTxOut(300_000, p2wpkh),
					wire.NewTxOut(300_000, p2wpkh),
					wire.NewTxOut(300_000, p2wpkh),
				)
			},
			want: store.ShapeUnknown,
		},
		{
			name: "an output nobody pays out to is not a cooperative close",
			build: func() *wire.MsgTx {
				return closeTx(wire.NewTxOut(900_000, p2pkh))
			},
			want: store.ShapeUnknown,
		},
		{
			name: "a witness that did not come from a channel at all",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(900_000, p2wpkh))
				tx.TxIn[0].Witness = wire.TxWitness{{0x30}, {0x02}}
				return tx
			},
			want: store.ShapeUnknown,
		},
		{
			// The empty first item is the multisig off-by-one. Without it this is
			// some other four-item witness.
			name: "a four-item witness whose first item is not empty",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(900_000, p2wpkh))
				tx.TxIn[0].Witness = wire.TxWitness{{0x01}, {0x30}, {0x30}, {0x52}}
				return tx
			},
			want: store.ShapeUnknown,
		},
		{
			name: "a locktime that is a date rather than a height",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(900_000, p2wpkh))
				tx.LockTime = 1_700_000_000
				return tx
			},
			want: store.ShapeUnknown,
		},
		{
			name: "a transaction spending more than the funding output",
			build: func() *wire.MsgTx {
				tx := closeTx(wire.NewTxOut(900_000, p2wpkh))
				tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}})
				return tx
			},
			want: store.ShapeUnknown,
		},
		{
			name:  "no transaction at all",
			build: func() *wire.MsgTx { return nil },
			want:  store.ShapeUnknown,
		},
		{
			name: "no outputs at all",
			build: func() *wire.MsgTx {
				return closeTx()
			},
			want: store.ShapeUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyShape(ShapeFacts{
				Tx: tc.build(), TxID: tc.txid, OurCloseTxID: tc.ours,
			})
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Transaction ids are hex, and hex has two spellings. Comparing them
// case-sensitively would mean failing to recognise the user's own close and
// telling them a stranger had force-closed their channel.
func TestOurOwnCloseIsRecognisedWhateverTheCase(t *testing.T) {
	t.Parallel()

	tx := closeTx(wire.NewTxOut(500_000, p2wsh), wire.NewTxOut(AnchorValueSat, p2wsh))
	const lower = "abcdef0123456789"

	got := ClassifyShape(ShapeFacts{Tx: tx, TxID: lower, OurCloseTxID: "ABCDEF0123456789"})
	if got != store.ShapeCommitmentOurs {
		t.Errorf("got %q, want our own commitment to be recognised", got)
	}
}

// Whatever a transaction turns out to be, it must be something the schema
// accepts — and never nothing, because a spend with no shape is a spend nobody
// downstream knows how to treat.
func TestEveryVerdictIsOneTheSchemaAccepts(t *testing.T) {
	t.Parallel()

	scripts := [][]byte{p2wpkh, p2wsh, p2tr, p2pkh, {}}
	values := []int64{0, AnchorValueSat, 500_000}
	locktimes := []uint32{0, 1, 0x20_00_00_01, 1_700_000_000}

	for _, script := range scripts {
		for _, value := range values {
			for _, locktime := range locktimes {
				tx := closeTx(wire.NewTxOut(value, script))
				tx.LockTime = locktime
				got := ClassifyShape(ShapeFacts{Tx: tx, TxID: "a", OurCloseTxID: "b"})
				if !got.Valid() {
					t.Fatalf("script %x value %d locktime %d produced %q",
						script, value, locktime, got)
				}
			}
		}
	}
}

// sweepTx builds a transaction spending one of a commitment's outputs, with the
// witness the caller wants to test.
func sweepTx(witness wire.TxWitness) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}, Witness: witness})
	tx.AddTxOut(wire.NewTxOut(400_000, p2wpkh))
	return tx
}

// The witness is not a hint, it is a statement. A contested output carries two
// spending paths and the spender has to say in the clear which one they took —
// so this is the difference between somebody being punished for a revoked
// commitment and somebody collecting after the wait, and it is not a guess.
func TestWhatASpendOfACommitmentsOutputLooksLike(t *testing.T) {
	t.Parallel()

	revocation := wire.TxWitness{{0x30, 0x44}, {0x01}, {0x63, 0x21}}
	delayed := wire.TxWitness{{0x30, 0x44}, {}, {0x63, 0x21}}

	tests := []struct {
		name  string
		facts SweepFacts
		want  store.SpendShape
	}{
		{
			name:  "somebody used a revocation secret, which only a revoked commitment leaves possible",
			facts: SweepFacts{Tx: sweepTx(revocation), Role: store.RoleUnknown},
			want:  store.ShapeJustice,
		},
		{
			name:  "somebody collected after the delay ran",
			facts: SweepFacts{Tx: sweepTx(delayed), Role: store.RoleUnknown},
			want:  store.ShapeDelayedSweep,
		},
		{
			// Justice on a payment in flight is still justice, and saying "a
			// payment was claimed" would lose the fact that a breach was answered.
			name:  "a revocation spend of a payment in flight is still justice",
			facts: SweepFacts{Tx: sweepTx(revocation), Role: store.RoleHTLC},
			want:  store.ShapeJustice,
		},
		{
			name:  "an ordinary claim of a payment in flight",
			facts: SweepFacts{Tx: sweepTx(wire.TxWitness{{0x30}, {0x20}, {0x00}, {0x63}}), Role: store.RoleHTLC},
			want:  store.ShapeHTLCClaim,
		},
		{
			name: "a contested output that moved after its delay expired",
			facts: SweepFacts{
				Tx: sweepTx(wire.TxWitness{{0x30}}), Role: store.RoleToLocal,
				SpendHeight: 1000, DeadlineHeight: 900,
			},
			want: store.ShapeDelayedSweep,
		},
		{
			// Before the deadline with a witness that says nothing: alarming, but
			// not provable. Saying "justice" here would be inventing evidence.
			name: "a contested output that moved before its delay, with nothing to prove why",
			facts: SweepFacts{
				Tx: sweepTx(wire.TxWitness{{0x30}}), Role: store.RoleToLocal,
				SpendHeight: 800, DeadlineHeight: 900,
			},
			want: store.ShapeUnknown,
		},
		{
			name:  "an anchor being spent, which is fee bumping and not an outcome",
			facts: SweepFacts{Tx: sweepTx(wire.TxWitness{{0x30}}), Role: store.RoleAnchor},
			want:  store.ShapeUnknown,
		},
		{
			name:  "no transaction at all",
			facts: SweepFacts{Role: store.RoleToLocal},
			want:  store.ShapeUnknown,
		},
		{
			name:  "an input index the transaction does not have",
			facts: SweepFacts{Tx: sweepTx(revocation), InputIndex: 5},
			want:  store.ShapeUnknown,
		},
		{
			name:  "a negative input index",
			facts: SweepFacts{Tx: sweepTx(revocation), InputIndex: -1},
			want:  store.ShapeUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifySweep(tc.facts); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The branch selector is one byte of one, not any truthy-looking thing. A
// three-item witness whose middle item is something else took neither path.
func TestTheBranchSelectorIsReadExactly(t *testing.T) {
	t.Parallel()

	for _, selector := range [][]byte{{0x00}, {0x02}, {0x01, 0x00}, {0x01, 0x01}} {
		got := ClassifySweep(SweepFacts{
			Tx:   sweepTx(wire.TxWitness{{0x30}, selector, {0x63}}),
			Role: store.RoleUnknown,
		})
		if got == store.ShapeJustice {
			t.Errorf("selector %x was read as a revocation spend", selector)
		}
	}
}

// Whatever the input, the verdict must be one the schema accepts.
func TestEverySweepVerdictIsOneTheSchemaAccepts(t *testing.T) {
	t.Parallel()

	roles := []store.OutpointRole{
		store.RoleToLocal, store.RoleToRemote, store.RoleHTLC,
		store.RoleAnchor, store.RoleUnknown, "",
	}
	witnesses := []wire.TxWitness{
		nil,
		{{0x30}},
		{{0x30}, {0x01}, {0x63}},
		{{0x30}, {}, {0x63}},
		{{}, {0x30}, {0x30}, {0x52}},
	}
	heights := []int32{0, 500, 900, 1000}

	for _, role := range roles {
		for _, witness := range witnesses {
			for _, spendHeight := range heights {
				for _, deadline := range heights {
					got := ClassifySweep(SweepFacts{
						Tx: sweepTx(witness), Role: role,
						SpendHeight: spendHeight, DeadlineHeight: deadline,
					})
					if !got.Valid() {
						t.Fatalf("role %q witness %v produced %q", role, witness, got)
					}
				}
			}
		}
	}
}

func TestACommitmentsOutputsAreAllRegistered(t *testing.T) {
	t.Parallel()

	tx := closeTx(
		wire.NewTxOut(500_000, p2wsh),
		wire.NewTxOut(AnchorValueSat, p2wsh),
		wire.NewTxOut(300_000, p2wsh),
	)
	got := CommitmentOutputs(tx.TxHash(), tx, store.BranchSQ, 42)

	if len(got) != 3 {
		t.Fatalf("registered %d of 3 outputs", len(got))
	}
	for i, o := range got {
		if o.Vout != int32(i) {
			t.Errorf("output %d is recorded at index %d", i, o.Vout)
		}
		if o.Branch != store.BranchSQ {
			t.Errorf("output %d is on branch %q", i, o.Branch)
		}
		// Without this the reorganisation that removes the commitment cannot
		// remove what it created.
		if o.SourceSpendEventID != 42 {
			t.Errorf("output %d does not point back at the commitment", i)
		}
		if o.ScriptHex == "" {
			t.Errorf("output %d was registered with no script to match on", i)
		}
		if !o.Role.Valid() {
			t.Errorf("output %d has role %q", i, o.Role)
		}
	}

	// The one role that can be named honestly. The rest are the same kind of
	// script and telling them apart needs keys we do not have.
	if got[1].Role != store.RoleAnchor {
		t.Errorf("the fee-bumping output was recorded as %q", got[1].Role)
	}
	if got[0].Role != store.RoleUnknown || got[2].Role != store.RoleUnknown {
		t.Errorf("a role was guessed at: %q and %q", got[0].Role, got[2].Role)
	}
}

func TestNothingIsRegisteredForNothing(t *testing.T) {
	t.Parallel()

	if got := CommitmentOutputs(chainhash.Hash{}, nil, store.BranchSQ, 1); got != nil {
		t.Errorf("registered %d outputs for no transaction", len(got))
	}

	// An output with no script cannot be matched on, so watching it would be a
	// row that never fires.
	tx := closeTx(wire.NewTxOut(500_000, nil), wire.NewTxOut(300_000, p2wsh))
	got := CommitmentOutputs(tx.TxHash(), tx, store.BranchSQ, 1)
	if len(got) != 1 || got[0].Vout != 1 {
		t.Errorf("registered %+v", got)
	}
}
