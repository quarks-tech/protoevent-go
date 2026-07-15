-- The retention sweep deletes rows below MIN(offset) AND older than the
-- NOW(6)-relative cutoff. The seq predicate matches most of the table in
-- steady state, so without an index on create_time the optimizer falls back
-- to a full table scan on every terminal sweep pass (measured: 83ms at 200k
-- rows, growing linearly) — and when nothing is deletable, EVERY pass is the
-- terminal pass. With this index the cutoff bounds the scan: in steady state
-- only pinned or deletable rows are older than the cutoff, making idle
-- sweeps near-O(1).
ALTER TABLE outbox_messages ADD INDEX idx_outbox_create_time (create_time);
