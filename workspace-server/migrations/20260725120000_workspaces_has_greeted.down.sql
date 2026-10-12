BEGIN;

-- Reverse of the has_greeted column add. Safe: dropping it only removes the
-- boot marker; the greet-once gate and restart-context arbitration fall back to
-- their prior (derived-query / unconditional-fire) behavior when the column is
-- gone only if the code is also rolled back — this down is paired with a code
-- revert.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS has_greeted;

COMMIT;
