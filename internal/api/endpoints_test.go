package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

func hashOf(tag string) chainhash.Hash {
	var h chainhash.Hash
	copy(h[:], tag)
	return h
}

// The dashboard's first job is to answer "am I OK?" above the fold. Assembling
// that from four requests means four chances to render half an answer, so it
// comes from one.
func TestStatusAnswersEverythingAtOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	fork := &chainview.BlockRef{Hash: hashOf("fork"), Height: 850_000}
	h.sen.set(func(f *fakeSentinel) {
		f.state = sentinel.State{
			Phase:      sentinel.PhaseSplit,
			Fork:       fork,
			DetectedAt: 1_790_000_000,
			SFTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sf-tip"), Height: 850_140},
			},
			SQTip: &chainview.BlockMeta{
				BlockRef: chainview.BlockRef{Hash: hashOf("sq-tip"), Height: 850_122},
			},
			SFHealth: chainview.HealthOK,
			SQHealth: chainview.HealthOK,
		}
	})

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))

	if got.Headline.State != StateAttention {
		t.Errorf("headline state = %q, want attention during a split", got.Headline.State)
	}
	if got.Split.State != string(sentinel.PhaseSplit) {
		t.Errorf("split state = %q", got.Split.State)
	}
	if got.Split.Fork == nil || got.Split.Fork.Height != 850_000 {
		t.Fatalf("fork = %+v, want the recorded separation point", got.Split.Fork)
	}
	if got.Split.Fork.DetectedAt != 1_790_000_000 {
		t.Errorf("detected_at = %d", got.Split.Fork.DetectedAt)
	}

	sf := got.Split.Branches[string(chainview.BranchSF)]
	if sf.TipHeight != 850_140 || sf.SinceForkDepth != 140 {
		t.Errorf("sf branch = %+v, want 140 blocks since the separation", sf)
	}
	sq := got.Split.Branches[string(chainview.BranchSQ)]
	if sq.TipHeight != 850_122 || sq.SinceForkDepth != 122 {
		t.Errorf("sq branch = %+v", sq)
	}

	if got.Views[string(chainview.BranchSF)].PeerCount != 10 {
		t.Errorf("sf view = %+v", got.Views[string(chainview.BranchSF)])
	}
	if len(got.Readiness) == 0 {
		t.Error("no readiness checks were reported")
	}
}

// A branch with no tip yet, and no separation point, must still render.
func TestStatusBeforeAnythingHasHappened(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.sen.set(func(f *fakeSentinel) {
		f.state = sentinel.State{Phase: sentinel.PhaseUnarmed}
	})

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State != StateGettingReady {
		t.Errorf("headline state = %q, want getting_ready", got.Headline.State)
	}
	if got.Split.Fork != nil {
		t.Errorf("a separation point was reported before there was one: %+v", got.Split.Fork)
	}
	if _, ok := got.Split.Branches[string(chainview.BranchSF)]; !ok {
		t.Error("the branches object is missing a chain, so the UI has to guess")
	}
}

// The headline and the list beneath it must not disagree: a user told "alerts
// cannot reach you" at the top and shown a green tick below has no idea which to
// believe.
func TestTheHeadlineAgreesWithTheChecksBeneathIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.alerter.mu.Lock()
	h.alerter.names = nil
	h.alerter.mu.Unlock()

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State != StateActionNeeded {
		t.Fatalf("headline state = %q, want action_needed with no way to reach the user", got.Headline.State)
	}
	for _, item := range got.Readiness {
		if item.ID == CheckAlertTransports && item.OK {
			t.Error("the transport check passes while the headline says it does not")
		}
	}
}

// A configured transport that is broken is the case this check exists for.
// Reporting merely that transports are configured would call a dead alarm healthy.
func TestTransportCheckReportsTheLastTestNotTheConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	// A recorded failure for the configured transport.
	if _, err := h.store.UpsertAlert(ctx, store.Alert{
		Tier:     store.TierWarning,
		Kind:     alert.KindTransportFailing,
		DedupKey: alert.KindTransportFailing + ":my-phone",
		Subject:  "my-phone",
		Message:  "could not deliver",
	}); err != nil {
		t.Fatal(err)
	}

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	for _, item := range got.Readiness {
		if item.ID != CheckAlertTransports {
			continue
		}
		if item.OK {
			t.Fatal("a transport whose last test failed was reported as working")
		}
		if item.Action == nil {
			t.Error("a failing alarm with nothing to do about it is anxiety, not information")
		}
		return
	}
	t.Fatal("the transport check is missing from the readiness list")
}

// Before the first self-test there is no result to report, and greeting a new
// install with an alarm about itself would be wrong.
func TestTransportCheckBeforeTheFirstTest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	if err := h.store.SetMetaInt64(
		context.Background(), store.MetaLastSelfTestAt, 0); err != nil {
		t.Fatal(err)
	}

	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State != StateProtected {
		t.Errorf("headline state = %q, want a calm start while the first test is pending",
			got.Headline.State)
	}
}

// Most affected operators never made a deliberate choice about which rules their
// node follows, so this line is often the first thing that tells them.
func TestEnforcementCheckReadsTheNodesOwnDescription(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		subversion string
		want       string
	}{
		"enforcing":     {"/Satoshi:29.3.0/Knots:20260508/", "new rules"},
		"not enforcing": {"/Satoshi:29.0.0/", "existing rules"},
		"cannot tell":   {"", "Not sure"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			h.sen.set(func(f *fakeSentinel) {
				f.sfIdentity = chainview.Identity{Subversion: tc.subversion}
			})

			got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
			for _, item := range got.Readiness {
				if item.ID != CheckSFEnforcing {
					continue
				}
				if !contains(item.Label, tc.want) {
					t.Errorf("label = %q, want it to mention %q", item.Label, tc.want)
				}
				// Only a real divergence settles this, so it is never stated as
				// certain, and it never drags the dashboard into looking broken.
				if !contains(item.Label, "likely") && tc.subversion != "" {
					t.Errorf("label = %q, want it hedged", item.Label)
				}
				return
			}
			t.Fatal("the enforcement check is missing")
		})
	}

	// And it never turns the whole dashboard red: which side of a fork someone is
	// on is a fact about their setup, not a fault in it.
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.sfIdentity = chainview.Identity{} })
	got := decode[Status](t, h.do(t, http.MethodGet, "/api/v1/status", ""))
	if got.Headline.State != StateProtected {
		t.Errorf("headline state = %q, want an unknown client not to raise an alarm",
			got.Headline.State)
	}
}

func TestTimeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	for i := range 5 {
		if _, err := h.store.AppendTimeline(ctx, store.TimelineEntry{
			At: int64(1_790_000_000 + i), Kind: "split.state_changed",
			Summary: "something happened", Data: `{"old":"ARMED","new":"SPLIT"}`,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := decode[[]TimelineEntry](t, h.do(t, http.MethodGet, "/api/v1/timeline", ""))
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5", len(got))
	}
	// Ascending id, which is both stable and chronological — a list that reorders
	// between refreshes is unreadable.
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Errorf("entries are not in ascending order: %d then %d", got[i-1].ID, got[i].ID)
		}
	}
	if string(got[0].Data) != `{"old":"ARMED","new":"SPLIT"}` {
		t.Errorf("the stored event was not passed through: %s", got[0].Data)
	}

	after := decode[[]TimelineEntry](t, h.do(t,
		http.MethodGet, "/api/v1/timeline?after_id="+itoa(got[2].ID), ""))
	if len(after) != 2 {
		t.Errorf("got %d entries after id %d, want 2", len(after), got[2].ID)
	}

	limited := decode[[]TimelineEntry](t, h.do(t, http.MethodGet, "/api/v1/timeline?limit=2", ""))
	if len(limited) != 2 {
		t.Errorf("got %d entries with limit 2", len(limited))
	}
}

// A parameter that is not a number is refused rather than silently treated as
// the default: quietly showing something other than what was asked for is worse
// than saying no.
func TestTimelineRefusesNonsenseParameters(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	for _, query := range []string{"?limit=abc", "?after_id=-1", "?limit=-5"} {
		resp := h.do(t, http.MethodGet, "/api/v1/timeline"+query, "")
		if got := errorCode(t, resp); got != CodeBadRequest {
			t.Errorf("%s gave %q, want %q", query, got, CodeBadRequest)
		}
	}
}

func TestAlertsListAndAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	up, err := h.store.UpsertAlert(ctx, store.Alert{
		Tier: store.TierWarning, Kind: "split_detected", DedupKey: "split_detected",
		Message: "The chains have separated.", CreatedAt: 1, LastRaisedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := decode[[]Alert](t, h.do(t, http.MethodGet, "/api/v1/alerts", ""))
	if len(got) != 1 || got[0].Message == "" {
		t.Fatalf("got %+v", got)
	}

	resp := h.do(t, http.MethodPost, "/api/v1/alerts/"+itoa(up.ID)+"/ack", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("acknowledging returned %d, want 204", resp.StatusCode)
	}

	// Acknowledging twice is not an error: a duplicated click, or two open tabs,
	// must not produce a failure the user has to think about.
	resp = h.do(t, http.MethodPost, "/api/v1/alerts/"+itoa(up.ID)+"/ack", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("acknowledging twice returned %d", resp.StatusCode)
	}

	unacked := decode[[]Alert](t, h.do(t, http.MethodGet, "/api/v1/alerts?unacked=true", ""))
	if len(unacked) != 0 {
		t.Errorf("got %d unacknowledged alerts after acknowledging the only one", len(unacked))
	}
}

func TestAckingSomethingThatIsNotThere(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.do(t, http.MethodPost, "/api/v1/alerts/9999/ack", "")
	if got := errorCode(t, resp); got != CodeNotFound {
		t.Errorf("got %q, want %q", got, CodeNotFound)
	}

	resp = h.do(t, http.MethodPost, "/api/v1/alerts/not-a-number/ack", "")
	if got := errorCode(t, resp); got != CodeBadRequest {
		t.Errorf("got %q, want %q", got, CodeBadRequest)
	}
}

func TestTestingAlerts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	// An empty body means "test everything", which is the common case and must
	// not require the caller to send `{}`.
	got := decode[[]alert.SelfTestResult](t, h.do(t, http.MethodPost, "/api/v1/alerts/test", ""))
	if len(got) != 1 || got[0].Transport != "my-phone" || !got[0].OK {
		t.Errorf("got %+v", got)
	}

	got = decode[[]alert.SelfTestResult](t, h.do(t,
		http.MethodPost, "/api/v1/alerts/test", `{"transport":"my-phone"}`))
	if len(got) != 1 {
		t.Errorf("got %+v", got)
	}

	// A name nothing answers to is refused. A cheerful empty result would let the
	// user conclude their notifications were fine.
	resp := h.doWith(t, http.MethodPost, "/api/v1/alerts/test",
		`{"transport":"typo"}`, func(r *http.Request) {
			r.Header.Set("Origin", h.origin())
		})
	h.alerter.mu.Lock()
	h.alerter.testErr = alert.ErrNoSuchTransport
	h.alerter.mu.Unlock()
	_ = resp

	resp = h.do(t, http.MethodPost, "/api/v1/alerts/test", `{"transport":"typo"}`)
	if got := errorCode(t, resp); got != CodeNotFound {
		t.Errorf("got %q, want %q", got, CodeNotFound)
	}
}

// The endpoint records a label and nothing else. Watching, deadlines and alerts
// all continue, so no single authenticated request — and no single mis-click
// during a live split — can switch the defence off.
func TestConfirmResolutionStopsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	if err := h.store.SaveSplitState(ctx, store.Split{
		State: store.StateResolving, ForkHash: hashOf("fork").String(), ForkHeight: 850_000,
	}); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodPost, "/api/v1/split/confirm-resolution", `{"outcome":"SF_WON"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}

	recorded, err := h.store.GetSplitState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.State != store.StateResolvedSFWon {
		t.Errorf("state = %q, want the outcome recorded", recorded.State)
	}
	// The separation point is untouched: it anchors rescans and decides which
	// channels are exposed.
	if recorded.ForkHeight != 850_000 {
		t.Errorf("the separation point moved to %d", recorded.ForkHeight)
	}
	// And nothing was asked to stand down.
	if h.sen.Paused() {
		t.Error("recording an outcome stopped watching")
	}
}

func TestConfirmResolutionOnlyWhileASplitIsEnding(t *testing.T) {
	t.Parallel()

	for _, state := range []store.SplitState{
		store.StateUnarmed, store.StateArmed, store.StateSplit,
		store.StateResolvedSFWon, store.StateResolvedSQWon,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			if err := h.store.SaveSplitState(
				context.Background(), store.Split{State: state}); err != nil {
				t.Fatal(err)
			}

			resp := h.do(t, http.MethodPost, "/api/v1/split/confirm-resolution",
				`{"outcome":"SF_WON"}`)
			if got := errorCode(t, resp); got != CodeWrongState {
				t.Errorf("got %q, want %q", got, CodeWrongState)
			}
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("status %d, want 409", resp.StatusCode)
			}
		})
	}
}

func TestConfirmResolutionRejectsNonsense(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	if err := h.store.SaveSplitState(ctx, store.Split{State: store.StateResolving}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{}`, `{"outcome":"MAYBE"}`, `not json`, `{"outcome":""}`} {
		resp := h.do(t, http.MethodPost, "/api/v1/split/confirm-resolution", body)
		if got := errorCode(t, resp); got != CodeBadRequest {
			t.Errorf("body %q gave %q, want %q", body, got, CodeBadRequest)
		}
	}
}

func decodeInto(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// This audience is never shown a raw error string. A storage failure can carry a
// file path, and the support-bundle export makes anything shown here portable.
func TestStorageFailuresAreNotShownToTheUser(t *testing.T) {
	t.Parallel()

	requests := []struct {
		method string
		path   string
		body   string
		setup  func(*harness)
	}{
		{http.MethodGet, "/api/v1/status", "", nil},
		{http.MethodGet, "/api/v1/timeline", "", nil},
		{http.MethodGet, "/api/v1/alerts", "", nil},
		{http.MethodPost, "/api/v1/alerts/1/ack", "", nil},
		{http.MethodPost, "/api/v1/split/confirm-resolution", `{"outcome":"SF_WON"}`, nil},
	}

	for _, req := range requests {
		t.Run(req.method+" "+req.path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			if req.setup != nil {
				req.setup(h)
			}
			// The database is gone underneath a running daemon: a disk full, a
			// permissions change, a container restarted out from under it.
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}

			resp := h.do(t, req.method, req.path, req.body)
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
				// A read that answers from cached state rather than storage is fine.
				return
			}
			if got := errorCode(t, resp); got != CodeInternal {
				t.Errorf("got %q, want %q", got, CodeInternal)
			}
		})
	}
}

func TestInternalFailuresSayNothingRevealing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodGet, "/api/v1/timeline", "")
	var env envelope
	if err := decodeInto(resp, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatal("a dead database was reported as success")
	}
	for _, leak := range []string{"sql:", "database", ".db", "/tmp", "closed", "SQLITE"} {
		if contains(env.Error.Message, leak) {
			t.Errorf("the message shown to the user contains %q: %q", leak, env.Error.Message)
		}
	}
}

// A row whose stored payload is not valid JSON must not break the whole response
// for every other row.
func TestOneMalformedTimelineRowDoesNotBreakTheRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	for _, data := range []string{`{"ok":true}`, `not json at all`, ``} {
		if _, err := h.store.AppendTimeline(ctx, store.TimelineEntry{
			At: 1, Kind: "split.state_changed", Summary: "something", Data: data,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := decode[[]TimelineEntry](t, h.do(t, http.MethodGet, "/api/v1/timeline", ""))
	if len(got) != 3 {
		t.Fatalf("got %d entries, want all 3", len(got))
	}
	if string(got[0].Data) != `{"ok":true}` {
		t.Errorf("the valid row was altered: %s", got[0].Data)
	}
	for _, e := range got[1:] {
		if e.Data != nil {
			t.Errorf("an unreadable payload was passed through: %s", e.Data)
		}
	}
}

// An unknown path is a 404, not a panic or an empty 200.
func TestUnknownPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	if resp := h.do(t, http.MethodGet, "/api/v1/nothing-here", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	// And a method the endpoint does not offer.
	if resp := h.do(t, http.MethodDelete, "/api/v1/status", ""); resp.StatusCode == http.StatusOK {
		t.Errorf("DELETE on a read-only endpoint returned %d", resp.StatusCode)
	}
}
