package mirror

import (
	"strings"
	"time"
)

// Rejection is why the other chain would not take a transaction.
//
// Classified rather than passed through as prose, because the remedies differ
// completely and only one of them is something the user can act on.
type Rejection string

// The ways a transaction gets refused.
const (
	// RejectFeeTooLow means the transaction pays less than the target chain is
	// currently accepting.
	//
	// **The one that cannot be fixed from here.** Forktower holds no keys, so it
	// cannot raise the fee, add a child to pay for it, or re-sign anything. The
	// transaction is somebody else's signed bytes and the only honest thing to do
	// is say so.
	RejectFeeTooLow Rejection = "fee_too_low"
	// RejectMissingParent means the transaction spends something the target chain
	// has never seen. Usually because the transaction it follows is not there —
	// which on a split is entirely normal and is itself worth reporting.
	RejectMissingParent Rejection = "missing_parent"
	// RejectConflict means something else on the target chain already spends the
	// same outputs. On a split that is not an error: it usually means the channel
	// was closed differently over there.
	RejectConflict Rejection = "conflict"
	// RejectNonStandard means the target chain will not relay this shape of
	// transaction at all. On a branch with different rules that is a real
	// possibility rather than a theoretical one.
	RejectNonStandard Rejection = "non_standard"
	// RejectOther is everything else, reported with whatever the node said.
	RejectOther Rejection = "other"
)

// Classify works out why a broadcast was refused.
//
// Pure, and matched on the message because that is what a node gives: the
// numeric codes cover the transport rather than the reason, and the reasons
// arrive as strings whose wording has been stable for years across
// implementations even where it is not guaranteed. An unrecognised message is
// `other` and is reported verbatim, so a wording we have not seen degrades to
// "here is what your node said" rather than to silence.
func Classify(err error) Rejection {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())

	switch {
	case containsAny(msg,
		"min relay fee not met", "mempool min fee not met", "insufficient fee",
		"fee rate", "min-relay-fee", "does not have enough fee"):
		return RejectFeeTooLow

	case containsAny(msg,
		"missing-inputs", "missingorspent", "missing inputs", "bad-txns-inputs-missing"):
		return RejectMissingParent

	case containsAny(msg,
		"txn-mempool-conflict", "conflict", "bad-txns-inputs-spent", "double-spend"):
		return RejectConflict

	case containsAny(msg, "non-mandatory-script-verify", "scriptpubkey", "non-standard",
		"nonstandard", "bare-multisig", "dust", "version"):
		return RejectNonStandard

	default:
		return RejectOther
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// Retriable reports whether trying the same bytes again could ever work.
//
// **The distinction that decides whether to keep trying.** A fee that is too low
// today may be enough tomorrow, and a parent that is missing now may arrive —
// both are worth another attempt. A transaction the chain will not relay at all,
// or one whose inputs something else has already spent, will not become
// acceptable by being sent again, and retrying it forever is noise that hides
// the transactions still worth watching.
func (r Rejection) Retriable() bool {
	switch r {
	case RejectFeeTooLow, RejectMissingParent, RejectOther:
		return true
	case RejectConflict, RejectNonStandard:
		return false
	default:
		return true
	}
}

// Explain says what a rejection means, for the person whose money it is.
//
// Each one says what happened, whether Forktower can do anything, and — where
// the answer is no — why not. "We could not copy it" without the second half
// leaves somebody waiting for a fix that is not coming.
func (r Rejection) Explain(nodeSaid string) string {
	switch r {
	case RejectFeeTooLow:
		return "the other chain will not take this transaction because its fee is " +
			"too low for that chain right now. Forktower cannot raise it: the " +
			"transaction was signed by your node, and changing a signed transaction " +
			"in any way would make it invalid. It will keep trying, in case fees " +
			"there fall."
	case RejectMissingParent:
		return "the other chain has not seen the transaction this one spends from, " +
			"so it will not take this one yet. That is normal while the two chains " +
			"differ. Forktower will keep trying."
	case RejectConflict:
		return "something already on the other chain spends the same money, so this " +
			"cannot go there too. That usually means the channel was closed a " +
			"different way over there, which is worth looking at."
	case RejectNonStandard:
		return "the other chain will not relay this kind of transaction at all. " +
			"That can happen when the two chains have different rules about what " +
			"transactions are allowed, and it is not something Forktower can work " +
			"around."
	case RejectOther:
		if nodeSaid == "" {
			return "the other chain refused this transaction and did not say why."
		}
		return "the other chain refused this transaction. It said: " + nodeSaid
	default:
		return "the other chain refused this transaction."
	}
}

// Retry schedule.
const (
	// FirstDelay is how long to wait before the first retry.
	FirstDelay = 30 * time.Second
	// MaxDelay caps the wait. A mirror that has backed off to hours is one that
	// would miss a fee market opening up.
	MaxDelay = 15 * time.Minute
	// MaxAttempts is when to stop and say so.
	//
	// Generous, because the thing being waited for is usually a fee market or a
	// missing parent, and both change on their own. But finite: a transaction
	// retried forever is one nobody ever gets told about, and being told "this
	// could not be copied and here is why" is the point.
	MaxAttempts = 40
)

// Backoff is how long to wait before attempt number n, counting from one.
//
// Doubling, capped. Deliberately not jittered: there is exactly one of these per
// transaction talking to one node the user runs, so there is no herd to
// disperse, and a predictable schedule is one a person can be told about.
func Backoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := FirstDelay
	for range attempt - 1 {
		delay *= 2
		if delay >= MaxDelay {
			return MaxDelay
		}
	}
	return delay
}
