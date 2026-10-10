BEGIN;

-- Reverse of the wake_intents ledger add. Paired with a code revert: dropping
-- the table removes the durable wake dedup/convergence record, so the
-- desired-state owner (wake_lifecycle.go) must be rolled back alongside. The
-- unique index is dropped with the table.
DROP TABLE IF EXISTS wake_intents;

COMMIT;
