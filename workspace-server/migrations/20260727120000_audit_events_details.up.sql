-- 20260727120000_audit_events_details.up.sql
--
-- Adds the `details` column to audit_events so a lifecycle event can name its
-- SUBJECT (which workspace was deleted, which token id was revoked, the caller
-- IP, …). Without it the table could record that *something* happened but not
-- *to what* — the exact gap that left a 2026-07-23 workspace creation in a
-- client tenant with no attributable record.
--
-- Why TEXT and not JSONB
-- ----------------------
-- `details` participates in the HMAC canonical form (see computeAuditHMAC in
-- internal/handlers/audit.go). JSONB is a normalising type: Postgres reorders
-- keys and strips whitespace on store, so the bytes read back are NOT the bytes
-- signed, and every row would verify as tampered. TEXT round-trips exactly.
-- The writer always stores compact, key-sorted JSON (Go's json.Marshal over a
-- map), so the column is still machine-parseable with `details::jsonb`.
--
-- Backward compatibility: the canonical form OMITS the "details" key entirely
-- when the column is NULL, so any row written before this migration hashes
-- exactly as it did before and keeps verifying.

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS details TEXT;

-- Lifecycle events are queried by operation ("workspace.create",
-- "auth_token.revoke", …) far more often than by agent; the pre-existing
-- indexes cover agent_id/session_id/workspace_id/timestamp only.
CREATE INDEX IF NOT EXISTS idx_audit_events_operation ON audit_events (operation);
