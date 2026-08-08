package web_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/web"
)

// Constructs that turn a value chosen by someone else into markup or code.
//
// This audience's threat model includes the channel counterparty, and a later
// version of the API carries a name that counterparty chose. There is no
// framework here doing escaping, so the rule is that markup is never built from
// data — enforced by this scan and, at runtime, by the DOM shim the rendering
// tests use.
var forbidden = []struct {
	pattern string
	why     string
}{
	{"innerHTML", "builds markup from a value; use textContent"},
	{"outerHTML", "builds markup from a value; use textContent"},
	{"insertAdjacentHTML", "builds markup from a value; use textContent"},
	{"document.write", "builds markup from a value; use textContent"},
	{"eval(", "runs a value as code"},
	{"new Function(", "runs a value as code"},
	{"setTimeout(\"", "runs a string as code"},
	{"setInterval(\"", "runs a string as code"},
	{"javascript:", "runs a value as code"},
	{"srcdoc", "builds a document from a value"},
}

// Sources of code this page must never load: the whole point of no build step
// and no framework is that what is in the repository is what runs.
var forbiddenSources = []string{
	"http://", "https://", "//cdn.", "integrity=", "crossorigin",
}

func TestNoAssetBuildsMarkupFromData(t *testing.T) {
	t.Parallel()

	for _, name := range assetNames(t) {
		body := readAsset(t, name)
		for _, bad := range forbidden {
			if strings.Contains(body, bad.pattern) {
				t.Errorf("%s contains %q, which %s", name, bad.pattern, bad.why)
			}
		}
	}
}

// An inline handler would need a policy that permits inline script, and that
// policy is the one thing standing between an injected string and code
// execution.
func TestNoInlineHandlersOrStyles(t *testing.T) {
	t.Parallel()

	html := readAsset(t, "index.html")
	for _, attr := range []string{
		"onclick=", "onerror=", "onload=", "onsubmit=", "onmouseover=", "style=",
	} {
		if strings.Contains(html, attr) {
			t.Errorf("index.html contains %q; the policy this page is served with forbids it", attr)
		}
	}
	// A script element with a body rather than a src is inline script by another
	// name.
	if strings.Contains(html, "<script>") {
		t.Error("index.html contains an inline script")
	}
}

func TestNothingIsLoadedFromElsewhere(t *testing.T) {
	t.Parallel()

	for _, name := range assetNames(t) {
		body := readAsset(t, name)
		for _, source := range forbiddenSources {
			if strings.Contains(body, source) {
				t.Errorf("%s references %q; this page loads nothing it does not ship", name, source)
			}
		}
	}
}

// What is embedded is what is served. A file added to the directory but not the
// embed list would be missing from the binary with no sign at build time.
func TestEverythingTheDashboardNeedsIsEmbedded(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"index.html", "app.js", "style.css", "favicon.png", "logo.png",
	} {
		if _, err := fs.ReadFile(web.Files, name); err != nil {
			t.Errorf("%s is not compiled into the binary: %v", name, err)
		}
	}

	// And nothing else: the test files next door must not ship.
	entries, err := fs.ReadDir(web.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "index.html", "app.js", "style.css", "favicon.png", "logo.png":
		default:
			t.Errorf("%s is compiled into the binary but is not a dashboard asset", entry.Name())
		}
	}

	// The tests, the shim and the response fixture live beside the assets and
	// must not travel with them.
	for _, name := range []string{
		"app_test.js", "domshim.js",
		"testdata/status.json", "testdata/status-suspected.json",
	} {
		if _, err := fs.ReadFile(web.Files, name); err == nil {
			t.Errorf("%s ships inside the binary", name)
		}
	}
}

// The page must reach only for elements that exist. A mistyped id fails silently
// in a browser and loudly in front of a user.
func TestEveryElementTheScriptReachesForExists(t *testing.T) {
	t.Parallel()

	html := readAsset(t, "index.html")
	script := readAsset(t, "app.js")

	for _, id := range idsUsedBy(script) {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("app.js reaches for %q, which index.html does not define", id)
		}
	}
}

// The rendering tests run against a DOM that implements textContent and refuses
// innerHTML, so the escaping rule is enforced at runtime rather than only by the
// scan above.
func TestRendering(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed, so the rendering tests did not run — " +
			"the static scan above still did")
	}

	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, filepath.Join(dir, "app_test.js"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the rendering tests failed:\n%s", out)
	}
	if testing.Verbose() {
		t.Log("\n" + string(out))
	}
}

// assetNames lists the assets a scan can read. The images are binary and
// generated, so scanning them for text would produce noise, not findings.
func assetNames(t *testing.T) []string {
	t.Helper()
	return []string{"index.html", "app.js", "style.css"}
}

func readAsset(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// idsUsedBy pulls every getElementById argument out of the script.
func idsUsedBy(script string) []string {
	var ids []string
	// The page wraps getElementById in a helper, so both shapes are collected.
	for _, marker := range []string{"el('", "getElementById('"} {
		rest := script
		for {
			at := strings.Index(rest, marker)
			if at < 0 {
				break
			}
			rest = rest[at+len(marker):]
			end := strings.Index(rest, "'")
			if end < 0 {
				break
			}
			if id := rest[:end]; id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// The page has to reference the images it ships, or they are dead weight in the
// binary. Checked from the page rather than from the directory: a file that is
// embedded but never asked for is exactly as useless as one that is missing.
func TestThePageUsesTheImagesItShips(t *testing.T) {
	t.Parallel()

	html := readAsset(t, "index.html")
	for _, ref := range []string{`href="/favicon.png"`, `src="/logo.png"`} {
		if !strings.Contains(html, ref) {
			t.Errorf("index.html does not use %s", ref)
		}
	}
	// The logo is decoration beside a heading that already says the name, so it
	// is hidden from a screen reader rather than read out twice.
	if !strings.Contains(html, `src="/logo.png" alt=""`) {
		t.Error("the logo has no empty alt attribute, so it is announced twice")
	}
}
