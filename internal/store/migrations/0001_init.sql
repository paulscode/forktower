-- Initial schema.
--
-- Conventions throughout: times are unix seconds UTC; heights are integers;
-- hashes and txids are lowercase hex in the usual display order; amounts are
-- satoshis. A `branch` is exactly 'sf' or 'sq'.

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Exactly one row. The daemon's authoritative answer to "have the chains
-- separated, and if so where".
CREATE TABLE split_state (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  state       TEXT    NOT NULL DEFAULT 'UNARMED',
  fork_height INTEGER,
  fork_hash   TEXT,
  detected_at INTEGER,
  updated_at  INTEGER NOT NULL
);

INSERT INTO split_state (id, state, updated_at)
VALUES (1, 'UNARMED', CAST(strftime('%s', 'now') AS INTEGER));

-- Rolling per-branch telemetry. Pruned to a bounded window: unlike the audit
-- trail, this is a cache of recent tips, and losing old rows costs nothing.
CREATE TABLE branch_blocks (
  branch      TEXT    NOT NULL,
  height      INTEGER NOT NULL,
  hash        TEXT    NOT NULL,
  prev_hash   TEXT    NOT NULL,
  block_time  INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (branch, hash)
);
CREATE INDEX idx_branch_blocks_height ON branch_blocks (branch, height);

-- Alerts are deduplicated by dedup_key: re-raising the same condition bumps
-- last_raised_at rather than creating a second row, so an escalating situation
-- does not bury the user in near-identical notifications.
CREATE TABLE alerts (
  id             INTEGER PRIMARY KEY,
  tier           TEXT    NOT NULL,
  kind           TEXT    NOT NULL,
  dedup_key      TEXT    NOT NULL UNIQUE,
  subject        TEXT,
  message        TEXT    NOT NULL,
  created_at     INTEGER NOT NULL,
  last_raised_at INTEGER NOT NULL,
  acked_at       INTEGER
);
CREATE INDEX idx_alerts_unacked ON alerts (acked_at, id);

-- One row per delivery attempt, successful or not. A transport that has quietly
-- stopped working is the classic way an alarm becomes decorative, so the
-- attempts are recorded rather than inferred.
CREATE TABLE alert_deliveries (
  id           INTEGER PRIMARY KEY,
  alert_id     INTEGER NOT NULL REFERENCES alerts (id),
  transport    TEXT    NOT NULL,
  attempted_at INTEGER NOT NULL,
  ok           INTEGER NOT NULL,
  error        TEXT
);
CREATE INDEX idx_alert_deliveries_alert ON alert_deliveries (alert_id, id);

-- Append-only record of everything the engines saw and did.
CREATE TABLE timeline (
  id      INTEGER PRIMARY KEY,
  at      INTEGER NOT NULL,
  kind    TEXT    NOT NULL,
  summary TEXT    NOT NULL,
  data    TEXT
);
CREATE INDEX idx_timeline_at ON timeline (at, id);
