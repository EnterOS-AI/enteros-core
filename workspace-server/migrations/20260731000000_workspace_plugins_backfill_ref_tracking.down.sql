-- Revert the ref: backfill (core#4977).
--
-- Every 'ref:%' row was 'none' before the up-migration: the form did not exist
-- in any prior release, so nothing could have written it. Collapsing them back
-- to 'none' therefore restores the exact pre-migration state, and returns the
-- drift sweeper to selecting zero branch-pinned rows (the pre-fix behavior).
--
-- Note this deliberately also reverts rows written by the NEW install path
-- after the deploy. That is correct for a rollback: the Go code being rolled
-- back to cannot interpret 'ref:' values, and resolveLatestSHA in the old
-- build would hand the forge a literal '#ref:main' and fail every resolve.
-- Leaving them behind would be worse than reverting them.
--
-- tag:/sha: rows are untouched — the up-migration only ever set those to the
-- value they already implied, and they predate this change.

UPDATE workspace_plugins
   SET tracked_ref = 'none',
       updated_at = NOW()
 WHERE tracked_ref LIKE 'ref:%';
