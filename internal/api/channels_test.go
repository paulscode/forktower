package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

const (
	fundingA = "1111111111111111111111111111111111111111111111111111111111111111"
	fundingB = "2222222222222222222222222222222222222222222222222222222222222222"
	commitTX = "3333333333333333333333333333333333333333333333333333333333333333"
)

// otherChainTip is where the watched chain is in these tests. Far enough past
// every deadline height used here that "remaining" is a real number rather than
// zero.
var otherChainTip = chainview.BlockMeta{
	BlockRef: chainview.BlockRef{Hash: chainhash.Hash{0x01}, Height: 1200},
}

func addChannel(t *testing.T, h *harness, txid string, mutate func(*store.Channel)) int64 {
	t.Helper()
	ctx := context.Background()
	if err := h.store.UpsertLNNode(ctx, store.LNNode{
		ID: "02node", Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	c := store.Channel{
		LNNodeID: "02node", FundingTxID: txid, FundingVout: 0,
		CapacitySat: 2_100_000, ChanType: store.ChanAnchors,
		PeerPubkey: "03aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		UpdatedAt:  1,
	}
	if mutate != nil {
		mutate(&c)
	}
	id, _, err := h.store.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Relevance != "" {
		if err := h.store.SetChannelRelevance(ctx, id, c.Relevance, "because", 1); err != nil {
			t.Fatal(err)
		}
	}
	if c.CloseState != "" && c.CloseState != store.CloseOpen {
		if err := h.store.SetChannelCloseSF(ctx, id, c.CloseState, commitTX, 900, 1); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func addSpend(t *testing.T, h *harness, channelID int64, mutate func(*store.Spend)) int64 {
	t.Helper()
	sp := store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: fundingA, OutpointVout: 0,
		SpendTxID: commitTX, SpendTxHex: "00", BlockHash: "aa", BlockHeight: 1000,
		Shape: store.ShapeCommitmentUnknown, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	}
	if mutate != nil {
		mutate(&sp)
	}
	id, _, err := h.store.RecordSpend(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addDeadline(t *testing.T, h *harness, spendID int64, mutate func(*store.Deadline)) int64 {
	t.Helper()
	d := store.Deadline{
		SpendEventID: spendID, Kind: store.DeadlineCSV, DeadlineHeight: 2000,
		State: store.DeadlineCounting, Escalation: 1, UpdatedAt: 1,
	}
	if mutate != nil {
		mutate(&d)
	}
	id, _, err := h.store.UpsertDeadline(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func channels(t *testing.T, h *harness) []Channel {
	t.Helper()
	return decode[[]Channel](t, h.do(t, http.MethodGet, "/api/v1/channels", ""))
}

// The row a person reads leads with who, how much and how long — not heights,
// hashes or channel ids.
func TestTheExposureTableLeadsWithMoneyAndTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) {
		f.state.SQCadence.IntervalSecs = 1800
		f.state.SQCadence.Samples = 10
		f.state.SQTip = &otherChainTip
	})

	id := addChannel(t, h, fundingA, func(c *store.Channel) {
		c.PeerAlias = "ACINQ"
		c.Relevance = store.Relevant
	})
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, nil)

	got := channels(t, h)
	if len(got) != 1 {
		t.Fatalf("got %d channels", len(got))
	}
	row := got[0]

	if row.Display.Partner != "ACINQ" {
		t.Errorf("partner = %q", row.Display.Partner)
	}
	if row.Display.AtRiskSat != 2_100_000 {
		t.Errorf("at risk = %d satoshis", row.Display.AtRiskSat)
	}
	// A block count is not an answer. The chain in this test takes half an hour a
	// block, so the same count means three times what instinct says.
	if row.Display.TimeLeft == "" {
		t.Error("no time left was given at all")
	}
	if !row.Display.TimeLeftIsEstimate {
		t.Error("the time was given without saying it is an estimate")
	}
	if row.Display.Status == "" {
		t.Error("no status was given")
	}
	if row.Threat.State != ThreatConfirmed {
		t.Errorf("threat = %q", row.Threat.State)
	}
	if row.Threat.HeadlineDeadline == nil {
		t.Fatal("no headline countdown")
	}
	// The block count stays available for Details.
	if row.Threat.HeadlineDeadline.RemainingBlocks == 0 {
		t.Error("the block count was dropped")
	}
}

// The rule that decides whether this dashboard is usable by the people it is
// for: nothing on the screen may be an internal classification, a height, or a
// hash.
func TestNothingOnScreenIsAnInternalName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) {
		f.state.SQCadence.IntervalSecs = 600
		f.state.SQCadence.Samples = 10
		f.state.SQTip = &otherChainTip
	})

	shapes := []store.SpendShape{
		store.ShapeMutualClose, store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeJustice, store.ShapeDelayedSweep,
		store.ShapeHTLCClaim, store.ShapeUnknown,
	}
	statuses := []store.SpendStatus{
		store.SpendConfirmed, store.SpendMempool, store.SpendReorgedOut,
	}
	relevances := []store.Relevance{
		store.Relevant, store.Irrelevant, store.RelevanceUnknown,
	}

	longNumber := regexp.MustCompile(`\d{5,}`)

	for _, shape := range shapes {
		for _, status := range statuses {
			for _, relevance := range relevances {
				sub := newHarness(t, nil)
				sub.sen.set(func(f *fakeSentinel) {
					f.state.SQCadence.IntervalSecs = 600
					f.state.SQCadence.Samples = 10
					f.state.SQTip = &otherChainTip
				})
				id := addChannel(t, sub, fundingA, func(c *store.Channel) {
					c.Relevance = relevance
					c.PeerAlias = "a partner"
				})
				spendID := addSpend(t, sub, id, func(sp *store.Spend) {
					sp.Shape, sp.Status = shape, status
				})
				addDeadline(t, sub, spendID, nil)

				for _, row := range channels(t, sub) {
					text := row.Display.Partner + " " + row.Display.Status + " " +
						row.Display.StatusAction + " " + row.Display.TimeLeft
					// One list, in one place, shared with the notification and
					// timeline checks — see internal/words.
					if leak := words.FindInternal(text); leak != "" {
						t.Fatalf("shape %q status %q put %q on the screen: %q",
							shape, status, leak, text)
					}
					if found := longNumber.FindString(text); found != "" {
						t.Fatalf("shape %q put the number %q on the screen: %q",
							shape, found, text)
					}
					if strings.Contains(text, fundingA[:8]) {
						t.Fatalf("a transaction id reached the screen: %q", text)
					}
				}
			}
		}
	}
}

// Every combination must produce a status sentence. A row with a blank status is
// a row that tells the reader nothing at all.
func TestEveryChannelGetsSomethingToRead(t *testing.T) {
	t.Parallel()

	for _, state := range []string{
		ThreatNone, ThreatWatch, ThreatMempool, ThreatConfirmed,
		ThreatResolved, ThreatLoss, "something-new",
	} {
		got := describeChannel(store.Channel{CapacitySat: 100}, Threat{State: state}, 600)
		if got.Status == "" {
			t.Errorf("threat %q produced no status", state)
		}
		if got.Partner == "" {
			t.Errorf("threat %q produced no partner", state)
		}
	}
}

// The counterparty picks their own name and is the adversary here.
func TestThePartnerNameIsTheLeastMisleadingOneAvailable(t *testing.T) {
	t.Parallel()

	withAlias := partnerName(store.Channel{PeerAlias: "ACINQ", PeerPubkey: "03abcdef0123456789"})
	if withAlias != "ACINQ" {
		t.Errorf("got %q", withAlias)
	}

	// No alias: a shortened key, because sixty-six characters of hex in a table
	// column is a wall rather than an identifier.
	withoutAlias := partnerName(store.Channel{PeerPubkey: "03abcdef0123456789abcdef"})
	if len(withoutAlias) > 16 || !strings.HasPrefix(withoutAlias, "03abcdef") {
		t.Errorf("got %q", withoutAlias)
	}

	// Nothing at all still reads as something.
	if got := partnerName(store.Channel{}); got == "" {
		t.Error("a channel with no partner information produced an empty name")
	}
	// A whitespace-only alias is not a name.
	if got := partnerName(store.Channel{PeerAlias: "   ", PeerPubkey: "03abcdef0123456789"}); got == "" ||
		strings.TrimSpace(got) == "" {
		t.Errorf("a blank alias produced %q", got)
	}
}

// Overstating what is at stake is the safe direction; understating it is not.
func TestWhatIsAtStakeIsAnUpperBound(t *testing.T) {
	t.Parallel()

	c := store.Channel{CapacitySat: 2_100_000}
	for _, state := range []string{ThreatMempool, ThreatConfirmed, ThreatLoss} {
		if got := atRisk(c, state); got != 2_100_000 {
			t.Errorf("threat %q puts %d at risk, want the whole capacity", state, got)
		}
	}
	for _, state := range []string{ThreatNone, ThreatWatch, ThreatResolved} {
		if got := atRisk(c, state); got != 0 {
			t.Errorf("threat %q puts %d at risk, want nothing", state, got)
		}
	}
	// A state nobody recognised errs toward saying something is at stake.
	if got := atRisk(c, "something-new"); got != 2_100_000 {
		t.Errorf("an unrecognised state put %d at risk", got)
	}
}

// A channel with one thing settled and another still running is not settled.
func TestTheWorstThingThatHappenedWins(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	id := addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Relevant })
	addSpend(t, h, id, func(sp *store.Spend) {
		sp.SpendTxID = commitTX
		sp.Shape = store.ShapeMutualClose
	})
	live := addSpend(t, h, id, func(sp *store.Spend) {
		sp.SpendTxID = fundingB
		sp.Shape = store.ShapeCommitmentUnknown
	})
	addDeadline(t, h, live, nil)

	got := channels(t, h)
	if got[0].Threat.State != ThreatConfirmed {
		t.Errorf("threat = %q, want the live one to win", got[0].Threat.State)
	}
}

// A countdown that ran out is the worst state there is, and must not be masked
// by anything calmer on the same channel.
func TestALossIsNeverMasked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	id := addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Relevant })
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, func(d *store.Deadline) { d.State = store.DeadlineExpired })

	got := channels(t, h)
	if got[0].Threat.State != ThreatLoss {
		t.Errorf("threat = %q, want a loss", got[0].Threat.State)
	}
	if got[0].Display.StatusAction == "" {
		t.Error("a lost channel offers nothing to do about it")
	}
}

// With no measured cadence, nothing is said about time. The engine starts from
// the network's nominal ten minutes, which on a minority chain before a retarget
// is wrong by a factor of four — and a confident wrong number is worse than
// none.
func TestNoMeasuredCadenceMeansNoClaimAboutTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) {
		// A seeded estimate, never measured.
		f.state.SQCadence.IntervalSecs = 600
		f.state.SQCadence.Samples = 0
		f.state.SQTip = &otherChainTip
	})

	id := addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Relevant })
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, nil)

	got := channels(t, h)
	if got[0].Display.TimeLeft != "" {
		t.Errorf("claimed %q about time from an unmeasured cadence", got[0].Display.TimeLeft)
	}
	if got[0].Threat.HeadlineDeadline.EstWallclockSecs != 0 {
		t.Error("projected a wall-clock time from an unmeasured cadence")
	}
	// The block count is still there, because that part is known.
	if got[0].Threat.HeadlineDeadline.RemainingBlocks == 0 {
		t.Error("the block count was dropped too")
	}
}

// A channel closed on the user's own chain but still exposed on the other is the
// case people do not expect, and the row has to say so.
func TestAChannelClosedOnOneChainOnlyStillReadsAsWatched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	addChannel(t, h, fundingA, func(c *store.Channel) {
		c.Relevance = store.Relevant
		c.CloseState = store.CloseCoop
	})

	got := channels(t, h)
	if got[0].Threat.State != ThreatWatch {
		t.Errorf("threat = %q", got[0].Threat.State)
	}
	if got[0].Display.StatusAction == "" {
		t.Error("a closed-but-exposed channel offers nothing to look at")
	}
}

// A channel established as not exposed says so plainly, and offers nothing to
// do — because there is nothing to do.
func TestAChannelWithNoExposureSaysSoAndAsksNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.sen.set(func(f *fakeSentinel) { f.state.SQTip = &otherChainTip })

	addChannel(t, h, fundingA, func(c *store.Channel) { c.Relevance = store.Irrelevant })

	got := channels(t, h)
	if got[0].Threat.State != ThreatNone {
		t.Errorf("threat = %q", got[0].Threat.State)
	}
	if got[0].Display.StatusAction != "" {
		t.Errorf("a safe channel asks the user to do %q", got[0].Display.StatusAction)
	}
	if got[0].Display.AtRiskSat != 0 {
		t.Errorf("a safe channel puts %d at risk", got[0].Display.AtRiskSat)
	}
}

// The exposure table names the user's counterparties and what they are worth,
// which is not something to serve to anyone who asks.
func TestTheNewEndpointsNeedAuthentication(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	for _, path := range []string{"/api/v1/channels", "/api/v1/spends", "/api/v1/deadlines"} {
		resp := h.doWith(t, http.MethodGet, path, "", nil)
		if got := errorCode(t, resp); got != CodeUnauthorized {
			t.Errorf("%s answered %q without a session, want %q", path, got, CodeUnauthorized)
		}
	}
}
