package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TimelineEntry is one thing the engines saw or did.
//
// The timeline is append-only. It is what a user, a support request, or a
// post-mortem reads to reconstruct what happened and when, so entries are never
// edited or removed in place; when it grows too large the oldest are archived to
// a file rather than deleted.
type TimelineEntry struct {
	ID      int64
	At      int64
	Kind    string
	Summary string
	// Data is the event's JSON payload, or empty. Held as text because this is a
	// record for reading, not a structure to query.
	Data string
}

// Timeline listing bounds.
const (
	DefaultTimelineLimit = 100
	MaxTimelineLimit     = 500
)

// AppendTimeline records an entry and returns its id.
func (s *Store) AppendTimeline(ctx context.Context, e TimelineEntry) (int64, error) {
	if e.Kind == "" {
		return 0, errors.New("store: timeline entry needs a kind")
	}
	if e.Summary == "" {
		return 0, errors.New("store: timeline entry needs a summary")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO timeline (at, kind, summary, data) VALUES (?, ?, ?, ?)`,
		e.At, e.Kind, e.Summary, nullString(e.Data))
	if err != nil {
		return 0, fmt.Errorf("appending timeline entry %q: %w", e.Kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading new timeline id: %w", err)
	}
	return id, nil
}

// ListTimeline returns entries with an id greater than afterID, oldest first.
//
// Cursored on id rather than offset so that a caller polling for new entries
// cannot miss or repeat one as rows are appended between calls.
func (s *Store) ListTimeline(ctx context.Context, afterID int64, limit int) ([]TimelineEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, kind, summary, data
		 FROM timeline WHERE id > ? ORDER BY id ASC LIMIT ?`,
		afterID, clampLimit(limit, DefaultTimelineLimit, MaxTimelineLimit))
	if err != nil {
		return nil, fmt.Errorf("listing timeline after %d: %w", afterID, err)
	}
	// Nothing new to learn from Close once rows.Err() has been checked below.
	defer func() { _ = rows.Close() }()

	var out []TimelineEntry
	for rows.Next() {
		var (
			e    TimelineEntry
			data sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Summary, &data); err != nil {
			return nil, fmt.Errorf("scanning timeline entry: %w", err)
		}
		e.Data = data.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing timeline after %d: %w", afterID, err)
	}
	return out, nil
}

// CountTimeline reports how many entries exist, for the rotation threshold.
func (s *Store) CountTimeline(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM timeline`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting timeline entries: %w", err)
	}
	return n, nil
}
