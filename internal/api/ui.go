package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/paulscode/forktower/web"
)

// uiFiles maps a request path to the asset that answers it.
//
// An explicit map rather than a file server: a file server can be talked into
// listing a directory or serving something that was not meant to be public, and
// this is three files. Nothing here can serve anything not named below.
const (
	contentTypeHTML = "text/html; charset=utf-8"
	contentTypeJS   = "text/javascript; charset=utf-8"
	contentTypeCSS  = "text/css; charset=utf-8"
)

var uiFiles = map[string]struct {
	name        string
	contentType string
}{
	"/":           {"index.html", contentTypeHTML},
	"/index.html": {"index.html", contentTypeHTML},
	"/app.js":     {"app.js", contentTypeJS},
	"/style.css":  {"style.css", contentTypeCSS},
}

// MountUI adds the dashboard to the server's routes.
//
// Separate from New so a caller that only wants the API — the test harness, a
// user's own tooling — gets exactly that.
func (s *Server) MountUI() {
	s.mux.HandleFunc("GET /", s.handleUI)
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	// Anything under /api/ that reached here is a path no handler claimed. Saying
	// so in JSON matters: an API caller that received an HTML page would report a
	// parse failure rather than the 404 it actually got.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, CodeNotFound, "There is nothing at that address.")
		return
	}

	asset, ok := uiFiles[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := fs.ReadFile(web.Files, asset.name)
	if err != nil {
		// The files are compiled into the binary, so this cannot happen without
		// the binary itself being wrong.
		s.fail(w, r, "reading a dashboard file", err)
		return
	}

	w.Header().Set("Content-Type", asset.contentType)
	// Revalidate every time. The dashboard is small, and a stale page after an
	// upgrade would show a user an answer the daemon no longer stands behind.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
