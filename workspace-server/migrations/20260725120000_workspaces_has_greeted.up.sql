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
-- new workspace created after this migration greets exactly once. IF NOT EXISTS
-- keeps the up idempotent under a runner that re-applies forward migrations.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS has_greeted BOOLEAN NOT NULL DEFAULT false;

-- Backfill: every workspace that ALREADY exists at migration time has passed
-- its first-boot moment (the greeting feature shipped earlier). Mark them
-- greeted so their next restart is arbitrated as a REAL restart (fires
-- restart-context) and they never re-greet. Without this, the default false
-- would make every existing box look like a fresh boot on its next restart —
-- a wave of spurious re-greets AND skipped restart-context. New rows created
-- after this statement keep the column default (false) and greet once.
UPDATE workspaces SET has_greeted = true;

COMMIT;
