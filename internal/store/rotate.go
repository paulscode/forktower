package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RotationKind is the timeline entry a rotation writes about itself.
const RotationKind = "timeline.rotated"

// rotationTarget is how far below the limit a rotation aims to get.
//
// Half, rather than just under it. Rotating down to the limit means the very
// next event crosses it again, and a device that rotates on every write is worse
// off than one that never did.
const rotationTarget = 2

// rowOverheadBytes is what a timeline row costs beyond its text: the id, the
// timestamp, and SQLite's own per-row bookkeeping.
//
// An approximation, and openly one. This does not measure the file — pages are
// not freed to the filesystem by a delete, and the number that matters here is
// how much the *content* has grown, which is what decides whether it is worth
// sealing some away.
const rowOverheadBytes = 32

// TimelineSizeBytes estimates how much the timeline is holding.
func (s *Store) TimelineSizeBytes(ctx context.Context) (int64, error) {
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(LENGTH(kind) + LENGTH(summary) + LENGTH(COALESCE(data, '')) + ?)
		   FROM timeline`, rowOverheadBytes).Scan(&size)
	if err != nil {
		return 0, fmt.Errorf("measuring the timeline: %w", err)
	}
	return size.Int64, nil
}

// RotateTimeline seals the oldest part of the timeline into a file beside the
// database, once the timeline has grown past a limit.
//
// **Rotation, not trimming, and the difference is the whole point.** An audit
// trail that can be quietly shortened is not an audit trail — but a split that
// runs for weeks writes continuously on a device already hosting two Bitcoin
// nodes, so something has to give. What gives is where the old entries live, not
// whether they exist. The archive sits next to the database, is plain JSON
// anybody can read without this program, and is part of the full support bundle.
//
// Safe to interrupt at any point. The archive is written to a temporary file and
// renamed into place, so a half-written one is never mistaken for a finished
// one; nothing is deleted until the archive has been read back and the rows in
// it confirmed; and the archive is named after the rows it contains, so running
// again after a crash writes the same file rather than a second one.
//
// Returns how many entries were archived and where they went. Zero and empty
// mean there was nothing to do, which is the ordinary case.
func (s *Store) RotateTimeline(
	ctx context.Context, maxBytes int64,
) (archived int64, path string, err error) {
	if maxBytes <= 0 {
		return 0, "", fmt.Errorf("store: a timeline limit of %d is not a limit", maxBytes)
	}

	size, err := s.TimelineSizeBytes(ctx)
	if err != nil {
		return 0, "", err
	}
	if size <= maxBytes {
		return 0, "", nil
	}

	entries, err := s.oldestUntil(ctx, size-maxBytes/rotationTarget)
	if err != nil {
		return 0, "", err
	}
	if len(entries) == 0 {
		// Over the limit with nothing old enough to seal away means one enormous
		// entry, which rotation cannot help with and must not spin on.
		return 0, "", nil
	}

	path, err = s.writeArchive(entries)
	if err != nil {
		return 0, "", err
	}

	// Read back what was actually written, and delete only that. The archive is
	// the thing being trusted from here on, so it is the thing that gets checked
	// — not the slice in memory that was supposed to have produced it.
	confirmed, err := readArchive(path)
	if err != nil {
		return 0, "", fmt.Errorf("checking the archive before deleting anything: %w", err)
	}
	if len(confirmed) != len(entries) {
		return 0, "", fmt.Errorf(
			"the archive holds %d entries and %d were meant to be sealed away; "+
				"nothing has been deleted", len(confirmed), len(entries))
	}

	if err := s.deleteArchived(ctx, confirmed, path); err != nil {
		return 0, "", err
	}
	return int64(len(confirmed)), path, nil
}

// oldestUntil reads the oldest entries, up to roughly a number of bytes.
//
// By age rather than by count, because entries differ in size by an order of
// magnitude: one long event payload is worth a hundred short ones, and a
// count-based rule would either seal away far too much or far too little
// depending on what had happened lately.
func (s *Store) oldestUntil(ctx context.Context, wantBytes int64) ([]TimelineEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, kind, summary, COALESCE(data, '') FROM timeline ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("reading the timeline to rotate it: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out   []TimelineEntry
		taken int64
	)
	for rows.Next() {
		var e TimelineEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.Summary, &e.Data); err != nil {
			return nil, fmt.Errorf("reading a timeline entry: %w", err)
		}
		// The rotation records themselves stay. They are the thread that leads a
		// reader from the live timeline to the archives, and sealing one away
		// would hide the existence of the file it points at.
		if e.Kind == RotationKind {
			continue
		}
		out = append(out, e)
		taken += int64(len(e.Kind)+len(e.Summary)+len(e.Data)) + rowOverheadBytes
		if taken >= wantBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the timeline to rotate it: %w", err)
	}
	return out, nil
}

// archiveFile is what a sealed part of the timeline looks like on disk.
//
// Plain JSON with the entries as they were stored, so that somebody with the
// file and no copy of this program still has the record. That is the point of
// keeping it rather than deleting it.
type archiveFile struct {
	// Format is a version, because a file meant to outlive the program that
	// wrote it should say what it is.
	Format  int             `json:"format"`
	From    int64           `json:"from_id"`
	To      int64           `json:"to_id"`
	Count   int             `json:"count"`
	Entries []TimelineEntry `json:"entries"`
}

const archiveFormat = 1

// writeArchive seals entries into a file beside the database and returns where.
func (s *Store) writeArchive(entries []TimelineEntry) (string, error) {
	first, last := entries[0], entries[len(entries)-1]

	body, err := json.MarshalIndent(archiveFile{
		Format:  archiveFormat,
		From:    first.ID,
		To:      last.ID,
		Count:   len(entries),
		Entries: entries,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("preparing the timeline archive: %w", err)
	}
	body = append(body, '\n')

	path := s.archivePath(first, last)
	// Written aside and renamed into place. A rename is atomic, so a crash
	// halfway through can leave a partial temporary file but never a partial
	// archive that the next step would read back and believe.
	temp := path + ".partial"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return "", fmt.Errorf("writing the timeline archive: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return "", fmt.Errorf("putting the timeline archive in place: %w", err)
	}
	return path, nil
}

// archivePath names an archive after what is in it.
//
// Named from the content rather than from the clock, deliberately: running again
// after a crash produces the same name and overwrites an identical file, rather
// than leaving two archives of the same entries and no way to tell which is
// which. The timestamp comes from the newest entry sealed away, which is what a
// person looking for "the week of the split" actually wants.
func (s *Store) archivePath(first, last TimelineEntry) string {
	stamp := time.Unix(last.At, 0).UTC().Format("20060102T150405Z")
	name := fmt.Sprintf("timeline-%s-%d-%d.json", stamp, first.ID, last.ID)
	return filepath.Join(filepath.Dir(s.Path()), name)
}

// readArchive reads a sealed file back.
func readArchive(path string) ([]TimelineEntry, error) {
	body, err := os.ReadFile(path) //nolint:gosec // a path this package just wrote
	if err != nil {
		return nil, err
	}
	var file archiveFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("%s is not readable as an archive: %w", path, err)
	}
	if file.Count != len(file.Entries) {
		return nil, fmt.Errorf("%s says it holds %d entries and holds %d",
			path, file.Count, len(file.Entries))
	}
	return file.Entries, nil
}

// deleteArchived removes exactly the entries confirmed to be in the archive, and
// records that it happened.
//
// One transaction, so a reader never sees a timeline with the entries gone and
// no note saying where they went.
func (s *Store) deleteArchived(ctx context.Context, entries []TimelineEntry, path string) error {
	if len(entries) == 0 {
		return nil
	}
	first, last := entries[0], entries[len(entries)-1]

	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM timeline WHERE id = ?`, e.ID); err != nil {
				return fmt.Errorf("removing an archived timeline entry: %w", err)
			}
		}

		// The note that leads a reader from here to the file. Written in the same
		// transaction as the deletion, and written in the *user's* register rather
		// than as a technical record — somebody reading their own history should
		// not have to work out what happened to the earlier part of it.
		summary := fmt.Sprintf(
			"Earlier history was moved to a file to keep the database small. "+
				"Nothing was deleted: %d entries are in %s.",
			len(entries), filepath.Base(path))

		data, err := json.Marshal(map[string]any{
			"archive": filepath.Base(path),
			"count":   len(entries),
			"from_id": first.ID,
			"to_id":   last.ID,
			"from_at": first.At,
			"to_at":   last.At,
		})
		if err != nil {
			return fmt.Errorf("recording the rotation: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO timeline (at, kind, summary, data) VALUES (?, ?, ?, ?)`,
			last.At, RotationKind, summary, string(data)); err != nil {
			return fmt.Errorf("recording the rotation: %w", err)
		}
		return nil
	})
}
