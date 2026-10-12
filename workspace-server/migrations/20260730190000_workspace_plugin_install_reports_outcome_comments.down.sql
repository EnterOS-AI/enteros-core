-- migration: 20260730190000_workspace_plugin_install_reports_outcome_comments.down.sql
--
-- Drops the column comments set by the up migration, returning both columns to
-- the uncommented state 20260730060000 left them in — that migration documents
-- itself in SQL `--` comments and never issued a COMMENT ON, so "no comment" is
-- the correct thing to revert TO. Setting them to the old, wrong text instead
-- would re-assert an invariant the runtime does not hold.
--
-- No data or schema is touched, in either direction.

COMMENT ON COLUMN workspace_plugin_install_reports.failed IS NULL;
COMMENT ON COLUMN workspace_plugin_install_reports.swapped IS NULL;
