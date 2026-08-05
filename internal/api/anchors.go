package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/paulscode/forktower/internal/anchors"
)

// Anchors is the anchor-peer list this installation is using.
//
// An interface so the handler can be tested without a filesystem, and so that
// nothing here can reach for a list on its own — importing is something a user
// does, never something the dashboard decides to do.
type Anchors interface {
	Load() anchors.Active
	Import(raw, sig []byte) (anchors.List, error)
}

// MountAnchors adds the anchor-list routes. Without a store there are no routes,
// and the dashboard simply does not offer the section.
func (s *Server) MountAnchors(a Anchors) {
	if a == nil {
		return
	}
	s.anchors = a
	s.mux.Handle("GET /api/v1/anchors", s.guard(s.handleAnchors))
	s.mux.Handle("POST /api/v1/anchors/import", s.guard(s.handleImportAnchors))
}

// AnchorList is what the dashboard shows about the peers the second node starts
// from.
type AnchorList struct {
	// Version is the list's own counter, which is what a replacement has to beat.
	Version int64 `json:"version"`
	// Source is "built-in" or "imported".
	Source string `json:"source"`
	// Peers are the addresses, so a user can see exactly what they are running
	// rather than a count they have to trust.
	Peers []string `json:"peers"`
	// Signer is the short form of the key this build trusts, empty when the
	// build has none. Shown so a user can compare it against the key they expect
	// — a signature check is only worth as much as knowing whose signature.
	Signer string `json:"signer"`
	// CanImport says whether this build can accept a list at all.
	CanImport bool `json:"can_import"`
	// Fallback names what was wrong with an imported list, when one is present
	// and not in use. Empty in the ordinary case.
	Fallback string `json:"fallback,omitempty"`
}

func (s *Server) handleAnchors(w http.ResponseWriter, r *http.Request) {
	writeData(w, anchorListOf(s.anchors.Load()))
}

func anchorListOf(active anchors.Active) AnchorList {
	// Never nil: an empty list is the shipped state, and `[]` says that where
	// `null` would read as a failure to look.
	peers := active.Peers
	if peers == nil {
		peers = []string{}
	}
	return AnchorList{
		Version:   active.Version,
		Source:    string(active.Source),
		Peers:     peers,
		Signer:    anchors.Fingerprint(),
		CanImport: anchors.HaveSigningKey(),
		Fallback:  active.Fallback,
	}
}

// importRequest carries a list and its detached signature.
//
// Both as text in one request, because they are useless apart and asking a user
// to upload two files in the right order is asking them to get it wrong.
type importRequest struct {
	List      string `json:"list"`
	Signature string `json:"signature"`
}

// maxImportBytes bounds what may be posted here.
//
// A list is a few kilobytes at most — MaxPeers addresses and a couple of
// directives. This is not a file upload endpoint and should not behave like one.
const maxImportBytes = 64 << 10

func (s *Server) handleImportAnchors(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"That did not arrive as a list and a signature.")
		return
	}
	if req.List == "" || req.Signature == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"An anchor list needs both the list and its signature.")
		return
	}

	accepted, err := s.anchors.Import([]byte(req.List), []byte(req.Signature))
	if err != nil {
		// **A refusal is a 200 with a reason, not a 4xx.** The request was
		// perfectly well formed; what happened is that the list did not check
		// out, and that is an answer rather than a mistake by the caller. It is
		// also the answer most worth reading carefully, so it is returned as
		// something the page can show rather than as an error code.
		writeData(w, importResult{
			Accepted: false,
			Reason:   importReason(err),
			Active:   anchorListOf(s.anchors.Load()),
		})
		return
	}

	writeData(w, importResult{
		Accepted: true,
		Active:   anchorListOf(anchors.Active{List: accepted}),
	})
}

// importResult says what happened, and what is in use either way.
//
// The active list is returned on both paths so the page never has to guess what
// it is now running — after a refusal, what is in use is what was in use before,
// and showing it is how a user knows nothing changed.
type importResult struct {
	Accepted bool       `json:"accepted"`
	Reason   string     `json:"reason,omitempty"`
	Active   AnchorList `json:"active"`
}

// importReason turns a refusal into something worth reading.
//
// Each of these is a different problem with a different remedy, and collapsing
// them into "invalid" would leave a user with no idea which.
func importReason(err error) string {
	switch {
	case errors.Is(err, anchors.ErrNoSigningKey):
		return "This build of Forktower has no key to check an anchor list against, " +
			"so it cannot accept one."
	case errors.Is(err, anchors.ErrUnreadableSignature):
		return "That signature is not readable. It should be the contents of the " +
			"list's .sig file."
	case errors.Is(err, anchors.ErrBadSignature):
		return "That signature does not match that list. Either the list has been " +
			"altered since it was signed, or the two files do not belong together."
	case errors.Is(err, anchors.ErrNotNewer):
		return "That list is not newer than the one already in use, so it has been " +
			"refused. An older list is refused even when it is properly signed: its " +
			"peers may have gone dark since, and a second chain nobody can reach " +
			"looks exactly like a chain where nothing is happening."
	case errors.Is(err, anchors.ErrUnsupportedFormat):
		return "That list was written for a newer version of Forktower than this one."
	case errors.Is(err, anchors.ErrTooManyPeers):
		return "That list names more peers than Forktower will use."
	case errors.Is(err, anchors.ErrNoVersion), errors.Is(err, anchors.ErrNotAList):
		return "That file is not an anchor-peer list."
	default:
		return "That list could not be used: " + err.Error()
	}
}
