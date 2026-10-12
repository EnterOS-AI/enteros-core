BEGIN;

-- Reverse of the desired/observed generation column add. Paired with a code
-- revert: dropping these columns removes the wake convergence signal, and the
-- desired-state owner (wake_lifecycle.go) must be rolled back alongside so no
-- reader references the missing columns.
ALTER TABLE workspaces
    DROP COLUMN IF EXISTS observed_generation,
    DROP COLUMN IF EXISTS desired_generation;

COMMIT;
