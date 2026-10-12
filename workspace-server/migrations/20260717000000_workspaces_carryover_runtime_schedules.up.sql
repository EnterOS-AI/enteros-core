BEGIN;

-- P4b (scheduler-as-trigger-plugin RFC, core#4435): the volume-side org-re-import
-- schedule-inheritance buffer.
--
-- When an org agent is re-imported/re-added onto a FRESH data volume the
-- successor mints a NEW workspace id and a NEW volume, so the predecessor's
-- runtime-authored schedule grid — which for a volume-native workspace lives on
-- the predecessor's persisted volume, owned by the trigger daemon, NOT in the
-- core workspace_schedules table — would be abandoned. The predecessor's
-- /internal/schedules runtime API dies the instant its container stops (while the
-- volume persists, CascadeDelete erase=false), so the grid CANNOT be read at
-- restore time. It is therefore CAPTURED at teardown (container still up) into
-- this column on the removed predecessor row, and RESTORED onto the successor
-- once it comes online and advertises the scheduler capability; the column is
-- cleared back to NULL once restored (one-shot).
--
-- ADDITIVE + DARK: nothing reads or writes this column until the capture/restore
-- code ships, and the legacy workspace_schedules table + its DB-world
-- re-point migration path (org_import.migrateRuntimeSchedulesFromRemovedPredecessor)
-- stay in place until P4b drops them. NULL = nothing captured. JSONB holds the
-- filtered array of source='runtime' grid entries. IF NOT EXISTS keeps the
-- migration idempotent under a runner that re-applies ups.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS carryover_runtime_schedules JSONB;

COMMIT;
