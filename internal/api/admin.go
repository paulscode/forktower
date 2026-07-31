package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/paulscode/forktower/internal/store"
)

// maxRequestBody bounds what this server will read from a client. The same
// figure the other endpoints use; these bodies hold one number.
const maxRequestBody = 4096

// rescanRequest is what the dashboard sends.
type rescanRequest struct {
	// FromHeight is where to sweep from. Absent or zero means "from where the
	// chains separated", which is the answer almost everyone wants and the only
	// one most people could name.
	FromHeight int32 `json:"from_height"`
}

// RescanResult says what is about to happen.
type RescanResult struct {
	FromHeight int32 `json:"from_height"`
	ToHeight   int32 `json:"to_height"`
	// Display is the same thing in a sentence.
	Display string `json:"display"`
}

// handleRescan re-reads a stretch of the other chain.
//
// Offered because the daemon cannot always know it missed something. A Lightning
// node connected after a split had already begun, a database restored from a
// backup, a stretch of blocks read while a backend was lying — none of those
// announce themselves, and the person running this may know about them when the
// software does not.
//
// Deliberately not destructive and deliberately not exclusive: everything the
// sweep records is idempotent, so asking twice costs some reading and changes
// nothing, and the live scan carries on throughout.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if s.watcher == nil {
		writeError(w, http.StatusServiceUnavailable, CodeWrongState,
			"Forktower is not watching the other chain, so there is nothing to re-read.")
		return
	}

	var req rescanRequest
	if !readOptionalBody(w, r, &req) {
		return
	}
	if req.FromHeight < 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"The height to start from cannot be negative.")
		return
	}

	var from, to int32
	var queued bool
	if req.FromHeight == 0 {
		from, to, queued = s.watcher.RescanFromFork(r.Context())
	} else {
		from, to, queued = s.watcher.Rescan(r.Context(), req.FromHeight)
	}

	if !queued {
		// Nothing to do, said as a refusal rather than as a success. A button that
		// reports "done" without having done anything is how somebody comes to
		// believe a check has been run.
		writeError(w, http.StatusConflict, CodeWrongState,
			"There is nothing behind where Forktower has already read. If the chains "+
				"have not separated yet, there is no earlier point to go back to.")
		return
	}

	writeData(w, RescanResult{
		FromHeight: from,
		ToHeight:   to,
		Display: "Forktower is re-reading the other chain. It carries on watching for " +
			"new blocks while it does.",
	})
}

// readOptionalBody reads a JSON body, treating an empty one as an empty object.
//
// Empty is the ordinary case here: a button with no options sends nothing, and
// refusing that would make the simplest request the awkward one. Bounded like
// every other body this server reads.
func readOptionalBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That request was not readable.")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, into); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That request was not readable.")
		return false
	}
	return true
}

// standDownResult is what the two watching controls report.
type standDownResult struct {
	// WatchingActive is the state afterwards, so a caller never has to infer it
	// from which endpoint they called.
	WatchingActive bool   `json:"watching_active"`
	Display        string `json:"display"`
}

// handleStandDown stops watching the other chain, on purpose.
//
// Refused while any countdown is running, and that refusal is the whole reason
// this is a separate endpoint from confirming a split is over. A single
// authenticated POST — or one mis-click during a live incident — must not be
// able to switch off the defence at the moment it is doing something.
func (s *Server) handleStandDown(w http.ResponseWriter, r *http.Request) {
	if s.standDown == nil {
		writeError(w, http.StatusServiceUnavailable, CodeWrongState,
			"This version of Forktower cannot stand down.")
		return
	}

	counting, err := s.store.ListDeadlines(r.Context(), store.DeadlineCounting)
	if err != nil {
		s.fail(w, r, "checking whether anything is still counting down", err)
		return
	}
	if len(counting) > 0 {
		writeError(w, http.StatusConflict, CodeDeadlinesCounting,
			"Forktower is still counting down on "+
				countPhrase(len(counting), "deadline")+
				". Watching cannot be stood down until those are finished.")
		return
	}

	if err := s.standDown.Set(r.Context(), true); err != nil {
		s.fail(w, r, "recording that watching is stood down", err)
		return
	}
	s.log.Warn("watching the other chain has been stood down, on request")

	writeData(w, standDownResult{
		WatchingActive: false,
		Display: "Forktower has stopped watching the other chain. Nothing there is " +
			"being checked until you turn it back on.",
	})
}

// handleResume starts watching again.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if s.standDown == nil {
		writeError(w, http.StatusServiceUnavailable, CodeWrongState,
			"This version of Forktower cannot stand down.")
		return
	}
	if err := s.standDown.Set(r.Context(), false); err != nil {
		s.fail(w, r, "recording that watching has resumed", err)
		return
	}
	s.log.Info("watching the other chain has resumed, on request")

	writeData(w, standDownResult{
		WatchingActive: true,
		Display:        "Forktower is watching the other chain again.",
	})
}
