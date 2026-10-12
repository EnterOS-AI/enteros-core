BEGIN;

-- RFC #4402: is_busy is the self-healing successor to active_tasks — a boolean
-- "is the agent in a turn right now", fed by the heartbeat and (in B3) read
-- through a freshness TTL so it can never strand the way the active_tasks
-- counter does. B1 only ADDS the column; the heartbeat handler dual-writes it
-- (from the runtime's own is_busy when sent, else derived from active_tasks>0).
-- No consumer reads it until B3. NOT NULL DEFAULT false so every existing row
-- and the read-side get a guaranteed boolean. IF NOT EXISTS keeps the migration
-- idempotent under a runner that re-applies ups.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS is_busy BOOLEAN NOT NULL DEFAULT false;

COMMIT;
