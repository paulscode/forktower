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
