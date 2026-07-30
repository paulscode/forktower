package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/paulscode/forktower/internal/alert"
	"github.com/paulscode/forktower/internal/store"
)

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	limit, ok := intParam(w, r, "limit", 0)
	if !ok {
		return
	}

	rows, err := s.store.ListAlerts(r.Context(), store.AlertFilter{
		UnackedOnly: r.URL.Query().Get("unacked") == "true",
		Limit:       int(limit),
	})
	if err != nil {
		s.fail(w, r, "reading alerts", err)
		return
	}

	out := make([]Alert, 0, len(rows))
	for _, a := range rows {
		out = append(out, alertOf(a))
	}
	writeData(w, out)
}

func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "That is not an alert number.")
		return
	}

	changed, err := s.store.AckAlert(r.Context(), id, s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "There is no alert with that number.")
		return
	}
	if err != nil {
		s.fail(w, r, "acknowledging an alert", err)
		return
	}
	// Acknowledging twice is not an error. A duplicated click, or two open tabs,
	// must not produce a failure the user has to think about.
	_ = changed
	w.WriteHeader(http.StatusNoContent)
}

type testAlertsRequest struct {
	// Transport names one transport; empty means all of them.
	Transport string `json:"transport"`
}

func (s *Server) handleTestAlerts(w http.ResponseWriter, r *http.Request) {
	if s.alerter == nil {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"This Forktower has no notifications set up.")
		return
	}

	var req testAlertsRequest
	// An empty body means "test everything", which is the common case and must
	// not require the caller to send `{}`.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "That request was not readable.")
			return
		}
	}

	var names []string
	if req.Transport != "" {
		names = append(names, req.Transport)
	}

	results, err := s.alerter.TestTransports(r.Context(), names...)
	if errors.Is(err, alert.ErrNoSuchTransport) {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"There is no notification channel with that name.")
		return
	}
	if err != nil {
		s.fail(w, r, "testing the notification transports", err)
		return
	}
	writeData(w, results)
}
