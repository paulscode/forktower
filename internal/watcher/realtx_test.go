package watcher

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// Transactions LND actually broadcast, captured from a staged regtest world by
// `forkbench`, saved as raw bytes and never touched by hand.
//
// The crafted transactions elsewhere in this package prove the rules are
// implemented as written. Only these prove the rules match what a real Lightning
// implementation does — which is a different claim, and the one that decides
// whether a user is told the truth about their own channel. Every one of them
// was made by `lncli closechannel` against a real channel with real payments
// through it.
//
// Regenerate with: make forkbench-fixtures
func loadFixture(t *testing.T, name string) *wire.MsgTx {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("%s is not hex: %v", name, err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(decoded)); err != nil {
		t.Fatalf("%s is not a transaction: %v", name, err)
	}
	return tx
}

// The three shapes a channel close takes, as LND really produces them.
func TestTheClassifierAgreesWithWhatLNDActuallyBroadcasts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		// ours, when set, stands in for the user's own node having reported this
		// closing transaction.
		ours bool
		want store.SpendShape
	}{
		{
			// A revoked commitment: the counterparty was rolled back to an older
			// channel state and made to publish it. This is the attack.
			name:    "a commitment nobody can attribute",
			fixture: "force_close_commitment.hex",
			want:    store.ShapeCommitmentUnknown,
		},
		{
			// The identical shape on the chain, published honestly by the user's
			// own node. Forktower cannot tell the two apart from the chain, which
			// is exactly why it says so rather than guessing — and why the user's
			// own node reporting the transaction id is the only thing that
			// resolves it.
			name:    "an honest force close looks the same from outside",
			fixture: "force_close_user.hex",
			want:    store.ShapeCommitmentUnknown,
		},
		{
			name:    "the same honest force close, once our node has claimed it",
			fixture: "force_close_user.hex",
			ours:    true,
			want:    store.ShapeCommitmentOurs,
		},
		{
			// The one that must not be mistaken for an attack. Telling somebody
			// their agreed close was a breach is the false alarm that teaches them
			// to stop reading the alerts.
			name:    "a close both sides agreed to",
			fixture: "coop_close.hex",
			want:    store.ShapeMutualClose,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := loadFixture(t, tc.fixture)
			facts := ShapeFacts{Tx: tx, TxID: tx.TxHash().String()}
			if tc.ours {
				facts.OurCloseTxID = tx.TxHash().String()
			}
			if got := ClassifyShape(facts); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The marks a commitment leaves are the whole basis of the classification, so it
// is worth checking they are really there in a transaction LND made rather than
// only in the ones this project builds.
func TestARealCommitmentCarriesTheMarksWeLookFor(t *testing.T) {
	t.Parallel()

	tx := loadFixture(t, "force_close_commitment.hex")

	var anchors int
	for _, out := range tx.TxOut {
		if out.Value == AnchorValueSat {
			anchors++
		}
	}
	obscuredLocktime := tx.LockTime>>24 == commitmentLocktimeMarker
	obscuredSequence := tx.TxIn[0].Sequence>>24 == commitmentSequenceMarker

	if anchors == 0 && !obscuredLocktime && !obscuredSequence {
		t.Fatal("a real commitment carries none of the marks the classifier looks for")
	}
	t.Logf("anchors=%d obscured locktime=%v obscured sequence=%v",
		anchors, obscuredLocktime, obscuredSequence)

	// It spends exactly one output — the funding one — which is what makes "more
	// than one input" a safe reason to refuse to classify.
	if len(tx.TxIn) != 1 {
		t.Errorf("a real commitment has %d inputs", len(tx.TxIn))
	}
}

// A real cooperative close must have none of them, or the two rules would
// overlap and the dangerous reading would win on a transaction that deserves the
// benign one.
func TestARealCooperativeCloseCarriesNoneOfThem(t *testing.T) {
	t.Parallel()

	tx := loadFixture(t, "coop_close.hex")

	for _, out := range tx.TxOut {
		if out.Value == AnchorValueSat {
			t.Error("a cooperative close has an output the size of an anchor")
		}
		if !isPlainPayment(out.PkScript) {
			t.Errorf("a cooperative close pays to a script we do not recognise: %x",
				out.PkScript)
		}
	}
	if tx.LockTime>>24 == commitmentLocktimeMarker {
		t.Error("a cooperative close has a commitment's locktime pattern")
	}
	if tx.TxIn[0].Sequence>>24 == commitmentSequenceMarker {
		t.Error("a cooperative close has a commitment's sequence pattern")
	}

	// The witness this project expects on any spend of a funding output: an
	// empty item for the multisig off-by-one, two signatures, and the script.
	witness := tx.TxIn[0].Witness
	if len(witness) != mutualCloseWitnessItems || len(witness[0]) != 0 {
		t.Errorf("a real funding spend has a %d-item witness starting with %d bytes",
			len(witness), len(witness[0]))
	}
}

// The scanner must find the close when it is handed the block it landed in, from
// a watchset built the way the real one is. This is the whole path — outpoint to
// match to verdict — against a transaction nobody here wrote.
func TestARealCloseIsFoundAndClassifiedEndToEnd(t *testing.T) {
	t.Parallel()

	tx := loadFixture(t, "force_close_commitment.hex")
	funding := tx.TxIn[0].PreviousOutPoint

	ws := NewWatchSet(Target{Outpoint: funding, Kind: KindFunding, ChannelID: 1})
	matches := ScanBlock(block(t, coinbase(), tx), ws)

	if len(matches) != 1 {
		t.Fatalf("found %d matches for a real channel close", len(matches))
	}
	if matches[0].Target.ChannelID != 1 {
		t.Errorf("matched channel %d", matches[0].Target.ChannelID)
	}

	shape := ClassifyShape(ShapeFacts{
		Tx: matches[0].Tx, TxID: matches[0].TxID.String(),
	})
	if shape != store.ShapeCommitmentUnknown {
		t.Errorf("a real revoked commitment was classified %q", shape)
	}

	// And what it created is watched, so the outcome can be reported.
	outputs := CommitmentOutputs(matches[0].TxID, matches[0].Tx, store.BranchSQ, 9)
	if len(outputs) != len(tx.TxOut) {
		t.Errorf("registered %d of %d outputs", len(outputs), len(tx.TxOut))
	}
	var anchors int
	for _, o := range outputs {
		if o.Role == store.RoleAnchor {
			anchors++
		}
	}
	if anchors == 0 {
		t.Error("a real anchors commitment produced no output recognised as an anchor")
	}
}
