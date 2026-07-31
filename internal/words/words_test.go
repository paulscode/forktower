package words_test

import (
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// The branch codes are spelled out in this package rather than imported, so that
// it can be used from both packages that define their own type over them. This
// is what stops that duplication drifting.
func TestTheBranchCodesMatchWhatTheRestOfTheProgramUses(t *testing.T) {
	t.Parallel()

	if got := words.Chain(string(store.BranchSF)); got != words.OwnChain {
		t.Errorf("the store's own chain maps to %q", got)
	}
	if got := words.Chain(string(store.BranchSQ)); got != words.OtherChain {
		t.Errorf("the store's other chain maps to %q", got)
	}
	if got := words.Chain(string(chainview.BranchSF)); got != words.OwnChain {
		t.Errorf("the chain view's own chain maps to %q", got)
	}
	if got := words.Chain(string(chainview.BranchSQ)); got != words.OtherChain {
		t.Errorf("the chain view's other chain maps to %q", got)
	}
}

// Anything that is not one of the two is a fault somewhere else, and telling the
// user about it helps nobody.
func TestAnUnknownBranchIsSaidVaguelyRatherThanTechnically(t *testing.T) {
	t.Parallel()

	for _, branch := range []string{"", "elsewhere", "SF", "sq ", "sfsq"} {
		got := words.Chain(branch)
		if got != words.EitherChain {
			t.Errorf("Chain(%q) = %q", branch, got)
		}
	}
	// And it reads as a sentence, which the version this replaced did not.
	if strings.Contains(words.EitherChain, "not recognise") {
		t.Error("the fallback describes the program's confusion rather than the situation")
	}
}

// The check that stops internal names reaching a screen.
func TestInternalNamesAreFound(t *testing.T) {
	t.Parallel()

	leaks := []string{
		"the shape was commitment_unknown",
		"branch: sq",
		"a csv delay of 144",
		"state pending_close",
		"the to_local output",
		"reorged_out at height 5",
	}
	for _, text := range leaks {
		if got := words.FindInternal(text); got == "" {
			t.Errorf("nothing was found in %q", text)
		}
	}
}

// The trap this list was nearly built with. Every one of these sentences is
// good user-facing English containing a word that is also a schema value, and a
// check that refused them would push whoever hit it into writing something
// worse.
func TestOrdinaryEnglishIsNotMistakenForAnInternalName(t *testing.T) {
	t.Parallel()

	fine := []string{
		"It has not confirmed yet.",
		"The split may be ending.",
		"A countdown has stopped: it was resolved before it ran out.",
		"The time has expired.",
		"Forktower is counting down on one of your channels.",
		"Somebody collected after the waiting period ended.",
		"The claim that protects you was made.",
		"We are handling it.",
		"Your node's chain and the other chain no longer agree.",
		"Forktower is watching two independent views of the chain.",
		"This is a transfer of value.",
		"Nothing was lost.",
	}
	for _, text := range fine {
		if got := words.FindInternal(text); got != "" {
			t.Errorf("%q was rejected because of %q", text, got)
		}
	}
}

// Whole words only, or the shortest names would fire on anything.
func TestOnlyWholeWordsCount(t *testing.T) {
	t.Parallel()

	// Each of these contains an internal name as a substring, and none of them
	// is one.
	notLeaks := []string{
		"a transfer was made", // contains "sf"
		"the square root",     // contains "sq"
		"it was unconfirmed",  // contains "confirmed"
		"a scidding noise",    // contains "scid"
		"the outpointer",      // contains "outpoint"
	}
	for _, text := range notLeaks {
		if got := words.FindInternal(text); got != "" {
			t.Errorf("%q was rejected because of %q", text, got)
		}
	}
}

// The engine's shouted state names are matched case-sensitively, which is the
// whole reason they are kept apart from the rest.
func TestShoutedStateNamesAreCaughtButTheirEnglishIsNot(t *testing.T) {
	t.Parallel()

	if got := words.FindInternal("the daemon is ARMED"); got != "ARMED" {
		t.Errorf("a shouted state name was not caught: %q", got)
	}
	if got := words.FindInternal("we are in SPLIT"); got != "SPLIT" {
		t.Errorf("a shouted state name was not caught: %q", got)
	}
	// The same words as prose are exactly what this software says all the time.
	for _, text := range []string{
		"The split may be ending.",
		"Forktower is armed and watching.",
		"The split has ended.",
	} {
		if got := words.FindInternal(text); got != "" {
			t.Errorf("%q was rejected because of %q", text, got)
		}
	}
}

// The list has to cover the schema. A value added to the store that is not here
// is a value a leak check would silently stop covering.
func TestTheListCoversTheSchemasOwnNames(t *testing.T) {
	t.Parallel()

	// Every value the schema declares which is not also an English word. The
	// ones left out are listed in the second slice, with the sentence each is
	// legitimately used in, so that leaving one out is a decision rather than an
	// oversight.
	mustBeCaught := []string{
		string(store.ShapeMutualClose), string(store.ShapeCommitmentOurs),
		string(store.ShapeCommitmentUnknown), string(store.ShapeCommitmentRevoked),
		string(store.ShapeDelayedSweep), string(store.ShapeHTLCClaim),
		string(store.SpendReorgedOut),
		string(store.ClosePending), string(store.CloseCoop),
		string(store.CloseForce), string(store.CloseBreach),
		string(store.ChanStaticRemote),
		string(store.DeadlineCSV), string(store.DeadlineHTLCIncoming),
		string(store.DeadlineHTLCOutgoing),
		string(store.RoleToLocal), string(store.RoleToRemote),
		string(store.BranchSF), string(store.BranchSQ),
	}
	for _, name := range mustBeCaught {
		if got := words.FindInternal("a sentence saying " + name + " out loud"); got == "" {
			t.Errorf("the schema declares %q and nothing would catch it on a screen", name)
		}
	}

	// Deliberately not caught, because each is a word somebody needs to read.
	// Written as pairs rather than a map because several schema values share the
	// same spelling — "unknown" is a shape, a channel type, a relevance and a
	// role — and a map would silently keep only the last reason.
	deliberatelyAllowed := []struct{ value, because string }{
		{string(store.SpendMempool), "waiting in the memory pool"},
		{string(store.SpendConfirmed), "it has not confirmed yet"},
		{string(store.ShapeJustice), "the claim that protects you"},
		{string(store.ShapeUnknown), "in a way Forktower does not recognise"},
		{string(store.ChanAnchors), "anchor outputs are not shown to users at all"},
		{string(store.ChanTaproot), "a word the people who use it know"},
		{string(store.ChanLegacy), "a legacy channel"},
		{string(store.Relevant), "relevant to you"},
		{string(store.Irrelevant), "not relevant"},
		{string(store.RelevanceUnknown), "we do not know"},
		{string(store.DeadlineCounting), "counting down"},
		{string(store.DeadlineResolved), "it was resolved"},
		{string(store.DeadlineExpired), "the time expired"},
		{string(store.RoleHTLC), "a payment in flight"},
		{string(store.RoleAnchor), "an anchor output"},
		{string(store.RoleUnknown), "we do not know"},
	}
	for _, allowed := range deliberatelyAllowed {
		got := words.FindInternal("a sentence saying " + allowed.value + " out loud")
		if got != "" {
			t.Errorf("%q is allowed on purpose (%s) but %q catches it",
				allowed.value, allowed.because, got)
		}
	}
}
