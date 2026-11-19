-- Rollback of 20261113120000_light. Run by an operator only (the runner never
-- applies .down.sql). Drops the Light ledger, the rendered schedules, the queued
-- actions and every Light column; activity_logs keeps its rows, minus run_id.
-- The control plane's Light objects (CronJobs, Jobs, Secret) are NOT touched:
-- disable Light on every agent (POST /workspaces/:id/light/disable) first.
DROP INDEX IF EXISTS idx_activity_logs_ws_run;
ALTER TABLE activity_logs DROP COLUMN IF EXISTS run_id;

DROP TABLE IF EXISTS light_pending_actions;
DROP TABLE IF EXISTS light_schedules;
DROP TABLE IF EXISTS light_runs;

DROP INDEX IF EXISTS idx_workspaces_light_state;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_continuous_before;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_last_error_at;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_last_error;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_retired_token_id;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_run_token_id;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_opened_by;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_state_at;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_state;
