package handlers

import (
	"context"
	"errors"
	"log"
	"os"
)

// delegation_ledger_writes.go — RFC #2829 follow-up (#318): wire
// DelegationLedger Insert + SetStatus calls into the existing
// activity_logs-driven flow without touching the legacy code path.
//
// Why a flag (not always-on)
// --------------------------
// The legacy flow writes everything to activity_logs and a tight
// strict-sqlmock test surface (~30 tests) pins exactly which SQL
// statements fire per handler invocation. Adding ledger writes
// always-on would force updating each of those tests in this PR.
// Gating behind DELEGATION_LEDGER_WRITE=1 lets ledger-driven
// behavior land independently of the test refactor — operators
// can flip it on in staging to populate the `delegations` table
// (and thus give the PR-3 sweeper + PR-4 dashboard data to work
// with) without coupling the rollout to a churn-y test diff.
//
// Default off → byte-identical to pre-#318 behavior. Flip after
// staging burn-in once the agent-side cutover (PR-5) has proven
// the round-trip end-to-end.

func ledgerWritesEnabled() bool {
	return os.Getenv("DELEGATION_LEDGER_WRITE") == "1"
}

// WarnOnPartialDelegationRollout fails LOUD on the one flag combination that is
// worse than either flag being off. Called once from main.go at startup.
//
// DELEGATION_LEDGER_WRITE=1 with DELEGATION_RESULT_INBOX_PUSH=0 is a TRAP:
//
//   - the ledger fills, so the sweeper stops being a no-op and starts
//     terminalizing delegations;
//   - the idle digest counts only NON-terminal delegations as "awaiting a reply",
//     so a swept delegation silently DROPS OFF the caller's list;
//   - and with the inbox flag off, NO delegate_result row is written — so the
//     caller is never told.
//
// The delegation therefore vanishes without a trace. That is precisely #4314 —
// the silent death this entire change set exists to fix — except now the sweeper
// is awake and doing it on purpose. The two flags must be flipped together, or
// the ledger left dark.
func WarnOnPartialDelegationRollout() {
	// The REVERSE combo is also wrong, and quieter. With INBOX_PUSH=1 and the ledger
	// dark, the drain path's reply rides its `!ledgerWritesEnabled()` arm — so callers
	// DO get delegate_result rows, while `delegations` stays empty. The digest then
	// counts unread replies to sends it has no record of, and the sweeper still cannot
	// see a wedged delegation. Half a rollout, in the other direction.
	if !ledgerWritesEnabled() && delegationResultInboxPushEnabled() {
		log.Printf("DELEGATION ROLLOUT MISCONFIGURED: DELEGATION_RESULT_INBOX_PUSH=1 but " +
			"DELEGATION_LEDGER_WRITE is off. Callers will receive delegate_result replies " +
			"for delegations the ledger has no row for: the digest counts replies to sends " +
			"it cannot see, and no wedged delegation can ever be swept. Set " +
			"DELEGATION_LEDGER_WRITE=1 or turn DELEGATION_RESULT_INBOX_PUSH back off.")
	}
	if ledgerWritesEnabled() && !delegationResultInboxPushEnabled() {
		log.Printf("DELEGATION ROLLOUT MISCONFIGURED: DELEGATION_LEDGER_WRITE=1 but " +
			"DELEGATION_RESULT_INBOX_PUSH is off. The sweeper will terminalize delegations, " +
			"the idle digest will drop them from 'awaiting reply', and NO reply will reach the " +
			"caller — a delegation that dies is invisible (#4314). Set DELEGATION_RESULT_INBOX_PUSH=1 " +
			"or turn DELEGATION_LEDGER_WRITE back off.")
	}

	// THE #4338 INTERLOCK — FAIL-CLOSED, BECAUSE A PROSE PRECONDITION IS NOT A GATE.
	//
	// Async MCP delegations (delegate_task_async — the route agents actually use) have
	// NO COMPLETION WRITER. `delivered` -> in_progress is the last status any code
	// writes for them; nothing moves them to `completed`. Flip DELEGATION_LEDGER_WRITE
	// today and every async MCP delegation sits at in_progress until its 6h deadline,
	// whereupon the sweeper fires "Delegation failed" at the caller — INCLUDING for the
	// ones whose target finished the work an hour in. A fleet-wide false-failure event
	// on the busiest delegation route, six hours after a flag flip that appeared to go
	// fine. Nobody would connect the two.
	//
	// That precondition used to live in a code comment ("do NOT flip until #4338
	// closes"). A gate whose fail arm is a comment isn't a gate — it's a hope. So it
	// is executable, and it REFUSES TO BOOT rather than warning:
	//
	//   - It is unreachable today. The flag is set in no environment, so this cannot
	//     fire by accident; the ONLY way to reach it is to deliberately flip the very
	//     flag it guards.
	//   - A refusal is immediate, loud, and trivially reversible (unset the flag). The
	//     failure it prevents is silent, six hours delayed, and reaches live agents.
	//   - Degrading to a warning would let the flip "succeed", which is precisely the
	//     outcome that produces the incident.
	//
	// #4338 wires the completion writer and flips asyncMCPCompletionWired to true. That
	// constant — not this comment, and not a checklist — is what unlocks Phase 2.
	if reason := delegationRolloutFatalReason(ledgerWritesEnabled(), asyncMCPCompletionWired); reason != "" {
		log.Fatalf("%s", reason)
	}
}

// delegationRolloutFatalReason is the #4338 interlock's DECISION, split out from the
// log.Fatalf so it can be tested. A gate nobody has watched fire is not a gate, and
// `log.Fatalf` cannot be observed from a normal test — so the predicate is pure and
// the caller does the exiting.
//
// Returns "" when the configuration is safe to boot.
func delegationRolloutFatalReason(ledgerWrites, completionWired bool) string {
	if ledgerWrites && !completionWired {
		return ("REFUSING TO START: DELEGATION_LEDGER_WRITE=1, but the async MCP " +
			"completion writer is not wired (#4338).\n" +
			"  delegate_task_async delegations never leave `in_progress`, so 6 hours from " +
			"now the sweeper would deadline-fail EVERY ONE of them — including the ones " +
			"that succeeded — and push a false 'Delegation failed' into each caller's inbox.\n" +
			"  This is the exact #4314 lie the ledger exists to remove, at fleet scale, on " +
			"the primary delegation route.\n" +
			"  Land #4338 (which flips asyncMCPCompletionWired), or set " +
			"DELEGATION_LEDGER_WRITE=0.")
	}
	return ""
}

// recordLedgerInsert is the gated wrapper around DelegationLedger.Insert.
// All callers in delegation.go go through here so flipping the flag
// requires no further code changes — the gate is one function.
//
// taskPreview is truncated by the ledger to `previewCap` bytes; pass
// the full task text without pre-truncating.
func recordLedgerInsert(ctx context.Context, callerID, calleeID, delegationID, taskPreview, idemKey string) {
	if !ledgerWritesEnabled() {
		return
	}
	NewDelegationLedger(nil).Insert(ctx, InsertOpts{
		DelegationID:   delegationID,
		CallerID:       callerID,
		CalleeID:       calleeID,
		TaskPreview:    taskPreview,
		IdempotencyKey: idemKey,
	})
}

// recordLedgerStatus is the gated wrapper around DelegationLedger.SetStatus.
// status MUST be one of the lifecycle values the ledger accepts
// (queued|dispatched|in_progress|completed|failed|stuck). errorDetail is
// non-empty for failed/stuck; resultPreview is non-empty for completed.
//
// Errors are logged inside the ledger and not propagated — the legacy
// activity_logs path remains authoritative; ledger is best-effort
// (matches the tenant_resources audit posture, memory ref:
// `reference_tenant_resources_audit`).
// It returns a ReplyAuthority — WHO OWNS the side effect that must happen exactly
// once per delegation: notifying the caller's inbox. Call sites gate on mayReply()
// and nothing else. Read the ReplyAuthority doc before you touch this: the value
// is tri-state precisely because "somebody else will tell the caller" and "nobody
// will tell the caller" are different answers, and collapsing them to `false`
// silently drops real replies.
func recordLedgerStatus(ctx context.Context, delegationID, status, errorDetail, resultPreview string) ReplyAuthority {
	if !ledgerWritesEnabled() {
		// DARK. There is no ledger to arbitrate with, so nobody else is going to
		// speak for this delegation — reply unconditionally, exactly as the code did
		// before the ledger existed. This is what keeps flag-off behaviour identical.
		return ReplyUnarbitrated
	}
	// Still best-effort — a ledger write failure must not fail the delegation, so
	// the error is not propagated. But it MUST NOT be silent.
	//
	// `_ =` here was hiding the most consequential write in the subsystem: the
	// drain path's terminalization (a2a_queue.go). If that call fails, the row
	// stays `queued`, the sweeper deadline-fails it 6h later, and the caller is
	// told a delegation that SUCCEEDED had failed. Swallowing the error meant the
	// only symptom would have been that lie, six hours downstream, with nothing in
	// the logs connecting the two.
	//
	// ErrInvalidTransition is expected and benign in exactly one shape — a late
	// drain arriving after the deadline arm already terminalized the row — so it
	// is logged at a lower key to keep it greppable without crying wolf.
	authority, err := NewDelegationLedger(nil).SetStatus(
		ctx, delegationID, status, errorDetail, resultPreview,
	)
	if err != nil {
		if errors.Is(err, ErrInvalidTransition) {
			log.Printf("delegation_ledger: %s -> %s refused (already terminal); "+
				"a late result arrived after the delegation was given up on", delegationID, status)
		} else {
			log.Printf("delegation_ledger: %s -> %s FAILED: %v", delegationID, status, err)
		}
		// Deliberately NOT `return false`. SetStatus has already decided who owns the
		// reply, and it is the only thing that can: on ErrInvalidTransition somebody
		// else terminalized and replied (ReplyNotMine), while on a DB error nobody
		// did (ReplyUnarbitrated). Overriding it here with a blanket "no" is how the
		// ledger being broken would have taken the caller's notification down with
		// it — a best-effort store must never be able to suppress a real reply.
	}
	return authority
}
