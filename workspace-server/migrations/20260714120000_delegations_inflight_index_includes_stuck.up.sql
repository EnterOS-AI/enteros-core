-- Both delegations partial indexes hand-typed the in-flight status list and so
-- OMITTED `stuck`. A partial index cannot serve a query whose predicate is WIDER
-- than the index's — so the moment DELEGATION_LEDGER_WRITE fills the table, the
-- two hottest queries in the subsystem fall back to a Seq Scan:
--
--   the sweeper       — runs every 5 minutes, fleet-wide
--   mail_summary      — runs on every idle tick, per workspace
--
-- Verified on a 200k-row table: `Bitmap Index Scan on
-- idx_delegations_inflight_heartbeat` becomes `Seq Scan`, and `Index Scan using
-- idx_delegations_caller_inflight` becomes `Seq Scan + Sort`.
--
-- This is the SAME hand-typed status list that caused #4314, in the one place the
-- Go-side SSOT (DelegationInFlightStates) cannot reach: SQL. It is pinned instead
-- by TestMigrationIndexPredicatesMatchTheVocabulary, which fails if this predicate
-- and the Go vocabulary ever diverge again.
--
-- Why a NEW migration rather than editing 049/20260714000000 in place: the runner
-- records applied filenames in schema_migrations, so an edited file never re-runs
-- on a deployed DB. And both originals use CREATE INDEX IF NOT EXISTS, so even a
-- re-run would silently keep the OLD, narrower index. The index must be dropped.
--
-- Why not CONCURRENTLY: the runner executes each file as a single Exec with no
-- explicit transaction, and a multi-statement Exec runs in an implicit transaction
-- — where CREATE INDEX CONCURRENTLY is illegal. A plain build is safe here
-- precisely because this ships BEFORE the flag: `delegations` is EMPTY in
-- production (the ledger has never been switched on) and tiny in staging, so the
-- ACCESS EXCLUSIVE lock is on ~zero rows. Doing this after the flip would be a
-- different decision.

DROP INDEX IF EXISTS idx_delegations_inflight_heartbeat;
CREATE INDEX IF NOT EXISTS idx_delegations_inflight_heartbeat
    ON delegations (last_heartbeat NULLS FIRST)
    WHERE status IN ('queued','dispatched','in_progress','stuck');

DROP INDEX IF EXISTS idx_delegations_caller_inflight;
CREATE INDEX IF NOT EXISTS idx_delegations_caller_inflight
    ON delegations (caller_id, created_at)
    WHERE status IN ('queued','dispatched','in_progress','stuck');
