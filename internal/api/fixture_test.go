package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// fixturePath is the contract between this package and the dashboard: a real
// response, committed, which the rendering tests then draw. It is what catches a
// field renamed on one side and not the other — a mismatch that produces a blank
// dashboard rather than an error anyone would see.
var fixturePath = filepath.Join("..", "..", "web", "testdata", "status.json")

// channelsFixturePath is the same contract for the exposure table, which is the
// part of the dashboard a worried person reads first.
var channelsFixturePath = filepath.Join("..", "..", "web", "testdata", "channels.json")

func TestTheDashboardFixtureMatchesWhatTheApiSends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	// A split in progress with everything populated, so the fixture exercises
	// every field the dashboard reads rather than only the calm ones.
	h.sen.set(func(f *fakeSentinel) {
		f.state = sentinel.State{
			Phase:      sentinel.PhaseSplit,
			Fork:       &chainview.BlockRef{Hash: hashOf("separation-point"), Height: 961_632},
			DetectedAt: 1_790_000_000,
			SFTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sf-tip"), Height: 961_771},
			},
			SQTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sq-tip"), Height: 961_753},
			},
			SFHealth: chainview.HealthOK,
			SQHealth: chainview.HealthOK,
		}
	})

	assertFixtureMatches(t, h, fixturePath)
}

// assertFixtureMatches records, or checks, one status response against a committed
// fixture. Shared so a second state cannot drift into being compared differently
// from the first.
func assertFixtureMatches(t *testing.T, h *harness, path string) {
	t.Helper()

	resp := h.do(t, http.MethodGet, "/api/v1/status", "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// Re-indented so a change shows up as a readable diff rather than one long line.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(envelope["data"], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pretty = append(pretty, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — run `go test ./internal/api -update` to create it", err)
	}
	if string(pretty) != string(want) {
		t.Errorf("the response shape changed, and the dashboard reads this fixture.\n"+
			"got:\n%s\nwant:\n%s\n"+
			"If the change is intended, run `go test ./internal/api -update` and check "+
			"that web/app_test.js still renders it.", pretty, want)
	}
}

// suspectedFixturePath is the same contract for the state *before* a split is
// confirmed.
//
// A second fixture because the first cannot reach this shape: `fork_candidate` is
// suppressed once a separation is recorded, so a confirmed-split response omits it
// by design. Without this the two newest fields on the response were drawn by the
// dashboard from a hand-written object in the rendering test, which agrees with
// itself no matter what the Go side is called — exactly the silent rename this
// mechanism exists to catch, and it would show a blank panel during a split.
var suspectedFixturePath = filepath.Join("..", "..", "web", "testdata", "status-suspected.json")

// The state a user is looking at while a split is building: the chains disagree,
// the daemon has not committed to it, and the screen must still say where they
// parted and what each says the disputed block is.
func TestTheSuspectedSplitFixtureMatchesWhatTheApiSends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.sen.set(func(f *fakeSentinel) {
		f.state = sentinel.State{
			Phase: sentinel.PhaseArmed,
			ForkCandidate: &chainview.BlockRef{
				Hash: hashOf("separation-candidate"), Height: 961_631,
			},
			ForkCandidateSince: 1_790_000_000,
			SplitSuspected:     true,
			Disagreement: &sentinel.HeightDisagreement{
				Height: 961_632,
				SFHash: hashOf("your-node-block"),
				SQHash: hashOf("other-chain-block"),
			},
			DisagreementSince: 1_790_000_000,
			SFTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sf-tip"), Height: 961_632},
			},
			SQTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sq-tip"), Height: 961_634},
			},
			SFHealth: chainview.HealthOK,
			SQHealth: chainview.HealthOK,
		}
	})

	assertFixtureMatches(t, h, suspectedFixturePath)
}

// The exposure table's own fixture: a channel under an active threat, with a
// countdown running, because that is the row the whole table exists to show and
// the one whose wording matters most.
func TestTheChannelsFixtureMatchesWhatTheApiSends(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.sen.set(func(f *fakeSentinel) {
		f.state.SQTip = &chainview.BlockMeta{
			BlockRef: chainview.BlockRef{Hash: hashOf("sq-tip"), Height: 961_753},
		}
		// Measured, and slow: a minority chain before a retarget. The projection
		// is the whole reason the block count is not shown on its own.
		f.state.SQCadence.IntervalSecs = 1800
		f.state.SQCadence.Samples = 12
	})

	// One channel under threat with a countdown, and one that is simply fine, so
	// the fixture exercises both halves of the table.
	underThreat := addChannel(t, h, fundingA, func(c *store.Channel) {
		c.PeerAlias = "ACINQ"
		c.Relevance = store.Relevant
		c.OpenHeight = 960_000
	})
	spendID := addSpend(t, h, underThreat, func(sp *store.Spend) {
		sp.BlockHeight = 961_600
	})
	addDeadline(t, h, spendID, func(d *store.Deadline) { d.DeadlineHeight = 962_600 })

	addChannel(t, h, fundingB, func(c *store.Channel) {
		c.PeerAlias = "alice's node"
		c.Relevance = store.Relevant
		c.CapacitySat = 40_000
		c.OpenHeight = 960_100
	})

	resp := h.do(t, http.MethodGet, "/api/v1/channels", "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(envelope["data"], "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pretty = append(pretty, '\n')

	if *update {
		if err := os.WriteFile(channelsFixturePath, pretty, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(channelsFixturePath)
	if err != nil {
		t.Fatalf("%v — run `go test ./internal/api -update` to create it", err)
	}
	if string(pretty) != string(want) {
		t.Errorf("the exposure table's shape changed, and the dashboard reads this "+
			"fixture.\ngot:\n%s\nwant:\n%s", pretty, want)
	}
}
