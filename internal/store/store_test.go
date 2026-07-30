package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp opens a fresh database in a temporary directory and closes it when the
// test finishes.
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "data", "forktower.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "nested", "dir", "forktower.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	v1, err := first.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 < 1 {
		t.Fatalf("schema version after first open = %d, want at least 1", v1)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must apply nothing further. A migration that runs twice would
	// fail on the CREATE TABLE, so this also proves they are recorded.
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening an existing database: %v", err)
	}
	defer func() { _ = second.Close() }()

	v2, err := second.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Errorf("schema version changed on reopen: %d then %d", v1, v2)
	}
}

// The schema declares foreign keys and relies on write-ahead logging. SQLite
// disables foreign keys by default, and a declared constraint that is not
// enforced is worse than none, so this is asserted rather than assumed.
func TestPragmasAreInEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want \"wal\"", journal)
	}

	var fk int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Error("foreign_keys is off, so the schema's constraints are not enforced")
	}

	// Prove enforcement rather than just the setting: a delivery must not be able
	// to reference an alert that does not exist.
	if _, err := s.RecordDelivery(ctx, Delivery{
		AlertID: 99999, Transport: "nowhere", AttemptedAt: 1, OK: true,
	}); err == nil {
		t.Error("recorded a delivery against a nonexistent alert; the foreign key is not enforced")
	}
}

func TestNewDatabaseIsNotReadableByOthers(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("new database mode is %04o; it holds channel details and should be 0600", perm)
	}

	dir, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("data directory mode is %04o, want 0700", perm)
	}

	if len(s.Warnings) != 0 {
		t.Errorf("a freshly created database warned about permissions: %v", s.Warnings)
	}
}

func TestPermissiveDatabaseWarnsButOpens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "forktower.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	// A warning, not a refusal: a permissive mode is worth reporting, but
	// refusing to start would leave the user with no protection at all, which is
	// the worse outcome.
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("a world-readable database refused to open; it should warn: %v", err)
	}
	defer func() { _ = second.Close() }()

	var found bool
	for _, w := range second.Warnings {
		if strings.Contains(w, "readable by other users") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning about the permissive mode; warnings were %v", second.Warnings)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open accepted an empty path")
	}
}

func TestCloseIsSafeTwice(t *testing.T) {
	t.Parallel()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	t.Parallel()

	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations embedded")
	}

	// Ordered, unique, and gapless from 1. A gap would usually mean a migration
	// was renamed or dropped after shipping, which forward-only migrations do not
	// permit.
	for i, m := range all {
		want := i + 1
		if m.version != want {
			t.Errorf("migration %d is version %d, want %d — versions must run 1..n with no gaps",
				i, m.version, want)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %s is empty", m.name)
		}
	}
}

func TestMetaRoundTripAndAbsence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if _, err := s.GetMeta(ctx, "never-set"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetMeta on an unset key returned %v, want ErrNotFound", err)
	}

	if err := s.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMeta(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1" {
		t.Errorf("GetMeta = %q, want %q", got, "v1")
	}

	// Setting again replaces rather than failing on the primary key.
	if err := s.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatalf("overwriting a meta value: %v", err)
	}
	if got, _ := s.GetMeta(ctx, "k"); got != "v2" {
		t.Errorf("after overwrite GetMeta = %q, want %q", got, "v2")
	}

	if err := s.SetMeta(ctx, "", "x"); err == nil {
		t.Error("SetMeta accepted an empty key")
	}
}

func TestMetaInt64UsesDefaultWhenAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	// Every caller of this asks "where did I get to, or the beginning", so an
	// unset key is a default rather than an error.
	got, err := s.GetMetaInt64(ctx, MetaLastScannedSQHeight, -1)
	if err != nil {
		t.Fatalf("GetMetaInt64 on an unset key: %v", err)
	}
	if got != -1 {
		t.Errorf("GetMetaInt64 = %d, want the supplied default -1", got)
	}

	if err := s.SetMetaInt64(ctx, MetaLastScannedSQHeight, 961632); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMetaInt64(ctx, MetaLastScannedSQHeight, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got != 961632 {
		t.Errorf("GetMetaInt64 = %d, want 961632", got)
	}
}

func TestMetaInt64RejectsNonNumeric(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)

	if err := s.SetMeta(ctx, "height", "not-a-number"); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetMetaInt64(ctx, "height", 0)
	if err == nil {
		t.Fatal("GetMetaInt64 accepted a non-numeric value")
	}
	// The message must name the key and the offending value, or an operator
	// cannot find what to fix.
	if !strings.Contains(err.Error(), "height") || !strings.Contains(err.Error(), "not-a-number") {
		t.Errorf("error names neither the key nor the value: %v", err)
	}
}
