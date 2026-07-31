package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fillTimeline writes n entries and returns them, so a test can check that every
// one of them survived.
func fillTimeline(t *testing.T, s *Store, n int) []TimelineEntry {
	t.Helper()
	ctx := context.Background()

	out := make([]TimelineEntry, 0, n)
	for i := range n {
		e := TimelineEntry{
			At:      int64(1_790_000_000 + i),
			Kind:    "split.branch_extended",
			Summary: strings.Repeat("a new block arrived on the other chain. ", 4),
			Data:    `{"branch":"sq","block":{"height":` + itoa(i) + `}}`,
		}
		id, err := s.AppendTimeline(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		e.ID = id
		out = append(out, e)
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits []byte
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// liveTimeline is everything still in the database.
func liveTimeline(t *testing.T, s *Store) []TimelineEntry {
	t.Helper()
	got, err := s.ListTimeline(context.Background(), 0, MaxTimelineLimit)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// archives lists the sealed files beside the database.
func archives(t *testing.T, s *Store) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(s.Path()), "timeline-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// The promise this whole feature rests on: nothing is destroyed. An audit trail
// that can be quietly shortened is not an audit trail, so every entry must be
// findable afterwards — in the database or in the file, and identical either way.
func TestRotationLosesNothing(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	written := fillTimeline(t, s, 200)
	size, err := s.TimelineSizeBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	archived, path, err := s.RotateTimeline(context.Background(), size/2)
	if err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Fatal("nothing was rotated out of a timeline over its limit")
	}

	// Every entry, exactly once, across both places.
	found := map[int64]TimelineEntry{}
	sealed, err := readArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sealed {
		found[e.ID] = e
	}
	for _, e := range liveTimeline(t, s) {
		if _, twice := found[e.ID]; twice {
			t.Errorf("entry %d is in both the database and the archive", e.ID)
		}
		found[e.ID] = e
	}

	for _, want := range written {
		got, ok := found[want.ID]
		if !ok {
			t.Fatalf("entry %d is in neither the database nor the archive", want.ID)
		}
		if got.At != want.At || got.Kind != want.Kind ||
			got.Summary != want.Summary || got.Data != want.Data {
			t.Errorf("entry %d changed on the way through:\n got  %+v\n want %+v",
				want.ID, got, want)
		}
	}
}

// The rotation is itself part of the record, so somebody reading their own
// history is told where the earlier part went rather than finding it absent.
func TestRotationRecordsItself(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	fillTimeline(t, s, 200)
	size, _ := s.TimelineSizeBytes(context.Background())
	_, path, err := s.RotateTimeline(context.Background(), size/2)
	if err != nil {
		t.Fatal(err)
	}

	var note TimelineEntry
	for _, e := range liveTimeline(t, s) {
		if e.Kind == RotationKind {
			note = e
		}
	}
	if note.ID == 0 {
		t.Fatal("a rotation happened and the timeline says nothing about it")
	}
	// Named, so the note leads somewhere.
	if !strings.Contains(note.Summary, filepath.Base(path)) {
		t.Errorf("the note does not say which file: %q", note.Summary)
	}
	// And says plainly that nothing was lost, because "moved to a file" reads as
	// "deleted" to somebody who is already worried.
	if !strings.Contains(strings.ToLower(note.Summary), "nothing was deleted") {
		t.Errorf("the note does not say nothing was deleted: %q", note.Summary)
	}
	if !json.Valid([]byte(note.Data)) {
		t.Errorf("the note's payload is not readable: %q", note.Data)
	}
}

// A crash between sealing the archive and deleting the rows must lose nothing,
// and running again must not produce a second archive of the same entries.
func TestACrashBetweenSealingAndDeletingLosesNothing(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	written := fillTimeline(t, s, 200)
	size, _ := s.TimelineSizeBytes(ctx)
	limit := size / 2

	// The first half of a rotation: the archive is written, and then nothing.
	entries, err := s.oldestUntil(ctx, size-limit/rotationTarget)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := s.writeArchive(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(liveTimeline(t, s)); got != len(written) {
		t.Fatalf("something was deleted before the archive was finished: %d of %d left",
			got, len(written))
	}

	// Starting again finds the same rows and writes the same file.
	archived, secondPath, err := s.RotateTimeline(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	if secondPath != firstPath {
		t.Errorf("a second run wrote a different archive:\n %s\n %s", firstPath, secondPath)
	}
	if got := archives(t, s); len(got) != 1 {
		t.Errorf("a crash and a retry produced %d archives: %v", len(got), got)
	}
	if archived != int64(len(entries)) {
		t.Errorf("archived %d entries, want the %d already sealed", archived, len(entries))
	}

	// And nothing is missing.
	sealed, err := readArchive(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	total := len(sealed)
	for _, e := range liveTimeline(t, s) {
		if e.Kind != RotationKind {
			total++
		}
	}
	if total != len(written) {
		t.Errorf("%d entries survived a crash and a retry, want %d", total, len(written))
	}
}

// A half-written archive must never be believed. It is written aside and renamed
// into place, so an interrupted write leaves a partial file with a name nothing
// reads.
func TestAHalfWrittenArchiveIsNeverBelieved(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	fillTimeline(t, s, 200)
	size, _ := s.TimelineSizeBytes(ctx)

	// Something that looks like an archive but is not finished.
	partial := filepath.Join(filepath.Dir(s.Path()), "timeline-20260101T000000Z-1-5.json.partial")
	if err := os.WriteFile(partial, []byte(`{"format":1,"count":99,"entries":[`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.RotateTimeline(ctx, size/2); err != nil {
		t.Fatalf("a leftover partial file broke rotation: %v", err)
	}
	// It was ignored, not read: the finished archives are the ones without the
	// suffix, and there is exactly one.
	if got := archives(t, s); len(got) != 1 {
		t.Errorf("found %d finished archives: %v", len(got), got)
	}
}

// An archive that does not say what it holds is refused rather than trusted,
// because the next step after trusting it is deleting the only other copy.
func TestAnArchiveThatDisagreesWithItselfIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "timeline-x.json")
	if err := os.WriteFile(path, []byte(`{"format":1,"count":9,"entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchive(path); err == nil {
		t.Error("an archive whose count does not match its contents was accepted")
	}

	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchive(path); err == nil {
		t.Error("a file that is not an archive was accepted as one")
	}
}

// Under the limit, nothing happens. This is the ordinary case and it must be
// free of side effects: no file, no note, no deletion.
func TestNothingHappensUnderTheLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	fillTimeline(t, s, 20)
	size, _ := s.TimelineSizeBytes(ctx)

	archived, path, err := s.RotateTimeline(ctx, size*10)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 0 || path != "" {
		t.Errorf("rotated %d entries into %q while under the limit", archived, path)
	}
	if got := archives(t, s); len(got) != 0 {
		t.Errorf("wrote %v while under the limit", got)
	}
	if got := len(liveTimeline(t, s)); got != 20 {
		t.Errorf("%d entries left, want 20", got)
	}
}

// An empty timeline is not a special case anybody should have to think about.
func TestAnEmptyTimelineRotatesToNothing(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	if size, err := s.TimelineSizeBytes(ctx); err != nil || size != 0 {
		t.Errorf("an empty timeline measures %d (%v)", size, err)
	}
	archived, path, err := s.RotateTimeline(ctx, 1024)
	if err != nil || archived != 0 || path != "" {
		t.Errorf("rotating an empty timeline gave %d, %q, %v", archived, path, err)
	}
}

// A limit that is not a limit is refused, rather than silently archiving
// everything.
func TestANonsenseLimitIsRefused(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	for _, limit := range []int64{0, -1} {
		if _, _, err := s.RotateTimeline(context.Background(), limit); err == nil {
			t.Errorf("a limit of %d was accepted", limit)
		}
	}
}

// Rotating twice archives two different stretches, and the notes accumulate.
func TestASecondRotationSealsTheNextStretch(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()

	first := fillTimeline(t, s, 200)
	size, _ := s.TimelineSizeBytes(ctx)
	limit := size / 2
	if _, _, err := s.RotateTimeline(ctx, limit); err != nil {
		t.Fatal(err)
	}

	second := fillTimeline(t, s, 200)
	if _, _, err := s.RotateTimeline(ctx, limit); err != nil {
		t.Fatal(err)
	}

	if got := archives(t, s); len(got) != 2 {
		t.Fatalf("two rotations produced %d archives: %v", len(got), got)
	}

	// Everything ever written is still findable.
	seen := map[int64]bool{}
	for _, path := range archives(t, s) {
		sealed, err := readArchive(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range sealed {
			seen[e.ID] = true
		}
	}
	var notes int
	for _, e := range liveTimeline(t, s) {
		if e.Kind == RotationKind {
			notes++
			continue
		}
		seen[e.ID] = true
	}
	if notes != 2 {
		t.Errorf("two rotations left %d notes behind", notes)
	}
	for _, e := range append(first, second...) {
		if !seen[e.ID] {
			t.Errorf("entry %d was lost across two rotations", e.ID)
		}
	}
}
