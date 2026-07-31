package watcher

import (
	"strings"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// Constants from the commitment format, named rather than spelled inline.
const (
	// AnchorValueSat is the size of the fee-bumping outputs an anchors-format
	// commitment carries. Nothing else in a channel close is this small, which
	// makes it the cheapest reliable sign that a transaction is a commitment.
	AnchorValueSat = 330

	// commitmentLocktimeMarker and commitmentSequenceMarker are the top bytes a
	// commitment transaction sets, because the commitment number is hidden in the
	// remaining bits of the locktime and the input sequence. A cooperative close
	// has no reason to set either.
	commitmentLocktimeMarker = 0x20
	commitmentSequenceMarker = 0x80

	// mutualCloseWitnessItems is the witness of a spend of the funding output: an
	// empty item for the multisig off-by-one, two signatures, and the script.
	// Shared with a commitment, which spends the same output the same way — so it
	// tells you the transaction closes a channel, not which kind of close it is.
	mutualCloseWitnessItems = 4

	// scriptPathWitnessItems is the witness of a spend of one of a commitment's
	// own outputs: a signature, the branch selector, and the script.
	scriptPathWitnessItems = 3
)

// ShapeFacts is everything the funding-spend classifier is allowed to look at.
type ShapeFacts struct {
	// Tx is the transaction that spent the funding output, and TxID its id.
	Tx   *wire.MsgTx
	TxID string
	// OurCloseTxID is the closing transaction the user's own Lightning node
	// reported, if it reported one. Empty when it did not.
	//
	// The only thing that can tell "our commitment" from "theirs": both look
	// identical on the chain, because a commitment reveals nothing about which
	// side holds it. Without this the honest answer is that we cannot tell.
	OurCloseTxID string
}

// ClassifyShape says what a spend of a channel's funding output appears to be.
//
// Pure and best-effort, and honest about which. Without the channel's keys there
// is no way to tell a current commitment from a revoked one, and no Lightning
// implementation exposes the transaction ids of the commitments the *other* side
// holds. So the strongest thing that can be said about a commitment that is not
// ours is that it is a commitment, and that is what is said.
//
// The bias is toward alarm. `commitment_unknown` is treated as hostile
// downstream, because the alternative — treating a possible breach as routine —
// is the failure this software exists to prevent. A transaction that fits
// nothing is `unknown`, which is also alertable: nothing is silently ignored.
func ClassifyShape(f ShapeFacts) store.SpendShape {
	if f.Tx == nil || len(f.Tx.TxIn) != 1 {
		// A close spends exactly one output: the funding one. More inputs means
		// this is something else entirely, and guessing would be worse than saying
		// so.
		return store.ShapeUnknown
	}

	// Checked before the cooperative close, and the order is deliberate. The two
	// rules are written to be mutually exclusive, but if they ever both fitted,
	// the dangerous reading is the one that has to win.
	if looksLikeCommitment(f.Tx) {
		if f.OurCloseTxID != "" && strings.EqualFold(f.TxID, f.OurCloseTxID) {
			// The user's own node said it broadcast this. Which is the one case
			// where "whose commitment is it" has an answer.
			return store.ShapeCommitmentOurs
		}
		return store.ShapeCommitmentUnknown
	}

	if looksLikeMutualClose(f.Tx) {
		return store.ShapeMutualClose
	}
	return store.ShapeUnknown
}

// looksLikeCommitment reports the marks a commitment transaction cannot avoid
// leaving.
//
// Any one of them is enough. The anchor outputs are the clearest, but a channel
// opened before anchors existed has none, and then the obscured commitment
// number in the locktime and sequence is what gives it away.
func looksLikeCommitment(tx *wire.MsgTx) bool {
	for _, out := range tx.TxOut {
		if out.Value == AnchorValueSat {
			return true
		}
	}
	if tx.LockTime>>24 == commitmentLocktimeMarker {
		return true
	}
	for _, in := range tx.TxIn {
		if in.Sequence>>24 == commitmentSequenceMarker {
			return true
		}
	}
	return false
}

// looksLikeMutualClose reports a transaction with the shape of a close both
// sides agreed to.
//
// Everything here is a negative: no anchors, no hidden commitment number, no
// more than the two outputs a cooperative close pays out, and nothing but plain
// payment scripts. That is the point — a cooperative close is a commitment with
// all the machinery removed, so it is recognised by the absence of it.
func looksLikeMutualClose(tx *wire.MsgTx) bool {
	if len(tx.TxIn) != 1 || len(tx.TxOut) == 0 || len(tx.TxOut) > 2 {
		return false
	}
	if len(tx.TxIn[0].Witness) != mutualCloseWitnessItems ||
		len(tx.TxIn[0].Witness[0]) != 0 {
		// A spend of the funding output's two-of-two script: an empty item, two
		// signatures, and the script itself. Anything else did not come from a
		// channel at all.
		return false
	}
	// A locktime that looks like a date rather than a height is not something a
	// cooperative close sets, and is one of the ways a commitment hides its
	// number.
	const locktimeIsATimestamp = 500_000_000
	if tx.LockTime >= locktimeIsATimestamp {
		return false
	}
	for _, out := range tx.TxOut {
		if !isPlainPayment(out.PkScript) {
			return false
		}
	}
	return true
}

// isPlainPayment reports a script that just pays somebody: no timelocks, no
// branches, nothing to wait for.
func isPlainPayment(script []byte) bool {
	switch {
	case len(script) == 22 && script[0] == 0x00 && script[1] == 0x14:
		return true // pay to witness public key hash
	case len(script) == 34 && script[0] == 0x00 && script[1] == 0x20:
		return true // pay to witness script hash
	case len(script) == 34 && script[0] == 0x51 && script[1] == 0x20:
		return true // pay to taproot
	default:
		return false
	}
}

// SweepFacts is everything the classifier of a commitment's own outputs is
// allowed to look at.
type SweepFacts struct {
	// Tx is the transaction that spent the output, and InputIndex which of its
	// inputs did so. The witness on that input is the strongest evidence
	// available, and it is on that input specifically.
	Tx         *wire.MsgTx
	InputIndex int
	// Role is what the output was thought to be. Best-effort: a commitment's
	// outputs are almost all the same kind of script, so this is often unknown.
	Role store.OutpointRole
	// SpendHeight is where this spend confirmed and DeadlineHeight where the
	// contested delay expires. Zero means not known.
	SpendHeight    int32
	DeadlineHeight int32
}

// ClassifySweep says what a spend of one of a commitment's outputs appears to
// be — which is how outcomes get reported rather than only threats.
//
// The witness is what decides it, and it decides it properly rather than by
// guesswork. A delayed output carries two spending paths: one anybody with the
// revocation secret may take immediately, and one the owner may take once the
// delay has run. The spender must say which they used, in the clear, in the
// witness — so a transaction that took the revocation path *is* somebody
// punishing a revoked commitment, and one that took the delayed path *is*
// somebody collecting after the wait. Neither is a guess.
//
// Timing is only a fallback, for the outputs whose scripts have no branch to
// choose.
func ClassifySweep(f SweepFacts) store.SpendShape {
	if f.Tx == nil || f.InputIndex < 0 || f.InputIndex >= len(f.Tx.TxIn) {
		return store.ShapeUnknown
	}
	witness := f.Tx.TxIn[f.InputIndex].Witness

	switch {
	case takesRevocationPath(witness):
		// Somebody held the revocation secret for this output and used it. That
		// only happens when the commitment it came from was revoked.
		return store.ShapeJustice

	case takesDelayedPath(witness):
		return store.ShapeDelayedSweep

	case f.Role == store.RoleHTLC:
		return store.ShapeHTLCClaim

	case f.Role == store.RoleToLocal && f.DeadlineHeight > 0 &&
		f.SpendHeight >= f.DeadlineHeight:
		// The delay ran out and the output moved. Nothing else can spend a
		// contested output at that point.
		return store.ShapeDelayedSweep

	default:
		// Not silently ignored: `unknown` still reaches the user, and the deadline
		// engine still treats an unresolved contested output as unresolved.
		return store.ShapeUnknown
	}
}

// takesRevocationPath reports that the spender chose the branch only a revoked
// commitment leaves open.
//
// The script is an if-else, so the witness ends with a selector: a single byte
// of one for the revocation branch, and an empty push for the delayed one. Three
// items in all — a signature, the selector, and the script.
func takesRevocationPath(w wire.TxWitness) bool {
	return len(w) == scriptPathWitnessItems && len(w[1]) == 1 && w[1][0] == 0x01
}

// takesDelayedPath reports the other branch of the same script: the owner
// collecting after the delay.
func takesDelayedPath(w wire.TxWitness) bool {
	return len(w) == scriptPathWitnessItems && len(w[1]) == 0
}
