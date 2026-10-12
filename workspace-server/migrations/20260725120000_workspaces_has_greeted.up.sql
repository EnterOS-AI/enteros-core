BEGIN;

-- RFC concierge rule 2: has_greeted is the single authoritative "has this
-- workspace been greeted/booted" marker (SSOT). It REPLACES the derived
-- activity_logs user-chat query that first_boot_greeting.go used to infer
-- "already greeted" — that predicate answered "has the USER chatted", not
-- "has the box greeted", which double-greeted a greeted-but-silent box and let
-- restart-context fire on a genuine first boot.
--
-- Read by BOTH paths: the first-boot greet-once gate (skip when set) AND
-- restart-context arbitration (fire ONLY when set = a real restart). Written
-- true ONLY on CONFIRMED greeting delivery (commit-on-delivery), never at
-- decision time.
--
-- NOT NULL DEFAULT false so every read gets a guaranteed boolean and a brand-
-- new workspace created after this migration greets exactly once. The
-- ADD COLUMN IF NOT EXISTS only makes the DDL half re-entrant; it does NOT make
-- the whole migration idempotent — the unconditional backfill below would
-- re-clobber legitimately-false rows if this file were ever re-applied. That is
-- only theoretical: golang-migrate records each version and never re-runs a
-- forward migration, so this runs exactly once. Do not rely on re-apply safety.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS has_greeted BOOLEAN NOT NULL DEFAULT false;

-- Backfill (one-shot): every workspace that ALREADY exists at migration time has
-- passed its first-boot moment (the greeting feature shipped earlier). Mark them
-- greeted so their next restart is arbitrated as a REAL restart (fires
-- restart-context) and they never re-greet. Without this, the default false
-- would make every existing box look like a fresh boot on its next restart —
-- a wave of spurious re-greets AND skipped restart-context. New rows created
-- after this statement keep the column default (false) and greet once. This
-- UPDATE is intentionally unconditional (correct for a single forward run); it
-- is NOT re-apply-safe — see the note above.
UPDATE workspaces SET has_greeted = true;

COMMIT;
