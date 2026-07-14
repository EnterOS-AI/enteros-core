-- Revert to the pre-fix (narrower) partial indexes. Both originally omitted `stuck`
-- from the in-flight predicate, which is the defect the .up.sql corrects — so this
-- down-migration deliberately restores a KNOWN-BAD state, exactly as a down-migration
-- should: it undoes the change, it does not improve on it.
--
-- Consequence of running it: the sweeper's in-flight SELECT and mail_summary's
-- awaiting-reply count both query `status IN (queued,dispatched,in_progress,stuck)`,
-- which is WIDER than these predicates — so neither index can serve them and both
-- fall back to a Seq Scan on `delegations`.

DROP INDEX IF EXISTS idx_delegations_inflight_heartbeat;
CREATE INDEX IF NOT EXISTS idx_delegations_inflight_heartbeat
    ON delegations (last_heartbeat NULLS FIRST)
    WHERE status IN ('queued','dispatched','in_progress');

DROP INDEX IF EXISTS idx_delegations_caller_inflight;
CREATE INDEX IF NOT EXISTS idx_delegations_caller_inflight
    ON delegations (caller_id, created_at)
    WHERE status IN ('queued','dispatched','in_progress');
