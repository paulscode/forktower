package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

func spends(t *testing.T, h *harness, query string) []Spend {
	t.Helper()
	return decode[[]Spend](t, h.do(t, http.MethodGet, "/api/v1/spends"+query, ""))
}

func deadlines(t *testing.T, h *harness, query string) []Deadline {
	t.Helper()
	return decode[[]Deadline](t, h.do(t, http.MethodGet, "/api/v1/deadlines"+query, ""))
}

// The Details view. A reader who has opened this has asked for transaction ids
// and heights — but not for the names of this program's internal states.
func TestDetailsGivesTheFactsAndStillExplainsThem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	id := addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Relevant })
	addSpend(t, h, id, nil)

	got := spends(t, h, "")
	if len(got) != 1 {
		t.Fatalf("got %d spends", len(got))
	}
	sp := got[0]

	// The facts a reader came for.
	if sp.SpendTxID != commitTX || sp.BlockHeight != 1000 {
		t.Errorf("the record is missing what was asked for: %+v", sp)
	}
	// And the sentence explaining it.
	if sp.Display.What == "" {
		t.Error("a spend was shown with no explanation of what it was")
	}
	if strings.Contains(strings.ToLower(sp.Display.What), "commitment_unknown") {
		t.Errorf("an internal name reached the explanation: %q", sp.Display.What)
	}
	if sp.Display.Where != "the other chain" {
		t.Errorf("the chain was named %q", sp.Display.Where)
	}
	if !sp.Display.Confirmed {
		t.Error("a confirmed spend was not reported as confirmed")
	}
	// A full sixty-four characters is not more identifying to this reader.
	if len(sp.Display.ShortTxID) >= len(commitTX) {
		t.Errorf("the transaction was not shortened: %q", sp.Display.ShortTxID)
	}
}

// Every shape a spend can take must produce a sentence, and none of them may be
// an internal name.
func TestEverySpendShapeIsExplainedInWords(t *testing.T) {
	t.Parallel()

	shapes := []store.SpendShape{
		store.ShapeMutualClose, store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown, "something-new",
	}
	seen := map[string]bool{}
	for _, shape := range shapes {
		got := spendSentence(shape)
		if got == "" {
			t.Errorf("shape %q produced no explanation", shape)
		}
		if strings.Contains(strings.ToLower(got), string(shape)) {
			t.Errorf("shape %q was explained with its own internal name: %q", shape, got)
		}
		seen[got] = true
	}
	// Distinct shapes should read distinctly, or the explanation is not
	// explaining anything. The two unrecognised cases share one sentence, which
	// is why this is not a strict count.
	if len(seen) < 7 {
		t.Errorf("only %d distinct explanations for %d shapes", len(seen), len(shapes))
	}
}

func TestBranchesAreNamedTheWayTheRestOfThePageNamesThem(t *testing.T) {
	t.Parallel()

	if got := branchPhrase(store.BranchSF); got != "your node's chain" {
		t.Errorf("got %q", got)
	}
	if got := branchPhrase(store.BranchSQ); got != "the other chain" {
		t.Errorf("got %q", got)
	}
	// A branch nobody recognises still reads as words rather than as a code.
	got := branchPhrase("elsewhere")
	if got == "" || strings.Contains(got, "elsewhere") {
		t.Errorf("an unknown branch rendered as %q", got)
	}
}

func TestSpendsCanBeNarrowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	one := addChannel(t, h, fundingA, nil)
	two := addChannel(t, h, fundingB, nil)
	addSpend(t, h, one, nil)
	addSpend(t, h, two, func(sp *store.Spend) { sp.SpendTxID = fundingB })

	if got := spends(t, h, ""); len(got) != 2 {
		t.Errorf("unfiltered returned %d spends", len(got))
	}
	got := spends(t, h, "?channel_id="+itoa(one))
	if len(got) != 1 || got[0].ChannelID != one {
		t.Errorf("narrowing by channel returned %+v", got)
	}
	if got := spends(t, h, "?branch=sf"); len(got) != 0 {
		t.Errorf("the user's own chain returned %d spends", len(got))
	}
}

// A branch nobody recognises is refused rather than quietly returning
// everything, which would look like an answer.
func TestAnUnknownBranchIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.do(t, http.MethodGet, "/api/v1/spends?branch=elsewhere", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown branch returned %d", resp.StatusCode)
	}
}

// Countdowns explain what they are counting, in words.
func TestCountdownsExplainWhatTheyAreCounting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) {
		f.state.SQTip = &otherChainTip
		f.state.SQCadence.IntervalSecs = 1800
		f.state.SQCadence.Samples = 10
	})

	id := addChannel(t, h, fundingA, nil)
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, nil)

	got := deadlines(t, h, "")
	if len(got) != 1 {
		t.Fatalf("got %d countdowns", len(got))
	}
	d := got[0]
	if d.Display.What == "" {
		t.Error("a countdown was shown with nothing saying what it counts")
	}
	if strings.Contains(strings.ToLower(d.Display.What), "csv") {
		t.Errorf("an internal name reached the explanation: %q", d.Display.What)
	}
	if d.Display.TimeLeft == "" || !d.Display.TimeLeftIsEstimate {
		t.Errorf("the time was %q (estimate=%v)", d.Display.TimeLeft, d.Display.TimeLeftIsEstimate)
	}
	if d.RemainingBlocks == 0 {
		t.Error("the block count was dropped from Details, where it belongs")
	}
	if d.Display.Note != "" {
		t.Errorf("a countdown on real numbers carried a caveat: %q", d.Display.Note)
	}
}

// A countdown resting on an assumption says so, so a reader knows which kind of
// number they are looking at.
func TestAnAssumedCountdownSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	id := addChannel(t, h, fundingA, nil)
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, func(d *store.Deadline) { d.Assumed = true })

	got := deadlines(t, h, "")
	if !got[0].Assumed || got[0].Display.Note == "" {
		t.Errorf("an assumed countdown was shown as though it were known: %+v", got[0])
	}
}

// Every kind of countdown must say what it is counting.
func TestEveryCountdownKindIsExplained(t *testing.T) {
	t.Parallel()

	kinds := []store.DeadlineKind{
		store.DeadlineCSV, store.DeadlineHTLCIncoming, store.DeadlineHTLCOutgoing,
		"something-new",
	}
	longNumber := regexp.MustCompile(`\d`)
	for _, kind := range kinds {
		got := deadlineWhat(kind)
		if got == "" {
			t.Errorf("kind %q produced no explanation", kind)
		}
		if longNumber.MatchString(got) {
			t.Errorf("kind %q was explained with a number: %q", kind, got)
		}
	}
}

// Countdowns that have finished carry no time left, because there is none.
func TestAFinishedCountdownClaimsNoTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) {
		f.state.SQTip = &otherChainTip
		f.state.SQCadence.IntervalSecs = 600
		f.state.SQCadence.Samples = 10
	})

	id := addChannel(t, h, fundingA, nil)
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, func(d *store.Deadline) { d.State = store.DeadlineExpired })

	got := deadlines(t, h, "?state=expired")
	if len(got) != 1 {
		t.Fatalf("got %d finished countdowns", len(got))
	}
	if got[0].Display.TimeLeft != "" {
		t.Errorf("a finished countdown claimed %q remained", got[0].Display.TimeLeft)
	}
	// And the default is what is still running.
	if running := deadlines(t, h, ""); len(running) != 0 {
		t.Errorf("the default returned %d finished countdowns", len(running))
	}
}

func TestAnUnknownCountdownStateIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.do(t, http.MethodGet, "/api/v1/deadlines?state=elsewhere", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown state returned %d", resp.StatusCode)
	}
}

// A transaction id is shortened for reading, and a short one is left alone.
func TestTransactionsAreShortenedForReading(t *testing.T) {
	t.Parallel()

	got := shortID(commitTX)
	if !strings.Contains(got, "…") || len(got) >= len(commitTX) {
		t.Errorf("got %q", got)
	}
	if !strings.HasPrefix(got, commitTX[:8]) || !strings.HasSuffix(got, commitTX[len(commitTX)-8:]) {
		t.Errorf("the shortened form does not come from both ends: %q", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("a short id was altered: %q", got)
	}
}

// The inventory check has to distinguish "no channels" from "channels, none
// exposed" from "channels being watched", because those mean different things.
func TestTheInventoryCheckDistinguishesItsThreeAnswers(t *testing.T) {
	t.Parallel()

	t.Run("nothing found yet", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		item := itemByID(t, h.srv.Readiness(context.Background()), CheckChannelsInventoried)
		if item.OK {
			t.Error("no channels was reported as everything being in order")
		}
		if len(blockingFailures(h.srv.Readiness(context.Background()))) != 0 {
			t.Error("having no channels dragged the headline down")
		}
	})

	t.Run("channels, none exposed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Irrelevant })

		item := itemByID(t, h.srv.Readiness(context.Background()), CheckChannelsInventoried)
		if !item.OK {
			t.Errorf("channels with no exposure reported as a fault: %q", item.Label)
		}
		if !strings.Contains(item.Why, "one channel") {
			t.Errorf("the count does not read as English: %q", item.Why)
		}
	})

	t.Run("channels being watched", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Relevant })
		addChannel(t, h, fundingB, func(c *store.Channel) { c.Relevance = store.RelevanceUnknown })

		item := itemByID(t, h.srv.Readiness(context.Background()), CheckChannelsInventoried)
		if !item.OK {
			t.Errorf("watched channels reported as a fault: %q", item.Label)
		}
		if !strings.Contains(item.Why, "2 channels") {
			t.Errorf("the count is wrong or does not read as English: %q", item.Why)
		}
	})
}
