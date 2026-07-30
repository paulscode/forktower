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
