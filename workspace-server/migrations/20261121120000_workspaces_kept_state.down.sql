-- Rollback of 20261121120000_workspaces_kept_state. Run by an operator only
-- (the runner never applies .down.sql). Dropping the columns forgets which
-- removed workspaces still have state to erase: list them first
-- (SELECT id, kept_state_instance_id FROM workspaces WHERE kept_state_instance_id IS NOT NULL).
DROP INDEX IF EXISTS idx_workspaces_kept_state_since;
ALTER TABLE workspaces DROP COLUMN IF EXISTS kept_state_since;
ALTER TABLE workspaces DROP COLUMN IF EXISTS kept_state_instance_id;
