package handlers

// wake_lifecycle.go — the DESIRED-STATE OWNER for a workspace's proactive WAKE
// intents. DORMANT in this PR (PR-B): nothing in production calls DecideWake or
// the Mark* transitions yet; the emitters and the heartbeat convergence loop are
// wired in later PRs. The methods exist, are unit-tested in isolation, and pin
// the generation + ledger + dedup contract everything else will build on.
//
// SCOPE: WAKE intents ONLY — the proactive moments the platform makes the agent
// speak first: the first-boot greeting, restart-context, idle/stall/nudge
// prompts, and the generic lifecycle wake. This is explicitly NOT the
// plugin/config reconcile owner; it says nothing about desired plugin or config
// state. Do not fold reconcile concerns in here.
//
// GENERATION SEMANTICS (Kubernetes observedGeneration, applied to wakes):
//   - workspaces.desired_generation is a monotonically increasing counter the
//     platform bumps every time it mints a NEW wake intent — "this is the state
//     I want the box in". The bump is an in-SQL `+1 RETURNING` under the
//     workspaces row lock (bumpDesiredGeneration), so it is atomic and gap-free
//     even under concurrent DecideWake — never a read-modify-write that could
//     lose or duplicate a generation.
//   - workspaces.observed_generation is the highest desired_generation the
//     RUNTIME has acknowledged converging to, reported back via the heartbeat.
//     The convergence loop settles every wake intent whose generation is
//     <= observed_generation (MarkWakeSettled), and (in a later PR) re-emits any
//     delivered-but-unsettled intent below it.
//   - desired == observed  ⇒  fully converged, nothing outstanding.
//     observed  <  desired  ⇒  un-converged wake(s) still in flight.
//
// EXACTLY-ONCE ACROSS WAKES: every wake intent carries an idempotency_key that
// is UNIQUE per (workspace, key) in the wake_intents ledger. Re-deciding the
// same wake — a retried trigger, a duplicate heartbeat, two racing wake
// goroutines — collides on that unique index and is a no-op: no second intent,
// and (critically) NO generation bump. This is the durable cross-wake dedup an
// in-memory map could never provide, since the racing deciders can live in
// distinct goroutines or even distinct processes.
//
// ARBITRATION IS GENERIC HERE. DecideWake is purely about generation + ledger +
// dedup so it stays unit-testable in isolation. It deliberately does NOT couple
// to the greeting-specific has_greeted / claimGreetDelivery machinery — that
// delegation is PR-D's job. Keep this file free of per-kind side effects.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// WakeKind is the taxonomy of proactive wakes. The consts are the SSOT for the
// ledger's free-text `kind` column — a new kind needs no migration.
type WakeKind string

const (
	// WakeFirstBootGreet is the agent's first proactive chat message on a fresh
	// onboarding (RFC concierge rule 2). Greet-once per box across all wakes.
	WakeFirstBootGreet WakeKind = "first-boot-greet"
	// WakeRestartContext is the "welcome back, here's what changed" wake on a
	// genuine restart of an already-greeted box.
	WakeRestartContext WakeKind = "restart-context"
	// WakeIdle is a proactive nudge after a quiet window.
	WakeIdle WakeKind = "idle"
	// WakeStall is a wake when a turn appears stuck (stall watchdog).
	WakeStall WakeKind = "stall"
	// WakeNudge is a request-nudge sweep wake (an unanswered user request).
	WakeNudge WakeKind = "nudge"
	// WakeLifecycle is the generic catch-all lifecycle wake.
	WakeLifecycle WakeKind = "lifecycle"
)

// WakeDecision is the verdict DecideWake returns to a would-be emitter.
//   - Fire is true only when THIS call minted a fresh intent (won the dedup) and
//     the desired_generation was durably bumped.
//   - IdempotencyKey is the derived cross-wake key (returned in both cases so a
//     caller can log/trace the dedup identity).
//   - Generation is the desired_generation the fresh intent was minted at. It is
//     0 on a no-fire (duplicate) decision.
type WakeDecision struct {
	Fire           bool
	IdempotencyKey string
	Generation     int64
}

// errWakeIntentDuplicate is the internal sentinel bumpDesiredGeneration returns
// when the (workspace, idempotency_key) already has an intent. It signals the
// caller to DISCARD the speculative +1 (roll the tx back) so a duplicate wake
// never bumps the generation. Not exported — DecideWake is the only consumer.
var errWakeIntentDuplicate = errors.New("wake intent already exists for key")

// bumpDesiredGeneration performs the atomic mint-a-wake step INSIDE the caller's
// transaction: it increments workspaces.desired_generation in-SQL and inserts
// the wake_intents ledger row at that new generation. Both happen in the ONE tx
// the caller owns, so either the whole mint commits or none of it does.
//
// On success it returns the new generation (the value stamped on the fresh
// intent) and a nil error; the caller COMMITS to keep the +1.
//
// On a UNIQUE(workspace_id, idempotency_key) collision the INSERT is a no-op
// (ON CONFLICT DO NOTHING), the row already exists, and this returns the
// EXISTING intent's generation together with errWakeIntentDuplicate. The caller
// MUST roll the tx back so the speculative +1 is discarded — a duplicate wake
// must never advance the counter. Rolling back is safe and gap-free: the
// workspaces row lock the UPDATE took serializes concurrent bumps, so no other
// committed generation can sit between this discarded one and the next.
func bumpDesiredGeneration(ctx context.Context, tx *sql.Tx, workspaceID string, kind WakeKind, key string) (int64, error) {
	// Atomic, gap-free increment under the workspaces row lock. In-SQL `+1
	// RETURNING` — never a read-modify-write, which under concurrency could lose
	// or duplicate a generation.
	var newGen int64
	if err := tx.QueryRowContext(ctx,
		`UPDATE workspaces SET desired_generation = desired_generation + 1 WHERE id = $1 RETURNING desired_generation`,
		workspaceID).Scan(&newGen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("bumpDesiredGeneration: workspace %s not found", workspaceID)
		}
		return 0, err
	}

	// Mint the ledger row at the new generation. ON CONFLICT DO NOTHING turns a
	// duplicate wake into a no-op INSERT — detected via the absent RETURNING row.
	var intentGen int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO wake_intents (workspace_id, kind, idempotency_key, generation, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 ON CONFLICT (workspace_id, idempotency_key) DO NOTHING
		 RETURNING generation`,
		workspaceID, string(kind), key, newGen).Scan(&intentGen)
	if err == nil {
		// Fresh intent minted at newGen. Caller commits (keeps the +1).
		return intentGen, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// Duplicate: the key already has an intent. Read its generation to report
	// back, then signal the caller to roll the speculative +1 back.
	var existingGen int64
	if err := tx.QueryRowContext(ctx,
		`SELECT generation FROM wake_intents WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, key).Scan(&existingGen); err != nil {
		return 0, err
	}
	return existingGen, errWakeIntentDuplicate
}

// DecideWake is the single entry point an emitter asks "should I fire this wake,
// and at what generation?". It derives the per-kind idempotency key, opens a tx,
// and mints the wake via bumpDesiredGeneration.
//
//   - Fresh mint  → commit, return {Fire:true, key, Generation:newGen}.
//   - Duplicate   → roll back (discarding the speculative +1), return
//     {Fire:false, key, Generation:0}.
//
// It is intentionally GENERIC (no greeting/has_greeted coupling) so it can be
// reasoned about and tested purely as generation + ledger + dedup. Per-kind
// arbitration (e.g. delegating greet-once to claimGreetDelivery) is layered on
// by later PRs at the call sites, not here.
func (h *WorkspaceHandler) DecideWake(ctx context.Context, workspaceID string, kind WakeKind, dedupSeed string) (WakeDecision, error) {
	key := wakeIdempotencyKey(kind, workspaceID, dedupSeed)

	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return WakeDecision{}, err
	}
	// Default to discarding: only the fresh-mint path commits. A duplicate or an
	// error leaves the speculative +1 rolled back.
	defer func() { _ = tx.Rollback() }()

	gen, err := bumpDesiredGeneration(ctx, tx, workspaceID, kind, key)
	if errors.Is(err, errWakeIntentDuplicate) {
		// Duplicate wake: no fire, no bump (the deferred rollback discards the +1).
		return WakeDecision{Fire: false, IdempotencyKey: key, Generation: 0}, nil
	}
	if err != nil {
		return WakeDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return WakeDecision{}, err
	}
	return WakeDecision{Fire: true, IdempotencyKey: key, Generation: gen}, nil
}

// wakeIdempotencyKey derives the cross-wake dedup key per kind.
//   - Greet and restart-context are ONCE-per-box identities: the workspace id is
//     the whole key, so every re-decide across every wake collides (greet-once /
//     restart-once).
//   - Idle / stall / nudge / lifecycle recur, so the caller-supplied dedupSeed
//     (the idle window id, the stall cursor, the unanswered request id, …)
//     scopes each distinct occurrence. Without a seed they would collapse into a
//     single lifetime wake.
func wakeIdempotencyKey(kind WakeKind, workspaceID, dedupSeed string) string {
	switch kind {
	case WakeFirstBootGreet:
		return string(WakeFirstBootGreet) + ":" + workspaceID
	case WakeRestartContext:
		return string(WakeRestartContext) + ":" + workspaceID
	default:
		return string(kind) + ":" + workspaceID + ":" + dedupSeed
	}
}

// MarkWakeDelivered flips a wake intent from pending/dispatched → delivered once
// the wake has actually reached the user (commit-on-delivery). Idempotent: a
// second call after it is already delivered/settled matches no row and is a
// no-op.
func (h *WorkspaceHandler) MarkWakeDelivered(ctx context.Context, workspaceID, idempotencyKey string) error {
	//ssot:allow-status-set wake_intents is a SEPARATE table with its own status
	// vocabulary (pending/dispatched/delivered/settled/dropped); it is NOT the
	// delegations lifecycle and must not derive from it.
	_, err := db.DB.ExecContext(ctx,
		`UPDATE wake_intents SET status = 'delivered'
		 WHERE workspace_id = $1 AND idempotency_key = $2 AND status IN ('pending','dispatched')`,
		workspaceID, idempotencyKey)
	return err
}

// MarkWakeSettled settles every DELIVERED wake intent for a workspace whose
// generation is <= the runtime's reported observed_generation, stamping
// settled_at. This is the convergence step: once the box has observed a
// generation, the delivered wakes at or below it are done. Pending intents (not
// yet delivered) are deliberately left alone even when below observed — an
// undelivered wake is not converged just because time passed.
func (h *WorkspaceHandler) MarkWakeSettled(ctx context.Context, workspaceID string, observedGen int64) error {
	//ssot:allow-status-set wake_intents is a SEPARATE table with its own status
	// vocabulary; this is not the delegations lifecycle.
	_, err := db.DB.ExecContext(ctx,
		`UPDATE wake_intents SET status = 'settled', settled_at = now()
		 WHERE workspace_id = $1 AND generation <= $2 AND status = 'delivered'`,
		workspaceID, observedGen)
	return err
}

// currentDesiredGeneration reads workspaces.desired_generation — the platform's
// current "wanted" wake generation for a workspace. Used to inspect convergence
// (compared against a heartbeat's observed_generation) and by tests asserting
// the counter's monotonic gap-free advance.
func currentDesiredGeneration(ctx context.Context, workspaceID string) (int64, error) {
	var gen int64
	err := db.DB.QueryRowContext(ctx,
		`SELECT desired_generation FROM workspaces WHERE id = $1`, workspaceID).Scan(&gen)
	return gen, err
}
