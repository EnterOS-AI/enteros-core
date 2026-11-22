-- molecule-core#5193: what a plain delete keeps, and for how long.
--
-- A delete without erase_data / purge stops the workspace keep-state and marks
-- the row removed. On the control plane's Kubernetes substrate that leaves the
-- workspace's identity claim, config seed Secret and Service behind, and the
-- control plane erases them only through a teardown that names the stopped
-- instance. instance_id is cleared once the compute is stopped, so the id moves
-- here, with the time it was kept. The kept-state reaper
-- (internal/registry/cp_kept_state_reaper.go) erases the state once it has
-- been kept for the retention (24 h) and clears both columns.
--
-- Additive and idempotent: nullable columns, no default, no backfill (rows
-- removed before this migration have no stopped id left to record).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS kept_state_instance_id TEXT;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS kept_state_since TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_workspaces_kept_state_since
    ON workspaces (kept_state_since)
    WHERE kept_state_instance_id IS NOT NULL;
