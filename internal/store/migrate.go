package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// migrationFS holds the schema, embedded so a binary carries everything it needs
// to create its own database.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationNamePattern matches "0001_init.sql": a zero-padded version, an
// underscore, a description. The version orders application and is recorded, so
// a file that does not match is a mistake rather than something to skip
// silently.
var migrationNamePattern = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

// migrate applies every migration not yet recorded, in version order, each in
// its own transaction.
//
// Forward-only by design: there is no "down". A mistake in a shipped migration
// is corrected by a new migration, because rolling one back on a user's machine
// would mean deleting data we were trusted to keep.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version    INTEGER PRIMARY KEY,
		  applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("applying migration %s: %w", m.name, err)
		}
		// strftime rather than an injected clock: this timestamp is a record of
		// when the schema changed, not an input to any decision.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at)
			 VALUES (?, CAST(strftime('%s', 'now') AS INTEGER))`, m.version); err != nil {
			return fmt.Errorf("recording migration %s: %w", m.name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	// Nothing new to learn from Close once rows.Err() has been checked below.
	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	return applied, nil
}

// loadMigrations reads and orders the embedded migrations, rejecting anything
// unexpected. Exported for tests in this package only via migrationsForTest.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}

	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationNamePattern.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf(
				"store: migration %q is not named NNNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q has an unreadable version: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf(
				"store: migrations %q and %q share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}

	if len(out) == 0 {
		return nil, errors.New("store: no migrations embedded")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// SchemaVersion returns the highest applied migration version, or 0 on a
// database with none.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
