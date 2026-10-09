BEGIN;

-- RFC concierge wake-lifecycle (PR-B): wake_intents is the DURABLE LEDGER of
-- every proactive WAKE the platform decides to fire at a workspace — the
-- first-boot greeting, restart-context, idle/stall/nudge prompts, and the
-- generic lifecycle wake. It is the cross-wake, exactly-once record that the
-- desired-state owner (wake_lifecycle.go) writes and the heartbeat convergence
-- loop settles.
--
-- SCOPE: WAKE intents ONLY. This is NOT the plugin/config reconcile ledger and
-- MUST NOT be conflated with it — it records "the platform wants the box to
-- proactively speak", nothing about desired plugin/config state.
--
-- One row per minted wake:
--   kind             — the wake taxonomy (first-boot-greet, restart-context,
--                      idle, stall, nudge, lifecycle). Free text so a new kind
--                      needs no migration; the Go WakeKind consts are the SSOT.
--   idempotency_key  — the cross-wake dedup key. UNIQUE per (workspace, key):
--                      re-deciding the SAME wake (a retry, a duplicate trigger)
--                      collides here and does NOT mint a second intent or bump
--                      the generation. Derived per-kind by DecideWake.
--   generation       — the desired_generation this intent was minted AT (the
--                      value RETURNING'd by the atomic bump in the same tx). The
--                      convergence loop settles intents whose generation <= the
--                      runtime's observed_generation.
--   status           — pending  : minted, not yet dispatched to the runtime
--                      dispatched: handed to the emitter/queue
--                      delivered : the wake reached the user (commit-on-delivery)
--                      settled   : the runtime converged past this generation
--                      dropped   : abandoned (superseded / terminal failure)
--   settled_at       — stamped when the row flips to settled.
--
-- ON DELETE CASCADE so a removed workspace takes its wake ledger with it.
CREATE TABLE IF NOT EXISTS wake_intents (
    id              BIGSERIAL PRIMARY KEY,
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    generation      BIGINT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','dispatched','delivered','settled','dropped')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at      TIMESTAMPTZ
);

-- Cross-wake exactly-once: a duplicate DecideWake for the same (workspace, key)
-- conflicts here so the owner can treat it as a no-op (no second intent, no
-- generation bump). This is the atomic dedup that an in-memory map could not
-- provide across two distinct wake goroutines.
CREATE UNIQUE INDEX IF NOT EXISTS wake_intents_ws_key_uk
    ON wake_intents (workspace_id, idempotency_key);

COMMIT;
