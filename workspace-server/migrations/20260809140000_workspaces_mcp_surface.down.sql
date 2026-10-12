-- core#5137 rollback. Drops the corroboration record only; loaded_mcp_tools
-- (the runtime's raw claim) is untouched by both directions of this migration.
DROP INDEX IF EXISTS idx_activity_logs_ws_tooltrace_time;
ALTER TABLE workspaces DROP COLUMN IF EXISTS mcp_surface;
