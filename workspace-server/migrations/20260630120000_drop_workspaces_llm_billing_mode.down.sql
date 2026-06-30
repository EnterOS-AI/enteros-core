-- Reverse of 20260630120000: re-add the per-workspace override column.
--
-- NULL = inherit the (now-removed) org default; the original resolver
-- contract was workspaces.llm_billing_mode ?? org_default ?? 'platform_managed'.
--
-- Manual-only — the runner never executes .down.sql (forward-only). A
-- genuine revert ALSO needs the Release-1 code revert: no live consumer
-- reads this column anymore.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS llm_billing_mode TEXT
  CHECK (llm_billing_mode IS NULL OR llm_billing_mode IN ('platform_managed', 'byok', 'disabled'));
