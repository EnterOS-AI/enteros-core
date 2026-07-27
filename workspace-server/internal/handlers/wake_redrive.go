package handlers

// wake_redrive.go — the generation-loop RE-DRIVE (RFC concierge wake-lifecycle,
// PR-F). This is the follow-up the wake_lifecycle.go header promised and left
// undone: "(in a later PR) re-emits any delivered-but-unsettled intent below
// it." It closes the gap where the convergence loop only ever SETTLES and never
// re-drives a STUCK wake.
//
// ─────────────────────────────────────────────────────────────────────────────
// DESIGN NOTE
// ─────────────────────────────────────────────────────────────────────────────
//
// THE GAP. registry.Heartbeat's convergence step (MarkWakeSettled) flips
// delivered intents at/below observed_generation → settled. It does NOTHING for
// an intent that got stuck:
//   - PENDING / DISPATCHED (decided-but-undelivered): DecideWake bumped
//     desired_generation and minted the ledger row, but the emitter crashed (or
//     its delivery failed) before MarkWakeDelivered. The generation counter is
//     advanced yet the wake never reached anyone, and nothing retries it until
//     the next lifecycle event — for the once-per-box wakes (greet,
//     restart-context) that may be never.
//   - DELIVERED-but-UNSETTLED: the wake reached the user/agent/queue, but the
//     runtime never echoed an observed_generation high enough to settle it (a
//     runtime predating the versioned-heartbeat contract, or one wedged before
//     it could report convergence). MarkWakeSettled only touches DELIVERED rows
//     with generation <= observed, so this row lingers unsettled forever.
//
// WHAT RE-DRIVE DOES. ReDriveStuckWakes finds a workspace's stuck intents and,
// for each, re-emits it through its EXISTING idempotency_key. It is:
//
//   EXACTLY-ONCE-SAFE BY DELEGATION, NOT BY ITS OWN CLEVERNESS. Re-drive never
//   re-arbitrates a wake. It re-invokes the SAME emission path, which is already
//   idempotent on the same key:
//     - first-boot-greet re-emit re-runs the greeter, whose greet-once is owned
//       by the has_greeted boot marker (workspaceHasGreeted + claimGreetDelivery
//       CAS). A delivered greet has has_greeted=true, so the re-run claims
//       nothing and sends nothing — NO double greeting. An undelivered greet has
//       has_greeted=false and correctly re-greets. This is precisely the
//       "re-drive must respect the existing idempotency" / "never causes a double
//       greeting" constraint: the arbitration STAYS owned by has_greeted, and
//       re-drive touches it only through that CAS.
//     - the queue-backed wakes re-emit via EnqueueA2A on the same key, whose
//       active-row conflict dedups a still-pending item.
//   Re-drive itself owns only selection + bounding + the ledger status
//   transitions; the actual re-emission is delegated to a nil-safe hook
//   (wakeReEmit), exactly like the settle side delegates to wakeSettler.
//
//   BOUNDED — never re-emitted forever. Each re-drive increments the intent's
//   redrive_attempts (BEFORE firing the re-emit, so the bound holds even if the
//   re-emit panics) and stamps last_redriven_at. Once redrive_attempts reaches
//   redriveMaxAttempts the intent is marked 'dropped' (terminal) instead of
//   re-emitted — so a permanently-stuck intent (a silent old runtime) is
//   abandoned, not looped on. last_redriven_at additionally throttles re-drives
//   to at most once per redriveMinInterval, so the heartbeat path cannot hammer a
//   single stuck intent every beat.
//
//   AGE-GATED so a normally-converging wake is never touched. An intent younger
//   than redriveStuckAfter is left alone: the emitter delivers and the runtime
//   echoes observed_generation within a beat or two, so only an intent stuck PAST
//   that window is a genuine stall.
//
// WIRING. RegistryHandler fires ReDriveStuckWakes on the heartbeat convergence
// path (the same place the settle runs, gated on a contract-aware beat), async
// and best-effort so it adds no heartbeat latency and a failure never breaks the
// liveness ack. Behind the established nil-safe hook pattern end to end:
//   - RegistryHandler.wakeRedriver nil  → heartbeat never re-drives (unit tests,
//     deployments without a workspace handler).
//   - WorkspaceHandler.wakeReEmit  nil  → ReDriveStuckWakes is a full no-op (it
//     does not even scan): with nothing to re-emit there is nothing to drive.
//
// SCOPE OF THE PRODUCTION RE-EMITTER (WakeReEmitter). This PR wires re-emit for
// the FIRST-BOOT GREETING — the highest-value case, because it is a once-per-box
// wake with no natural re-fire cadence, and its has_greeted CAS makes re-emit
// provably safe. The recurring wakes (stall, nudge) already self-heal: their
// sweepers re-fire on an hourly-bucketed key every few minutes, so a lost one is
// re-minted without re-drive. restart-context is likewise once-per-box but its
// re-emit would require reconstructing the restart snapshot; rather than ship
// that heavier, divergence-prone path here, WakeReEmitter treats every non-greet
// kind as a no-op skip — the owner STILL bounds and eventually drops those stuck
// rows, so the ledger never lingers half-open. Extending the dispatcher to
// restart-context is a clean follow-up on top of this owner.

import (
	"context"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

const (
	// redriveMaxAttempts caps how many times a single stuck intent may be
	// re-emitted before it is marked 'dropped' (terminal). This is the
	// never-re-emitted-forever guarantee: a permanently-unconvergeable intent (a
	// silent old runtime) is abandoned after this many tries.
	redriveMaxAttempts = 5

	// redriveStuckAfter is the minimum age an intent must reach before it is
	// eligible for re-drive. A healthy wake delivers and the runtime echoes
	// observed_generation within a beat or two, so anything younger is still
	// converging normally and must not be disturbed. 10 minutes is comfortably
	// past that window while still recovering a genuinely stuck wake promptly.
	redriveStuckAfter = 10 * time.Minute

	// redriveMinInterval throttles re-drives of the SAME intent: it is re-driven
	// at most once per this window (gated on last_redriven_at). With
	// redriveMaxAttempts this bounds a permanently-stuck intent's lifetime to
	// ~redriveMaxAttempts * redriveMinInterval before it is dropped.
	redriveMinInterval = 10 * time.Minute

	// redriveBatchCap bounds the intents re-driven per ReDriveStuckWakes call so
	// one invocation can never fan out unboundedly; a large backlog drains across
	// successive heartbeats.
	redriveBatchCap = 20
)

// ReDriveResult reports what one ReDriveStuckWakes pass did — returned for
// observability and so tests assert behavior without diffing log lines.
type ReDriveResult struct {
	Redriven int // intents re-emitted this pass (attempt counted, re-emit fired)
	Dropped  int // intents abandoned this pass (attempt cap reached → 'dropped')
}

// SetWakeReEmitter wires the re-drive's re-emission hook. Late-wiring and
// nil-safe (mirrors SetWakeHooks): the zero-value handler leaves it unset, so
// ReDriveStuckWakes is a full no-op until production wires it (to WakeReEmitter)
// after the greeter exists. The hook re-emits a stuck wake through its EXISTING
// idempotency key; whether it actually reaches the user is governed by that
// wake's own idempotency (has_greeted CAS / queue dedup), never by re-drive.
func (h *WorkspaceHandler) SetWakeReEmitter(
	reEmit func(ctx context.Context, workspaceID string, kind WakeKind, idempotencyKey string, generation int64) error,
) {
	h.wakeReEmit = reEmit
}

// ReDriveStuckWakes is the re-drive owner: it finds workspaceID's stuck wake
// intents, re-emits each through its existing idempotency key (bounded), and
// drops any that has exhausted its re-drive budget.
//
// A "stuck" intent is a NON-terminal one (pending / dispatched / delivered —
// never settled or dropped) that is older than redriveStuckAfter and has not
// been re-driven within redriveMinInterval. For each:
//   - redrive_attempts >= redriveMaxAttempts → mark 'dropped' (terminal), never
//     re-emit again. This is the bound: a permanently-stuck intent ends here.
//   - otherwise → increment redrive_attempts + stamp last_redriven_at FIRST
//     (so the bound holds even if the re-emit crashes), THEN fire the re-emit
//     through the existing idempotency key. A re-emit error is logged, not
//     fatal — the attempt is already counted, so a repeatedly-failing re-emit
//     still converges to a drop rather than looping.
//
// Full no-op when the re-emit hook is unwired (nothing to re-emit ⇒ nothing to
// drive ⇒ not even a scan). Best-effort throughout: a per-intent DB error is
// logged and the pass continues; the whole method returns an error only when the
// candidate scan itself fails.
func (h *WorkspaceHandler) ReDriveStuckWakes(ctx context.Context, workspaceID string) (ReDriveResult, error) {
	var res ReDriveResult
	if h.wakeReEmit == nil {
		return res, nil
	}

	//ssot:allow-status-set wake_intents is a SEPARATE table with its own status
	// vocabulary (pending/dispatched/delivered/settled/dropped); the non-terminal
	// filter here is NOT the delegations lifecycle and must not derive from it.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, kind, idempotency_key, generation, redrive_attempts
		  FROM wake_intents
		 WHERE workspace_id = $1
		   AND status IN ('pending','dispatched','delivered')
		   AND created_at < now() - ($2 * INTERVAL '1 second')
		   AND (last_redriven_at IS NULL OR last_redriven_at < now() - ($3 * INTERVAL '1 second'))
		 ORDER BY generation
		 LIMIT $4
	`, workspaceID, int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), redriveBatchCap)
	if err != nil {
		return res, err
	}

	// Drain the cursor fully before issuing any per-intent UPDATE: some drivers
	// forbid a write on the same connection while a result set is open.
	type stuckIntent struct {
		id       int64
		kind     string
		key      string
		gen      int64
		attempts int
	}
	var candidates []stuckIntent
	for rows.Next() {
		var c stuckIntent
		if scanErr := rows.Scan(&c.id, &c.kind, &c.key, &c.gen, &c.attempts); scanErr != nil {
			rows.Close()
			return res, scanErr
		}
		candidates = append(candidates, c)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return res, rowsErr
	}
	rows.Close()

	for _, c := range candidates {
		if c.attempts >= redriveMaxAttempts {
			// Budget exhausted: abandon it. Marking 'dropped' is the terminal
			// transition that stops this intent from ever being selected again —
			// the never-re-emitted-forever guarantee. Re-scope the WHERE to the
			// non-terminal states so a racing settle/drop can't be clobbered.
			//ssot:allow-status-set wake_intents is a SEPARATE table with its own
			// status vocabulary; this is not the delegations lifecycle.
			if _, dropErr := db.DB.ExecContext(ctx, `
				UPDATE wake_intents
				   SET status = 'dropped', last_redriven_at = now()
				 WHERE id = $1 AND status IN ('pending','dispatched','delivered')
			`, c.id); dropErr != nil {
				log.Printf("wake re-drive: drop failed for intent %d (ws=%s key=%s): %v", c.id, workspaceID, c.key, dropErr)
				continue
			}
			log.Printf("wake re-drive: DROPPED stuck intent %d (ws=%s kind=%s key=%s) after %d attempts — abandoning",
				c.id, workspaceID, c.kind, c.key, c.attempts)
			res.Dropped++
			continue
		}

		// Count the attempt BEFORE re-emitting so the bound is durable even if the
		// re-emit crashes (worst case a burned attempt / missed re-emit — strictly
		// better than an unbounded loop).
		if _, bumpErr := db.DB.ExecContext(ctx, `
			UPDATE wake_intents
			   SET redrive_attempts = redrive_attempts + 1, last_redriven_at = now()
			 WHERE id = $1
		`, c.id); bumpErr != nil {
			log.Printf("wake re-drive: attempt-bump failed for intent %d (ws=%s key=%s): %v", c.id, workspaceID, c.key, bumpErr)
			continue
		}

		// Re-emit through the EXISTING idempotency key. Exactly-once safety is the
		// re-emission path's own (has_greeted CAS / queue dedup), not re-drive's.
		if reErr := h.wakeReEmit(ctx, workspaceID, WakeKind(c.kind), c.key, c.gen); reErr != nil {
			log.Printf("wake re-drive: re-emit failed for intent %d (ws=%s kind=%s key=%s): %v — attempt already counted",
				c.id, workspaceID, c.kind, c.key, reErr)
		} else {
			log.Printf("wake re-drive: RE-EMITTED stuck intent %d (ws=%s kind=%s key=%s, attempt %d/%d)",
				c.id, workspaceID, c.kind, c.key, c.attempts+1, redriveMaxAttempts)
		}
		res.Redriven++
	}
	return res, nil
}

// WakeReEmitter builds the production re-emit dispatcher for
// WorkspaceHandler.SetWakeReEmitter. It re-emits a stuck wake through its
// existing idempotency key, per kind:
//
//   - WakeFirstBootGreet → re-invoke the first-boot greeter (async, so a slow
//     greet turn never blocks the re-drive loop). The greeter's has_greeted CAS
//     is the authoritative greet-once dedup: a delivered greet re-runs to a
//     no-op (no double greeting), an undelivered one re-greets. This is the whole
//     safety of the re-drive — arbitration stays owned by has_greeted.
//   - every other kind → no-op skip (logged). The recurring wakes (stall, nudge)
//     self-heal via their own sweepers' hourly-bucketed re-fire; restart-context
//     re-emit needs a reconstructed snapshot and is a deliberate follow-up. The
//     owner still bounds and eventually drops these stuck rows, so nothing
//     lingers half-open.
//
// greeter is the same func RegistryHandler.SetFirstBootGreeter is wired with
// (FirstBootGreeter's return). nil greeter degrades greet re-emit to a skip.
func WakeReEmitter(greeter func(workspaceID string, toolCount int)) func(ctx context.Context, workspaceID string, kind WakeKind, idempotencyKey string, generation int64) error {
	return func(_ context.Context, workspaceID string, kind WakeKind, idempotencyKey string, _ int64) error {
		switch kind {
		case WakeFirstBootGreet:
			if greeter == nil {
				log.Printf("wake re-drive: greet re-emit skipped for %s — no greeter wired", workspaceID)
				return nil
			}
			// toolCount 0: the greeter re-derives everything else, and the
			// tool-count only shapes the STATIC fallback text — an in-character
			// re-greet (or a no-op on an already-greeted box) is unaffected. Async:
			// the greeter runs a ~90s agent turn; the firstBootGreetingPending gate
			// keeps it exclusive so a concurrent greet makes this a no-op.
			globalGoAsync(func() { greeter(workspaceID, 0) })
			return nil
		default:
			log.Printf("wake re-drive: no re-emitter for kind %q (ws=%s key=%s) — skipping (owner still bounds+drops it)",
				kind, workspaceID, idempotencyKey)
			return nil
		}
	}
}
