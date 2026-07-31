// Package words holds the phrases Forktower uses when it is talking to a person.
//
// Not every user-facing string belongs here, and most do not. An alert
// interrupts somebody, a timeline entry is read calmly afterwards, and a
// headline states where things stand right now — the same event is worth saying
// three different ways, and collapsing them into one would make all three
// worse. Those stay where they are written.
//
// What belongs here is the *vocabulary*: the small set of terms the
// documentation makes normative, where every part of the program must use the
// same word for the same thing. A user who reads "the other chain" on the
// dashboard and something else in a notification has been given two things to
// hold in their head instead of one.
//
// The evidence that this is worth having: before it existed, three packages each
// had their own copy of the two chain names, and they had already drifted apart
// on what to say when the chain is not one of the two.
//
// This package depends on nothing but the standard library's string handling.
// That is deliberate — it has to be usable from the decision logic that is
// forbidden from touching storage or the network, and from both packages that
// define their own `Branch` type.
package words

import "strings"

// The two chains, as a person reads them.
//
// The internal names are "sf" and "sq" and they mean nothing outside this
// codebase. A user shown either of them has been shown nothing.
const (
	// OwnChain is the chain the user's own Bitcoin node follows.
	OwnChain = "your node's chain"
	// OtherChain is the one it does not — the chain Forktower watches on their
	// behalf, and the one nobody else is looking at.
	OtherChain = "the other chain"
	// EitherChain is what to say when which chain is not known.
	//
	// Vague on purpose. The alternative that one package had grown — "a chain
	// Forktower does not recognise" — describes the program's confusion rather
	// than the user's situation, and reads as broken English in the sentences it
	// has to sit inside. This one is honest and grammatical in both a sentence
	// and a table cell, and in practice it never appears at all.
	EitherChain = "one of the chains"
)

// The branch names as they are stored and passed on the wire.
//
// Spelled out here rather than imported, because two packages define their own
// `Branch` type over the same two values and this package must be usable from
// both. A test holds these to what those packages actually declare, so the
// duplication cannot drift.
const (
	branchOwn   = "sf"
	branchOther = "sq"
)

// Chain names one of the two chains for a person.
//
// Takes a plain string rather than a branch type for the reason above. Anything
// that is not one of the two gets EitherChain: a value that reached here without
// being one of them is a fault somewhere else, and telling the user about it
// helps nobody.
func Chain(branch string) string {
	switch branch {
	case branchOwn:
		return OwnChain
	case branchOther:
		return OtherChain
	default:
		return EitherChain
	}
}

// Internal lists the names this program uses among itself which could never be
// ordinary English, whatever the sentence.
//
// One list, because it was three: the tests that check the dashboard, the
// notifications and the timeline for leaks each kept their own, so a value added
// to the schema had to be remembered in several places or a check quietly
// stopped covering it.
//
// **What is deliberately *not* here is the trap.** A first draft of this list
// included "confirmed", "resolved", "split", "counting" and "expired" — every
// one of them a schema value, and every one of them a word a person needs to
// read. "It has not confirmed yet" and "The split may be ending" are good
// sentences, and a check that forbade them would push whoever hit it into
// writing a worse one. Only names that are unmistakably this program's own
// belong here.
func Internal() []string {
	return []string{
		// The two chains.
		"sf", "sq",
		// What a spend turned out to be.
		"mutual_close", "commitment_ours", "commitment_unknown", "commitment_revoked",
		"delayed_sweep", "htlc_claim",
		// How firmly it happened.
		"reorged_out",
		// How far a channel has got towards being closed.
		"pending_close", "coop_closed", "force_closed", "breach_closed",
		// How a channel's commitment is built.
		"static_remote",
		// Which clock is running.
		"csv", "htlc_incoming", "htlc_outgoing", "cltv",
		// What an output of a commitment is for.
		"to_local", "to_remote",
		// Identifiers a person has no use for.
		"scid", "outpoint", "scriptpubkey", "txid",
	}
}

// InternalStates lists the engine's own state names, which are shouted in
// capitals precisely so they cannot be mistaken for prose.
//
// Matched case-sensitively, and that is the whole reason they are separate:
// "SPLIT" must never reach a screen, while "split" is a perfectly good English
// word this software uses constantly.
func InternalStates() []string {
	return []string{
		"UNARMED", "ARMED", "SPLIT", "RESOLVING",
		"RESOLVED_SF_WON", "RESOLVED_SQ_WON",
		"WRONG_BRANCH", "ECLIPSE_SUSPECT", "DEGRADED", "SYNCING",
	}
}

// FindInternal returns the first internal name appearing in some text a person
// would read, or empty if there is none.
//
// The matching lives here rather than in each caller, because three tests each
// doing their own substring search is three chances to do it slightly
// differently — and a check that is subtly weaker than it looks is worse than no
// check, since everybody believes it is running.
//
// Whole words only. "csv" must not fire on a sentence containing it as part of
// a longer word, and "sf" would otherwise match inside anything.
func FindInternal(text string) string {
	lower := strings.ToLower(text)
	for _, name := range Internal() {
		if containsWord(lower, name) {
			return name
		}
	}
	for _, name := range InternalStates() {
		if containsWord(text, name) {
			return name
		}
	}
	return ""
}

// containsWord reports whether text contains name with nothing word-like on
// either side of it.
func containsWord(text, name string) bool {
	from := 0
	for {
		at := strings.Index(text[from:], name)
		if at < 0 {
			return false
		}
		at += from
		before := at == 0 || !wordByte(text[at-1])
		afterAt := at + len(name)
		after := afterAt == len(text) || !wordByte(text[afterAt])
		if before && after {
			return true
		}
		from = at + 1
	}
}

// wordByte reports whether a byte is the sort of character that would make an
// adjacent match part of a longer word.
func wordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return true
	default:
		return false
	}
}
