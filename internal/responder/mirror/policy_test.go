package mirror

import (
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

// ours is a transaction on one of the user's channels, moving the ordinary way:
// from the chain their node follows to the one it does not.
func ours(shape store.SpendShape) Inputs {
	return Inputs{
		Shape: shape, From: store.BranchSF, To: store.BranchSQ, ChannelKnown: true,
	}
}

// **The property this whole file exists for.**
//
// The table is written the other way round from the usual: it lists what *is*
// allowed, and then asserts that every other combination in the space is
// refused. A shape added to the classifier later lands in deny without anybody
// having to remember to come here, which is the only arrangement that survives
// the code changing.
func TestEverythingNotOnTheAllowlistIsRefused(t *testing.T) {
	t.Parallel()

	// Written out longhand rather than derived, so that adding a rule means
	// deliberately adding a line here.
	allowed := map[string]bool{
		"sf->sq mutual_close/":                 true,
		"sf->sq commitment_ours/":              true,
		"sf->sq delayed_sweep/commitment_ours": true,
		"sf->sq htlc_claim/commitment_ours":    true,
		"sf->sq justice/commitment_unknown":    true,
		"sf->sq justice/commitment_revoked":    true,
		"sq->sf mutual_close/":                 true,
	}

	shapes := []store.SpendShape{
		store.ShapeMutualClose, store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown,
		// A shape nobody has invented yet. It must be refused too.
		store.SpendShape("some_new_thing"),
	}
	sources := []store.SpendShape{
		"", store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeMutualClose,
	}
	directions := []struct{ from, to store.Branch }{
		{store.BranchSF, store.BranchSQ},
		{store.BranchSQ, store.BranchSF},
	}

	for _, dir := range directions {
		for _, shape := range shapes {
			for _, source := range sources {
				in := Inputs{
					Shape: shape, From: dir.from, To: dir.to,
					ChannelKnown: true, SourceShape: source,
				}
				got := Decide(in)

				key := string(dir.from) + "->" + string(dir.to) + " " +
					string(shape) + "/" + string(source)
				// Rules that ignore the source shape are keyed with an empty one,
				// so try both before concluding.
				bare := string(dir.from) + "->" + string(dir.to) + " " + string(shape) + "/"
				want := allowed[key] || (allowed[bare] && ignoresSource(shape))

				if got.Mirror != want {
					t.Errorf("%s: mirror = %v, want %v (%s)", key, got.Mirror, want, got.Reason)
				}
				if strings.TrimSpace(got.Reason) == "" {
					t.Errorf("%s: decided with no reason", key)
				}
				if got.Rule == "" {
					t.Errorf("%s: decided with no rule", key)
				}
			}
		}
	}
}

// ignoresSource is which rules do not depend on what a transaction follows.
func ignoresSource(shape store.SpendShape) bool {
	return shape == store.ShapeMutualClose || shape == store.ShapeCommitmentOurs
}

// The harm doc 05 names outright: copying the counterparty's force-close would
// put the user's money at risk on a chain where it is not at risk now.
func TestTheirCommitmentIsNeverMirrored(t *testing.T) {
	t.Parallel()

	for _, shape := range []store.SpendShape{
		store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked,
	} {
		got := Decide(ours(shape))
		if got.Mirror {
			t.Errorf("%s was mirrored, which manufactures exposure that did not exist", shape)
		}
		if got.Rule != DenyTheirCommitment {
			t.Errorf("%s was refused under %q, want %q", shape, got.Rule, DenyTheirCommitment)
		}
		if !strings.Contains(got.Reason, "at risk there when it is not at risk now") {
			t.Errorf("the reason does not say what the harm is: %q", got.Reason)
		}
	}
}

// Whose sweep it is turns entirely on what it follows, and getting that
// backwards would have Forktower helping the counterparty collect.
func TestASweepIsOursOnlyWhenItFollowsOurOwnClose(t *testing.T) {
	t.Parallel()

	for _, shape := range []store.SpendShape{store.ShapeDelayedSweep, store.ShapeHTLCClaim} {
		mine := ours(shape)
		mine.SourceShape = store.ShapeCommitmentOurs
		if got := Decide(mine); !got.Mirror {
			t.Errorf("%s following our own close was refused: %s", shape, got.Reason)
		}

		theirs := ours(shape)
		theirs.SourceShape = store.ShapeCommitmentUnknown
		got := Decide(theirs)
		if got.Mirror {
			t.Errorf("%s following their close was mirrored, which helps them collect", shape)
		}
		if got.Rule != DenyNotOursToSend {
			t.Errorf("%s: refused under %q", shape, got.Rule)
		}
	}
}

// A justice transaction is the mirror image of a sweep: punishing *their*
// commitment means we sent it.
func TestJusticeIsOursWhenItPunishesTheirClose(t *testing.T) {
	t.Parallel()

	for _, source := range []store.SpendShape{
		store.ShapeCommitmentUnknown, store.ShapeCommitmentRevoked,
	} {
		in := ours(store.ShapeJustice)
		in.SourceShape = source
		got := Decide(in)
		if !got.Mirror {
			t.Errorf("justice punishing %s was refused: %s", source, got.Reason)
		}
		if got.Rule != RuleJusticeWeSent {
			t.Errorf("justice punishing %s allowed under %q", source, got.Rule)
		}
	}

	// Punishing our own close means they sent it, and it is not ours to move.
	in := ours(store.ShapeJustice)
	in.SourceShape = store.ShapeCommitmentOurs
	got := Decide(in)
	if got.Mirror {
		t.Error("a justice transaction the counterparty sent against us was mirrored")
	}
	if !strings.Contains(got.Reason, "the other party sent it") {
		t.Errorf("the reason does not say whose it is: %q", got.Reason)
	}

	// And with nothing known about what it follows, it is not attributed at all.
	in.SourceShape = ""
	if Decide(in).Mirror {
		t.Error("justice with no known source was mirrored on an assumption")
	}
}

// The one setting that creates exposure rather than reducing it.
func TestAFundingTransactionNeedsTheUsersExplicitDecision(t *testing.T) {
	t.Parallel()

	base := Inputs{
		From: store.BranchSF, To: store.BranchSQ,
		ChannelKnown: true, IsFundingTx: true,
	}

	got := Decide(base)
	if got.Mirror {
		t.Error("a funding transaction was mirrored without the user asking")
	}
	if got.Rule != DenyFundingNotOptedIn {
		t.Errorf("refused under %q, want %q", got.Rule, DenyFundingNotOptedIn)
	}
	if !strings.Contains(got.Reason, "not there now") {
		t.Errorf("the reason does not say what it would do: %q", got.Reason)
	}

	opted := base
	opted.FundingOptIn = true
	if got := Decide(opted); !got.Mirror || got.Rule != RuleFundingOptIn {
		t.Errorf("an opted-in funding transaction was refused: %+v", got)
	}

	// The opt-in decides it whatever the classifier made of the transaction:
	// a funding transaction is not a spend of anything we watch.
	for _, shape := range []store.SpendShape{store.ShapeUnknown, store.ShapeCommitmentUnknown} {
		withShape := opted
		withShape.Shape = shape
		if !Decide(withShape).Mirror {
			t.Errorf("an opted-in funding transaction was refused because of shape %q", shape)
		}
	}
}

// The mirror is not a general relay. Bridging arbitrary transactions moves other
// people's money between chains without their consent.
func TestATransactionThatIsNotOursIsNotMoved(t *testing.T) {
	t.Parallel()

	for _, shape := range []store.SpendShape{
		store.ShapeMutualClose, store.ShapeCommitmentOurs, store.ShapeJustice,
	} {
		in := ours(shape)
		in.ChannelKnown = false
		got := Decide(in)
		if got.Mirror {
			t.Errorf("%s on a channel that is not ours was mirrored", shape)
		}
		if got.Rule != DenyUnknownChannel {
			t.Errorf("%s: refused under %q", shape, got.Rule)
		}
	}

	// Including a funding transaction, which does not get to skip the check.
	in := Inputs{
		From: store.BranchSF, To: store.BranchSQ,
		IsFundingTx: true, FundingOptIn: true, ChannelKnown: false,
	}
	if Decide(in).Mirror {
		t.Error("a funding transaction for somebody else's channel was mirrored")
	}
}

// The direction back to the user's own chain is deliberately much narrower:
// their node is already acting there, and almost anything moved this way would
// be doing something on their behalf that they did not choose.
func TestOnlyAnAgreedCloseComesBackTowardsYourOwnChain(t *testing.T) {
	t.Parallel()

	coop := Inputs{
		Shape: store.ShapeMutualClose, From: store.BranchSQ, To: store.BranchSF,
		ChannelKnown: true,
	}
	if got := Decide(coop); !got.Mirror {
		t.Errorf("an agreed close was not brought back: %s", got.Reason)
	}

	// Notably including our own force-close, which would close a channel here
	// that is still live.
	own := coop
	own.Shape = store.ShapeCommitmentOurs
	got := Decide(own)
	if got.Mirror {
		t.Error("a force-close was copied back onto the user's own chain, which " +
			"would close a channel their node has not closed")
	}
	if !strings.Contains(got.Reason, "acting on your node's behalf") {
		t.Errorf("the reason does not explain the restraint: %q", got.Reason)
	}
}

// A direction that is not one has a bug behind it, and must not be papered over.
func TestABranchToItselfIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ from, to store.Branch }{
		{store.BranchSQ, store.BranchSQ},
		{store.BranchSF, store.BranchSF},
		{"", ""},
		{store.BranchSF, "mainnet"},
		{"mainnet", store.BranchSF},
	} {
		in := Inputs{
			Shape: store.ShapeMutualClose, From: tc.from, To: tc.to, ChannelKnown: true,
		}
		if got := Decide(in); got.Mirror {
			t.Errorf("%q -> %q was accepted as a direction", tc.from, tc.to)
		}
	}
}

// Every verdict is readable by the person whose money it is.
func TestEveryReasonIsWrittenForAPerson(t *testing.T) {
	t.Parallel()

	jargon := []string{
		"sf", "sq", "nil", "upsert", "shape", "commitment_unknown", "htlc_claim",
		"delayed_sweep", "spend_event", "outpoint",
	}

	for _, shape := range []store.SpendShape{
		store.ShapeMutualClose, store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown,
	} {
		for _, source := range []store.SpendShape{"", store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown} {
			in := ours(shape)
			in.SourceShape = source
			reason := strings.ToLower(Decide(in).Reason)
			for _, word := range jargon {
				if strings.Contains(reason, " "+word+" ") {
					t.Errorf("%s/%s: the reason uses %q: %q", shape, source, word, reason)
				}
			}
			// Long enough to be a sentence rather than a label. Deliberately not
			// "must mention a chain": several refusals are about *whose*
			// transaction it is, which is the right thing to say, and forcing a
			// chain into them would make them worse.
			const shortestUsefulSentence = 40
			if len(reason) < shortestUsefulSentence {
				t.Errorf("%s/%s: the reason is a label rather than an explanation: %q",
					shape, source, reason)
			}
		}
	}
}

// Two rules permit a transaction for genuinely different reasons, and the stored
// record has to be able to tell them apart afterwards.
func TestEachPermissionNamesTheRuleThatGrantedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   Inputs
		want Rule
	}{
		{ours(store.ShapeMutualClose), RuleCoopClose},
		{ours(store.ShapeCommitmentOurs), RuleOwnCommitment},
		{withSource(ours(store.ShapeDelayedSweep), store.ShapeCommitmentOurs), RuleOwnSweep},
		{withSource(ours(store.ShapeHTLCClaim), store.ShapeCommitmentOurs), RuleOwnSweep},
		{withSource(ours(store.ShapeJustice), store.ShapeCommitmentUnknown), RuleJusticeWeSent},
	} {
		got := Decide(tc.in)
		if !got.Mirror {
			t.Errorf("%s was refused: %s", tc.in.Shape, got.Reason)
			continue
		}
		if got.Rule != tc.want {
			t.Errorf("%s allowed under %q, want %q", tc.in.Shape, got.Rule, tc.want)
		}
	}
}

func withSource(in Inputs, source store.SpendShape) Inputs {
	in.SourceShape = source
	return in
}
