package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// openAtVersion builds a database carrying only the migrations up to `upTo`,
// which is what a user running an older build actually has on disk.
//
// Done by applying the embedded SQL directly rather than by adding a knob to
// Open: production has exactly one migration path — forward, all of it — and a
// way to stop halfway would be a way to ship a half-migrated database.
func openAtVersion(ctx context.Context, path string, upTo int) (*Store, error) {
	all, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version    INTEGER PRIMARY KEY,
		  applied_at INTEGER NOT NULL
		)`); err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.version > upTo {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			return nil, err
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`,
			m.version); err != nil {
			return nil, err
		}
	}
	return &Store{db: db, path: path}, nil
}

// A user upgrading has a database with a split already recorded in it. The new
// schema has to arrive over the top of that without disturbing any of it — this
// is the only migration path that will ever actually be taken, and it is the one
// a fresh-database test does not exercise.
func TestMigration0002AppliesOverAPopulatedDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "forktower.db")

	// A database at schema 1, with the kind of state a running daemon has.
	first, err := openAtVersion(ctx, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SaveSplitState(ctx, Split{
		State: StateSplit, ForkHash: "aa" + strings.Repeat("0", 62),
		ForkHeight: 850_000, DetectedAt: 1_790_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.UpsertAlert(ctx, Alert{
		Tier: TierWarning, Kind: "split_detected", DedupKey: "split_detected",
		Message: "The chains have separated.", CreatedAt: 1, LastRaisedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.AppendTimeline(ctx, TimelineEntry{
		At: 1, Kind: "split.state_changed", Summary: "The chains separated.",
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.SetMetaInt64(ctx, MetaTrustAnchorHeight, 849_999); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// The upgrade.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating a populated database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version < 2 {
		t.Fatalf("schema version %d after upgrading, want at least 2", version)
	}

	// Everything that was there is still there. A split the daemon had already
	// recorded is the one thing an upgrade must not lose.
	split, err := s.GetSplitState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if split.State != StateSplit || split.ForkHeight != 850_000 {
		t.Errorf("the recorded split changed across the upgrade: %+v", split)
	}
	anchor, err := s.GetMetaInt64(ctx, MetaTrustAnchorHeight, 0)
	if err != nil {
		t.Fatal(err)
	}
	if anchor != 849_999 {
		t.Errorf("trust anchor = %d across the upgrade, want 849999", anchor)
	}
	alerts, err := s.ListAlerts(ctx, AlertFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Errorf("got %d alerts across the upgrade, want 1", len(alerts))
	}
	entries, err := s.ListTimeline(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d timeline entries across the upgrade, want 1", len(entries))
	}

	// And the new tables work.
	node := seedNode(t, s)
	if _, _, err := s.UpsertChannel(ctx, sampleChannel(node)); err != nil {
		t.Errorf("the new schema is not usable after upgrading: %v", err)
	}

	// Applying again changes nothing: a daemon restarting must not re-run it.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening an already-migrated database: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	channels, err := reopened.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Errorf("got %d channels after reopening, want the one that was there", len(channels))
	}
}
