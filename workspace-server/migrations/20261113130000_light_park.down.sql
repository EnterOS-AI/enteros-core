-- Rollback of 20261113130000_light_park. Run by an operator only (the runner
-- never applies .down.sql). Revoke any light_pending_token_id first.
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_pending_token_id;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_park_watermark;
ALTER TABLE workspaces DROP COLUMN IF EXISTS light_park_trigger;
