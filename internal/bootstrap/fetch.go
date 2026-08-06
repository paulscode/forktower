package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// Transfer bounds. Chosen for a two-gigabyte part arriving over Tor on an
// appliance, which is the hard case and the common one.
const (
	// StallTimeout is how long a transfer may deliver nothing before it is
	// abandoned and retried. A Tor circuit that has died often leaves a socket
	// that never returns and never errors, so without this a download stops
	// making progress and nothing ever says so.
	StallTimeout = 3 * time.Minute

	// copyChunk is how much is moved between progress checks. Large enough that
	// the bookkeeping is free, small enough that the stall watchdog and
	// cancellation are responsive.
	copyChunk = 1 << 20

	// MaxAttempts bounds retries of a single part before the whole run gives up.
	// High, because over Tor an interrupted transfer is ordinary rather than
	// exceptional, and each attempt resumes where the last one stopped instead of
	// starting again.
	MaxAttempts = 30

	// retryDelay is the pause between attempts. Fixed rather than exponential:
	// the failures this recovers from are circuit-level and transient, and
	// backing off to minutes would turn a blip into a stalled evening.
	retryDelay = 5 * time.Second
)

// Progress is what the dashboard shows while a transfer runs.
type Progress struct {
	// BytesDone and BytesTotal describe the assembled file.
	BytesDone  int64
	BytesTotal int64
	// Part and Parts are one-based, for a sentence like "part 3 of 5".
	Part  int
	Parts int
	// Elapsed is how long this run has been going.
	Elapsed time.Duration
	// Remaining is an estimate, or zero when there is not enough to judge by.
	Remaining time.Duration
}

// Percent is how far along the transfer is.
func (p Progress) Percent() float64 {
	if p.BytesTotal <= 0 {
		return 0
	}
	return float64(p.BytesDone) / float64(p.BytesTotal) * 100
}

// Fetcher downloads a snapshot's parts into a single assembled file.
//
// Restartable at any point: the only state that survives a crash is the length
// of the file on disk, and everything else — which part is next, how far into it,
// what still has to be verified — is derived from that. There is no progress
// file to fall out of step with the data it describes.
type Fetcher struct {
	// Snapshot is what to fetch.
	Snapshot Snapshot
	// Path is the assembled file.
	Path string
	// Client makes the requests. Required: the caller decides whether it goes
	// through a proxy, and this package will not quietly fall back to a direct
	// connection if that decision is missing.
	Client *http.Client
	// Logger receives transfer diagnostics. Nil discards them.
	Logger *slog.Logger
	// OnProgress is called every chunk or so. Nil is fine. Must not block.
	OnProgress func(Progress)
	// Now reads the clock. Nil uses time.Now.
	Now func() time.Time
	// RetryDelay is the pause between attempts. Zero uses retryDelay.
	//
	// Injectable so that a test of the retry paths does not spend a real five
	// seconds per attempt. A test that sleeps to pass is slow now and flaky
	// later, and the retry behaviour is too important to leave untested because
	// exercising it is tedious.
	RetryDelay time.Duration
}

// ErrPublishedFileChanged means the server is offering a file of a different
// length than the one this release was built against.
//
// **Not retryable, and that is the point.** Everything else here recovers by
// trying again, because the failures are transient. A published asset that has
// been replaced will not change back, so retrying thirty times would spend
// minutes proving something already known — and would end with a file whose
// compiled-in checksum could never match anyway.
var ErrPublishedFileChanged = errors.New("bootstrap: the published file has changed")

// ErrRangeIgnored means the server sent the whole file when asked for the rest of
// it.
//
// Its own error because the consequence of not noticing is silent corruption: the
// bytes would be appended after the ones already held, producing a file of the
// right length made of the wrong content, which only the final whole-file digest
// would catch — after the entire transfer had finished.
var ErrRangeIgnored = errors.New("bootstrap: the server ignored a range request")

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fetcher) retryDelay() time.Duration {
	if f.RetryDelay > 0 {
		return f.RetryDelay
	}
	return retryDelay
}

func (f *Fetcher) log() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.New(discardHandler{})
}

// Fetch downloads whatever of the snapshot is not already on disk and verifies
// the result.
//
// Returns nil only when the file at Path is complete and its digest matches the
// compiled-in one.
func (f *Fetcher) Fetch(ctx context.Context) error {
	if f.Client == nil {
		return errors.New("bootstrap: no http client was supplied")
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return fmt.Errorf("bootstrap: making room for the download: %w", err)
	}

	file, err := os.OpenFile(f.Path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("bootstrap: opening %s: %w", f.Path, err)
	}
	defer func() { _ = file.Close() }()

	total := f.Snapshot.TotalBytes()
	size, err := fileSize(file)
	if err != nil {
		return err
	}
	if size > total {
		// Longer than the snapshot can be. Something else wrote here, or an older
		// snapshot's file is in the way; either way the content is not what this
		// is about to claim it is.
		f.log().Warn("discarding a staged file that is longer than the snapshot",
			slog.Int64("found", size), slog.Int64("expected", total))
		if err := truncate(file, 0); err != nil {
			return err
		}
		size = 0
	}

	index, within, ok := f.Snapshot.PartAt(size)
	if !ok {
		return fmt.Errorf("bootstrap: %s is %d bytes, which is not a point any part "+
			"boundary explains", f.Path, size)
	}

	digest := sha256.New()
	if within > 0 {
		// Re-read the partial part to put the hasher back where it was. Only the
		// current part, never the whole file: earlier parts were checked when they
		// completed, and re-reading nine gigabytes to resume a transfer would make
		// every restart cost minutes.
		f.log().Info("resuming a download",
			slog.Int64("have", size), slog.Int64("of", total))
		if err := seedDigest(file, f.Snapshot.BytesBefore(index), within, digest); err != nil {
			return err
		}
	}

	started := f.now()
	attempts := 0

	for index < len(f.Snapshot.Parts) {
		part := f.Snapshot.Parts[index]

		n, err := f.stream(ctx, file, streamArgs{
			part:      part,
			within:    within,
			fileSize:  size,
			digest:    digest,
			started:   started,
			partIndex: index,
		})
		within += n
		size += n

		switch {
		case err == nil:
			// Whole part delivered.
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrPublishedFileChanged):
			// Nothing to retry towards. Reported as it is, so the message names
			// the actual problem rather than "gave up after 30 attempts".
			return err
		case errors.Is(err, ErrRangeIgnored):
			// Not retryable by resuming, because resuming is the thing that
			// failed. Start this part again from its beginning.
			//
			// **This counts as an attempt, and it has to.** Restarting sets the
			// offset back to zero, and from zero no range is requested — so the
			// same error normally cannot recur. "Normally" is not "never": a
			// server that answers 416 whatever it is asked lands here every time,
			// and without a counter the loop would restart the same part for
			// ever, at full speed, saying nothing.
			attempts++
			if attempts >= MaxAttempts {
				return fmt.Errorf("bootstrap: %s could not be resumed or restarted "+
					"after %d attempts: %w", part.Name, attempts, err)
			}
			f.log().Warn("restarting a part because the server ignored the resume point",
				slog.String("part", part.Name),
				slog.Int("attempt", attempts))
			if err := f.resetPart(file, index, digest); err != nil {
				return err
			}
			within, size = 0, f.Snapshot.BytesBefore(index)
			if err := sleep(ctx, f.retryDelay()); err != nil {
				return err
			}
			continue
		default:
			attempts++
			if attempts >= MaxAttempts {
				return fmt.Errorf("bootstrap: gave up on %s after %d attempts: %w",
					part.Name, attempts, err)
			}
			f.log().Warn("a transfer stopped and will be resumed",
				slog.String("part", part.Name),
				slog.Int("attempt", attempts),
				slog.String("error", err.Error()))
			if err := sleep(ctx, f.retryDelay()); err != nil {
				return err
			}
			continue
		}

		// **A clean end-of-body short of the part's length is not success.** A
		// server closing the connection early looks exactly like a completed
		// transfer to the reader, so without this the digest would be computed
		// over a partial part, fail, and throw away bytes that were perfectly
		// good. Resumed instead, which is the same path a mid-transfer error
		// takes.
		if within < part.Bytes {
			attempts++
			if attempts >= MaxAttempts {
				return fmt.Errorf("bootstrap: %s kept ending early, at %s of %s",
					part.Name, HumanBytes(within), HumanBytes(part.Bytes))
			}
			f.log().Warn("a transfer ended early and will be resumed",
				slog.String("part", part.Name),
				slog.Int64("have", within),
				slog.Int64("of", part.Bytes))
			if err := sleep(ctx, f.retryDelay()); err != nil {
				return err
			}
			continue
		}
		if got := hex.EncodeToString(digest.Sum(nil)); got != part.SHA256 {
			f.log().Warn("a part did not match its checksum and will be fetched again",
				slog.String("part", part.Name))
			attempts++
			if attempts >= MaxAttempts {
				return fmt.Errorf("bootstrap: %s kept arriving corrupted", part.Name)
			}
			if err := f.resetPart(file, index, digest); err != nil {
				return err
			}
			within, size = 0, f.Snapshot.BytesBefore(index)
			continue
		}

		f.log().Info("a part is complete and verified", slog.String("part", part.Name))
		index++
		within = 0
		attempts = 0
		digest.Reset()
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("bootstrap: flushing %s: %w", f.Path, err)
	}
	return f.verifyWhole(ctx, file)
}

// streamArgs is what one attempt at one part needs to know.
//
// A struct rather than seven parameters, because the two offsets in it are easy
// to transpose at a call site and the compiler would not object: `within` is a
// position inside the current part, `fileSize` a position in the assembled file,
// and both are int64.
type streamArgs struct {
	part Part
	// within is how far into this part the file already extends.
	within int64
	// fileSize is the length of the assembled file, for progress reporting.
	fileSize int64
	// digest carries the hash of this part's bytes so far. Owned by the caller so
	// that it survives a retry without re-reading the disk.
	digest hash.Hash
	// started is when this run began, for the estimate.
	started time.Time
	// partIndex is zero-based, for reporting.
	partIndex int
}

// stream fetches the remainder of one part, appending to the file and hashing as
// it goes.
//
// It returns how many bytes it added, and the caller must account for that
// whether or not there was an error: a transfer that died halfway still moved
// real bytes onto the disk, and forgetting them would corrupt the resume point.
func (f *Fetcher) stream(ctx context.Context, file *os.File, a streamArgs) (int64, error) {
	url := f.Snapshot.URLFor(a.part)

	// The request gets a context of its own so the watchdog below can end it
	// without disturbing the caller's. **A blocked read is the failure being
	// defended against, and only cancelling the request can interrupt one** —
	// checking the clock between reads cannot help when the read never returns,
	// which is precisely what a dead Tor circuit produces.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var lastProgress atomic.Int64
	lastProgress.Store(f.now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go f.watchStall(&lastProgress, cancel, watchdogDone)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("bootstrap: building the request for %s: %w", a.part.Name, err)
	}
	// **Always an absolute range, never a suffix one.** `bytes=-N` — "the last N
	// bytes" — is answered by GitHub's asset storage with 501 and a short error
	// body, which a naive reader would append as if it were data. Measured
	// against the real endpoint; the absolute form works correctly there.
	if a.within > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(a.within, 10)+"-")
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := f.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("bootstrap: fetching %s: %w", a.part.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := statusOKFor(resp.StatusCode, a.within); err != nil {
		return 0, err
	}
	// The published size is known, so a server offering a different one is
	// serving something else. Worth refusing before nine gigabytes have moved.
	if total := parseContentRange(resp.Header.Get("Content-Range")); total > 0 &&
		total != a.part.Bytes {
		return 0, fmt.Errorf("%w: %s is %d bytes at the server, not the %d this "+
			"release expects", ErrPublishedFileChanged, a.part.Name, total, a.part.Bytes)
	}

	want := a.part.Bytes - a.within
	// Bounded by exactly what is outstanding. Without the limit a server sending
	// more than it should would overrun the next part's space in the file.
	body := io.LimitReader(resp.Body, want)

	written, copyErr := f.copyWatched(ctx, file, body, a, &lastProgress)

	// A cancelled request context with a live caller context means the watchdog
	// fired. Said in those words, because "context canceled" at this point would
	// send somebody looking for a shutdown that did not happen.
	if copyErr != nil && reqCtx.Err() != nil && ctx.Err() == nil {
		return written, fmt.Errorf("bootstrap: %s stopped sending for %s and was "+
			"abandoned; it will be resumed", a.part.Name, HumanDuration(StallTimeout))
	}
	return written, copyErr
}

// watchStall ends a transfer that has gone quiet.
//
// Polls rather than arming a timer per chunk: a timer reset a thousand times a
// second is a lot of allocation to detect something measured in minutes.
func (f *Fetcher) watchStall(last *atomic.Int64, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(stallCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if f.now().Sub(time.Unix(0, last.Load())) > StallTimeout {
				cancel()
				return
			}
		}
	}
}

// stallCheckInterval is how often the watchdog looks. Short relative to
// StallTimeout, so the reported delay is roughly the real one.
const stallCheckInterval = 10 * time.Second

// copyWatched moves the body onto the disk, hashing, reporting progress, and
// abandoning a transfer that has stopped delivering.
//
// The stall watchdog is the part worth keeping. A Tor circuit that dies mid
// transfer frequently leaves a socket that neither returns data nor reports an
// error, and the surrounding retry loop cannot help with a call that never
// returns — the download simply stops, with the dashboard still showing it as
// running, which is the exact shape of failure this project exists to complain
// about.
func (f *Fetcher) copyWatched(
	ctx context.Context, file *os.File, body io.Reader, a streamArgs, last *atomic.Int64,
) (int64, error) {
	total := f.Snapshot.TotalBytes()
	buf := make([]byte, copyChunk)

	var written int64

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		n, readErr := body.Read(buf)
		if n > 0 {
			// **Written before the digest is fed, and both before the count
			// advances.** The count is what the caller resumes from, so it must
			// never run ahead of what is actually on the disk — a write that
			// failed after the counter moved would leave a hole the resume logic
			// could not see.
			if _, err := file.Write(buf[:n]); err != nil {
				return written, fmt.Errorf("bootstrap: writing %s: %w", f.Path, err)
			}
			a.digest.Write(buf[:n])
			written += int64(n)
			last.Store(f.now().UnixNano())

			if f.OnProgress != nil {
				done := a.fileSize + written
				elapsed := f.now().Sub(a.started)
				f.OnProgress(Progress{
					BytesDone:  done,
					BytesTotal: total,
					Part:       a.partIndex + 1,
					Parts:      len(f.Snapshot.Parts),
					Elapsed:    elapsed,
					Remaining:  ETA(done, total, elapsed),
				})
			}
		}

		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, fmt.Errorf("bootstrap: reading %s: %w", a.part.Name, readErr)
		}
	}
}

// verifyWhole re-reads the assembled file and checks it against the digest of the
// original.
//
// **Read back from disk rather than accumulated in memory while writing.** The
// per-part digests already cover what arrived over the network; what this covers
// is what is actually stored — a part written to a failing disk, a file somebody
// edited, and the one thing per-part digests structurally cannot catch, which is
// intact parts assembled in the wrong order.
func (f *Fetcher) verifyWhole(ctx context.Context, file *os.File) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("bootstrap: rewinding %s: %w", f.Path, err)
	}

	f.log().Info("checking the assembled file")
	digest := sha256.New()
	buf := make([]byte, copyChunk)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buf)
		if n > 0 {
			digest.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("bootstrap: reading back %s: %w", f.Path, err)
		}
	}

	if got := hex.EncodeToString(digest.Sum(nil)); got != f.Snapshot.SHA256 {
		return fmt.Errorf("bootstrap: the assembled file does not match the "+
			"expected checksum (got %s). It has been left in place; delete %s to "+
			"start again", got, f.Path)
	}
	f.log().Info("the assembled snapshot is complete and verified")
	return nil
}

// resetPart discards a part that arrived wrong, so the next attempt starts from
// its boundary.
func (f *Fetcher) resetPart(file *os.File, index int, digest hash.Hash) error {
	digest.Reset()
	return truncate(file, f.Snapshot.BytesBefore(index))
}

func fileSize(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("bootstrap: measuring the staged file: %w", err)
	}
	return info.Size(), nil
}

func truncate(file *os.File, to int64) error {
	if err := file.Truncate(to); err != nil {
		return fmt.Errorf("bootstrap: shortening the staged file: %w", err)
	}
	if _, err := file.Seek(to, io.SeekStart); err != nil {
		return fmt.Errorf("bootstrap: seeking the staged file: %w", err)
	}
	return nil
}

// seedDigest replays bytes already on disk through a hasher.
func seedDigest(file *os.File, offset, length int64, digest hash.Hash) error {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("bootstrap: seeking the staged file: %w", err)
	}
	if _, err := io.CopyN(digest, file, length); err != nil {
		return fmt.Errorf("bootstrap: re-reading the part being resumed: %w", err)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseContentRange pulls the total size out of a `Content-Range: bytes a-b/c`
// header, returning zero when it says nothing useful.
func parseContentRange(v string) int64 {
	const prefix = "bytes "
	if len(v) <= len(prefix) || v[:len(prefix)] != prefix {
		return 0
	}
	for i := len(v) - 1; i > 0; i-- {
		if v[i] == '/' {
			n, err := strconv.ParseInt(v[i+1:], 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

// statusOKFor reports whether a response is a usable answer to a request that
// resumed at `from`.
//
// A 200 to a resumed request is the dangerous case: it means the whole file is
// coming, and appending it to what is already held would produce a plausible
// length made of duplicated content.
func statusOKFor(status int, from int64) error {
	switch {
	case from == 0 && (status == http.StatusOK || status == http.StatusPartialContent):
		return nil
	case from > 0 && status == http.StatusPartialContent:
		return nil
	case from > 0 && status == http.StatusOK:
		return ErrRangeIgnored
	case status == http.StatusRequestedRangeNotSatisfiable:
		return fmt.Errorf("bootstrap: the server says that range does not exist, "+
			"which means the published file has changed: %w", ErrRangeIgnored)
	default:
		return fmt.Errorf("bootstrap: the server answered %s", http.StatusText(status))
	}
}
