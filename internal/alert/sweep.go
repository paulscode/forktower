package alert

import (
	"context"
	"log/slog"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// sweepLoop looks for the conditions nothing announces.
//
// Two jobs on one timer. The first is the condition that has no event at all: a
// channel closed on the user's own chain whose close has still not reached the
// other one, which is an *absence* and so has nothing to attach to.
//
// The second is [Alerter.reconcile], which re-derives from stored state the
// alerts that do have events — because an event is delivered once, and an alert
// that can be missed permanently is the wrong shape for a program whose whole
// purpose is to be the thing that notices.
//
// Both also pick up anything that changed while the daemon was stopped, which no
// event ever will.
func (a *Alerter) sweepLoop(ctx context.Context) error {
	ticker := time.NewTicker(a.cfg.SweepInterval)
	defer ticker.Stop()

	// Once at startup, because the most likely reason a channel is in this state
	// is that it got there while nobody was running.
	a.sweep(ctx)
	a.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.sweep(ctx)
			// Everything that can be re-derived from what is stored, so that an
			// event nobody received is not an alert nobody ever gets.
			a.reconcile(ctx)
		}
	}
}

// sweep raises the slow-burn warning for channels closed on one chain only.
func (a *Alerter) sweep(ctx context.Context) {
	// Everything, then filtered here — because the safety rule is that `relevant`
	// *and* `unknown` are watched, and asking the store for only the relevant ones
	// quietly excluded the channels whose exposure nobody could establish. Those
	// are precisely the ones an attacker would choose.
	channels, err := a.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		a.log.Warn("could not re-read your channels to check for old exposures",
			slog.String("error", err.Error()))
		return
	}
	if len(channels) == 0 {
		return
	}

	spent, err := a.spentOnTheOtherChain(ctx)
	if err != nil {
		a.log.Warn("could not check which channels have already been closed on the "+
			"other chain", slog.String("error", err.Error()))
		return
	}

	for _, c := range channels {
		if c.Relevance == store.Irrelevant {
			// Established as not exposed, with a reason recorded. The only
			// classification that removes a channel from watching.
			continue
		}
		if c.CloseState == store.CloseOpen {
			// Still open on the user's own chain. Nothing unexpected about it.
			continue
		}
		if spent[c.ID] {
			// Already closed on the other chain too, so the exposure is over —
			// or it is a live incident with its own, much louder, alert.
			continue
		}
		a.raise(ctx, ClosedOnlyOnYourChain(c.ID))
	}
}

// spentOnTheOtherChain is the set of channels whose funding output has been seen
// spent on the chain the user's node does not follow.
func (a *Alerter) spentOnTheOtherChain(ctx context.Context) (map[int64]bool, error) {
	spends, err := a.store.ListSpends(ctx, store.SpendFilter{
		Branch: store.BranchSQ, Limit: store.MaxSpendLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(spends))
	for _, s := range spends {
		// A spend that was reorganised out is not a close: the exposure is back.
		if s.ChannelID != 0 && s.Status != store.SpendReorgedOut {
			out[s.ChannelID] = true
		}
	}
	return out, nil
}
