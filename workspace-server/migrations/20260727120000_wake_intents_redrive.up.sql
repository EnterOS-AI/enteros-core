BEGIN;

-- RFC concierge wake-lifecycle (PR-F): the generation-loop RE-DRIVE bookkeeping.
--
-- The convergence loop (registry.Heartbeat → MarkWakeSettled) only SETTLES
-- already-delivered wake intents whose generation the runtime has observed. It
-- does NOT re-drive an intent that got STUCK: a decided-but-undelivered intent
-- (pending/dispatched — the emitter crashed between DecideWake and the delivery
-- marker) or a delivered-but-unsettled one (delivered, but the runtime never
-- echoed an observed_generation high enough to settle it — e.g. a runtime that
-- predates the versioned-heartbeat contract). Nothing re-emits those, and
-- nothing ever abandons them, so the generation loop stays half-open forever.
--
-- The re-drive owner (wake_redrive.go ReDriveStuckWakes) re-emits a stuck intent
-- through its EXISTING idempotency_key — the downstream dedup (has_greeted CAS
-- for the greeting, the a2a-queue active-row conflict for the queue-backed
-- wakes) keeps that exactly-once-safe, so a re-drive never produces a duplicate
-- user-visible wake. It MUST be bounded: an intent that will never converge (a
-- permanently-silent old runtime) must not be re-emitted forever. These two
-- columns are that bound.
--
--   redrive_attempts — how many times the re-drive has re-emitted this intent.
--                      Incremented BEFORE the re-emit fires (so the bound holds
--                      even if the re-emit crashes). Once it reaches the owner's
--                      cap the intent is marked 'dropped' (terminal) instead of
--                      re-emitted again — the never-re-emit-forever guarantee.
--   last_redriven_at — when the intent was last re-driven (or dropped). The
--                      owner throttles on it (a stuck intent is re-driven at most
--                      once per its min-interval) so a hot heartbeat path cannot
--                      hammer a single stuck intent every beat.
--
-- Both default to the "never re-driven" state (0 / NULL) so every existing and
-- new intent starts clean. IF NOT EXISTS keeps the DDL re-entrant under a runner
-- that re-applies ups. No backfill needed.
ALTER TABLE wake_intents
    ADD COLUMN IF NOT EXISTS redrive_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_redriven_at TIMESTAMPTZ;

-- The re-drive SELECT scans a workspace's NON-terminal intents (status IN
-- pending/dispatched/delivered). A partial index on (workspace_id) restricted to
-- those statuses keeps that scan cheap on a large ledger without indexing the
-- terminal (settled/dropped) rows that dominate a converged fleet.
CREATE INDEX IF NOT EXISTS wake_intents_stuck_scan_idx
    ON wake_intents (workspace_id)
    WHERE status IN ('pending', 'dispatched', 'delivered');

COMMIT;
