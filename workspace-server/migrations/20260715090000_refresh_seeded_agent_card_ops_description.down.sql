BEGIN;

-- Intentionally irreversible/no-op. The up migration changes only an exact
-- retired operator-host description, while fresh databases already receive the
-- current domain-based text from the original seed. Rewriting that current
-- text on down would mutate rows the up migration never touched. Restoring the
-- retired operational instruction would also be unsafe.

COMMIT;
