package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

type mirrorPayload struct {
	Decisions []MirrorDecision `json:"decisions"`
	Summary   struct {
		Copied   int    `json:"copied"`
		Waiting  int    `json:"waiting"`
		NeedsYou int    `json:"needs_you"`
		Refused  int    `json:"refused"`
		Note     string `json:"note"`
	} `json:"summary"`
}

func mirrorRows(t *testing.T, h *harness, query string) mirrorPayload {
	t.Helper()
	return decode[mirrorPayload](t, h.do(t, http.MethodGet, "/api/v1/mirror"+query, ""))
}

func addDecision(
	t *testing.T, h *harness, txid string, state store.MirrorState, reason string,
) {
	t.Helper()
	_, _, err := h.store.RecordMirrorDecision(context.Background(), store.MirrorDecision{
		TxID: txid, SourceBranch: store.BranchSF, TargetBranch: store.BranchSQ,
		Shape: store.ShapeMutualClose, Reason: reason, State: state,
		FirstSeenAt: 1_790_000_000, UpdatedAt: 1_790_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// **The refusals are the larger half and the more interesting one.** A view
// showing only what was copied would leave "why was that not copied?" with no
// answer, which is the question the whole allowlist exists to be able to answer.
func TestARefusalIsShownWithItsReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	addDecision(t, h, "aa"+strings.Repeat("0", 62), store.MirrorDenied,
		"the other party closed this channel on this chain. Copying that to the "+
			"other chain would put your money at risk there when it is not at risk now")

	got := mirrorRows(t, h, "")
	if len(got.Decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(got.Decisions))
	}
	d := got.Decisions[0]
	if !d.Display.Refused {
		t.Error("a refusal was not marked as one")
	}
	if d.Display.Copied || d.Display.NeedsYou {
		t.Errorf("a refusal reads as something going wrong: %+v", d.Display)
	}
	if !strings.Contains(d.Display.What, "at risk there") {
		t.Errorf("the reason did not reach the page: %q", d.Display.What)
	}
	if got.Summary.Refused != 1 {
		t.Errorf("refused count = %d, want 1", got.Summary.Refused)
	}
}

// Each state reads differently, because they mean different things and only one
// of them needs the user.
func TestEachOutcomeReadsAsWhatItIs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state    store.MirrorState
		mustSay  string
		copied   bool
		refused  bool
		needsYou bool
	}{
		{store.MirrorAccepted, "Copied to", true, false, false},
		{store.MirrorDenied, "Not copied", false, true, false},
		{store.MirrorPending, "Waiting to be copied", false, false, false},
		{store.MirrorRejected, "still trying", false, false, false},
		{store.MirrorAbandoned, "stopped trying", false, false, true},
	} {
		h := newHarness(t, nil)
		addDecision(t, h, "bb"+strings.Repeat("0", 62), tc.state, "a reason")

		got := mirrorRows(t, h, "")
		if len(got.Decisions) != 1 {
			t.Fatalf("%s: got %d decisions", tc.state, len(got.Decisions))
		}
		d := got.Decisions[0].Display
		if !strings.Contains(d.What, tc.mustSay) {
			t.Errorf("%s: %q does not say %q", tc.state, d.What, tc.mustSay)
		}
		if d.Copied != tc.copied || d.Refused != tc.refused || d.NeedsYou != tc.needsYou {
			t.Errorf("%s: flags = %+v", tc.state, d)
		}
	}
}

// The summary leads with what needs doing, not with what went well.
func TestTheSummaryLeadsWithWhatNeedsDoing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	addDecision(t, h, "11"+strings.Repeat("0", 62), store.MirrorAccepted, "copied")
	addDecision(t, h, "22"+strings.Repeat("0", 62), store.MirrorPending, "waiting")
	addDecision(t, h, "33"+strings.Repeat("0", 62), store.MirrorAbandoned, "gave up")

	got := mirrorRows(t, h, "")
	if got.Summary.Copied != 1 || got.Summary.Waiting != 1 || got.Summary.NeedsYou != 1 {
		t.Errorf("counts = %+v", got.Summary)
	}
	if !strings.Contains(got.Summary.Note, "could not be copied") {
		t.Errorf("the note does not lead with the problem: %q", got.Summary.Note)
	}
}

func TestTheMirrorCanBeAskedForOneKindOfOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	addDecision(t, h, "44"+strings.Repeat("0", 62), store.MirrorDenied, "not on the list")
	addDecision(t, h, "55"+strings.Repeat("0", 62), store.MirrorAccepted, "copied")

	if got := mirrorRows(t, h, "?state=denied"); len(got.Decisions) != 1 {
		t.Errorf("asking for refusals gave %d rows", len(got.Decisions))
	}

	resp := h.do(t, http.MethodGet, "/api/v1/mirror?state=nonsense", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an invented state gave %d, want 400", resp.StatusCode)
	}
}

// No transaction ids in full: a table is unreadable with them, and the full one
// is in the payload for anybody who wants it.
func TestTheTransactionIdIsAbbreviatedForTheTableAndKeptInFull(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	txid := "abcdef" + strings.Repeat("0", 52) + "123456"
	addDecision(t, h, txid, store.MirrorAccepted, "copied")

	got := mirrorRows(t, h, "")
	d := got.Decisions[0]
	if d.TxID != txid {
		t.Errorf("the full id was lost: %q", d.TxID)
	}
	if len(d.Display.ShortTxID) >= len(txid) {
		t.Errorf("the id was not abbreviated: %q", d.Display.ShortTxID)
	}
	if !strings.HasPrefix(d.Display.ShortTxID, "abcdef") ||
		!strings.HasSuffix(d.Display.ShortTxID, "123456") {
		t.Errorf("the abbreviation is not recognisable: %q", d.Display.ShortTxID)
	}
}

// --- The one control that creates exposure ---

// It is off until somebody says otherwise, and saying so is a deliberate act
// against one named channel.
func TestTheFundingOptInIsOffUntilSomebodyTurnsItOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := addChannel(t, h, fundingA, nil)

	before := channels(t, h)
	if before[0].MirrorFundingOptIn {
		t.Fatal("a channel defaulted to copying its funding transaction")
	}

	resp := h.do(t, http.MethodPost,
		"/api/v1/channels/"+itoa(id)+"/mirror-funding", `{"enabled":true}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("turning it on gave %d", resp.StatusCode)
	}

	after := channels(t, h)
	if !after[0].MirrorFundingOptIn {
		t.Error("the decision did not stick")
	}
}

// Turning it back off goes through the same path, so it cannot be a thing that
// is easy to switch on and hard to switch off.
func TestTheFundingOptInCanBeWithdrawn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := addChannel(t, h, fundingA, nil)

	for _, enabled := range []string{"true", "false"} {
		resp := h.do(t, http.MethodPost,
			"/api/v1/channels/"+itoa(id)+"/mirror-funding", `{"enabled":`+enabled+`}`)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("setting it to %s gave %d", enabled, resp.StatusCode)
		}
	}

	if channels(t, h)[0].MirrorFundingOptIn {
		t.Error("the opt-in could not be withdrawn")
	}
}

func TestTheFundingOptInRefusesWhatItCannotActON(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	id := addChannel(t, h, fundingA, nil)

	for _, tc := range []struct {
		name string
		path string
		body string
		want int
	}{
		{"a channel that does not exist", "/api/v1/channels/4242/mirror-funding",
			`{"enabled":true}`, http.StatusNotFound},
		{"not a number", "/api/v1/channels/abc/mirror-funding",
			`{"enabled":true}`, http.StatusBadRequest},
		{"no body at all", "/api/v1/channels/" + itoa(id) + "/mirror-funding",
			"", http.StatusBadRequest},
		{"nonsense", "/api/v1/channels/" + itoa(id) + "/mirror-funding",
			`{"enabled":`, http.StatusBadRequest},
	} {
		resp := h.do(t, http.MethodPost, tc.path, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// A database that has gone away must be an error, not an empty list reading as
// "nothing has been copied".
func TestAFailedReadIsNotAnEmptyMirrorList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	addDecision(t, h, "66"+strings.Repeat("0", 62), store.MirrorAccepted, "copied")
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodGet, "/api/v1/mirror", "")
	if resp.StatusCode == http.StatusOK {
		t.Error("a store that had gone away reported nothing copied rather than an error")
	}
}
