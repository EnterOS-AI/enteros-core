-- internal#691 / billing-mode removal — Release 2 (contract) of the
-- expand→contract migration. Release 1 (2026-06-30) made the code stop
-- reading this column; this drops it.
--
-- The per-workspace `llm_billing_mode` override is fully retired. NO
-- consumer reads it anymore: platform-vs-BYOK is decided solely by the
-- resolved provider (providers.Manifest.DeriveProvider → the forwarded
-- MOLECULE_RESOLVED_PROVIDER signal), and MOLECULE_LLM_BILLING_MODE is no
-- longer emitted into the container env (guarded by no_billing_mode_env_test.go
-- and resolved_provider_env_test.go).
--
-- Dropping the column also drops its inline (column-scoped) CHECK constraint
-- automatically. Idempotent: DROP COLUMN IF EXISTS. The workspace-server
-- runner records applied files in schema_migrations, so this runs once.
ALTER TABLE workspaces
  DROP COLUMN IF EXISTS llm_billing_mode;
