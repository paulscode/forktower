-- Collapse the alerts that a per-height dedup key multiplied.
--
-- Two watcher alerts used to carry the block height in their identity:
-- `watcher_deep_reorg:<height>` and `watcher_stalled:<height>`. Both describe a
-- condition rather than a block — scanning has stopped — and both recurred at a
-- moving height, so each occurrence minted a new critical alert instead of
-- joining the one already there. One install accumulated 15,206 of them; another
-- eleven in ten minutes. The keys were made stable in 0.6.1 and 0.6.5
-- respectively, which stopped new ones, and left every existing one in place.
--
-- **This is not a cleanup of inconvenient history. It is the same deduplication
-- the fixed code performs, applied to rows written before it existed.** What
-- survives is what a correct daemon would have written: one entry per condition,
-- created when it first happened, last raised when it last happened. Nothing is
-- invented and no message is rewritten.
--
-- Scoped as narrowly as it can be. Only these two kinds, only where the key has
-- the superseded `<kind>:<digits>` shape, and matched with substr rather than
-- LIKE because `_` is a single-character wildcard in LIKE and both kinds contain
-- one. An alert raised by current code carries a bare key and cannot be touched
-- by any statement here.

-- Where the fixed code has already written the canonical bare-keyed row, the old
-- per-height rows are duplicates of it and nothing needs preserving from them.
DELETE FROM alerts
 WHERE kind IN ('watcher_deep_reorg', 'watcher_stalled')
   AND substr(dedup_key, 1, length(kind) + 1) = kind || ':'
   AND substr(dedup_key, length(kind) + 2) GLOB '[0-9]*'
   AND EXISTS (
         SELECT 1 FROM alerts AS canonical
          WHERE canonical.kind = alerts.kind
            AND canonical.dedup_key = alerts.kind
       );

-- Otherwise the oldest becomes that row. It keeps its own creation time, because
-- when the trouble started is the part worth keeping, and takes the group's most
-- recent raising, because that is when it last happened.
UPDATE alerts
   SET last_raised_at = (
         SELECT MAX(sibling.last_raised_at)
           FROM alerts AS sibling
          WHERE sibling.kind = alerts.kind
            AND substr(sibling.dedup_key, 1, length(sibling.kind) + 1) = sibling.kind || ':'
            AND substr(sibling.dedup_key, length(sibling.kind) + 2) GLOB '[0-9]*'
       ),
       dedup_key = kind
 WHERE kind IN ('watcher_deep_reorg', 'watcher_stalled')
   AND substr(dedup_key, 1, length(kind) + 1) = kind || ':'
   AND substr(dedup_key, length(kind) + 2) GLOB '[0-9]*'
   AND id = (
         SELECT MIN(oldest.id)
           FROM alerts AS oldest
          WHERE oldest.kind = alerts.kind
            AND substr(oldest.dedup_key, 1, length(oldest.kind) + 1) = oldest.kind || ':'
            AND substr(oldest.dedup_key, length(oldest.kind) + 2) GLOB '[0-9]*'
       );

-- The rest were never separate conditions.
DELETE FROM alerts
 WHERE kind IN ('watcher_deep_reorg', 'watcher_stalled')
   AND substr(dedup_key, 1, length(kind) + 1) = kind || ':'
   AND substr(dedup_key, length(kind) + 2) GLOB '[0-9]*';
