package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixture builds a snapshot of a few small parts, and the bytes to serve for
// each. Small enough to run in milliseconds, and structured exactly like the
// real one: several parts, the last one short.
type fixture struct {
	snapshot Snapshot
	bodies   map[string][]byte
}

func newFixture(t *testing.T, sizes ...int) *fixture {
	t.Helper()

	f := &fixture{bodies: map[string][]byte{}}
	whole := sha256.New()

	for i, size := range sizes {
		name := fmt.Sprintf("part.%02d", i)
		body := make([]byte, size)
		for j := range body {
			// Content that differs per part, so a part written in the wrong place
			// is visible rather than a run of matching zeroes.
			body[j] = byte(i*31 + j)
		}
		sum := sha256.Sum256(body)
		whole.Write(body)

		f.bodies[name] = body
		f.snapshot.Parts = append(f.snapshot.Parts, Part{
			Name:   name,
			Bytes:  int64(size),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}

	f.snapshot.Network = "main"
	f.snapshot.SHA256 = hex.EncodeToString(whole.Sum(nil))
	return f
}

// serverOpts bends the test server into the shapes a real one takes.
type serverOpts struct {
	// ignoreRange answers every request with the whole file.
	ignoreRange bool
	// truncateAt cuts each response short after this many bytes.
	truncateAt int
	// corrupt flips a byte in the named part.
	corrupt string
	// failFirst refuses the first n requests outright.
	failFirst int
	// wrongLength reports a different total in Content-Range.
	wrongLength bool
}

func (f *fixture) serve(t *testing.T, opts serverOpts) {
	t.Helper()

	var mu sync.Mutex
	seen := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		refuse := seen <= opts.failFirst
		mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := f.bodies[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == opts.corrupt {
			body = append([]byte(nil), body...)
			body[len(body)/2] ^= 0xff
		}

		from := int64(0)
		if raw := r.Header.Get("Range"); raw != "" && !opts.ignoreRange {
			// Only the absolute form is ever sent, so only it is understood.
			// A suffix range reaching here is a bug worth failing on.
			if !strings.HasPrefix(raw, "bytes=") || strings.HasPrefix(raw, "bytes=-") {
				t.Errorf("the fetcher sent an unusable Range header: %q", raw)
				w.WriteHeader(http.StatusNotImplemented)
				return
			}
			start, err := strconv.ParseInt(strings.TrimSuffix(
				strings.TrimPrefix(raw, "bytes="), "-"), 10, 64)
			if err != nil {
				t.Errorf("unparseable Range header %q", raw)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			from = start
			total := len(body)
			if opts.wrongLength {
				total += 12345
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", from, len(body)-1, total))
			w.WriteHeader(http.StatusPartialContent)
		}

		out := body[from:]
		if opts.truncateAt > 0 && len(out) > opts.truncateAt {
			out = out[:opts.truncateAt]
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	f.snapshot.BaseURL = srv.URL + "/"
}

func (f *fixture) fetcher(t *testing.T) *Fetcher {
	t.Helper()
	return &Fetcher{
		Snapshot: f.snapshot,
		Path:     filepath.Join(t.TempDir(), StagedFileName),
		Client:   srvClient(),
		// Retries are exercised deliberately here, so they must not cost a real
		// five seconds each.
		RetryDelay: time.Millisecond,
	}
}

// srvClient is a plain client. NewHTTPClient's proxy handling is tested
// separately; here the transport must not be the thing under test.
func srvClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func TestFetchAssemblesThePartsInOrder(t *testing.T) {
	f := newFixture(t, 1000, 1000, 317)
	f.serve(t, serverOpts{})
	fetch := f.fetcher(t)

	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, p := range f.snapshot.Parts {
		want = append(want, f.bodies[p.Name]...)
	}
	if string(got) != string(want) {
		t.Error("the assembled file is not the concatenation of the parts in order")
	}
}

// The resume path, which is the one that matters: a two-gigabyte part over Tor
// will be interrupted, and starting over each time would never finish.
func TestFetchResumesFromWhatIsAlreadyOnDisk(t *testing.T) {
	f := newFixture(t, 1000, 1000, 317)
	f.serve(t, serverOpts{})
	fetch := f.fetcher(t)

	// Simulate a run that died partway through the second part.
	var partial []byte
	partial = append(partial, f.bodies["part.00"]...)
	partial = append(partial, f.bodies["part.01"][:400]...)
	if err := os.WriteFile(fetch.Path, partial, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch after an interruption: %v", err)
	}

	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != f.snapshot.TotalBytes() {
		t.Fatalf("the resumed file is %d bytes, want %d", len(got), f.snapshot.TotalBytes())
	}
	// The whole-file digest is checked inside Fetch, so reaching here with the
	// right length means the resumed bytes landed in the right place. Checked
	// again explicitly, because "Fetch returned nil" is exactly the claim under
	// test.
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != f.snapshot.SHA256 {
		t.Error("the resumed file does not match the snapshot's digest")
	}
}

// A server that answers a resumed request with the whole file must be caught.
//
// **This is the one that silently corrupts.** The bytes would be appended after
// the ones already held, producing a file of plausible length made of duplicated
// content — and nothing but the final digest, hours later, would notice.
func TestAServerThatIgnoresRangeIsCaughtRatherThanAppendedTo(t *testing.T) {
	f := newFixture(t, 1000, 400)
	f.serve(t, serverOpts{ignoreRange: true})
	fetch := f.fetcher(t)

	// Half of the first part already fetched, so the next request resumes.
	if err := os.WriteFile(fetch.Path, f.bodies["part.00"][:500], 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// It recovers by restarting the part, so the result must still be correct.
	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != f.snapshot.SHA256 {
		t.Error("a server that ignored the range left a corrupted file behind")
	}
}

func TestStatusOKForRejectsAWholeFileAnsweringAResumedRequest(t *testing.T) {
	if err := statusOKFor(http.StatusOK, 0); err != nil {
		t.Errorf("a 200 to a fresh request was refused: %v", err)
	}
	if err := statusOKFor(http.StatusPartialContent, 500); err != nil {
		t.Errorf("a 206 to a resumed request was refused: %v", err)
	}
	if err := statusOKFor(http.StatusOK, 500); !errors.Is(err, ErrRangeIgnored) {
		t.Errorf("a 200 to a resumed request gave %v, want ErrRangeIgnored", err)
	}
	if err := statusOKFor(http.StatusRequestedRangeNotSatisfiable, 500); err == nil {
		t.Error("a 416 was accepted")
	}
	if err := statusOKFor(http.StatusNotFound, 0); err == nil {
		t.Error("a 404 was accepted")
	}
}

// A part that arrives wrong is discarded and fetched again, rather than poisoning
// everything after it.
func TestACorruptedPartIsFetchedAgain(t *testing.T) {
	f := newFixture(t, 600, 400)

	// Corrupt only the first response for part.01, then serve it correctly.
	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body := append([]byte(nil), f.bodies[name]...)
		if name == "part.01" {
			mu.Lock()
			served++
			first := served == 1
			mu.Unlock()
			if first {
				body[10] ^= 0xff
			}
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	f.snapshot.BaseURL = srv.URL + "/"

	fetch := f.fetcher(t)
	fetch.Now = fastClock()
	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch did not recover from a corrupted part: %v", err)
	}
	if served < 2 {
		t.Errorf("the corrupted part was requested %d times; it should have been "+
			"fetched again", served)
	}
}

// A body that ends early is resumed, not treated as a completed part.
//
// A server closing the connection halfway looks identical to a finished transfer
// from the reader's side. Without the length check the digest would be computed
// over half a part, fail, and throw away bytes that were perfectly good.
func TestABodyThatEndsEarlyIsResumedRatherThanDiscarded(t *testing.T) {
	f := newFixture(t, 1000, 200)

	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()

		name := strings.TrimPrefix(r.URL.Path, "/")
		body := f.bodies[name]
		from := int64(0)
		if raw := r.Header.Get("Range"); raw != "" {
			start, _ := strconv.ParseInt(
				strings.TrimSuffix(strings.TrimPrefix(raw, "bytes="), "-"), 10, 64)
			from = start
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", from, len(body)-1, len(body)))
			w.WriteHeader(http.StatusPartialContent)
		}
		out := body[from:]
		// Cut the very first response short.
		if n == 1 && len(out) > 300 {
			out = out[:300]
		}
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	f.snapshot.BaseURL = srv.URL + "/"

	fetch := f.fetcher(t)
	fetch.Now = fastClock()
	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch did not recover from a short body: %v", err)
	}

	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != f.snapshot.SHA256 {
		t.Error("the file assembled after a short body is wrong")
	}
}

// A transient failure is retried; the transfer does not give up on the first
// refusal.
func TestATransientFailureIsRetried(t *testing.T) {
	f := newFixture(t, 500)
	f.serve(t, serverOpts{failFirst: 2})

	fetch := f.fetcher(t)
	fetch.Now = fastClock()
	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch gave up on a server that refused twice: %v", err)
	}
}

// A server reporting a different total size is serving a different file.
func TestAPartOfTheWrongPublishedLengthIsRefused(t *testing.T) {
	f := newFixture(t, 1000, 400)
	f.serve(t, serverOpts{wrongLength: true})
	fetch := f.fetcher(t)
	fetch.Now = fastClock()

	// Resume, so that a Content-Range header is sent at all.
	if err := os.WriteFile(fetch.Path, f.bodies["part.00"][:100], 0o600); err != nil {
		t.Fatal(err)
	}

	err := fetch.Fetch(context.Background())
	if err == nil {
		t.Fatal("a part whose published length had changed was accepted")
	}
	if !errors.Is(err, ErrPublishedFileChanged) {
		t.Errorf("the error was %v, want ErrPublishedFileChanged", err)
	}
}

// A staged file longer than the snapshot is discarded rather than trusted.
func TestAnOverlongStagedFileIsDiscarded(t *testing.T) {
	f := newFixture(t, 300, 200)
	f.serve(t, serverOpts{})
	fetch := f.fetcher(t)

	junk := make([]byte, f.snapshot.TotalBytes()+50)
	if err := os.WriteFile(fetch.Path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != f.snapshot.SHA256 {
		t.Error("an over-long staged file was not properly replaced")
	}
}

// Parts that are individually intact but assembled in the wrong order is the one
// failure per-part digests structurally cannot see. The whole-file check is what
// covers it, and it reads what is on the disk rather than what was written.
func TestTheWholeFileDigestCatchesPartsInTheWrongOrder(t *testing.T) {
	f := newFixture(t, 400, 400)
	fetch := f.fetcher(t)

	var reversed []byte
	reversed = append(reversed, f.bodies["part.01"]...)
	reversed = append(reversed, f.bodies["part.00"]...)
	if err := os.WriteFile(fetch.Path, reversed, 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(fetch.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	if err := fetch.verifyWhole(context.Background(), file); err == nil {
		t.Error("a file made of the right parts in the wrong order passed verification")
	}
}

func TestProgressIsReported(t *testing.T) {
	f := newFixture(t, 4<<20, 1<<20)
	f.serve(t, serverOpts{})

	var mu sync.Mutex
	var last Progress
	calls := 0

	fetch := f.fetcher(t)
	fetch.OnProgress = func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if p.BytesDone < last.BytesDone {
			t.Errorf("progress went backwards: %d after %d", p.BytesDone, last.BytesDone)
		}
		last = p
	}

	if err := fetch.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("no progress was reported for a five-megabyte transfer")
	}
	if last.BytesDone != f.snapshot.TotalBytes() {
		t.Errorf("the last progress reported %d of %d bytes",
			last.BytesDone, f.snapshot.TotalBytes())
	}
	if last.Parts != len(f.snapshot.Parts) {
		t.Errorf("progress reported %d parts, want %d", last.Parts, len(f.snapshot.Parts))
	}
}

func TestCancellationStopsTheTransfer(t *testing.T) {
	f := newFixture(t, 1000)
	f.serve(t, serverOpts{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.fetcher(t).Fetch(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fetch on a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestFetchRefusesWithoutAClient(t *testing.T) {
	f := newFixture(t, 10)
	fetch := &Fetcher{Snapshot: f.snapshot, Path: filepath.Join(t.TempDir(), "x")}
	if err := fetch.Fetch(context.Background()); err == nil {
		t.Error("Fetch ran without an http client, so the proxy decision was skipped")
	}
}

func TestParseContentRangeReadsTheTotal(t *testing.T) {
	for header, want := range map[string]int64{
		"bytes 0-99/100":        100,
		"bytes 500-1999/2000":   2000,
		"bytes 0-0/1":           1,
		"bytes */100":           100,
		"":                      0,
		"items 0-99/100":        0,
		"bytes 0-99/*":          0,
		"bytes 0-99/notanumber": 0,
	} {
		if got := parseContentRange(header); got != want {
			t.Errorf("parseContentRange(%q) = %d, want %d", header, got, want)
		}
	}
}

// fastClock makes the retry delays instant, so a test that exercises them does
// not spend seconds sleeping. A test that sleeps to pass is slow now and flaky
// later.
func fastClock() func() time.Time {
	var mu sync.Mutex
	base := time.Unix(1_700_000_000, 0)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		base = base.Add(time.Millisecond)
		return base
	}
}

// A server that refuses every range must not spin for ever.
//
// Restarting a part sets the offset to zero, and from zero no range is requested
// — so this error normally cannot recur. "Normally" is not "never": a server
// answering 416 whatever it is asked lands on the restart path every single
// time, and an uncounted restart is an infinite loop running at full speed and
// saying nothing.
func TestAServerRefusingEveryRangeGivesUpRatherThanSpinning(t *testing.T) {
	f := newFixture(t, 500)

	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	t.Cleanup(srv.Close)
	f.snapshot.BaseURL = srv.URL + "/"

	fetch := f.fetcher(t)
	fetch.Now = fastClock()

	done := make(chan error, 1)
	go func() { done <- fetch.Fetch(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that refused everything reported a complete download")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Fetch never gave up on a server that refuses every request")
	}

	mu.Lock()
	got := requests
	mu.Unlock()
	if got > MaxAttempts+1 {
		t.Errorf("the server was asked %d times for a %d-attempt limit", got, MaxAttempts)
	}
}
