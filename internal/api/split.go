package api

import (
	"encoding/json"
	"net/http"

	"github.com/paulscode/forktower/internal/store"
)

type confirmResolutionRequest struct {
	Outcome string `json:"outcome"`
}

// handleConfirmResolution records which chain the operator believes persisted.
//
// It records a label and nothing else. Watching, deadlines and alerts all
// continue exactly as before, so no single authenticated request — and no single
// mis-click during a live split — can switch the defence off. Winding down is a
// separate, reversible endpoint, and separating them is the whole point.
func (s *Server) handleConfirmResolution(w http.ResponseWriter, r *http.Request) {
	var req confirmResolutionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That request was not readable.")
		return
	}

	var outcome store.SplitState
	switch req.Outcome {
	case "SF_WON":
		outcome = store.StateResolvedSFWon
	case "SQ_WON":
		outcome = store.StateResolvedSQWon
	default:
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"Say which chain persisted: SF_WON or SQ_WON.")
		return
	}

	ctx := r.Context()
	current, err := s.store.GetSplitState(ctx)
	if err != nil {
		s.fail(w, r, "reading the split state", err)
		return
	}
	// Only meaningful while the split looks like it is ending. Recording an
	// outcome earlier would be a guess, and recording one later would overwrite a
	// decision already made.
	if current.State != store.StateResolving {
		writeError(w, http.StatusConflict, CodeWrongState,
			"There is no split waiting to be resolved right now.")
		return
	}

	current.State = outcome
	if err := s.store.SaveSplitState(ctx, current); err != nil {
		s.fail(w, r, "recording the outcome", err)
		return
	}

	s.log.Info("an operator recorded which chain persisted",
		"outcome", string(outcome))
	w.WriteHeader(http.StatusNoContent)
}
