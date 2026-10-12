BEGIN;

-- Reverse of the B1 column add. Safe because no B1 code path reads is_busy
-- (consumers cut over in B3); dropping it only removes the dual-write target.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS is_busy;

COMMIT;
