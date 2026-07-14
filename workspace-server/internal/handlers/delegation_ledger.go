package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/textutil"
)

// delegation_ledger.go — durable per-task ledger for A2A delegation
// (RFC #2829 PR-1).
//
// activity_logs is an event stream — one row per state transition. Replaying
// the stream gives you history. This file's table (delegations) is the
// folded current state — one row per delegation_id with a single status,
// last_heartbeat, deadline, and result_preview.
//
// Why both: PR-3 needs a sweeper that joins on
//   (status='in_progress' AND last_heartbeat < now() - interval '10 minutes')
// which is impossible to express against the event stream without a window
// function over every (delegation_id, latest event) pair — a planner-killing
// query at scale. The dedicated table makes the sweeper an indexed scan.
//
// Writes go to BOTH tables. activity_logs remains the audit-grade record
// for forensics; delegations is the queryable view for dashboards + sweeper
// joins. Symmetric-write pattern — same posture as tenant_resources (PR
// #2343), per memory `reference_tenant_resources_audit`.

// DelegationLedger writes the per-task durable row alongside the existing
// activity_logs event-stream writes. All methods are best-effort: a ledger
// write failure logs but does NOT propagate up — activity_logs remains the
// audit-grade source of truth.
//
// Same shape as `tenant_resources` reconciler (PR #2343): orchestration
// continues even when the ledger write fails, and the next status update
// (or PR-3 reconciler) will heal the ledger.
type DelegationLedger struct {
	db *sql.DB
}

// NewDelegationLedger returns a ledger backed by the package db handle.
// Tests can construct one with a sqlmock-backed *sql.DB.
func NewDelegationLedger(handle *sql.DB) *DelegationLedger {
	if handle == nil {
		handle = db.DB
	}
	return &DelegationLedger{db: handle}
}

// previewCap caps stored preview at 4KB. The full prompt/response is
// already in activity_logs.{request,response}_body — this is the
// at-a-glance view for the dashboard, not a forensic record.
//
// Truncation goes through textutil.TruncateBytesNoMarker so it's
// rune-safe (#2026 / #2959 / #2962 bug class: byte-slice mid-codepoint
// → Postgres JSONB rejects → silent INSERT failure → audit gap).
const previewCap = 4096

// InsertOpts is the agent's record-of-intent. Caller, callee, task preview,
// and the chosen delegation_id are required; idempotency_key is optional.
type InsertOpts struct {
	DelegationID   string
	CallerID       string
	CalleeID       string
	TaskPreview    string
	IdempotencyKey string // empty → NULL
	// Deadline defaults to now + 6h when zero. Callers can pass a tighter
	// per-task deadline (cron, interactive request) by setting it.
	Deadline time.Time
}

// Insert writes the queued row. ON CONFLICT (delegation_id) DO NOTHING so
// the agent's retry-on-restart codepath is naturally idempotent — a duplicate
// Insert with the same delegation_id is a no-op. (Idempotency_key dedupe is
// a separate UNIQUE index handled by the same DO NOTHING.)
func (l *DelegationLedger) Insert(ctx context.Context, opts InsertOpts) {
	if opts.DelegationID == "" || opts.CallerID == "" || opts.CalleeID == "" {
		log.Printf("delegation_ledger Insert: missing required field, skipping")
		return
	}
	deadline := opts.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(6 * time.Hour)
	}
	idemArg := sql.NullString{String: opts.IdempotencyKey, Valid: opts.IdempotencyKey != ""}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO delegations (
			delegation_id, caller_id, callee_id, task_preview,
			status, deadline, idempotency_key
		) VALUES ($1, $2, $3, $4, 'queued', $5, $6)
		ON CONFLICT (delegation_id) DO NOTHING
	`, opts.DelegationID, opts.CallerID, opts.CalleeID,
		textutil.TruncateBytesNoMarker(opts.TaskPreview, previewCap), deadline, idemArg)
	if err != nil {
		log.Printf("delegation_ledger Insert(%s): %v", opts.DelegationID, err)
	}
}

// ---------------------------------------------------------------------------
// THE STATUS VOCABULARY — ONE DEFINITION, DERIVED BY EVERY CONSUMER.
//
// #4314 was caused by a HAND-TYPED SQL status list. The sweeper wrote `stuck`;
// the mail digest counted `status IN ('queued','dispatched','in_progress')`; and
// so a wedged delegation silently vanished from the caller's "awaiting reply"
// count — the single case an operator most needs to see was the one the platform
// made invisible.
//
// Nothing may hand-type these again. Any query that needs "is this delegation
// still awaiting an answer" derives it from here, so ADDING A STATE cannot
// silently drop rows out of somebody's IN-list.
//
// (The cross-repo SSOT is the SDK contract — Phase 4, once the ledger is live and
// the contract can be written from observed behaviour rather than a stale comment.
// This is the in-core spine it will be generated against.)
var (
	// DelegationTerminalStates: the delegation is OVER. No further transition,
	// exactly one caller notification, and it stops being "awaiting a reply".
	DelegationTerminalStates = []string{"completed", "failed"}

	// DelegationInFlightStates: still awaiting an answer. NOTE `stuck` IS HERE —
	// a wedged target has not answered, so the caller is still waiting, and this is
	// precisely the row the "⚠ the target agent may have an issue" warning is
	// rendered from. Excluding it is the #4314 bug.
	DelegationInFlightStates = []string{"queued", "dispatched", "in_progress", "stuck"}

	// DelegationAllStates is the schema CHECK constraint's vocabulary, in full.
	// Anything validating a caller-supplied status derives from this — a
	// hand-typed switch is the same drift with different syntax.
	DelegationAllStates = append(
		append([]string{}, DelegationInFlightStates...), DelegationTerminalStates...)
)

// IsValidDelegationStatus reports whether s is one of the schema's six states.
func IsValidDelegationStatus(s string) bool {
	for _, v := range DelegationAllStates {
		if v == s {
			return true
		}
	}
	return false
}

// IsTerminalDelegationStatus reports whether s is a state the delegation can
// never leave. Callers that must reject a non-terminal status (the agent-facing
// status-update endpoint) derive from this rather than hand-typing the pair —
// that hand-typed pair is the #4314 bug class.
func IsTerminalDelegationStatus(s string) bool {
	for _, v := range DelegationTerminalStates {
		if v == s {
			return true
		}
	}
	return false
}

// sqlTerminalStates renders the terminal vocabulary as a SQL IN-list literal.
func sqlTerminalStates() string {
	quoted := make([]string, len(DelegationTerminalStates))
	for i, s := range DelegationTerminalStates {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ",")
}

// sqlInFlightStates renders the in-flight vocabulary as a SQL IN-list literal.
// Safe to interpolate: the values are package constants, never user input.
func sqlInFlightStates() string {
	quoted := make([]string, len(DelegationInFlightStates))
	for i, s := range DelegationInFlightStates {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ",")
}

// allowedTransitions enforces the lifecycle in code as defense-in-depth on
// the schema CHECK. Terminal states — **completed and failed, NOT stuck** —
// reject any further status update: once a delegation is done, it stays done.
//
// `stuck` IS NOT TERMINAL. It is a WARNING: "the target has stopped
// heartbeating and may have a problem." Treating it as a death was wrong, and
// wrong in a way that corrupts the ledger and lies to the caller:
//
//	a2a_proxy.go:915 enqueues to a2a_queue precisely when the target is
//	settling/restarting — i.e. NOT heartbeating — and queue rows are
//	infinite-TTL by default (a2a_queue.go:160). So the platform's own design is
//	to HOLD the message across an arbitrarily long target outage and deliver it
//	on the target's next heartbeat.
//
// Terminalizing on a 10-minute heartbeat gap therefore declared dead exactly the
// delegations the queue was busy keeping alive. The target would come back, the
// queue would drain, the drain would call recordLedgerStatus("completed") — and
// hit the terminal guard, get ErrInvalidTransition, and be DISCARDED. Final
// state: the ledger permanently reads `stuck` for a delegation that completed
// successfully, and the caller was told "Delegation failed" before receiving the
// real answer.
//
// So: `stuck` marks the row (the digest reads it to render "⚠ the target agent
// may have an issue" — a warning, which is what was actually asked for), emits NO
// caller notification, and REMAINS RECOVERABLE. The 6h deadline is the only thing
// that terminalizes and notifies. One death notice, and only when the platform has
// actually given up.
//
// DERIVED FROM THE REAL WRITERS, NOT FROM A PRESUMED LIFECYCLE. Every row is
// born 'queued' (Insert hard-codes it), and the paths that terminalize it are:
//
//	delegation.go:701   sync delegate returns   queued → completed
//	delegation.go:733   proxy/empty error       queued → failed
//	delegation.go:802   agent status-update     queued → dispatched
//	delegation.go:843   agent status-update     {queued,dispatched} → completed
//	a2a_queue.go:660    async drain             {queued,dispatched} → completed
//	delegation_sweeper  deadline / no heartbeat any pre-terminal → failed / stuck
//
// `queued → completed` is therefore the MOST COMMON transition in the system,
// and an earlier revision of this matrix forbade it — a delegation that
// succeeded got ErrInvalidTransition and stayed 'queued' until the sweeper
// deadline-failed it six hours later. It was invisible only because the ledger
// is dark AND no path called SetStatus from the drain. Both of those are being
// fixed in this PR, so the fiction had to go.
//
// The "queued → in_progress" jump (skipping dispatched) is allowed: lazy
// callers that don't ack the dispatched stage shouldn't be penalised,
// since the agent ultimately cares about whether work started, not which
// HTTP layer happened to ack first. NOTE: no writer currently produces
// 'in_progress' at all — it is legal per the CHECK constraint and accepted here,
// but do not read its presence as evidence that any code emits it.
// `stuck` is reachable from EVERY pre-terminal state, not just in_progress.
//
// The sweeper marks a row stuck when the TARGET WORKSPACE has stopped
// heartbeating (workspaces.last_heartbeat_at — see delegation_sweeper.go). A
// target can go silent while the delegation is still `queued` or `dispatched`,
// so restricting `stuck` to in_progress would hand the sweeper an
// ErrInvalidTransition and leave the row un-terminalized, re-attempted every
// sweep until its deadline.
//
// `stuck` means "the callee stopped talking", which is orthogonal to how far
// the handshake had got.
var allowedTransitions = map[string]map[string]bool{
	"queued": {"dispatched": true, "in_progress": true, "completed": true, "failed": true, "stuck": true},
	// stuck is a RECOVERABLE WARNING, not a death: the target can come back and
	// the queued message still deliver. Only the deadline can kill it.
	"stuck": {"completed": true, "failed": true},
	// NO dispatched -> queued. `queued` is the delegation's INITIAL state (Insert
	// writes it) and means "not yet dispatched". A busy target whose message the
	// platform enqueued HAS been dispatched — a2a_queue is holding it for delivery.
	// The delegation is `dispatched`; that the delivery channel calls its own row
	// `queued` is a fact about the channel, not about the delegation.
	//
	// Review flagged the rejection as a bug because executeDelegation ATTEMPTED the
	// transition on every busy-target delegation and got a misleading
	// "refused (already terminal)" log for its trouble. That was real — but the fix is
	// to stop the writer attempting a backward transition, not to redefine what
	// `queued` means. See executeDelegation's enqueued branch.
	"dispatched":  {"in_progress": true, "completed": true, "failed": true, "stuck": true},
	"in_progress": {"completed": true, "failed": true, "stuck": true},
}

// ErrInvalidTransition is returned by SetStatus when the transition would
// move out of a terminal state. Callers SHOULD ignore (it's a duplicate
// terminal write) but they're surfaced for tests.
var ErrInvalidTransition = errors.New("delegation ledger: invalid status transition")

// ReplyAuthority is the ledger's answer to the only question the delegation reply
// call sites actually ask: "must I be the one to tell the caller?"
//
// IT IS NOT A BOOL, AND THE MISSING THIRD STATE WAS A BUG. The first cut of the
// single-reply rule gated every inbox push on a `didTransition bool`. But a bool
// cannot distinguish "somebody else owns the reply" from "there is no arbiter at
// all" — so both collapsed to false, and a delegation with no ledger row got NO
// reply from anyone. The agent reported `failed`; the caller was never told; it
// waits forever. That is precisely the #4314 lie this change set exists to remove,
// reintroduced by its own fix, and it is not exotic: EVERY delegation in flight at
// the moment DELEGATION_LEDGER_WRITE is flipped has no row.
//
// So the ledger returns who owns the reply, and mayReply() is the only thing that
// reads it.
type ReplyAuthority int

const (
	// ReplyNotMine — the ledger arbitrated, and gave the reply to SOMEBODY ELSE.
	// The row was already in this status, a competing terminalizer won the
	// compare-and-swap, or the transition was refused as backwards. Silence is
	// CORRECT: the caller has been, or is being, notified exactly once — by the
	// writer that won. This is the only value that suppresses a reply.
	ReplyNotMine ReplyAuthority = iota

	// ReplyMine — THIS call performed the terminal transition. We own the single
	// reply and we must send it.
	ReplyMine

	// ReplyUnarbitrated — NOBODY WILL EVER SPEAK FOR THIS DELEGATION UNLESS WE DO.
	// Three cases, and what unites them is that no future writer revisits the row:
	//
	//   - writes are DARK: there is no ledger at all, so nothing arbitrates anything
	//   - NO ROW exists (created before the flip, or its best-effort Insert was lost)
	//   - the CAS committed but RowsAffected() could not be read, so we cannot tell
	//     whether we won — and if we DID, the row is now terminal, drops out of the
	//     sweeper's in-flight SELECT, and is never looked at again
	//
	// THIS IS NOT "NO". With no arbiter, we fall back to the pre-ledger behaviour and
	// reply. The failure mode of replying is a DUPLICATE; the failure mode of silence
	// is a caller that waits forever for a delegation that already ended. Those are not
	// close: prefer the duplicate. This is also what preserves dark-mode behaviour
	// byte-for-byte.
	ReplyUnarbitrated

	// ReplyDeferred — the ledger FAILED TO ACT, and the row is DEFINITIVELY UNCHANGED.
	// The SELECT errored, or the UPDATE errored: either way no write landed, so the row
	// is still exactly what it was — still in-flight, still in the sweeper's in-flight
	// SELECT, and the sweeper WILL COME BACK for it in five minutes.
	//
	// So stay silent. This looks like ReplyUnarbitrated and is its exact opposite:
	// there, nobody would ever speak again, so we had to. Here, somebody WILL — us, on
	// the next tick — so speaking now guarantees a DOUBLE reply when the retry lands.
	//
	// Review proved that (a DB blip during a deadline sweep):
	//
	//	sweep 1 (blip):      status=queued  replies=1  <- replied to a transition that
	//	                                                  provably did not happen
	//	sweep 2 (recovered): status=failed  replies=2  <- and again, legitimately
	//
	// The distinction is exactly "is the row still in-flight?", and it is knowable:
	// a failed SELECT/UPDATE means yes, a failed RowsAffected() means unknown.
	ReplyDeferred
)

// mayReply is the single predicate every delegation-reply call site must gate on.
// Centralised deliberately: the gate used to be spelled
// `didTransition || !ledgerWritesEnabled()` at five separate call sites, i.e. five
// chances to write only the first half and silently drop the dark-mode reply.
//
// Written as an explicit allow-list, not `a != ReplyNotMine`. The negative form was
// what let ReplyDeferred (added later) silently inherit "reply" — a new state joining
// the wrong side of a gate by default is how the double-reply got in.
func mayReply(a ReplyAuthority) bool {
	return a == ReplyMine || a == ReplyUnarbitrated
}

// SetStatus is the catch-all updater. Status MUST be one of the lifecycle
// values. errorDetail is non-empty only for failed/stuck. resultPreview is
// non-empty only for completed.
//
// Idempotent: re-applying the same terminal status with the same payload
// returns nil; transitioning back out of a terminal state returns
// ErrInvalidTransition. (Forward-only protection — once 'completed' you
// don't get to revise to 'failed'.)
//
// SetStatus returns a ReplyAuthority — see the type, and read it before touching
// any call site. It is deliberately NOT a bool.
//
// The authority is load-bearing, and returning only `error` was a live
// double-notify bug: SetStatus returns nil on TWO NON-transitions — a missing row,
// and a same-status replay — and the sweeper read `err == nil` as "I performed the
// transition". Reachable without exotic timing: the sweeper picks up a
// past-deadline `queued` row while the agent concurrently POSTs its own terminal
// status. The agent terminalizes and notifies; the sweeper's SetStatus then sees
// current == status, returns nil, and notifies AGAIN. The caller's agent is told
// twice that one delegation ended.
//
// The UPDATE is now a COMPARE-AND-SWAP on the observed status, so exactly one of
// two racing terminalizers can win — the read-then-write below is not atomic, and
// the old comment admitted the race while the callers ignored it.
func (l *DelegationLedger) SetStatus(ctx context.Context,
	delegationID, status, errorDetail, resultPreview string,
) (authority ReplyAuthority, err error) {
	if delegationID == "" || status == "" {
		return ReplyNotMine, errors.New("delegation ledger: missing required field")
	}

	var current string
	err = l.db.QueryRowContext(ctx,
		`SELECT status FROM delegations WHERE delegation_id = $1`,
		delegationID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		// NO ROW TO ARBITRATE WITH. The delegation is real — an agent is waiting on
		// it — but the ledger has no record: it was created before the flag flip, or
		// its best-effort Insert was lost. We are not "not the writer"; there IS no
		// writer. Returning ReplyNotMine here (as the first cut did) meant NOBODY
		// told the caller its delegation had ended.
		log.Printf("delegation_ledger SetStatus(%s, %s): no row — cannot arbitrate, "+
			"caller will be notified unconditionally", delegationID, status)
		return ReplyUnarbitrated, nil
	}
	if err != nil {
		// The SELECT failed, so NO UPDATE RAN. The row is definitively unchanged and
		// still in-flight — the sweeper's in-flight SELECT will pick it up on the next
		// tick and retry. Replying now would be a guaranteed duplicate then.
		return ReplyDeferred, err
	}

	// Same-status replay (e.g. duplicate completion notification): usually a
	// no-op. If the replay carries terminal detail that the first write lacked,
	// fill the missing nullable column once. This keeps duplicate notifications
	// idempotent while preserving the first observed result/error when a legacy
	// path wrote the terminal status before it had the detail payload.
	if current == status {
		if errorDetail == "" && resultPreview == "" {
			// A genuine duplicate: the row is ALREADY in this state, so whoever put it
			// there owns the reply and has sent it. Silence is correct.
			return ReplyNotMine, nil
		}
		_, err = l.db.ExecContext(ctx, `
			UPDATE delegations
			SET error_detail = COALESCE(error_detail, NULLIF($2, '')),
			    result_preview = COALESCE(result_preview, NULLIF($3, '')),
			    updated_at = CASE
			      WHEN (error_detail IS NULL AND NULLIF($2, '') IS NOT NULL)
			        OR (result_preview IS NULL AND NULLIF($3, '') IS NOT NULL)
			      THEN now()
			      ELSE updated_at
			    END
			WHERE delegation_id = $1
		`, delegationID, errorDetail, textutil.TruncateBytesNoMarker(resultPreview, previewCap))
		return ReplyNotMine, err // detail backfill is still NOT a transition
	}

	// Forward-only on terminal states.
	if next, ok := allowedTransitions[current]; !ok || !next[status] {
		// Terminal already — refuse to revise. The writer that terminalized it owns
		// the reply and has already sent it.
		return ReplyNotMine, ErrInvalidTransition
	}

	// COMPARE-AND-SWAP on the status we observed. If a concurrent terminalizer
	// (the agent's own status POST, or the drain) moved the row between our SELECT
	// and this UPDATE, we match zero rows and report didTransition=false — so the
	// loser stays silent and the caller is notified exactly once.
	res, err := l.db.ExecContext(ctx, `
		UPDATE delegations
		SET status = $2,
		    error_detail = NULLIF($3, ''),
		    result_preview = NULLIF($4, ''),
		    updated_at = now()
		WHERE delegation_id = $1
		  AND status = $5
	`, delegationID, status, errorDetail,
		textutil.TruncateBytesNoMarker(resultPreview, previewCap), current)
	if err != nil {
		// The UPDATE errored, so it did NOT land. Same as the SELECT case: the row is
		// definitively unchanged, still in-flight, and the sweeper will retry it.
		return ReplyDeferred, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		// We cannot tell whether we won the CAS. The driver failed us, not the
		// ledger's arbitration — so this is UNARBITRATED, not "not mine", and the
		// caller gets told. A duplicate "your delegation ended" beats a caller that
		// waits forever: if the UPDATE did commit, the row is terminal, drops out of
		// the in-flight SELECT and is never revisited, so nothing else will ever
		// speak for it. DO surface the error either way — the old comment claimed
		// "the deadline arm re-notifies", which is false; the deadline arm IS the
		// caller here.
		return ReplyUnarbitrated, err
	}
	if rows == 1 {
		return ReplyMine, nil
	}
	// Zero rows: a concurrent terminalizer moved the row between our SELECT and our
	// UPDATE. It won, it replies, we stay quiet. THIS is the case the single-reply
	// rule exists for.
	return ReplyNotMine, nil
}

// Heartbeat stamps last_heartbeat = now() for an in-flight delegation. Used
// by the callee whenever it makes progress; PR-3's sweeper compares to
// NOW() to decide stuckness. No-op on terminal-state delegations.
//
// Best-effort: failure logs but doesn't propagate.
func (l *DelegationLedger) Heartbeat(ctx context.Context, delegationID string) {
	if delegationID == "" {
		return
	}
	// DERIVED. This list used to be hand-typed as ('completed','failed','stuck') —
	// i.e. it treated `stuck` as terminal, contradicting the vocabulary 200 lines
	// above. Latent only because Heartbeat has no production caller yet; the
	// sweeper's COALESCE(d.last_heartbeat, ...) is already written to consume it.
	// Once wired, a delegation that wedges, is marked `stuck`, and then RECOVERS
	// could never refresh its heartbeat — the WHERE excluded it — so the sweeper
	// would keep reading the frozen pre-wedge timestamp and re-mark it stuck
	// forever. A recoverable state you cannot observe recovering is not recoverable.
	_, err := l.db.ExecContext(ctx, `
		UPDATE delegations
		SET last_heartbeat = now(), updated_at = now()
		WHERE delegation_id = $1
		  AND status NOT IN (`+sqlTerminalStates()+`)
	`, delegationID)
	if err != nil {
		log.Printf("delegation_ledger Heartbeat(%s): %v", delegationID, err)
	}
}
