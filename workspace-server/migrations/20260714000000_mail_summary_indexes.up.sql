-- Mail-summary read API indexes (task #219 phase-2 D5, PR feat/mail-summary-api).
--
-- The idle digest polls GET /workspaces/:id/mail/summary once per idle tick
-- per workspace, fleet-wide — both aggregates must be index-served or the
-- endpoint rides the hottest table's history:
--
--   1. received/replies unread (acked_seq mode):
--        WHERE workspace_id=$1 AND activity_type='a2a_receive' AND seq > $2
--      Neither idx_activity_ws_type_time (created_at leg) nor
--      idx_activity_ws_created_seq (created_at before seq) serves the seq
--      range — the planner heap-filters seq over the workspace's ENTIRE
--      a2a_receive history. Partial index keyed exactly on the predicate.
--
--   2. sent-awaiting-reply:
--        WHERE caller_id=$1 AND status IN ('queued','dispatched','in_progress')
--      idx_delegations_caller_created has no status leg (full history scan).
--      Same partial-index pattern 049's sweeper index already uses.
--
-- Idempotent: IF NOT EXISTS — the boot-time runner re-applies every *.up.sql.
CREATE INDEX IF NOT EXISTS idx_activity_a2a_receive_ws_seq
    ON activity_logs (workspace_id, seq)
    WHERE activity_type = 'a2a_receive';

CREATE INDEX IF NOT EXISTS idx_delegations_caller_inflight
    ON delegations (caller_id, created_at)
    WHERE status IN ('queued','dispatched','in_progress');
