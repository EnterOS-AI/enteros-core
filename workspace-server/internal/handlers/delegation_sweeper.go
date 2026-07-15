package handlers

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// delegation_sweeper.go — RFC #2829 PR-3: stuck-task sweeper.
//
// What it does
// ------------
// Periodically scans the `delegations` table (PR-1 schema) for in-flight
// rows that have either:
//
//   1. Blown past their `deadline` — agent claims to still be working but
//      the hard ceiling fired. Mark `failed` with error_detail = "deadline
//      exceeded".
//   2. Stopped heartbeating for >stuckThreshold while still claiming
//      in_progress. Mark `stuck` with error_detail = "no heartbeat for Ns".
//
// Why both rules
// --------------
// Deadline catches forever-heartbeating agents that never make progress
// (a wedged agent looping on a heartbeat call inside its main work loop
// looks "alive" by liveness signals but is not actually advancing).
// Heartbeat-staleness catches agents that crash or get OOM-killed
// without graceful shutdown — no terminal status update fires, but the
// heartbeat stops cold.
//
// Order matters: deadline check fires first because deadline → failed
// is a stronger statement than deadline → stuck. A stuck row can be
// retried by the operator; a failed row says "give up, retry was
// already exhausted or not viable."
//
// Frequency
// ---------
// 5min default cadence. Faster than that wastes DB round-trips for the
// hot index; slower means a stuck task isn't caught until ~5min after
// the heartbeat stops. Operators can override via DELEGATION_SWEEPER_INTERVAL_S.
//
// Threshold
// ---------
// Default 10× the runtime's heartbeat interval (≈100s for hermes that
// beats every 10s during stream output). 10× is the heuristic from the
// RFC #2829 design discussion: it tolerates legitimate slow LLM
// responses (a single completion can stall a heartbeat for 30-60s) while
// still catching real wedges within ~2 minutes. Operators override via
// DELEGATION_STUCK_THRESHOLD_S.
//
// Safety
// ------
// All transitions go through DelegationLedger.SetStatus so the
// terminal-state forward-only protection applies — a delegation that
// just transitioned to completed concurrently with the sweep won't be
// flipped back to failed/stuck. The ledger's same-status replay no-op
// also makes re-running the sweep idempotent.

const (
	defaultSweeperInterval = 5 * time.Minute

	// 10min = 60× the typical 10s hermes heartbeat. Tightens to ~10×
	// once the user community settles on a tighter heartbeat cadence;
	// today's mix of runtimes (hermes 10s, claude-code 30-60s, langchain
	// minute-scale) needs the looser threshold to avoid false positives.
	defaultStuckThreshold = 10 * time.Minute
)

// DelegationSweeper runs the periodic sweep. Construct via
// NewDelegationSweeper, then Start(ctx) in main.go to begin ticking.
type DelegationSweeper struct {
	db        *sql.DB
	ledger    *DelegationLedger
	interval  time.Duration
	threshold time.Duration
	// startedAt gates the stuck arm for one threshold after boot. The staleness
	// signal is workspaces.last_heartbeat_at — written by the workspaces TO THIS
	// SERVER — so our OWN downtime makes every callee look stale at once. Without
	// this, a workspace-server outage longer than the threshold would, on the
	// first sweep after restart, mark the entire in-flight set stuck.
	startedAt time.Time
}

// NewDelegationSweeper builds a sweeper bound to the package db.DB
// (production wiring) or a test handle. Reads optional env overrides
// at construction time so a long-running process picks them up via
// restart, not mid-flight.
func NewDelegationSweeper(handle *sql.DB, ledger *DelegationLedger) *DelegationSweeper {
	if handle == nil {
		handle = db.DB
	}
	if ledger == nil {
		ledger = NewDelegationLedger(handle)
	}
	return &DelegationSweeper{
		db:        handle,
		ledger:    ledger,
		interval:  envDuration("DELEGATION_SWEEPER_INTERVAL_S", defaultSweeperInterval),
		threshold: envDuration("DELEGATION_STUCK_THRESHOLD_S", defaultStuckThreshold),
		startedAt: time.Now(),
	}
}

// envDuration parses an integer-seconds env var into a Duration. Falls
// back to def on missing/invalid input — never fails fast on misconfig
// (a typo'd env var should run with sane defaults, not crash startup).
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("delegation_sweeper: invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return time.Duration(n) * time.Second
}

// Interval exposes the configured tick cadence — tests use it; main.go
// uses it implicitly via Start.
func (s *DelegationSweeper) Interval() time.Duration { return s.interval }

// Threshold exposes the heartbeat-staleness threshold.
func (s *DelegationSweeper) Threshold() time.Duration { return s.threshold }

// Start ticks Sweep() at the configured interval until ctx is cancelled.
// Defers panic recovery so a single bad row can't kill the sweeper.
//
// Wired into main.go: `go sweeper.Start(ctx)`. No-op until both the
// `delegations` table (PR-1) and the result-push flag (PR-2) have rolled
// out — the sweeper just won't find any rows to mark.
func (s *DelegationSweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("DelegationSweeper: started (interval=%s, stuck-threshold=%s)",
		s.interval, s.threshold)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("DelegationSweeper: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	// First sweep immediately so operators see it run on startup, not
	// after waiting one interval.
	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("DelegationSweeper: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// SweepResult records what the last sweep changed. Surfaced via the
// admin dashboard (PR-4); also useful for tests to assert behavior
// without diffing log lines.
type SweepResult struct {
	DeadlineFailures int
	StuckMarked      int
	// ReplyErrors counts terminalizations whose caller-notification write
	// failed. Both reply writes are best-effort (log-and-continue) so they can
	// never abort a terminalization — which means a sweep where EVERY reply
	// failed would otherwise report deadline_failures=N, errors=0 and look
	// perfectly healthy while no caller was told anything.
	ReplyErrors int
	Errors      int
}

// Sweep runs one pass: find every in-flight delegation, mark deadline-
// exceeded as failed, mark heartbeat-stale as stuck. Returns counts
// for observability.
//
// SQL strategy: one indexed scan over the partial inflight index, two
// updaters per offending row. We fold both checks into a single SELECT
// to amortize the round-trip — the row count in flight at any time
// is small (single-digit hundreds even on a busy tenant), so reading
// them all and dispatching SetStatus per-row is cheaper than two
// separate UPDATEs with bespoke WHERE clauses.
func (s *DelegationSweeper) Sweep(ctx context.Context) SweepResult {
	res := SweepResult{}

	// caller_id/callee_id are selected because a terminal transition OWES the
	// caller a reply (see emitTerminalDelegationReply). The sweeper used to
	// select neither — it could not have notified anyone even if it had tried.
	//
	// THE HEARTBEAT SIGNAL IS THE TARGET WORKSPACE'S, not a per-delegation one.
	//
	// delegations.last_heartbeat has exactly one writer — DelegationLedger.
	// Heartbeat — and that method has ZERO production call sites. It is
	// therefore always NULL, which means the stale-heartbeat arm below skipped
	// every row and `stuck` was UNREACHABLE: dead code wearing a live comment.
	// (An earlier revision of this change claimed to fix an error-loop that the
	// widened queued/dispatched -> stuck transition would have hit. It could
	// not have: with no heartbeat writer, no row ever reaches that arm.)
	//
	// The signal that DOES exist is workspaces.last_heartbeat_at, written by the
	// registry heartbeat every workspace already sends. If the TARGET workspace
	// has stopped heartbeating, its in-flight delegations are wedged — which is
	// precisely the "the target agent may have an issue" case the idle digest is
	// supposed to surface. COALESCE prefers a per-delegation beat if one is ever
	// wired, and falls back to the target's liveness, which is real today.
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.delegation_id, d.caller_id, d.callee_id, d.status,
		       COALESCE(d.last_heartbeat, w.last_heartbeat_at) AS beat,
		       d.deadline
		  FROM delegations d
		  LEFT JOIN workspaces w ON w.id = d.callee_id
		 WHERE d.status IN (`+sqlInFlightStates()+`)
	`)
	if err != nil {
		log.Printf("DelegationSweeper: query failed: %v", err)
		res.Errors++
		return res
	}
	defer rows.Close()

	now := time.Now()
	type candidate struct {
		id       string
		caller   string
		callee   string
		status   string
		lastBeat sql.NullTime
		deadline time.Time
	}
	var todo []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.caller, &c.callee, &c.status, &c.lastBeat, &c.deadline); err != nil {
			log.Printf("DelegationSweeper: scan failed: %v", err)
			res.Errors++
			continue
		}
		todo = append(todo, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("DelegationSweeper: rows.Err: %v", err)
		res.Errors++
	}

	for _, c := range todo {
		// Deadline first — the ONLY arm that terminalizes, and therefore the only
		// arm that notifies. The platform has waited the full 6h and given up;
		// that is a true, final statement about the delegation.
		if now.After(c.deadline) {
			detail := "deadline exceeded by sweeper"
			authority, err := s.ledger.SetStatus(ctx, c.id, "failed", detail, "")
			if err != nil {
				log.Printf("DelegationSweeper: SetStatus(%s, failed): %v", c.id, err)
				res.Errors++
				// Deliberately NOT a `continue`: fall through and let the AUTHORITY
				// decide, because "the ledger errored" is two different situations and
				// only one of them is ours to speak for.
				//
				//   RowsAffected() unreadable -> ReplyUnarbitrated -> we reply.
				//     The UPDATE may have committed. If it did, the row is terminal, drops
				//     out of the in-flight SELECT above, and NO FUTURE SWEEP REVISITS IT.
				//     This arm is the last thing that will ever look at it, so bailing out
				//     here would lose the caller's only notification permanently.
				//
				//   SELECT or UPDATE errored -> ReplyDeferred -> we stay silent.
				//     No write landed; the row is definitively unchanged and still
				//     in-flight, so THE NEXT SWEEP (5 minutes) picks it up and terminalizes
				//     it properly. Replying now would guarantee a SECOND reply then —
				//     review proved exactly that with a DB blip: sweep 1 replied on a
				//     transition that had provably not happened, sweep 2 replied again.
				//
				// The distinction is "is the row still in-flight?", and it is knowable.
				// Treating every ledger error as "nobody will ever speak again" was the
				// over-correction; it is only true when we cannot tell if we won.
			}
			if !mayReply(authority) {
				// Either somebody else terminalized this row between our SELECT and our
				// UPDATE (the agent's own status POST, or a late drain) — THEY owe the
				// reply — or the write did not land and we will be back for it.
				//
				// Note this also skips the res.DeadlineFailures++ below, which is the
				// point: counting a deadline failure for a row we left `queued` makes the
				// metric lie in the same breath as the reply did.
				continue
			}
			// The caller is owed a reply for a terminal transition it did not
			// perform. Without this the delegation just dies in silence (#4314).
			if emitTerminalDelegationReply(ctx, c.caller, c.callee, c.id, "failed", detail) {
				res.ReplyErrors++
			}
			res.DeadlineFailures++
			continue
		}

		// Heartbeat staleness. A NULL last_heartbeat counts as stale ONLY
		// if the row has lived past one threshold since creation — gives
		// the agent one full window to emit its first beat. We fold this
		// by treating NULL as "created_at — but we don't have created_at
		// in the SELECT. Approximate: NULL last_heartbeat + deadline more
		// than (5h, default deadline=6h) away from now means the row was
		// created ≤1h ago, give it a free pass. Simpler heuristic: NULL
		// heartbeat is only stale if deadline is already imminent (within
		// 1 threshold).
		var lastBeat time.Time
		if c.lastBeat.Valid {
			lastBeat = c.lastBeat.Time
		}
		if !c.lastBeat.Valid {
			// The target has never heartbeat at all (brand-new workspace, or
			// one that has not registered). Don't mark stuck — let the deadline
			// catch it. Reduces false positives during the agent's first beat
			// window after restart.
			continue
		}
		if now.Sub(lastBeat) > s.threshold {
			// BOOT GRACE. The signal is workspaces.last_heartbeat_at — written BY
			// the workspaces TO THIS SERVER. So if this server was down for longer
			// than the threshold, EVERY callee looks stale the instant we come
			// back, before anyone has had a chance to beat again. Sweeping
			// immediately on boot (Start() does, deliberately) would then mark the
			// entire in-flight set stuck in one pass, from our own downtime.
			//
			// Suppress the stuck arm for one full threshold after start: long
			// enough for every live workspace to re-beat. The deadline arm is NOT
			// suppressed — it reads a per-row timestamp that our downtime cannot
			// forge.
			if now.Sub(s.startedAt) < s.threshold {
				continue
			}
			// ALREADY STUCK — leave it alone. `stuck` is non-terminal, so the row
			// stays in the candidate set (it must: only the deadline can kill it).
			// But re-calling SetStatus every sweep would take the same-status branch
			// with a non-empty detail, firing the COALESCE UPDATE — a new MVCC tuple
			// + WAL record every 5 minutes for up to 6 hours (~72 writes that change
			// nothing) on every wedged delegation in the fleet.
			if c.status == "stuck" {
				continue
			}
			detail := "no heartbeat for " + now.Sub(lastBeat).Round(time.Second).String()
			// NO REPLY HERE — `stuck` is a WARNING, not a death (see
			// allowedTransitions). The target may be settling/restarting with its
			// message still held in a2a_queue, and the queue will deliver it on the
			// target's next heartbeat. Telling the caller "Delegation failed" now
			// would be a lie that the real answer then contradicts.
			//
			// The caller learns about this through the idle digest's warning
			// ("⚠ n sent >6h with no reply — the target agent may have an issue"),
			// which is what was actually asked for. If the target never returns,
			// the deadline arm terminalizes and notifies ONCE, truthfully.
			// `stuck` is NOT terminal and emits NO reply — it is a warning the digest
			// renders, not an answer to the caller. So this arm only counts, and
			// ReplyMine is the honest test for "I am the one who marked it".
			authority, err := s.ledger.SetStatus(ctx, c.id, "stuck", detail, "")
			if err != nil {
				log.Printf("DelegationSweeper: SetStatus(%s, stuck): %v", c.id, err)
				res.Errors++
				continue
			}
			if authority == ReplyMine {
				res.StuckMarked++
			}
		}
	}

	if res.DeadlineFailures > 0 || res.StuckMarked > 0 || res.Errors > 0 {
		log.Printf("DelegationSweeper: sweep complete — deadline_failures=%d stuck=%d errors=%d",
			res.DeadlineFailures, res.StuckMarked, res.Errors)
	}
	return res
}
