package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSplitStateStartsUnarmedAndRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	got, err := s.GetSplitState(ctx)
	if err != nil {
		t.Fatalf("GetSplitState on a fresh database: %v", err)
	}
	if got.State != StateUnarmed {
		t.Errorf("initial state = %q, want %q", got.State, StateUnarmed)
	}
	if got.ForkKnown() {
		t.Error("a fresh database claims to know where the chains separated")
	}

	want := Split{
		State:      StateSplit,
		ForkHeight: 961632,
		ForkHash:   "0000000000000000000161687732d89fddeb491149e72f52e518c22fe001ba8c",
		DetectedAt: 1790000000,
		UpdatedAt:  1790000001,
	}
	if err := s.SaveSplitState(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err = s.GetSplitState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", got, want)
	}
	if !got.ForkKnown() {
		t.Error("ForkKnown() is false after recording a separation point")
	}
}

func TestSaveSplitStateRejectsUnknownState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	err := s.SaveSplitState(ctx, Split{State: "MAYBE", UpdatedAt: 1})
	if err == nil {
		t.Fatal("accepted an unknown split state")
	}
	if !strings.Contains(err.Error(), "MAYBE") {
		t.Errorf("error does not name the offending state: %v", err)
	}
}

func TestSplitStateStaysASingleRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	for i := range 3 {
		if err := s.SaveSplitState(ctx, Split{
			State: StateArmed, UpdatedAt: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM split_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("split_state holds %d rows, want exactly 1", n)
	}
}

func TestRecordBranchBlockIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	b := BranchBlock{
		Branch: BranchSQ, Height: 100, Hash: "aa", PrevHash: "a9",
		BlockTime: 500, ReceivedAt: 501,
	}

	// Re-reading recent blocks after a restart must not multiply rows; that
	// property is what makes resuming a scan safe.
	for range 3 {
		if err := s.RecordBranchBlock(ctx, b); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.CountBranchBlocks(ctx, BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recording the same block three times produced %d rows, want 1", n)
	}
}

func TestBranchBlocksArePrunedToTheRetentionWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	const extra = 25
	for i := range BranchBlockRetention + extra {
		if err := s.RecordBranchBlock(ctx, BranchBlock{
			Branch:    BranchSQ,
			Height:    int32(i),
			Hash:      fmt.Sprintf("hash-%05d", i),
			PrevHash:  fmt.Sprintf("hash-%05d", i-1),
			BlockTime: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.CountBranchBlocks(ctx, BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if n != BranchBlockRetention {
		t.Errorf("retained %d blocks, want %d", n, BranchBlockRetention)
	}

	// The newest are what matter, so the window must keep the top of the chain.
	hashes, err := s.RecentBranchHashes(ctx, BranchSQ, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		fmt.Sprintf("hash-%05d", BranchBlockRetention+extra-1),
		fmt.Sprintf("hash-%05d", BranchBlockRetention+extra-2),
		fmt.Sprintf("hash-%05d", BranchBlockRetention+extra-3),
	}
	for i := range want {
		if hashes[i] != want[i] {
			t.Errorf("recent hash %d = %q, want %q", i, hashes[i], want[i])
		}
	}
}

func TestBranchesArePrunedIndependently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	// Pruning one chain must not touch the other: they advance at very different
	// rates during a split, and losing the slower one's history would break
	// exactly the comparison the daemon exists to make.
	for i := range BranchBlockRetention + 10 {
		if err := s.RecordBranchBlock(ctx, BranchBlock{
			Branch: BranchSQ, Height: int32(i), Hash: fmt.Sprintf("sq-%05d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordBranchBlock(ctx, BranchBlock{
		Branch: BranchSF, Height: 1, Hash: "sf-00001",
	}); err != nil {
		t.Fatal(err)
	}

	sq, err := s.CountBranchBlocks(ctx, BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if sq != BranchBlockRetention {
		t.Errorf("sq retained %d, want %d", sq, BranchBlockRetention)
	}

	sf, err := s.CountBranchBlocks(ctx, BranchSF)
	if err != nil {
		t.Fatal(err)
	}
	if sf != 1 {
		t.Errorf("sf retained %d, want its single block untouched", sf)
	}
}

func TestRecordBranchBlockRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if err := s.RecordBranchBlock(ctx, BranchBlock{Branch: "elsewhere", Hash: "aa"}); err == nil {
		t.Error("accepted a block on an unknown chain")
	}
	if err := s.RecordBranchBlock(ctx, BranchBlock{Branch: BranchSQ}); err == nil {
		t.Error("accepted a block with no hash")
	}
	if _, err := s.RecentBranchHashes(ctx, "elsewhere", 5); err == nil {
		t.Error("RecentBranchHashes accepted an unknown chain")
	}
}

func TestRecentBranchHashesOnEmptyChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	got, err := s.RecentBranchHashes(ctx, BranchSF, 10)
	if err != nil {
		t.Fatalf("reading hashes from a chain with none: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d hashes from an empty chain", len(got))
	}
}

func TestBranchAndSplitStateValidity(t *testing.T) {
	t.Parallel()

	for _, ok := range []Branch{BranchSF, BranchSQ} {
		if !ok.Valid() {
			t.Errorf("branch %q should be valid", ok)
		}
	}
	for _, bad := range []Branch{"", "SF", "main", "other"} {
		if bad.Valid() {
			t.Errorf("branch %q should not be valid", bad)
		}
	}

	for _, ok := range []SplitState{
		StateUnarmed, StateArmed, StateSplit, StateResolving,
		StateResolvedSFWon, StateResolvedSQWon,
	} {
		if !ok.Valid() {
			t.Errorf("state %q should be valid", ok)
		}
	}
	for _, bad := range []SplitState{"", "split", "RESOLVED", "UNKNOWN"} {
		if bad.Valid() {
			t.Errorf("state %q should not be valid", bad)
		}
	}
}

// A height that does not fit in int32 means the database is corrupt or was
// written by something else. Truncating it silently would be worst for the fork
// height, which bounds how far the user's own chain is treated as verified.
func TestImplausibleForkHeightIsRefusedNotTruncated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE split_state SET fork_height = ? WHERE id = 1`, int64(1)<<40); err != nil {
		t.Fatal(err)
	}

	_, err := s.GetSplitState(ctx)
	if err == nil {
		t.Fatal("an out-of-range fork height was accepted; it would have been truncated")
	}
	if !strings.Contains(err.Error(), "fork_height") {
		t.Errorf("error does not name the column: %v", err)
	}
}
