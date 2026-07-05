-- MUST-FIX 3 (acked delivery): durable per-workspace inbox delivery cursor.
--
-- The inbox poller in the runtime drains activity_logs rows in seq order and
-- POSTs /workspaces/:id/activity/ack {acked_seq} after each batch. This table
-- is the durable, monotonic record of the highest seq a workspace has
-- confirmed handled. The retention prune (cmd/server/main.go) reads it to make
-- deletion age-AND-acked: an old row is only reclaimed once its consumer has
-- acked past it (with a hard time ceiling as the ultimate backstop), so a slow
-- or restarted poller can no longer lose un-drained inbox rows to the cleaner.
--
-- Shape:
--   * workspace_id   PK, FK → workspaces(id) ON DELETE CASCADE. One cursor row
--                    per workspace; deleting a workspace reclaims its cursor.
--   * last_acked_seq BIGINT NOT NULL DEFAULT 0. Monotonic; the ack handler
--                    only ever advances it via GREATEST(old, new), so a
--                    re-ordered / duplicate / stale ack is a safe no-op. It is
--                    compared against activity_logs.seq (the monotonic
--                    tiebreaker added in 20260604000000_activity_logs_seq).
--                    DEFAULT 0 means "nothing acked yet" — a fresh workspace
--                    with no cursor row is treated as last_acked_seq = 0 by the
--                    prune's COALESCE, so NONE of its rows are eligible for the
--                    acked soft-floor branch (only the hard ceiling can reclaim
--                    them). Strictly conservative.
--   * updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(), refreshed on each ack.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS so the boot-time runner (and the
-- CI migrate-replay step) can re-apply this safely.

CREATE TABLE IF NOT EXISTS inbox_delivery_state (
    workspace_id   UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    last_acked_seq BIGINT      NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
