package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TowerKind is which watchtower implementation a tower runs.
//
// The two do not interoperate: teos speaks the never-finalised BOLT13 and LND's
// tower speaks its own wire protocol, so a Core Lightning node cannot register
// with an LND tower and vice versa. The kind therefore decides which fields on a
// tower row mean anything at all.
type TowerKind string

// The watchtower implementations Forktower can talk to.
const (
	TowerLND  TowerKind = "lnd"
	TowerTeos TowerKind = "teos"
)

// Valid reports whether k is a kind this schema accepts.
func (k TowerKind) Valid() bool {
	switch k {
	case TowerLND, TowerTeos:
		return true
	default:
		return false
	}
}

// TowerStatus is how a tower is doing, in one vocabulary for both
// implementations.
//
// These are teos's own states plus `unknown` for before we have asked. For an
// LND tower we derive what we can, and two of them are unreachable by
// construction: LND has no subscriptions, and its watchtower client has no way
// to prove a tower misbehaved. See [TowerStatus.PossibleFor].
type TowerStatus string

// Tower statuses.
const (
	// TowerReachable means it answered, and had nothing to complain about.
	TowerReachable TowerStatus = "reachable"
	// TowerTemporarilyUnreachable means it has stopped answering but the client
	// is still retrying. Distinct from TowerUnreachable because the honest thing
	// to tell a user about a tower being retried is different from what to tell
	// them about one that has been given up on.
	TowerTemporarilyUnreachable TowerStatus = "temporarily_unreachable"
	// TowerUnreachable means retries have been exhausted.
	TowerUnreachable TowerStatus = "unreachable"
	// TowerSubscriptionError means the subscription has expired or run out of
	// slots, so appointments are being refused. teos only.
	TowerSubscriptionError TowerStatus = "subscription_error"
	// TowerMisbehaving means the tower returned a receipt whose signature does
	// not check out. teos only, and it comes with a proof — this is the one
	// place in the system where a user can be shown evidence rather than an
	// inference.
	TowerMisbehaving TowerStatus = "misbehaving"
	// TowerStatusUnknown means we have not asked yet.
	TowerStatusUnknown TowerStatus = "unknown"
)

// Valid reports whether s is a status this schema accepts.
func (s TowerStatus) Valid() bool {
	switch s {
	case TowerReachable, TowerTemporarilyUnreachable, TowerUnreachable,
		TowerSubscriptionError, TowerMisbehaving, TowerStatusUnknown:
		return true
	default:
		return false
	}
}

// PossibleFor reports whether a tower of this kind can ever reach this status.
//
// Not a validation rule — a caller that gets this wrong has a bug in its
// monitor, not bad input — but worth being able to assert. An LND tower
// reporting `misbehaving` would mean somebody had invented evidence that cannot
// exist, and that is worth catching in a test rather than showing to a user.
func (s TowerStatus) PossibleFor(kind TowerKind) bool {
	if kind == TowerLND {
		return s != TowerSubscriptionError && s != TowerMisbehaving
	}
	return true
}

// Tower is a watchtower, ours or somebody else's.
type Tower struct {
	ID     int64
	Kind   TowerKind
	Pubkey string
	// URI is pubkey@host:port, as a user would paste it. May be empty for a
	// tower we learned about from the client rather than from configuration.
	URI string
	// Managed means this installation runs it. An external tower's
	// configuration is not ours, and the UI must not imply we can change it.
	Managed     bool
	FirstSeenAt int64
	// LastOKAt is the last time it answered. Zero means it never has, which is a
	// different fact from having answered once and stopped.
	LastOKAt     int64
	Status       TowerStatus
	StatusDetail string
	// BlobTypes is the session types an LND tower accepts, as stored JSON.
	// Empty for teos, which never sees a channel type.
	BlobTypes string
	// SubscriptionExpiryHeight and SubscriptionSlotsRemaining are teos only, and
	// nil for an LND tower. A teos subscription expires; an LND session does
	// not, so a zero here would be a claim rather than an absence.
	SubscriptionExpiryHeight   *int32
	SubscriptionSlotsRemaining *int32
	UpdatedAt                  int64
}

// UpsertTower records a tower, or updates what is known about it, and reports
// whether anything actually changed.
//
// Keyed on the pubkey, because that is the tower's identity. The address it
// answers at is not: a hidden service can be republished and a LAN address
// changes, and neither makes it a different tower.
//
// Deliberately does not touch the status. That comes from the monitor, on
// evidence a configuration reload does not have, and re-reading the config must
// not silently mark a tower healthy — see [Store.SetTowerStatus].
func (s *Store) UpsertTower(ctx context.Context, t Tower) (id int64, changed bool, err error) {
	if t.Pubkey == "" {
		return 0, false, errors.New("store: a tower needs its pubkey")
	}
	if !t.Kind.Valid() {
		return 0, false, fmt.Errorf("store: %q is not a watchtower kind this schema accepts", t.Kind)
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			existingID int64
			same       bool
		)
		row := tx.QueryRowContext(ctx,
			`SELECT id, kind = ? AND uri IS ? AND managed = ?
			   FROM towers WHERE pubkey = ?`,
			t.Kind, nullString(t.URI), boolToInt(t.Managed), t.Pubkey)

		switch scanErr := row.Scan(&existingID, &same); {
		case scanErr == nil:
			id = existingID
			if same {
				return nil
			}
			if _, e := tx.ExecContext(ctx,
				`UPDATE towers SET kind = ?, uri = ?, managed = ?, updated_at = ?
				  WHERE id = ?`,
				t.Kind, nullString(t.URI), boolToInt(t.Managed),
				t.UpdatedAt, existingID); e != nil {
				return fmt.Errorf("updating tower %s: %w", t.Pubkey, e)
			}
			changed = true
			return nil

		case errors.Is(scanErr, sql.ErrNoRows):
			res, e := tx.ExecContext(ctx,
				`INSERT INTO towers
				   (kind, pubkey, uri, managed, first_seen_at, status, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				t.Kind, t.Pubkey, nullString(t.URI), boolToInt(t.Managed),
				t.FirstSeenAt, TowerStatusUnknown, t.UpdatedAt)
			if e != nil {
				return fmt.Errorf("recording tower %s: %w", t.Pubkey, e)
			}
			newID, e := res.LastInsertId()
			if e != nil {
				return fmt.Errorf("reading new tower id: %w", e)
			}
			id, changed = newID, true
			return nil

		default:
			return fmt.Errorf("looking up tower %s: %w", t.Pubkey, scanErr)
		}
	})
	if err != nil {
		return 0, false, err
	}
	return id, changed, nil
}

// TowerHealth is what a monitor learned on one pass.
type TowerHealth struct {
	Status       TowerStatus
	Detail       string
	BlobTypes    string
	LastOKAt     int64
	ExpiryHeight *int32
	SlotsLeft    *int32
}

// SetTowerStatus records what the monitor found.
//
// Separate from UpsertTower for the same reason SetChannelCloseSF is separate
// from UpsertChannel: it comes from somewhere else, on different evidence, and a
// configuration reload must not be able to declare a tower healthy.
//
// LastOKAt is only ever moved forward, and only by a status that means the tower
// answered. A tower that has gone quiet keeps the timestamp of the last time it
// did not — which is the number the user actually needs.
func (s *Store) SetTowerStatus(ctx context.Context, id int64, h TowerHealth, now int64) error {
	if !h.Status.Valid() {
		return fmt.Errorf("store: %q is not a tower status this schema accepts", h.Status)
	}
	lastOK := nullInt64(h.LastOKAt)
	if h.Status == TowerReachable {
		lastOK = now
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE towers SET
		   status = ?, status_detail = ?, blob_types = ?,
		   last_ok_at = COALESCE(?, last_ok_at),
		   subscription_expiry_height = ?, subscription_slots_remaining = ?,
		   updated_at = ?
		 WHERE id = ?`,
		h.Status, nullString(h.Detail), nullString(h.BlobTypes), lastOK,
		nullOptionalInt32(h.ExpiryHeight), nullOptionalInt32(h.SlotsLeft),
		now, id)
	if err != nil {
		return fmt.Errorf("recording tower status: %w", err)
	}
	return requireOneRow(res, "tower", id)
}

// TowerFilter narrows ListTowers.
type TowerFilter struct {
	// Kind, when set, restricts to towers of that implementation.
	Kind TowerKind
	// ManagedOnly restricts to towers this installation runs.
	ManagedOnly bool
}

// ListTowers returns towers in ascending id order, which is the order they were
// first seen.
func (s *Store) ListTowers(ctx context.Context, f TowerFilter) ([]Tower, error) {
	// Fixed query, filters as parameters — see ListChannels for why.
	const query = `
		SELECT id, kind, pubkey, uri, managed, first_seen_at, last_ok_at,
		       status, status_detail, blob_types,
		       subscription_expiry_height, subscription_slots_remaining,
		       updated_at
		  FROM towers
		 WHERE (? = '' OR kind = ?)
		   AND (? = 0  OR managed = 1)
		 ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query,
		f.Kind, f.Kind, boolToInt(f.ManagedOnly))
	if err != nil {
		return nil, fmt.Errorf("listing towers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Tower
	for rows.Next() {
		t, scanErr := scanTower(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing towers: %w", err)
	}
	return out, nil
}

func scanTower(rows *sql.Rows) (Tower, error) {
	var (
		t          Tower
		uri        sql.NullString
		managed    int64
		lastOK     sql.NullInt64
		detail     sql.NullString
		blobTypes  sql.NullString
		expiry     sql.NullInt64
		slotsLeft  sql.NullInt64
		kind       string
		statusText string
	)
	if err := rows.Scan(&t.ID, &kind, &t.Pubkey, &uri, &managed, &t.FirstSeenAt,
		&lastOK, &statusText, &detail, &blobTypes, &expiry, &slotsLeft,
		&t.UpdatedAt); err != nil {
		return Tower{}, fmt.Errorf("reading a tower: %w", err)
	}
	t.Kind = TowerKind(kind)
	t.Status = TowerStatus(statusText)
	t.URI = uri.String
	t.Managed = managed == 1
	t.LastOKAt = lastOK.Int64
	t.StatusDetail = detail.String
	t.BlobTypes = blobTypes.String
	t.SubscriptionExpiryHeight = optionalInt32From(expiry)
	t.SubscriptionSlotsRemaining = optionalInt32From(slotsLeft)
	return t, nil
}

// Coverage is whether one tower can actually protect one channel.
type Coverage struct {
	ChannelID int64
	TowerID   int64
	Coverable bool
	// Reason is required in both directions. "Not coverable" with no reason is
	// an accusation without evidence; "coverable" with no reason gives a reader
	// nothing to check.
	Reason string
	// NumBackups is **not a per-channel figure**, because no Lightning node
	// reports one. It is the states backed up on the sessions of this channel's
	// *type*, shared with every other channel of that type. Recorded here anyway
	// because it is the only backup signal there is, and stored per channel so a
	// reader sees it beside the verdict it belongs to — but it must never be
	// presented as "this channel has N backups".
	NumBackups   int64
	LastBackupAt int64
	// SweepFeeSatPerKW is the rate negotiated for the session covering this
	// channel, nil until a session exists. Fixed at negotiation and pre-signed
	// into every justice transaction: nobody can bump it, because the tower
	// holds no keys.
	SweepFeeSatPerKW *int32
	CheckedAt        int64
}

// UpsertCoverage records what a monitor concluded about one channel at one
// tower.
func (s *Store) UpsertCoverage(ctx context.Context, c Coverage) error {
	if c.Reason == "" {
		return errors.New("store: a coverage verdict needs a reason, in either direction")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tower_channel_coverage
		   (channel_id, tower_id, coverable, reason, num_backups,
		    last_backup_at, negotiated_sweep_fee_sat_kw, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (channel_id, tower_id) DO UPDATE SET
		   coverable = excluded.coverable,
		   reason = excluded.reason,
		   num_backups = excluded.num_backups,
		   last_backup_at = excluded.last_backup_at,
		   negotiated_sweep_fee_sat_kw = excluded.negotiated_sweep_fee_sat_kw,
		   checked_at = excluded.checked_at`,
		c.ChannelID, c.TowerID, boolToInt(c.Coverable), c.Reason, c.NumBackups,
		nullInt64(c.LastBackupAt), nullOptionalInt32(c.SweepFeeSatPerKW), c.CheckedAt)
	if err != nil {
		return fmt.Errorf("recording coverage of channel %d: %w", c.ChannelID, err)
	}
	return nil
}

// CoverageFilter narrows ListCoverage.
type CoverageFilter struct {
	ChannelID int64
	TowerID   int64
	// UncoverableOnly restricts to the channels no tower can protect, which is
	// the set the readiness UI leads with.
	UncoverableOnly bool
}

// ListCoverage returns coverage verdicts, channel first then tower, which keeps
// a channel's answers together.
func (s *Store) ListCoverage(ctx context.Context, f CoverageFilter) ([]Coverage, error) {
	// Fixed query, filters as parameters — see ListChannels for why.
	const query = `
		SELECT channel_id, tower_id, coverable, reason, num_backups,
		       last_backup_at, negotiated_sweep_fee_sat_kw, checked_at
		  FROM tower_channel_coverage
		 WHERE (? = 0 OR channel_id = ?)
		   AND (? = 0 OR tower_id = ?)
		   AND (? = 0 OR coverable = 0)
		 ORDER BY channel_id ASC, tower_id ASC`

	rows, err := s.db.QueryContext(ctx, query,
		f.ChannelID, f.ChannelID,
		f.TowerID, f.TowerID,
		boolToInt(f.UncoverableOnly))
	if err != nil {
		return nil, fmt.Errorf("listing coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Coverage
	for rows.Next() {
		var (
			c         Coverage
			coverable int64
			lastBk    sql.NullInt64
			feeRate   sql.NullInt64
		)
		if err := rows.Scan(&c.ChannelID, &c.TowerID, &coverable, &c.Reason,
			&c.NumBackups, &lastBk, &feeRate, &c.CheckedAt); err != nil {
			return nil, fmt.Errorf("reading a coverage row: %w", err)
		}
		c.Coverable = coverable == 1
		c.LastBackupAt = lastBk.Int64
		c.SweepFeeSatPerKW = optionalInt32From(feeRate)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing coverage: %w", err)
	}
	return out, nil
}
