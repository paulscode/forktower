package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Known meta keys. Kept as constants so a typo is a build failure rather than a
// value that silently reads back empty.
const (
	// MetaLastScannedSQHeight is the high-water mark for scanning the other
	// chain. It advances only after a block has been fully processed and
	// committed, which is what makes a crash mid-scan safe to resume.
	MetaLastScannedSQHeight = "last_scanned_sq_height"
	// MetaSQBranchVerifiedAt records when the other chain's backend was last
	// confirmed to be on the branch we think it is.
	MetaSQBranchVerifiedAt = "sq_backend_branch_verified_at"
	// MetaTrustAnchorHeight is how far the user's own node is treated as already
	// verified history.
	MetaTrustAnchorHeight = "trust_anchor_height"
)

// SetMeta stores a single key/value pair, replacing any previous value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if key == "" {
		return errors.New("store: meta key must not be empty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("setting meta %q: %w", key, err)
	}
	return nil
}

// GetMeta reads a single value. It returns ErrNotFound when the key has never
// been set, which callers distinguish from an empty stored value.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("meta %q: %w", key, ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("reading meta %q: %w", key, err)
	}
	return value, nil
}

// SetMetaInt64 stores a number.
func (s *Store) SetMetaInt64(ctx context.Context, key string, value int64) error {
	return s.SetMeta(ctx, key, formatInt64(value))
}

// GetMetaInt64 reads a number, returning def when the key has never been set.
//
// A default rather than an error for the absent case, because every caller of
// this wants "where did I get to, or the beginning".
func (s *Store) GetMetaInt64(ctx context.Context, key string, def int64) (int64, error) {
	raw, err := s.GetMeta(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return def, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := parseInt64(raw)
	if err != nil {
		return 0, fmt.Errorf("meta %q holds %q, which is not a number: %w", key, raw, err)
	}
	return n, nil
}
