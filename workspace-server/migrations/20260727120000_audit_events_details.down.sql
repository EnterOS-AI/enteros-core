-- 20260727120000_audit_events_details.down.sql
DROP INDEX IF EXISTS idx_audit_events_operation;
ALTER TABLE audit_events DROP COLUMN IF EXISTS details;
