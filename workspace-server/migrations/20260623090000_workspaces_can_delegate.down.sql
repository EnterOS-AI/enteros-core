-- core#2127: drop the can_delegate column.
--
-- Note: any workspace that was PATCHed to can_delegate=FALSE before the
-- rollback will silently regain delegation on the downgrade (no audit row
-- preserved). The migration is reversible for clean-up; the runtime fallback
-- (any handler that still reads can_delegate) MUST be tolerant of the column
-- being absent — see workspace_abilities.go / mcp_tools.go.

ALTER TABLE workspaces
    DROP COLUMN IF EXISTS can_delegate;
