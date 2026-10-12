BEGIN;

-- Reverse of the P4b carryover-buffer column add (core#4435). Safe: the column
-- is a transient one-shot buffer — dropping it only discards any captured-but-
-- not-yet-restored grids, which simply re-capture on the next re-import. No
-- consumer keeps a long-lived read of it.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS carryover_runtime_schedules;

COMMIT;
