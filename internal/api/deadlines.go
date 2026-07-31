package api

import (
	"net/http"

	"github.com/paulscode/forktower/internal/deadline"
	"github.com/paulscode/forktower/internal/store"
)

// Deadline is one clock running against the user.
type Deadline struct {
	ID             int64  `json:"id"`
	SpendEventID   int64  `json:"spend_event_id"`
	Kind           string `json:"kind"`
	DeadlineHeight int32  `json:"deadline_height"`
	State          string `json:"state"`
	Escalation     int32  `json:"escalation"`
	// Assumed means an input was missing and a cautious floor was used. The
	// countdown is real; it may simply be shorter than the truth.
	Assumed         bool   `json:"assumed"`
	ResolvedByTxID  string `json:"resolved_by_txid,omitempty"`
	RemainingBlocks int32  `json:"remaining_blocks"`
	UpdatedAt       int64  `json:"updated_at"`

	Display DeadlineDisplay `json:"display"`
}

// DeadlineDisplay is the countdown in words.
type DeadlineDisplay struct {
	// What this clock is counting, in plain language.
	What string `json:"what"`
	// TimeLeft is a human duration, empty when there is no way to project one.
	TimeLeft           string `json:"time_left"`
	TimeLeftIsEstimate bool   `json:"time_left_is_estimate"`
	// Note is said when the countdown rests on an assumption, so a reader knows
	// which kind of number they are looking at.
	Note string `json:"note,omitempty"`
}

func (s *Server) handleDeadlines(w http.ResponseWriter, r *http.Request) {
	state := store.DeadlineState(r.URL.Query().Get("state"))
	if state == "" {
		state = store.DeadlineCounting
	}
	if !state.Valid() {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"state must be one of \"counting\", \"resolved\" or \"expired\"")
		return
	}

	rows, err := s.store.ListDeadlines(r.Context(), state)
	if err != nil {
		s.fail(w, r, "reading the countdowns", err)
		return
	}

	tip, cadence := s.otherChainProgress()

	out := make([]Deadline, 0, len(rows))
	for _, d := range rows {
		remaining := deadline.Remaining(d.DeadlineHeight, tip)
		out = append(out, Deadline{
			ID: d.ID, SpendEventID: d.SpendEventID, Kind: string(d.Kind),
			DeadlineHeight: d.DeadlineHeight, State: string(d.State),
			Escalation: d.Escalation, Assumed: d.Assumed,
			ResolvedByTxID: d.ResolvedByTxID, RemainingBlocks: remaining,
			UpdatedAt: d.UpdatedAt,
			Display:   describeDeadline(d, remaining, cadence),
		})
	}
	writeData(w, out)
}

// The sentences for each kind of clock.
const (
	deadlineCSVWhat = "How long you have to respond to a channel close on the other chain"
	deadlineInWhat  = "How long you have to claim a payment that was coming to you"
	deadlineOutWhat = "How long before a payment you sent times out"

	deadlineAssumedNote = "Your Lightning node did not say how long this window is, " +
		"so Forktower is counting from a cautious floor. The real window may be longer."
)

func describeDeadline(d store.Deadline, remaining int32, cadenceSecs float64) DeadlineDisplay {
	out := DeadlineDisplay{What: deadlineWhat(d.Kind)}
	if d.Assumed {
		out.Note = deadlineAssumedNote
	}
	if d.State != store.DeadlineCounting {
		return out
	}
	if projected, ok := deadline.Project(remaining, cadenceSecs); ok {
		out.TimeLeft = deadline.HumanDuration(projected)
		out.TimeLeftIsEstimate = true
	}
	return out
}

func deadlineWhat(kind store.DeadlineKind) string {
	switch kind {
	case store.DeadlineCSV:
		return deadlineCSVWhat
	case store.DeadlineHTLCIncoming:
		return deadlineInWhat
	case store.DeadlineHTLCOutgoing:
		return deadlineOutWhat
	default:
		return deadlineCSVWhat
	}
}
