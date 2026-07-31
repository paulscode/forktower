-- The towers that answer a breach, and the transactions the mirror moved or
-- refused to move.
--
-- Same conventions as 0001 and 0002: times are unix seconds UTC; heights are
-- integers; hashes and txids are lowercase hex in the usual display order;
-- amounts are satoshis unless the column says otherwise. A `branch` is exactly
-- 'sf' or 'sq'.
--
-- 0002 records what is happening to the user's money. This one records what is
-- being done about it.

-- Every tower we know about, whether we started it or the user pointed us at
-- somebody else's.
--
-- Keyed on the pubkey rather than the URI, because the pubkey is the tower's
-- identity and the address it is reachable at is not: a hidden service can be
-- republished, a LAN address changes, and neither means it became a different
-- tower.
CREATE TABLE towers (
  id            INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL CHECK (kind IN ('lnd', 'teos')),
  pubkey        TEXT NOT NULL,
  uri           TEXT,
  -- 1 = a tower this installation runs and is responsible for. 0 = external,
  -- which changes what we may promise about it: we do not control an external
  -- tower's configuration and must not imply otherwise.
  managed       INTEGER NOT NULL DEFAULT 0 CHECK (managed IN (0, 1)),
  first_seen_at INTEGER NOT NULL,
  -- Last time it answered at all. NULL means it never has, which is a
  -- different fact from "answered once and has now stopped" and the readiness
  -- UI says so differently.
  last_ok_at    INTEGER,
  -- One vocabulary for both implementations: teos's own TowerStatus, plus
  -- 'unknown' for before we have asked. LND can never reach
  -- 'subscription_error' or 'misbehaving' — it has no subscriptions, and its
  -- client cannot prove a tower misbehaved. That asymmetry is real and is left
  -- visible rather than smoothed over.
  status        TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN (
                  'reachable', 'temporarily_unreachable', 'unreachable',
                  'subscription_error', 'misbehaving', 'unknown')),
  status_detail TEXT,
  -- JSON array of the blob types this tower accepts. LND only; NULL for teos,
  -- which never sees a channel type because Core Lightning builds the penalty
  -- transaction itself and hands over an opaque blob.
  blob_types    TEXT,
  -- teos only. A teos subscription expires — 4320 blocks, about thirty days, by
  -- default — and a split can outlast one registered just before it. LND
  -- sessions have no equivalent, so both are NULL for an LND tower.
  subscription_expiry_height   INTEGER,
  subscription_slots_remaining INTEGER,
  updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_towers_pubkey ON towers(pubkey);

-- Whether one tower can actually protect one channel.
--
-- Per channel, not per tower, because that is how the failure happens: a
-- channel whose commitment type the tower does not support is refused at
-- session creation while every other channel backs up normally and the tower
-- goes on reporting itself healthy. Nothing tells the user, which is why this
-- table exists rather than a single flag on the tower.
CREATE TABLE tower_channel_coverage (
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  tower_id   INTEGER NOT NULL REFERENCES towers(id),
  coverable  INTEGER NOT NULL DEFAULT 0 CHECK (coverable IN (0, 1)),
  -- Required, and required in both directions. "Not coverable" with no reason
  -- is an accusation without evidence, and "coverable" with no reason gives a
  -- reader nothing to check.
  reason     TEXT NOT NULL,
  num_backups    INTEGER NOT NULL DEFAULT 0,
  last_backup_at INTEGER,
  -- The sweep fee rate negotiated for the session covering this channel, in
  -- sat/kW because that is the unit the policy is actually expressed in;
  -- sat/vB is a display conversion. NULL until a session exists.
  --
  -- Worth recording because it is fixed when the session is negotiated and
  -- pre-signed into every justice transaction afterwards. Nobody can bump it —
  -- the tower holds no keys — so a rate agreed before the fork is the rate that
  -- will be paid during it.
  negotiated_sweep_fee_sat_kw INTEGER,
  checked_at INTEGER NOT NULL,
  UNIQUE (channel_id, tower_id)
);
CREATE INDEX idx_coverage_channel ON tower_channel_coverage(channel_id);

-- Every transaction the mirror considered, and what it decided.
--
-- Decisions rather than attempts, and the difference matters. The mirror is an
-- allowlist with default deny, so the transactions it declines are the larger
-- set and the ones whose absence a reader will want explained — "why was the
-- counterparty's commitment not mirrored?" is a question that has to be
-- answerable from the database, not from a log line that has since rotated
-- away. So 'denied' is a state here and not an absence.
CREATE TABLE mirror_decisions (
  id            INTEGER PRIMARY KEY,
  txid          TEXT NOT NULL,
  source_branch TEXT NOT NULL CHECK (source_branch IN ('sf', 'sq')),
  target_branch TEXT NOT NULL CHECK (target_branch IN ('sf', 'sq')),
  channel_id    INTEGER REFERENCES channels(id),
  -- What the M2 classifier made of it. Recorded as observed, because the policy
  -- decision below is only as good as this and a reader checking one needs the
  -- other.
  shape         TEXT NOT NULL,
  -- Which rule permitted it, or which rule refused it. Never empty.
  reason        TEXT NOT NULL,
  state         TEXT NOT NULL CHECK (state IN (
                  'denied', 'pending', 'accepted', 'rejected', 'abandoned')),
  attempts      INTEGER NOT NULL DEFAULT 0,
  first_seen_at INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_error    TEXT,
  UNIQUE (txid, target_branch)
);
CREATE INDEX idx_mirror_state ON mirror_decisions(state);
CREATE INDEX idx_mirror_channel ON mirror_decisions(channel_id);

-- The one setting in this schema that adds exposure rather than reducing it.
--
-- Mirroring a funding transaction creates money on the other branch that was
-- not there before, so doc 05 §2 makes it per-channel and opt-in. Default off,
-- and — like sq_relevance and sf_close_state — a registry poll must never write
-- it. It is a user's decision and nothing else may overwrite one.
ALTER TABLE channels ADD COLUMN mirror_funding_opt_in INTEGER NOT NULL DEFAULT 0
  CHECK (mirror_funding_opt_in IN (0, 1));
