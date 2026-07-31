package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/bus"
)

// TimelineSubscriberName identifies this consumer in the bus's drop diagnostics.
const TimelineSubscriberName = "timeline"

// TimelineSubscriber writes every event to the timeline.
//
// The timeline is the record a user looks at afterwards to understand what
// happened, and the record a support bundle carries. It is append-only for that
// reason: an audit trail that can be quietly trimmed is not an audit trail.
type TimelineSubscriber struct {
	store  *Store
	bus    *bus.Bus
	log    *slog.Logger
	now    func() int64
	events <-chan bus.Event
}

// NewTimelineSubscriber subscribes immediately.
//
// Subscribed in the constructor rather than in Run: a goroutine that subscribes
// when it happens to be scheduled leaves a window during startup where events are
// published to nobody, and the timeline's whole job is to have missed nothing.
func NewTimelineSubscriber(st *Store, b *bus.Bus, log *slog.Logger, now func() int64) *TimelineSubscriber {
	if log == nil {
		log = slog.New(discardHandler{})
	}
	return &TimelineSubscriber{
		store:  st,
		bus:    b,
		log:    log,
		now:    now,
		events: b.Subscribe(TimelineSubscriberName, bus.AllKinds()...),
	}
}

// Run records events until the context ends.
func (t *TimelineSubscriber) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-t.events:
			if !ok {
				return nil
			}
			t.record(ctx, e)
		}
	}
}

func (t *TimelineSubscriber) record(ctx context.Context, e bus.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		// The summary is the part a person reads, so a payload that will not
		// encode costs the Details view and nothing else.
		t.log.Debug("could not encode an event for the timeline",
			slog.String("kind", e.Kind()), slog.String("error", err.Error()))
		payload = nil
	}

	// Written with a context that outlives a shutdown arriving mid-write. The
	// event has already happened; losing the record of it because the daemon was
	// stopping would leave a gap exactly where someone will later look.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timelineWriteTimeout)
	defer cancel()

	if _, err := t.store.AppendTimeline(wctx, TimelineEntry{
		At:      t.now(),
		Kind:    e.Kind(),
		Summary: Summarize(e),
		Data:    string(payload),
	}); err != nil {
		t.log.Warn("could not record an event in the timeline",
			slog.String("kind", e.Kind()), slog.String("error", err.Error()))
	}
}

const timelineWriteTimeout = 5 * time.Second

// Summarize is the one sentence describing an event, in the past tense.
//
// A different register from an alert: an alert interrupts someone and has to say
// what to do about it, while this is read afterwards to understand what happened.
// Sharing one string between the two would make both worse.
//
// No hashes, no heights, no internal state names — the timeline sits on the same
// page as everything else a non-technical reader sees.
func Summarize(e bus.Event) string {
	switch ev := e.(type) {
	case bus.SplitStateChanged:
		return summarizeSplitState(ev)

	case bus.SplitBranchExtended:
		return fmt.Sprintf("A new block arrived on %s.", branchPhrase(ev.Branch))

	case bus.ViewHealthChanged:
		return summarizeViewHealth(ev)

	case bus.AlertRaised:
		return asSentence("Forktower raised an alert: " + ev.Message)

	case bus.ChannelUpserted:
		return summarizeChannel(ev)

	case bus.ChannelClosedSF:
		return summarizeChannelClose(ev)

	case bus.FundingSpent:
		return "One of your channels was closed on the other chain."

	case bus.SecondOrderSpent:
		return "Money from one of your closed channels was moved on the other chain."

	case bus.MempoolSighting:
		return "A transaction closing one of your channels was seen on the other " +
			"chain, before any block had accepted it."

	case bus.DeadlineEscalated:
		return summarizeDeadline(ev)

	case bus.DeadlineResolved:
		if ev.ByTxid == "" {
			return "A countdown stopped: what started it is no longer on the other chain."
		}
		return "A countdown was answered before it ran out."

	case bus.DeadlineExpiredLoss:
		return "A countdown ran out with nobody having answered it."

	case bus.SpendReorgedOut:
		return "Something that had happened on the other chain was undone by a " +
			"change to that chain."

	default:
		// An event kind this build does not know still belongs in the record.
		return "Something happened that this version of Forktower cannot describe."
	}
}

func summarizeSplitState(ev bus.SplitStateChanged) string {
	switch SplitState(ev.New) {
	case StateArmed:
		return "Forktower started watching both chains."
	case StateUnarmed:
		return "Forktower stopped watching."
	case StateSplit:
		return "The chains separated: your node and the rest of the network stopped agreeing."
	case StateResolving:
		return "One of the chains stopped producing blocks, so the split may be ending."
	case StateResolvedSFWon, StateResolvedSQWon:
		return "The split was recorded as over."
	default:
		return "The relationship between the two chains changed."
	}
}

func summarizeChannel(ev bus.ChannelUpserted) string {
	if !ev.New {
		return "Something about one of your channels changed."
	}
	if Relevance(ev.Channel.Relevance) == Irrelevant {
		return "Forktower found one of your channels, and it is not exposed on the other chain."
	}
	return "Forktower found one of your channels and started watching it."
}

// summarizeChannelClose says the thing people do not expect. A channel that has
// closed feels finished, and on the chain nobody is looking at it is not: the
// close has not happened there, so the old commitments the counterparty holds
// can still be spent. The timeline is read afterwards to understand what
// happened, and this is the entry that has to earn its place.
func summarizeChannelClose(ev bus.ChannelClosedSF) string {
	if CloseState(ev.State) == ClosePending {
		return "One of your channels started closing on your own chain. It still needs " +
			"watching until that close reaches the other one."
	}
	return "One of your channels closed on your own chain. It still needs watching until " +
		"that close reaches the other one."
}

// summarizeDeadline says how much time is left, in the register of something
// read afterwards rather than something interrupting.
//
// The time estimate is included when there is one, because a block count alone
// tells a reader nothing they can act on.
func summarizeDeadline(ev bus.DeadlineEscalated) string {
	if ev.EstWallClock == "" {
		return fmt.Sprintf("A countdown on one of your channels reached %d blocks left.",
			ev.RemainingBlocks)
	}
	return fmt.Sprintf("A countdown on one of your channels reached %d blocks left, "+
		"which was %s.", ev.RemainingBlocks, ev.EstWallClock)
}

// The two view states this summary distinguishes. Kept as literals rather than
// imported from the chain views: the event carries them as strings because the
// wire format is a deliberate choice, and this package has no other business
// knowing how a backend describes itself.
const (
	viewStateOK          = "OK"
	viewStateWrongBranch = "WRONG_BRANCH"
)

func summarizeViewHealth(ev bus.ViewHealthChanged) string {
	where := branchPhrase(ev.View)
	switch ev.New {
	case viewStateOK:
		return fmt.Sprintf("Forktower could see %s again.", where)
	case viewStateWrongBranch:
		return fmt.Sprintf("Forktower found it was not looking at %s, and paused watching.", where)
	default:
		return fmt.Sprintf("Forktower had trouble seeing %s.", where)
	}
}

// asSentence terminates a line that ends up carrying text from elsewhere. The
// timeline reads as prose, and one entry stopping mid-air makes the whole column
// look broken.
func asSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	switch s[len(s)-1] {
	case '.', '!', '?':
		return s
	default:
		return s + "."
	}
}

// branchPhrase is the words a user reads for one of the two chains.
func branchPhrase(branch string) string {
	switch Branch(branch) {
	case BranchSF:
		return "your node's chain"
	case BranchSQ:
		return "the other chain"
	default:
		return "one of the chains"
	}
}
