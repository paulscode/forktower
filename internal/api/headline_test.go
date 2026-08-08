package api

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// Golden files rather than inline assertions: the exact wording is the
// deliverable here, and a diff in a review is the only way a change to what a
// frightened user reads gets the attention it deserves.
func TestHeadlineGoldenFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   HeadlineInput
	}{
		{
			name: "getting_ready",
			in:   HeadlineInput{Phase: store.StateUnarmed, AlertsReachable: true},
		},
		{
			name: "protected",
			in: HeadlineInput{
				Phase: store.StateArmed, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
			},
		},
		{
			name: "attention_split",
			in: HeadlineInput{
				Phase: store.StateSplit, DetectedAt: 1_790_000_000, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
			},
		},
		{
			name: "action_needed_watching_paused",
			in: HeadlineInput{
				Phase: store.StateSplit, AlertsReachable: true,
				Paused: true, PausedSince: 1_790_000_500,
			},
		},
		{
			name: "at_risk",
			in: HeadlineInput{
				Phase: store.StateSplit, AlertsReachable: true,
				ExposedDeadline: &ExposedDeadline{Partner: "ACINQ", Since: 1_790_001_000},
			},
		},
		{
			// The case doc 13 singles out: a backend is unhappy but protection is
			// intact, so this must read as a note rather than an emergency.
			name: "degraded_while_protected",
			in: HeadlineInput{
				Phase: store.StateArmed, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthDegraded,
			},
		},
		{
			name: "setup_incomplete_no_alerts",
			in:   HeadlineInput{Phase: store.StateArmed, AlertsReachable: false},
		},
		{
			name: "split_with_no_way_to_reach_you",
			in: HeadlineInput{
				Phase: store.StateSplit, DetectedAt: 1_790_000_000, AlertsReachable: false,
			},
		},
		{
			name: "attention_failing_check",
			in: HeadlineInput{
				Phase: store.StateArmed, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
				FailingChecks: []ReadinessItem{{
					ID:    CheckSQSynced,
					Label: "Cannot reach the other chain",
					Why:   "Forktower would not see a channel being closed on that chain.",
					Action: &Action{
						Label: "Fix the setup", Href: anchorSetup,
					},
				}},
			},
		},
		{
			name: "split_resolving",
			in: HeadlineInput{
				Phase: store.StateResolving, DetectedAt: 1_790_000_000, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
			},
		},
		{
			name: "split_ended",
			in: HeadlineInput{
				Phase: store.StateResolvedSFWon, DetectedAt: 1_790_000_000, AlertsReachable: true,
				SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeHeadline(tc.in)

			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, '\n')

			path := filepath.Join("testdata", "headline", tc.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, encoded, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — run `go test ./internal/api -update` to create it", err)
			}
			if string(encoded) != string(want) {
				t.Errorf("the wording changed.\n got:\n%s\nwant:\n%s", encoded, want)
			}
		})
	}
}

// Rules that hold for every headline, whatever the situation. Checked over the
// whole table rather than case by case, so a new case cannot quietly break one.
func TestEveryHeadlineIsFitToShow(t *testing.T) {
	t.Parallel()

	// Terms this audience must never be shown, and internal state names that mean
	// nothing outside this codebase.
	forbidden := []string{
		"outpoint", "reorg", "macaroon", "rune", "assumeutxo", "neutrino",
		"BIP157", "P2WSH", "ZMQ", "bake", "scriptPubKey", "csv", "cltv",
		"UNARMED", "ARMED", "SPLIT", "RESOLVING", "RESOLVED_", "sq_", "sf_",
		"WRONG_BRANCH", "ECLIPSE_SUSPECT", "SYNCING", "DEGRADED",
	}

	entries, err := os.ReadDir(filepath.Join("testdata", "headline"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no golden files, so this test proves nothing")
	}

	seen := map[HeadlineState]bool{}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", "headline", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var h Headline
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		seen[h.State] = true

		if h.Title == "" || h.Detail == "" {
			t.Errorf("%s: a headline with no %s leaves the user guessing",
				entry.Name(), map[bool]string{true: "title", false: "detail"}[h.Title == ""])
		}
		if !strings.HasSuffix(strings.TrimSpace(h.Title), ".") {
			t.Errorf("%s: the title is not a sentence: %q", entry.Name(), h.Title)
		}
		for _, word := range forbidden {
			if strings.Contains(h.Title+" "+h.Detail, word) {
				t.Errorf("%s: %q appears in text a non-technical user reads", entry.Name(), word)
			}
		}
		if h.Action != nil {
			if h.Action.Label == "" {
				t.Errorf("%s: an action with no label", entry.Name())
			}
			// Exactly one destination. Two would mean the UI has to choose, which
			// is precisely the decision this object exists to have already made.
			if (h.Action.Endpoint == "") == (h.Action.Href == "") {
				t.Errorf("%s: an action must have exactly one of endpoint or href: %+v",
					entry.Name(), h.Action)
			}
		}
		// Anything urgent must say what to do about it.
		if (h.State == StateAtRisk || h.State == StateActionNeeded) && h.Action == nil {
			t.Errorf("%s: %q with no next step is anxiety, not information",
				entry.Name(), h.State)
		}
	}

	for _, state := range []HeadlineState{
		StateGettingReady, StateProtected, StateAttention, StateActionNeeded, StateAtRisk,
	} {
		if !seen[state] {
			t.Errorf("no golden file covers %q", state)
		}
	}
}

// The state most users will only ever see. A tool that looks worried when nothing
// is wrong teaches people to ignore it, so this one is asserted directly rather
// than only through a golden file.
func TestProtectedIsCalmAndCarriesNoAction(t *testing.T) {
	t.Parallel()

	h := ComputeHeadline(HeadlineInput{
		Phase: store.StateArmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
	})

	if h.State != StateProtected {
		t.Fatalf("state = %q, want protected", h.State)
	}
	if h.Action != nil {
		t.Errorf("a calm state offers something to do: %+v", h.Action)
	}
	for _, word := range []string{"warning", "error", "fail", "risk", "urgent", "problem"} {
		if strings.Contains(strings.ToLower(h.Title+" "+h.Detail), word) {
			t.Errorf("the reassuring state contains %q: %q / %q", word, h.Title, h.Detail)
		}
	}
}

// Urgency ordering is the whole design of this function: a user with money at
// risk must not be shown a note about a degraded backend.
func TestMoreUrgentSituationsWin(t *testing.T) {
	t.Parallel()

	// Everything wrong at once.
	everything := HeadlineInput{
		Phase:           store.StateSplit,
		SQHealth:        chainview.HealthDown,
		Paused:          true,
		AlertsReachable: false,
		FailingChecks:   []ReadinessItem{{ID: CheckSQSynced, Label: "x", Why: "y"}},
		ExposedDeadline: &ExposedDeadline{Since: 1},
	}
	if got := ComputeHeadline(everything).State; got != StateAtRisk {
		t.Errorf("state = %q, want at_risk to outrank everything", got)
	}

	// Watching stopped outranks a missing notification channel: one means the
	// daemon is not doing its job, the other that it cannot tell you about it.
	noDeadline := everything
	noDeadline.ExposedDeadline = nil
	got := ComputeHeadline(noDeadline)
	if got.State != StateActionNeeded {
		t.Errorf("state = %q, want action_needed", got.State)
	}
	if !strings.Contains(got.Detail, "stopped watching") {
		t.Errorf("detail = %q, want it to name the paused watching", got.Detail)
	}

	// A degraded backend during a split is still reported as the split: that is
	// the thing the user needs to understand.
	splitAndDegraded := HeadlineInput{
		Phase: store.StateSplit, AlertsReachable: true, SQHealth: chainview.HealthDegraded,
	}
	if got := ComputeHeadline(splitAndDegraded); !strings.Contains(got.Detail, "separated") {
		t.Errorf("detail = %q, want the split to be what is reported", got.Detail)
	}
}

// A state the daemon has never produced still has to render as something.
func TestAnUnknownPhaseStillProducesAHeadline(t *testing.T) {
	t.Parallel()

	h := ComputeHeadline(HeadlineInput{
		Phase: store.SplitState("SOMETHING_NEW"), AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
	})
	if h.Title == "" || h.Detail == "" {
		t.Errorf("an unknown phase produced a blank headline: %+v", h)
	}
}

// A user reading this during a split needs both facts, and there is only one
// line to give them: what has happened, and that they will not be told about the
// next thing.
func TestAMissingAlarmDoesNotHideASplit(t *testing.T) {
	t.Parallel()

	got := ComputeHeadline(HeadlineInput{
		Phase: store.StateSplit, DetectedAt: 1_790_000_000, AlertsReachable: false,
	})

	if got.State != StateActionNeeded {
		t.Errorf("state = %q, want action_needed: only the user can fix this", got.State)
	}
	if !strings.Contains(got.Detail, "separated") {
		t.Errorf("the split is invisible: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "reach you") {
		t.Errorf("the missing alarm is invisible: %q", got.Detail)
	}
	if got.Since != 1_790_000_000 {
		t.Errorf("since = %d, want when the split was detected", got.Since)
	}
	// And with no split, it says only the thing that is true.
	quiet := ComputeHeadline(HeadlineInput{Phase: store.StateArmed, AlertsReachable: false})
	if strings.Contains(quiet.Detail, "separated") {
		t.Errorf("a split was reported when there was none: %q", quiet.Detail)
	}
}

// The calm line must not assert something nobody checked.
//
// "Your node and the rest of the network are on the same chain" was the default
// case, reached whenever no louder state applied — and it went on being shown
// while the daemon's own records held two different blocks at one height. Being
// slow to declare a split is deliberate; stating the opposite in the meantime was
// not part of that design, it was the absence of one.
func TestTheCalmHeadlineDoesNotClaimTheChainsAgreeWhileTheyDiffer(t *testing.T) {
	t.Parallel()

	agreeing := ComputeHeadline(HeadlineInput{
		Phase: store.StateArmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
	})
	if !strings.Contains(agreeing.Detail, "same chain") {
		t.Errorf("the ordinary case stopped saying the ordinary thing: %q", agreeing.Detail)
	}

	diverging := ComputeHeadline(HeadlineInput{
		Phase: store.StateArmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
		Diverging: true, DivergingSince: 1_790_000_000,
	})
	if strings.Contains(diverging.Detail, "same chain") {
		t.Errorf("claimed the chains agree while they were holding different blocks: %q",
			diverging.Detail)
	}
	if !strings.Contains(diverging.Detail, "different blocks") {
		t.Errorf("did not say what is actually the case: %q", diverging.Detail)
	}
	if diverging.Since != 1_790_000_000 {
		t.Errorf("since = %d, want when the disagreement began", diverging.Since)
	}
	// Still calm. Two chains briefly holding different blocks is ordinary, and
	// colouring the dashboard for it would teach the user to ignore the colour.
	if diverging.State != StateProtected {
		t.Errorf("state = %q, want %q for an unconfirmed disagreement",
			diverging.State, StateProtected)
	}
	if diverging.Action != nil {
		t.Errorf("asked the user to do something about a routine event: %+v", diverging.Action)
	}
}

// A confirmed split outranks it, so the softer line cannot mask the real one.
func TestAConfirmedSplitOutranksAnUnconfirmedDisagreement(t *testing.T) {
	t.Parallel()

	got := ComputeHeadline(HeadlineInput{
		Phase: store.StateSplit, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
		Diverging: true, DivergingSince: 1_790_000_000,
	})
	if !strings.Contains(got.Detail, "separated") {
		t.Errorf("a confirmed split was reported as an unconfirmed one: %q", got.Detail)
	}
}

// A possible split must not be buried under the noise of the thing causing it.
//
// The second node dips into resynchronising as it follows a chain, and that raises
// a failing check and a degraded-view state. Both of those used to outrank anything
// said about the chains themselves, so during the one event that matters the
// headline read "still catching up" — while a block explorer showed two chains.
func TestASuspectedSplitOutranksTheNoiseOfASyncingView(t *testing.T) {
	t.Parallel()

	got := ComputeHeadline(HeadlineInput{
		Phase: store.StateArmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthSyncing,
		FailingChecks: []ReadinessItem{{
			ID: CheckSQSynced, Label: "Still catching up with the other chain",
			Why: "Forktower cannot see the whole picture yet.",
		}},
		Diverging: true, SplitSuspected: true, DivergingSince: 1_790_000_000,
	})

	if got.State != StateAttention {
		t.Errorf("state = %q, want %q for a possible split", got.State, StateAttention)
	}
	if !strings.Contains(got.Title, "chain split") {
		t.Errorf("title = %q, want it to name the possibility", got.Title)
	}
	if strings.Contains(got.Detail, "catching up") {
		t.Errorf("a possible split was masked by a resyncing view: %q", got.Detail)
	}
}

// Three levels, and the difference between them is what is *claimed* — never
// whether anything is said at all. A user can see a fork on a block explorer the
// moment it exists, so silence is the one thing none of these may be.
func TestTheChainDisagreementLadderSaysSomethingAtEveryLevel(t *testing.T) {
	t.Parallel()

	base := HeadlineInput{
		Phase: store.StateArmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
	}

	fresh := base
	fresh.Diverging, fresh.DivergingSince = true, 1_790_000_000
	suspected := fresh
	suspected.SplitSuspected = true
	confirmed := base
	confirmed.Phase, confirmed.DetectedAt = store.StateSplit, 1_790_000_000

	for name, tc := range map[string]struct {
		in        HeadlineInput
		wantState HeadlineState
		wantSays  string
	}{
		"just noticed": {fresh, StateProtected, "different blocks"},
		"suspected":    {suspected, StateAttention, "different"},
		"confirmed":    {confirmed, StateAttention, "separated"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ComputeHeadline(tc.in)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if !strings.Contains(got.Detail, tc.wantSays) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantSays)
			}
			if strings.Contains(got.Detail, "same chain") {
				t.Errorf("claimed the chains agree: %q", got.Detail)
			}
		})
	}
}

// Two healthy views that disagree hold the phase at UNARMED — the transition
// table has nowhere else to put them until the evidence accrues — and that state
// used to outrank everything said about the chains.
//
// So a daemon started next to a fork showed "Getting set up — nothing to do yet",
// the calmest sentence this software has, for as long as it lasted. Somebody who
// installed Forktower *because* they heard the chains had split is the likely
// reader of it.
func TestASuspectedSplitIsNotMaskedByStillGettingSetUp(t *testing.T) {
	t.Parallel()

	got := ComputeHeadline(HeadlineInput{
		Phase: store.StateUnarmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthOK,
		Diverging: true, SplitSuspected: true, DivergingSince: 1_790_000_000,
	})

	if got.State == StateGettingReady {
		t.Error("a possible chain split was reported as ordinary setup")
	}
	if !strings.Contains(got.Title, "chain split") {
		t.Errorf("title = %q, want it to name the possibility", got.Title)
	}
	if strings.Contains(got.Detail, "Nothing needs your attention") {
		t.Errorf("detail = %q, want it not to dismiss a possible split", got.Detail)
	}
}

// And an ordinary install is still allowed to look ordinary: a view that is
// genuinely syncing reports no tip, so no separation can be tracked and no
// suspicion can be raised. The ordering above costs that case nothing.
func TestAnOrdinarySetupStillReadsAsSetup(t *testing.T) {
	t.Parallel()

	got := ComputeHeadline(HeadlineInput{
		Phase: store.StateUnarmed, AlertsReachable: true,
		SFHealth: chainview.HealthOK, SQHealth: chainview.HealthSyncing,
	})
	if got.State != StateGettingReady {
		t.Errorf("state = %q, want %q for an install that is simply starting",
			got.State, StateGettingReady)
	}
}
