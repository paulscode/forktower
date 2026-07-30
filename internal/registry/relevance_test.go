package registry

import (
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

// The exposure scenarios, plus the boundaries around them. Table-driven
// because these are the rules that decide what gets watched, and a rule nobody
// wrote a row for is a rule nobody checked.
func TestTheScenarioMatrix(t *testing.T) {
	t.Parallel()

	const fork int32 = 1000

	tests := []struct {
		name       string
		facts      Facts
		want       store.Relevance
		wantReason string
	}{
		{
			name:       "before any split, everything is provisionally watched",
			facts:      Facts{OpenHeight: 900},
			want:       store.Relevant,
			wantReason: ReasonProvisional,
		},
		{
			name: "before any split, a channel closed on the user's chain is still watched",
			facts: Facts{
				OpenHeight: 900, CloseState: store.CloseCoop, CloseHeight: 950,
			},
			want:       store.Relevant,
			wantReason: ReasonProvisional,
		},
		{
			name:       "ordinary open channel funded before the fork",
			facts:      Facts{ForkKnown: true, ForkHeight: fork, OpenHeight: 900},
			want:       store.Relevant,
			wantReason: ReasonFundedBeforeFork,
		},
		{
			// S6, and the reason this whole engine exists. A channel the user
			// watched close is not closed on the chain nobody is looking at, and the
			// revoked commitments the counterparty still holds are spendable there.
			name: "S6: closed on the user's chain after the fork, still exposed on the other",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				OpenHeight: 900, CloseState: store.CloseCoop, CloseHeight: 1200,
			},
			want:       store.Relevant,
			wantReason: ReasonClosedOnlyOnSF,
		},
		{
			name: "S6: a close the node announced but no block has confirmed does not count as closed",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				OpenHeight: 900, CloseState: store.ClosePending,
			},
			want:       store.Relevant,
			wantReason: ReasonClosedOnlyOnSF,
		},
		{
			name: "S7: funded after the fork, so it never existed on the other chain",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork, OpenHeight: 1001,
			},
			want:       store.Irrelevant,
			wantReason: ReasonFundedAfterFork,
		},
		{
			name: "S7: unless the funding transaction reached the other chain as well",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork, OpenHeight: 1001, FundingSeenOnSQ: true,
			},
			want:       store.Relevant,
			wantReason: ReasonFundedAfterForkButMirrored,
		},
		{
			name: "S8: closed before the fork, so it is closed on both chains",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				OpenHeight: 500, CloseState: store.CloseCoop, CloseHeight: 900,
			},
			want:       store.Irrelevant,
			wantReason: ReasonClosedBeforeFork,
		},
		{
			// The boundary that is easiest to get wrong by one block. The
			// fork point is the last block the two chains *shared*, so a funding
			// transaction confirmed in it exists on both of them. Calling that
			// irrelevant would stop watching a channel that is exposed.
			name: "funded in the fork block itself is watched, not dismissed",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork, OpenHeight: fork,
			},
			want:       store.Relevant,
			wantReason: ReasonFundedBeforeFork,
		},
		{
			name: "closed in the fork block itself is closed on both",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				OpenHeight: 500, CloseState: store.CloseCoop, CloseHeight: fork,
			},
			want:       store.Irrelevant,
			wantReason: ReasonClosedBeforeFork,
		},
		{
			name: "funded one block after the fork is the first block that is safe",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork, OpenHeight: fork + 1,
			},
			want:       store.Irrelevant,
			wantReason: ReasonFundedAfterFork,
		},
		{
			// A channel we cannot place is exactly the one an attacker would
			// choose, so not knowing is an instruction to keep looking.
			name:       "a channel with no known funding height is watched anyway",
			facts:      Facts{ForkKnown: true, ForkHeight: fork},
			want:       store.RelevanceUnknown,
			wantReason: ReasonOpenHeightUnknown,
		},
		{
			name: "a channel with no known funding height but a pre-fork close is still safe",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				CloseState: store.CloseForce, CloseHeight: 999,
			},
			want:       store.Irrelevant,
			wantReason: ReasonClosedBeforeFork,
		},
		{
			name: "a breach close after the fork keeps the channel watched",
			facts: Facts{
				ForkKnown: true, ForkHeight: fork,
				OpenHeight: 900, CloseState: store.CloseBreach, CloseHeight: 1100,
			},
			want:       store.Relevant,
			wantReason: ReasonClosedOnlyOnSF,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := Classify(tc.facts)
			if got != tc.want {
				t.Errorf("got %q, want %q (reason: %s)", got, tc.want, reason)
			}
			if reason != tc.wantReason {
				t.Errorf("reason:\n got  %q\n want %q", reason, tc.wantReason)
			}
		})
	}
}

// Whatever the answer, it must be one the schema accepts and one that comes with
// a sentence: a classification with no explanation gives the user nothing to act
// on, and an unwatched channel with no recorded reason is the failure this whole
// classification exists to avoid.
func TestEveryAnswerIsValidAndExplained(t *testing.T) {
	t.Parallel()

	closeStates := []store.CloseState{
		"", store.CloseOpen, store.ClosePending,
		store.CloseCoop, store.CloseForce, store.CloseBreach,
	}
	heights := []int32{-1, 0, 1, 999, 1000, 1001, 5000}

	for _, forkKnown := range []bool{false, true} {
		for _, seen := range []bool{false, true} {
			for _, cs := range closeStates {
				for _, open := range heights {
					for _, closeH := range heights {
						f := Facts{
							ForkKnown: forkKnown, ForkHeight: 1000,
							OpenHeight: open, CloseState: cs, CloseHeight: closeH,
							FundingSeenOnSQ: seen,
						}
						rel, reason := Classify(f)
						if !rel.Valid() {
							t.Fatalf("%+v produced %q, which the schema does not accept", f, rel)
						}
						if reason == "" {
							t.Fatalf("%+v produced %q with no explanation", f, rel)
						}
					}
				}
			}
		}
	}
}

// A negative height is nonsense from a node that should not have sent it, and
// must not be read as "before the fork" — which would quietly mark a channel
// safe on the strength of a bad number.
func TestNonsenseHeightsAreNotReadAsSafe(t *testing.T) {
	t.Parallel()

	rel, reason := Classify(Facts{ForkKnown: true, ForkHeight: 1000, OpenHeight: -5})
	if rel != store.RelevanceUnknown {
		t.Errorf("a negative funding height produced %q (%s), want it watched", rel, reason)
	}

	rel, _ = Classify(Facts{
		ForkKnown: true, ForkHeight: 1000, OpenHeight: 900, CloseHeight: -5,
	})
	if rel != store.Relevant {
		t.Errorf("a negative close height produced %q, want it watched", rel)
	}
}
