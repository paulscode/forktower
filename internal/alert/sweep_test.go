package alert

import (
	"context"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// The slow-burn sweep is the one alert nothing announces, so these tests drive
// the alerter's own clock and its stored state rather than the bus.

const (
	fundingA = "1111111111111111111111111111111111111111111111111111111111111111"
	fundingB = "2222222222222222222222222222222222222222222222222222222222222222"
	closeTX  = "3333333333333333333333333333333333333333333333333333333333333333"
)

// addChannel records a channel in the state the caller wants it in.
func addChannel(
	t *testing.T, st *store.Store, txid string, closeState store.CloseState,
	relevance store.Relevance,
) int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertLNNode(ctx, store.LNNode{
		ID: "02node", Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := st.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: txid, FundingVout: 0,
		CapacitySat: 1_000_000, ChanType: store.ChanAnchors, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelRelevance(ctx, id, relevance, "because the test said so", 1); err != nil {
		t.Fatal(err)
	}
	if closeState != store.CloseOpen {
		if err := st.SetChannelCloseSF(ctx, id, closeState, closeTX, 900, 1); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// slowBurnAlerts is every warning the sweep has raised. Narrowed to the one kind
// the sweep produces, because the alerter raises others in the background and a
// count of everything would be a count of something else.
func slowBurnAlerts(t *testing.T, st *store.Store) []store.Alert {
	t.Helper()
	all, err := st.ListAlerts(context.Background(), store.AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Alert
	for _, a := range all {
		if a.Kind == KindClosedOnlyOnYours {
			out = append(out, a)
		}
	}
	return out
}

// The exposure people do not expect, and the only alert here that nothing
// announces: a channel closed on the user's own chain whose close has still not
// reached the other one. Nothing happens to trigger it, which is exactly what
// makes it dangerous.
func TestAChannelClosedOnOneChainOnlyIsWarnedAbout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })

	channelID := addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	h.start(t)

	waitFor(t, "the slow-burn warning", func() bool {
		return len(slowBurnAlerts(t, h.store)) == 1
	})

	got := slowBurnAlerts(t, h.store)[0]
	if got.Tier != store.TierWarning {
		t.Errorf("tier = %q", got.Tier)
	}
	if got.DedupKey != ClosedOnlyOnYourChain(channelID).DedupKey {
		t.Errorf("dedup key = %q", got.DedupKey)
	}

	// Swept repeatedly, warned about once. The condition persists for as long as
	// nobody fixes it, and repeating it every day would be noise.
	before := got.LastRaisedAt
	h.clock.Add(int64(time.Hour.Seconds()))
	time.Sleep(40 * time.Millisecond) //nolint:forbidigo // several sweeps
	after := slowBurnAlerts(t, h.store)
	if len(after) != 1 {
		t.Errorf("repeated sweeps produced %d warnings", len(after))
	}
	_ = before
}

// A channel still open on the user's own chain is not in this state at all.
func TestAnOpenChannelIsNotWarnedAbout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })

	addChannel(t, h.store, fundingA, store.CloseOpen, store.Relevant)
	h.start(t)

	time.Sleep(40 * time.Millisecond) //nolint:forbidigo // several sweeps
	if got := slowBurnAlerts(t, h.store); len(got) != 0 {
		t.Errorf("an open channel produced %d warnings", len(got))
	}
}

// A channel the classifier established is not exposed on the other chain has
// nothing to warn about.
func TestAChannelWithNoExposureIsNotWarnedAbout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })

	addChannel(t, h.store, fundingA, store.CloseCoop, store.Irrelevant)
	h.start(t)

	time.Sleep(40 * time.Millisecond) //nolint:forbidigo // several sweeps
	if got := slowBurnAlerts(t, h.store); len(got) != 0 {
		t.Errorf("a channel with no exposure produced %d warnings", len(got))
	}
}

// Once the close has reached the other chain too, the slow-burn exposure is
// over — and if it reached it as something worse, that has its own and much
// louder alert.
func TestAChannelClosedOnBothChainsIsNotWarnedAbout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })
	ctx := context.Background()

	channelID := addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	if _, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: fundingA, OutpointVout: 0,
		SpendTxID: closeTX, SpendTxHex: "00", BlockHash: "aa", BlockHeight: 950,
		Shape: store.ShapeMutualClose, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.start(t)

	time.Sleep(40 * time.Millisecond) //nolint:forbidigo // several sweeps
	if got := slowBurnAlerts(t, h.store); len(got) != 0 {
		t.Errorf("a channel closed on both chains produced %d warnings", len(got))
	}
}

// A close that was recorded on the other chain and then reorganised away is not
// a close: the exposure is back, and so is the warning.
func TestACloseThatLeftTheOtherChainBringsTheWarningBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })
	ctx := context.Background()

	channelID := addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	spendID, _, err := h.store.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: fundingA, OutpointVout: 0,
		SpendTxID: closeTX, SpendTxHex: "00", BlockHash: "aa", BlockHeight: 950,
		Shape: store.ShapeMutualClose, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateSpendStatus(ctx, spendID, store.SpendReorgedOut,
		"aa", 950, 2); err != nil {
		t.Fatal(err)
	}
	h.start(t)

	waitFor(t, "the warning to come back", func() bool {
		return len(slowBurnAlerts(t, h.store)) == 1
	})
}

// Several channels in the same state are several warnings, each closable on its
// own.
func TestEachExposedChannelGetsItsOwnWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })

	addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	addChannel(t, h.store, fundingB, store.CloseForce, store.Relevant)
	h.start(t)

	waitFor(t, "both warnings", func() bool {
		return len(slowBurnAlerts(t, h.store)) == 2
	})
}

// The sweep runs at startup, because the likeliest reason a channel is in this
// state is that it got there while nobody was running — and no event will ever
// arrive to say so.
func TestTheSweepRunsAtStartupNotOnlyOnItsSchedule(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = time.Hour })

	addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	h.start(t)

	waitFor(t, "the warning without waiting an hour", func() bool {
		return len(slowBurnAlerts(t, h.store)) == 1
	})
}

// A store that has gone away must not stop the alerter.
func TestASweepAgainstAClosedStoreIsSurvived(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(c *Config) { c.SweepInterval = 5 * time.Millisecond })

	addChannel(t, h.store, fundingA, store.CloseCoop, store.Relevant)
	h.start(t)
	waitFor(t, "the warning", func() bool {
		return len(slowBurnAlerts(t, h.store)) == 1
	})

	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) //nolint:forbidigo // letting it meet a dead store
}

func TestTheSweepIntervalHasADefault(t *testing.T) {
	t.Parallel()

	if got := (Config{}).withDefaults().SweepInterval; got != DefaultSweepInterval {
		t.Errorf("sweep interval = %v, want %v", got, DefaultSweepInterval)
	}
	if got := (Config{SweepInterval: time.Minute}).withDefaults().SweepInterval; got != time.Minute {
		t.Errorf("an explicit interval was overwritten: %v", got)
	}
}
