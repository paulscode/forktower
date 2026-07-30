package store

import (
	"context"
	"fmt"
	"testing"
)

func TestAppendAndListTimeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range 5 {
		if _, err := s.AppendTimeline(ctx, TimelineEntry{
			At:      int64(1000 + i),
			Kind:    "split.branch_extended",
			Summary: fmt.Sprintf("the other chain reached block %d", i),
			Data:    fmt.Sprintf(`{"height":%d}`, i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListTimeline(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d entries, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID <= got[i-1].ID {
			t.Fatalf("entries are not in ascending id order: %d then %d", got[i-1].ID, got[i].ID)
		}
	}
	if got[0].Data != `{"height":0}` {
		t.Errorf("payload not preserved: %q", got[0].Data)
	}
}

// Paging is cursored on id rather than offset, so a caller polling for new
// entries cannot miss or repeat one as rows are appended between its calls.
func TestListTimelineIsCursoredNotOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range 4 {
		if _, err := s.AppendTimeline(ctx, TimelineEntry{
			At: int64(i), Kind: "k", Summary: fmt.Sprintf("entry %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListTimeline(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("first page has %d entries, want 2", len(first))
	}

	// New entries arrive between the two calls, as they would in practice.
	if _, err := s.AppendTimeline(ctx, TimelineEntry{At: 99, Kind: "k", Summary: "later"}); err != nil {
		t.Fatal(err)
	}

	second, err := s.ListTimeline(ctx, first[len(first)-1].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range second {
		for _, seen := range first {
			if e.ID == seen.ID {
				t.Errorf("entry %d appeared on both pages", e.ID)
			}
		}
	}
	if len(second) != 3 {
		t.Errorf("second page has %d entries, want the 2 remaining plus the 1 added", len(second))
	}
}

func TestListTimelineClampsLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range 3 {
		if _, err := s.AppendTimeline(ctx, TimelineEntry{
			At: int64(i), Kind: "k", Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListTimeline(ctx, 0, 1_000_000)
	if err != nil {
		t.Fatalf("an over-large limit was refused rather than clamped: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d entries, want 3", len(got))
	}
}

func TestAppendTimelineRequiresKindAndSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if _, err := s.AppendTimeline(ctx, TimelineEntry{At: 1, Summary: "s"}); err == nil {
		t.Error("accepted an entry with no kind")
	}
	if _, err := s.AppendTimeline(ctx, TimelineEntry{At: 1, Kind: "k"}); err == nil {
		t.Error("accepted an entry with no summary")
	}
}

func TestCountTimeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	n, err := s.CountTimeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a fresh timeline holds %d entries", n)
	}

	for i := range 7 {
		if _, err := s.AppendTimeline(ctx, TimelineEntry{
			At: int64(i), Kind: "k", Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err = s.CountTimeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("CountTimeline = %d, want 7", n)
	}
}
