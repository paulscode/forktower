// Checks against the release that actually exists.
//
// **None of these run under `make check`.** The gate must not depend on a
// network, or a GitHub outage becomes a failing build and the failing build
// stops meaning anything. They are run by hand before publishing a snapshot,
// which is the moment the compiled-in manifest and the published files can
// first disagree.
//
//	FORKTOWER_LIVE=1      cheap: the manifest, and one small real transfer
//	FORKTOWER_LIVE_FULL=1 expensive: a whole 1.9 GB part, end to end
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The compiled-in manifest must describe the release that actually exists.
//
// Every value in it was transcribed by hand. A digit dropped from a part length
// is not a compile error and not caught by any offline test — it is a download
// that runs for hours and fails at the end, on somebody else's machine. This
// asks the release itself.
//
// Skipped without -tags live, because `make check` must not depend on a network.
func TestLiveTheManifestMatchesThePublishedRelease(t *testing.T) {
	if os.Getenv("FORKTOWER_LIVE") == "" {
		t.Skip("set FORKTOWER_LIVE=1 to check against the real release")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/paulscode/forktower/releases/tags/utxo-snapshot-935000",
		http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var rel struct {
		Draft      bool `json:"draft"`
		Prerelease bool `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatal(err)
	}
	if rel.Draft {
		t.Fatal("the release is a draft, so its assets are not publicly downloadable")
	}

	published := map[string]int64{}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		published[a.Name] = a.Size
		urls[a.Name] = a.URL
	}

	s := MainnetHeight935000
	for _, p := range s.Parts {
		size, ok := published[p.Name]
		if !ok {
			t.Errorf("%s is compiled in but not published", p.Name)
			continue
		}
		if size != p.Bytes {
			t.Errorf("%s: compiled in as %d bytes, published as %d", p.Name, p.Bytes, size)
		}
		if got := s.URLFor(p); got != urls[p.Name] {
			t.Errorf("%s: built URL %q, real URL %q", p.Name, got, urls[p.Name])
		}
	}
}

// The fetcher against the real endpoint, including its redirect and its range
// handling.
//
// Uses the release's own SHA256SUMS as a one-part snapshot: a few hundred bytes
// through exactly the path the nine-gigabyte transfer takes — GitHub's redirect
// to its storage backend, and that backend's range support.
func TestLiveTheFetcherWorksAgainstTheRealEndpoint(t *testing.T) {
	if os.Getenv("FORKTOWER_LIVE") == "" {
		t.Skip("set FORKTOWER_LIVE=1 to check against the real release")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	base := MainnetHeight935000.BaseURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"SHA256SUMS", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	body = body[:n]

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	tiny := Snapshot{
		Network: ChainMain,
		BaseURL: base,
		SHA256:  digest,
		Parts: []Part{
			{Name: "SHA256SUMS", Bytes: int64(len(body)), SHA256: digest},
		},
	}

	dir := t.TempDir()
	fetch := &Fetcher{
		Snapshot: tiny,
		Path:     filepath.Join(dir, "live.dat"),
		Client:   NewHTTPClient(""),
	}
	if err := fetch.Fetch(ctx); err != nil {
		t.Fatalf("fetching from the real endpoint: %v", err)
	}

	// And again from halfway, which is the resume path — the one that matters on
	// a link that drops.
	if err := os.WriteFile(fetch.Path, body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fetch.Fetch(ctx); err != nil {
		t.Fatalf("resuming from the real endpoint: %v", err)
	}
	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Error("the resumed file does not match what the endpoint serves")
	}
}

// One real part, at full size, from the real release.
//
// The most expensive of these and the only one that proves the thing at scale:
// nearly two gigabytes through GitHub's redirect and storage backend, checked
// against the digest compiled into this package. Measured at 51 seconds over a
// fast link. Separate from the cheap checks because it moves real bandwidth.
func TestLiveRealPartEndToEnd(t *testing.T) {
	if os.Getenv("FORKTOWER_LIVE_FULL") == "" {
		t.Skip("set FORKTOWER_LIVE_FULL=1 to fetch a real 1.9 GB part")
	}
	official := MainnetHeight935000
	one := Snapshot{
		Network: ChainMain,
		BaseURL: official.BaseURL,
		Parts:   []Part{official.Parts[0]},
		SHA256:  official.Parts[0].SHA256,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	dir := os.Getenv("FORKTOWER_REALPART_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	start := time.Now()
	var last Progress
	fetch := &Fetcher{
		Snapshot:   one,
		Path:       filepath.Join(dir, "realpart.dat"),
		Client:     NewHTTPClient(""),
		OnProgress: func(p Progress) { last = p },
	}
	if err := fetch.Fetch(ctx); err != nil {
		t.Fatalf("fetching a real part: %v", err)
	}
	t.Logf("fetched %s in %s (%d bytes verified against the compiled-in digest)",
		HumanBytes(last.BytesTotal), time.Since(start).Round(time.Second), last.BytesTotal)
	_ = os.Remove(fetch.Path)
}
