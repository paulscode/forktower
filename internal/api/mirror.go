package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// MirrorDecision is one transaction the mirror considered, as the dashboard
// sees it.
type MirrorDecision struct {
	ID        int64  `json:"id"`
	TxID      string `json:"txid"`
	ChannelID int64  `json:"channel_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	State     string `json:"state"`
	// Reason is why it was copied, or why it was not. Present either way.
	Reason      string `json:"reason"`
	Attempts    int64  `json:"attempts"`
	FirstSeenAt int64  `json:"first_seen_at"`
	UpdatedAt   int64  `json:"updated_at"`

	Display MirrorDisplay `json:"display"`
}

// MirrorDisplay is one decision in words.
type MirrorDisplay struct {
	// What happened, in a sentence.
	What string `json:"what"`
	// Where it was being copied to.
	Where string `json:"where"`
	// ShortTxID is the transaction abbreviated for a table.
	ShortTxID string `json:"short_txid"`
	// Copied is true when it made it across. Refused means the policy declined
	// it, which is not a failure and must not read as one.
	Copied  bool `json:"copied"`
	Refused bool `json:"refused"`
	// NeedsYou is set when nothing further will happen on its own.
	NeedsYou bool `json:"needs_you"`
}

func (s *Server) handleMirror(w http.ResponseWriter, r *http.Request) {
	state := store.MirrorState(r.URL.Query().Get("state"))
	if state != "" && !state.Valid() {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"state must be one of \"denied\", \"pending\", \"accepted\", "+
				"\"rejected\" or \"abandoned\"")
		return
	}

	rows, err := s.store.ListMirrorDecisions(r.Context(), store.MirrorFilter{
		State: state,
	})
	if err != nil {
		s.fail(w, r, "reading what has been copied between the chains", err)
		return
	}

	out := make([]MirrorDecision, 0, len(rows))
	for _, d := range rows {
		out = append(out, mirrorView(d))
	}
	// The counts travel with the rows so the page can lead with a sentence
	// rather than making the reader total up a table.
	writeData(w, map[string]any{
		"decisions": out,
		"summary":   summariseMirror(rows),
	})
}

// mirrorView turns a stored decision into what the dashboard renders.
func mirrorView(d store.MirrorDecision) MirrorDecision {
	view := MirrorDecision{
		ID: d.ID, TxID: d.TxID, ChannelID: d.ChannelID,
		From: string(d.SourceBranch), To: string(d.TargetBranch),
		State: string(d.State), Reason: d.Reason, Attempts: d.Attempts,
		FirstSeenAt: d.FirstSeenAt, UpdatedAt: d.UpdatedAt,
	}
	view.Display = MirrorDisplay{
		Where:     branchPhrase(d.TargetBranch),
		ShortTxID: shortTxID(d.TxID),
	}

	switch d.State {
	case store.MirrorAccepted:
		view.Display.Copied = true
		view.Display.What = "Copied to " + branchPhrase(d.TargetBranch) + "."

	case store.MirrorDenied:
		// **Not a failure, and it must not read as one.** Most of what the mirror
		// sees it declines, and declining is the feature.
		view.Display.Refused = true
		view.Display.What = "Not copied — " + d.Reason

	case store.MirrorPending:
		view.Display.What = "Waiting to be copied to " + branchPhrase(d.TargetBranch) + "."

	case store.MirrorRejected:
		view.Display.What = branchPhrase(d.TargetBranch) +
			" has not accepted this yet. Forktower is still trying."

	case store.MirrorAbandoned:
		view.Display.NeedsYou = true
		view.Display.What = "Could not be copied to " + branchPhrase(d.TargetBranch) +
			". Forktower has stopped trying."

	default:
		view.Display.What = "Something happened to this transaction that this " +
			"version of Forktower cannot describe."
	}
	return view
}

// shortTxID abbreviates a transaction id for a table.
func shortTxID(txid string) string {
	const keep = 6
	if len(txid) <= keep*2 {
		return txid
	}
	return txid[:keep] + "…" + txid[len(txid)-keep:]
}

// mirrorOptInRequest is the body of the funding opt-in.
type mirrorOptInRequest struct {
	// Enabled is the decision. Named rather than implied by the endpoint, so that
	// turning it back off goes through the same path and cannot be forgotten.
	Enabled bool `json:"enabled"`
}

// handleChannelMirrorOptIn records the user's decision about copying a channel's
// funding transaction.
//
// **The only control in this interface that creates exposure rather than
// reducing it.** Copying a funding transaction puts the user's money on a chain
// it is not on now, so it is per-channel, off by default, and set only here —
// never by a poll, never inferred, never as a side effect of anything else.
func (s *Server) handleChannelMirrorOptIn(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That is not a channel number.")
		return
	}

	var req mirrorOptInRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"Say whether to turn copying on or off.")
		return
	}

	err = s.store.SetChannelMirrorOptIn(r.Context(), id, req.Enabled, s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "There is no channel with that number.")
		return
	}
	if err != nil {
		s.fail(w, r, "recording your decision about copying a funding transaction", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mirrorSummary is the one-line state of the whole mirror.
type mirrorSummary struct {
	// Copied, Waiting and NeedsYou count what has happened.
	Copied   int `json:"copied"`
	Waiting  int `json:"waiting"`
	NeedsYou int `json:"needs_you"`
	// Refused is how many the policy declined. Shown because the refusals are
	// the larger half and a user who sees only the copied ones would think the
	// mirror had barely done anything.
	Refused int `json:"refused"`
	// Note is the sentence under the table, empty when there is nothing to say.
	Note string `json:"note"`
}

// summariseMirror counts what the mirror has decided.
func summariseMirror(rows []store.MirrorDecision) mirrorSummary {
	var out mirrorSummary
	for _, d := range rows {
		switch d.State {
		case store.MirrorAccepted:
			out.Copied++
		case store.MirrorPending, store.MirrorRejected:
			out.Waiting++
		case store.MirrorAbandoned:
			out.NeedsYou++
		case store.MirrorDenied:
			out.Refused++
		}
	}

	switch {
	case out.NeedsYou > 0:
		out.Note = "Some transactions could not be copied to " + words.OtherChain +
			". Open Details to see which, and why."
	case out.Waiting > 0:
		out.Note = "Some transactions are still on their way to " + words.OtherChain + "."
	case out.Copied > 0:
		out.Note = "Your closes have been copied to " + words.OtherChain + "."
	}
	return out
}
