// Package store is the only place SQL lives. Callers use typed methods.
//
// Recording is idempotent: replaying the same block or the same event produces
// no duplicate rows, which is what makes crash recovery safe.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver; no cgo, so cross-compilation stays free
)

// Errors returned by this package.
var (
	// ErrNotFound is returned when a lookup finds no row. Callers distinguish
	// "absent" from "failed" with errors.Is rather than by inspecting a nil.
	ErrNotFound = errors.New("store: not found")
)

// Directory and file modes. The database holds channel details and the
// configuration beside it holds credentials, so neither is any other user's
// business.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Store is a handle on the daemon's state. Safe for concurrent use.
type Store struct {
	db   *sql.DB
	path string

	// Warnings holds non-fatal problems noticed while opening: a permissive file
	// mode, for instance. Reported rather than logged here so that the caller
	// decides where they go, and so a test can assert on them.
	Warnings []string
}

// Open opens or creates the database at path, applies any outstanding
// migrations, and verifies the pragmas the schema relies on.
//
// The parent directory is created if absent. An existing database is never
// re-created: migrations are forward-only and applied once each.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: no database path configured")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("creating data directory %s: %w", dir, err)
	}

	existed := fileExists(path)

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	// One connection. SQLite in WAL mode supports concurrent readers, but a
	// single connection removes any possibility of SQLITE_BUSY and costs nothing
	// at this workload: a block arrives every ten minutes and the dashboard polls
	// a handful of small queries. Predictability is worth more here than
	// throughput we do not need.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to database %s: %w", path, err)
	}

	if err := s.verifyPragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	if !existed {
		// Created by the driver with the process umask, which is usually 0644.
		if err := os.Chmod(path, fileMode); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("restricting permissions on %s: %w", path, err)
		}
	}

	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Checked after migrating: the WAL and shared-memory sidecars only exist once
	// something has been written.
	s.Warnings = append(s.Warnings, permissionWarnings(path)...)

	return s, nil
}

// dsn builds the driver connection string.
//
// The pragmas are set here rather than as statements after connecting because a
// single connection is reused: setting them per-connection in the DSN means they
// cannot be missed if the pool ever reconnects.
func dsn(path string) string {
	q := url.Values{}
	// Write-ahead logging: readers do not block the writer, and a crash mid-write
	// rolls back cleanly rather than leaving a half-written page.
	q.Add("_pragma", "journal_mode(WAL)")
	// Foreign keys are off by default in SQLite. The schema declares them, and a
	// declared constraint that is not enforced is worse than none.
	q.Add("_pragma", "foreign_keys(1)")
	// Wait rather than failing immediately if a lock is somehow contended.
	q.Add("_pragma", "busy_timeout(5000)")
	// Durability: full synchronisation. This database records what we saw and
	// when, and is read after a crash to decide what was missed.
	q.Add("_pragma", "synchronous(FULL)")
	return "file:" + path + "?" + q.Encode()
}

// verifyPragmas confirms the settings the schema depends on actually took
// effect. A DSN typo would otherwise leave foreign keys silently unenforced.
func (s *Store) verifyPragmas(ctx context.Context) error {
	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("reading journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("store: journal_mode is %q, want \"wal\"", journal)
	}

	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("reading foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("store: foreign_keys is off, so the schema's constraints would not be enforced")
	}
	return nil
}

// permissionWarnings reports any of the database files that other users can
// read. A warning rather than a refusal: a permissive mode is worth telling the
// operator about, but refusing to start would leave them unprotected, which is
// worse than the disclosure.
func permissionWarnings(path string) []string {
	var out []string
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			out = append(out, fmt.Sprintf(
				"database file %s is readable by other users (mode %04o); it holds channel "+
					"details — run: chmod 600 %s", p, perm, p))
		}
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Close releases the database. Safe to call more than once.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	if err != nil {
		return fmt.Errorf("closing database %s: %w", s.path, err)
	}
	return nil
}

// Path returns the database's location, for diagnostics.
func (s *Store) Path() string { return s.path }

// withTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		// A no-op once the transaction has been committed.
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}
