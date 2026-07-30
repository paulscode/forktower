package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func uiHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.srv.MountUI()
	return h
}

func TestTheDashboardIsServed(t *testing.T) {
	t.Parallel()
	h := uiHarness(t)

	cases := map[string]string{
		"/":           contentTypeHTML,
		"/index.html": contentTypeHTML,
		"/app.js":     contentTypeJS,
		"/style.css":  contentTypeCSS,
	}

	for path, contentType := range cases {
		resp := h.do(t, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, resp.StatusCode)
			continue
		}
		if got := resp.Header.Get("Content-Type"); got != contentType {
			t.Errorf("GET %s served %q, want %q", path, got, contentType)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Errorf("GET %s served nothing", path)
		}
	}
}

// A file server can be talked into listing a directory or serving something that
// was never meant to be public. This one serves three named files and nothing
// else, so there is nothing to talk it into.
func TestTheDashboardServesNothingElse(t *testing.T) {
	t.Parallel()
	h := uiHarness(t)

	paths := []string{
		"/web", "/web/", "/embed.go", "/app_test.js", "/domshim.js", "/web_test.go",
		"/../internal/config/config.go", "/index.html/", "/APP.JS", "/app.js/../embed.go",
	}
	for _, path := range paths {
		resp := h.doWith(t, http.MethodGet, path, "", nil)
		if resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			t.Errorf("GET %s was served: %q", path, body)
		}
	}

	// `/.` and `/..` never reach the server: the client resolves them to the root
	// first. Asserted rather than assumed, because "it was refused" and "it was
	// never asked" are different facts and only one of them is about this code.
	for _, path := range []string{"/.", "/.."} {
		resp := h.doWith(t, http.MethodGet, path, "", nil)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		if !strings.HasPrefix(string(body), "<!DOCTYPE html>") {
			t.Errorf("GET %s served something other than the dashboard: %q", path, body)
		}
	}
}

// The dashboard and the API share a listener. An API caller that received an
// HTML page for an unknown path would report a parse failure rather than the 404
// it actually got.
func TestUnknownApiPathsStayJson(t *testing.T) {
	t.Parallel()
	h := uiHarness(t)

	resp := h.do(t, http.MethodGet, "/api/v1/nothing-here", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != CodeNotFound {
		t.Errorf("got %q, want %q", got, CodeNotFound)
	}
}

// The policy the page is served with is what stops an injected string becoming
// code, so the page has to be able to run under it: its own script and stylesheet
// are same-origin files, and there is nothing inline.
func TestTheDashboardRunsUnderItsOwnPolicy(t *testing.T) {
	t.Parallel()
	h := uiHarness(t)

	resp := h.do(t, http.MethodGet, "/", "")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("the policy permits script from elsewhere: %s", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the policy has been loosened: %s", csp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	if !strings.Contains(page, `src="/app.js"`) || !strings.Contains(page, `href="/style.css"`) {
		t.Error("the page does not load its own script and stylesheet")
	}
}

// Serving the API without the dashboard is a supported arrangement: a user's own
// tooling, and the test harness, want exactly that.
func TestTheApiWorksWithoutTheDashboard(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil) // no MountUI

	if resp := h.do(t, http.MethodGet, "/api/v1/status", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/", ""); resp.StatusCode == http.StatusOK {
		t.Error("the dashboard was served without being mounted")
	}
}

// The dashboard must not be readable by a stranger who has not signed in, or the
// password mode protects the data and not the page describing it.
func TestTheDashboardStillNeedsNoSessionButItsDataDoes(t *testing.T) {
	t.Parallel()
	h := newHarnessWithPassword(t, testPassword, nil)
	h.srv.MountUI()

	// The page itself is a shell with no data in it, so serving it unauthenticated
	// is what lets someone reach the sign-in form at all.
	if resp := h.doWith(t, http.MethodGet, "/", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the sign-in page returned %d", resp.StatusCode)
	}
	// Everything it would display is behind the session.
	if resp := h.doWith(t, http.MethodGet, "/api/v1/status", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want the data to require signing in", resp.StatusCode)
	}
}
