-- Channels, the spends against them, and the deadlines those spends start.
--
-- Same conventions as 0001: times are unix seconds UTC; heights are integers;
-- hashes and txids are lowercase hex in the usual display order; amounts are
-- satoshis unless the column says msat. A `branch` is exactly 'sf' or 'sq'.
--
-- Migration 0001 records what the *chains* are doing. This one records what is
-- happening to the user's money on them.

-- The Lightning nodes Forktower reads from. Read-only: nothing here is ever
-- sent back to them.
CREATE TABLE ln_nodes (
  id           TEXT PRIMARY KEY,          -- node pubkey, lowercase hex
  impl         TEXT NOT NULL CHECK (impl IN ('lnd', 'cln')),
  alias        TEXT,
  last_seen_at INTEGER
);

-- One row per channel, keyed by its funding outpoint because that is the only
-- identifier that survives everything else changing.
CREATE TABLE channels (
  id                  INTEGER PRIMARY KEY,
  ln_node_id          TEXT NOT NULL REFERENCES ln_nodes(id),
  funding_txid        TEXT NOT NULL,
  funding_vout        INTEGER NOT NULL,
  -- scriptPubKey of the funding output. The Lightning source does not always
  -- supply it; the watcher fills it from the chain when it does not, because
  -- the watchset is built from scripts.
  funding_script_hex  TEXT,
  capacity_sat        INTEGER NOT NULL,
  chan_type           TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (chan_type IN ('legacy', 'static_remote',
                                             'anchors', 'taproot', 'unknown')),

  -- The two delays, and which is which matters more than anything else in this
  -- table. csv_delay_local is what *we* wait after our own force-close.
  -- csv_delay_remote is what the *peer* waits after theirs — which makes it the
  -- window we have to respond to a breach against us. Getting them the wrong way
  -- round produces a deadline that looks right and expires at the wrong time.
  csv_delay_local     INTEGER,
  csv_delay_remote    INTEGER,

  peer_pubkey         TEXT,
  -- Chosen by the counterparty, who is the adversary here. Clamped to printable
  -- characters and 32 bytes on ingest, and rendered as text and never as markup.
  peer_alias          TEXT,
  open_height         INTEGER,
  scid                TEXT,                -- short channel id, 'BLKxTXxOUT'

  sf_close_state      TEXT NOT NULL DEFAULT 'open'
                        CHECK (sf_close_state IN ('open', 'pending_close',
                                                  'coop_closed', 'force_closed',
                                                  'breach_closed')),
  sf_close_txid       TEXT,
  sf_close_height     INTEGER,

  -- Whether this channel is exposed on the chain the user's node does not
  -- follow. 'unknown' is a watching instruction, not a shrug: the watchset is
  -- never narrowed to 'relevant'.
  sq_relevance        TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (sq_relevance IN ('relevant', 'irrelevant', 'unknown')),
  sq_relevance_reason TEXT,

  updated_at          INTEGER NOT NULL,
  UNIQUE (funding_txid, funding_vout)
);

CREATE INDEX idx_channels_node ON channels(ln_node_id);
CREATE INDEX idx_channels_relevance ON channels(sq_relevance);

-- The most recent HTLC snapshot per channel, and only that: deleted and
-- re-inserted on every poll. In-flight HTLCs are a live picture, not a history,
-- and their deadlines can fall earlier than the commitment's.
CREATE TABLE htlc_snapshots (
  channel_id    INTEGER NOT NULL REFERENCES channels(id),
  taken_at      INTEGER NOT NULL,
  direction     TEXT NOT NULL CHECK (direction IN ('incoming', 'outgoing')),
  amount_msat   INTEGER NOT NULL,
  cltv_expiry   INTEGER NOT NULL,
  payment_hash  TEXT
);
CREATE INDEX idx_htlc_chan ON htlc_snapshots(channel_id);

-- Something spent an outpoint we were watching. Append-only: this is the record
-- of what happened, and doc 10 §4 forbids deleting from it.
CREATE TABLE spend_events (
  id             INTEGER PRIMARY KEY,
  branch         TEXT NOT NULL CHECK (branch IN ('sf', 'sq')),
  -- NULL for a second-order watch: an output of a commitment we already saw,
  -- which belongs to no channel of ours directly.
  channel_id     INTEGER REFERENCES channels(id),
  outpoint_txid  TEXT NOT NULL,
  outpoint_vout  INTEGER NOT NULL,
  spend_txid     TEXT NOT NULL,
  -- The whole transaction. Kept because the mirror needs to rebroadcast it
  -- later, and because a spend seen once on a chain nobody else is watching may
  -- not be fetchable again.
  spend_tx_hex   TEXT NOT NULL,
  block_hash     TEXT,                    -- NULL means seen in the mempool only
  block_height   INTEGER,
  shape          TEXT NOT NULL DEFAULT 'unknown'
                   CHECK (shape IN ('mutual_close', 'commitment_ours',
                                    'commitment_unknown', 'commitment_revoked',
                                    'justice', 'delayed_sweep', 'htlc_claim',
                                    'unknown')),
  status         TEXT NOT NULL CHECK (status IN ('mempool', 'confirmed',
                                                 'reorged_out')),
  first_seen_at  INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  -- The same spend seen twice is one row. Replaying a block must add nothing.
  UNIQUE (branch, outpoint_txid, outpoint_vout, spend_txid)
);

CREATE INDEX idx_spend_channel ON spend_events(channel_id);
CREATE INDEX idx_spend_status ON spend_events(branch, status);

-- Outputs of a confirmed commitment that now need watching in their own right:
-- the to_local output whose sweep we are racing, the HTLC outputs, the anchors.
CREATE TABLE watch_outpoints (
  branch                TEXT NOT NULL CHECK (branch IN ('sf', 'sq')),
  txid                  TEXT NOT NULL,
  vout                  INTEGER NOT NULL,
  script_hex            TEXT NOT NULL,
  source_spend_event_id INTEGER NOT NULL REFERENCES spend_events(id),
  role                  TEXT NOT NULL CHECK (role IN ('to_local', 'to_remote',
                                                      'htlc', 'anchor', 'unknown')),
  PRIMARY KEY (branch, txid, vout)
);

-- A clock running against the user. One per spend that started one.
CREATE TABLE deadlines (
  id               INTEGER PRIMARY KEY,
  spend_event_id   INTEGER NOT NULL REFERENCES spend_events(id),
  kind             TEXT NOT NULL CHECK (kind IN ('csv', 'htlc_incoming',
                                                 'htlc_outgoing')),
  deadline_height  INTEGER NOT NULL,      -- the height at which it is too late
  state            TEXT NOT NULL DEFAULT 'counting'
                     CHECK (state IN ('counting', 'resolved', 'expired')),
  escalation       INTEGER NOT NULL DEFAULT 0,  -- highest tier already alerted
  -- 1 when an input was unknown and a conservative floor was used instead.
  -- A deadline is never skipped for a missing input: an alarm that fires early
  -- is a bug report, one that never fires is a loss.
  assumed          INTEGER NOT NULL DEFAULT 0 CHECK (assumed IN (0, 1)),
  resolved_by_txid TEXT,
  updated_at       INTEGER NOT NULL,
  -- One deadline of each kind per spend. Recomputing must update, not multiply.
  UNIQUE (spend_event_id, kind)
);

CREATE INDEX idx_deadlines_state ON deadlines(state, deadline_height);
