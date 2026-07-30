package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/paulscode/forktower/internal/store"
)

// TimelineEntry is one thing that happened.
type TimelineEntry struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	// Data is the original event, for the Details view. Passed through as raw
	// JSON rather than re-encoded, so the record shown is the record stored.
	Data json.RawMessage `json:"data,omitempty"`
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	afterID, ok := intParam(w, r, "after_id", 0)
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit", 0)
	if !ok {
		return
	}

	rows, err := s.store.ListTimeline(r.Context(), afterID, int(limit))
	if err != nil {
		s.fail(w, r, "reading the timeline", err)
		return
	}

	out := make([]TimelineEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, TimelineEntry{
			ID: e.ID, At: e.At, Kind: e.Kind, Summary: e.Summary,
			Data: rawJSON(e.Data),
		})
	}
	writeData(w, out)
}

// rawJSON passes stored event data through untouched, unless it is not valid
// JSON — in which case it is omitted rather than emitted, because a malformed
// body would break the whole response for one bad row.
func rawJSON(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// intParam reads a non-negative integer query parameter.
//
// A value that is not a number is refused rather than silently treated as zero:
// `limit=abc` returning the default would quietly show a caller something other
// than what they asked for.
func intParam(w http.ResponseWriter, r *http.Request, name string, def int64) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"The value given for "+name+" is not a whole number.")
		return 0, false
	}
	return n, true
}

// Alert is one condition worth telling the user about.
type Alert struct {
	ID           int64  `json:"id"`
	Tier         string `json:"tier"`
	Kind         string `json:"kind"`
	DedupKey     string `json:"dedup_key"`
	Subject      string `json:"subject"`
	Message      string `json:"message"`
	CreatedAt    int64  `json:"created_at"`
	LastRaisedAt int64  `json:"last_raised_at"`
	// AckedAt is zero while the user has not acknowledged it.
	AckedAt int64 `json:"acked_at"`
}

func alertOf(a store.Alert) Alert {
	return Alert{
		ID: a.ID, Tier: string(a.Tier), Kind: a.Kind, DedupKey: a.DedupKey,
		Subject: a.Subject, Message: a.Message,
		CreatedAt: a.CreatedAt, LastRaisedAt: a.LastRaisedAt, AckedAt: a.AckedAt,
	}
}
