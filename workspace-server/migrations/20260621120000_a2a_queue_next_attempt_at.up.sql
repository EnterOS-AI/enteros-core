-- #3127 (Researcher follow-up): backoff for transient gateway-origin
-- queue retries. Without a per-row "not before" gate, DrainQueueForWorkspace
-- with capacity>1 can re-dispatch the same item inside the same call
-- immediately after MarkQueueItemTransientRetry (because the row's
-- status='queued' AND the for-loop in DrainQueueForWorkspace iterates up
-- to capacity times). MarkQueueItemTransientRetry sets the new column to
-- now() + 5s; DequeueNext's WHERE clause skips rows whose next_attempt_at
-- is still in the future. This breaks the tight retry loop without
-- requiring a schema-foreign "stop draining" branch.
--
-- Column is nullable: NULL = no backoff constraint (default state for
-- rows that have never been transient-retried, and for the legacy
-- MarkQueueItemFailed path that doesn't touch this column).
--
-- Partial index keeps the hot dispatch query tiny — the partial WHERE
-- clause is the same one DequeueNext uses (status='queued' AND no
-- future next_attempt_at), so the index serves both the equality and
-- the inequality side of the planner.

ALTER TABLE a2a_queue ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_a2a_queue_next_attempt_at
    ON a2a_queue (workspace_id, priority DESC, enqueued_at ASC)
    WHERE status = 'queued'
      AND next_attempt_at IS NOT NULL
      AND next_attempt_at > now();
