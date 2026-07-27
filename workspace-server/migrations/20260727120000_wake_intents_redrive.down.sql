BEGIN;

-- Reverse of the wake_intents re-drive bookkeeping add (PR-F). Paired with a
-- code revert of wake_redrive.go: without the columns the owner's re-drive
-- SELECT/UPDATE would error, so the Go side must roll back alongside. Dropping
-- the columns discards the per-intent attempt counter — a re-applied up starts
-- every intent clean again (0 / NULL), which is correct.
DROP INDEX IF EXISTS wake_intents_stuck_scan_idx;

ALTER TABLE wake_intents
    DROP COLUMN IF EXISTS redrive_attempts,
    DROP COLUMN IF EXISTS last_redriven_at;

COMMIT;
