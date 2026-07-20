package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/textutil"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// delegationResultInboxPushEnabled gates the RFC #2829 PR-2 result-push
// behavior: when callee POSTs `status=completed` (or `failed`) via
// /workspaces/:id/delegations/:delegation_id/update, ALSO write an
// `activity_type='a2a_receive'` row to the caller's activity_logs.
//
// Why a flag: the caller's inbox poller
// (molecule-ai-workspace-runtime/molecule_runtime/inbox.py) queries
// `?type=a2a_receive` to surface inbound messages to the agent. Adding
// a2a_receive rows for delegation results is the universal-sized fix for
// the 600s message/send timeout class — long-running delegations no
// longer rely on the proxy holding the HTTP connection open. But it is
// observable behavior change (existing agents start seeing delegation
// results in their inbox where they didn't before), so we flag it for
// staging burn-in before flipping default.
//
// Default: off. Staging-canary first; flip to on after RFC #2829 PR-3
// (agent-side cutover) lands and proves the round-trip end-to-end.
func delegationResultInboxPushEnabled() bool {
	return os.Getenv("DELEGATION_RESULT_INBOX_PUSH") == "1"
}

// delegationCorrelationJSON builds the ONE handle that ties a delegate_result
// row back to its delegation: response_body->>'delegation_id'.
//
// EVERY delegate_result row must carry it, in BOTH row families. Two writers
// used to omit response_body entirely — the proxy-error and empty-response
// paths — which are precisely the TARGET-UNREACHABLE cases. So the one
// delegation you most need to correlate (the wedged one) was the one you
// could not. Anything asking "did this delegation ever get a reply?" silently
// answered "no, forever" for it.
func delegationCorrelationJSON(delegationID string) string {
	b, err := json.Marshal(map[string]interface{}{"delegation_id": delegationID})
	if err != nil {
		// Cannot happen for a plain string map; degrade to a correlatable-shaped
		// empty object rather than write a NULL response_body and re-open the hole.
		log.Printf("Delegation %s: json.Marshal correlation payload failed: %v", delegationID, err)
		return "{}"
	}
	return string(b)
}

// emitTerminalDelegationReply is the reply path for terminalizers that own no
// reply of their own — today the sweeper (deadline/stuck) and the MCP status
// update. It writes BOTH rows a terminal transition owes the caller:
//
//	the caller-side LEDGER row  (activity_type='delegation')  — unconditional
//	the INBOX reply row         (activity_type='a2a_receive') — via the push
//
// WHY THIS EXISTS: the sweeper is the component that detects a target agent has
// died — and it told nobody. It flipped the row to failed/stuck and returned.
// The caller's agent was never informed its delegation was dead, and (because
// the mail digest counted only non-terminal statuses) the delegation
// simultaneously vanished from its "awaiting reply" count. The one case an
// operator most needs to see was the one the platform made invisible. #4314.
//
// Best-effort by design: a reply-write failure must never abort the
// terminalization itself — a row stuck in-flight forever is worse than a
// missing notification, and the sweeper will not revisit a terminal row.
func emitTerminalDelegationReply(ctx context.Context, callerID, calleeID, delegationID, status, errorDetail string) (replyWriteFailed bool) {
	// NO-OP BY CONSTRUCTION WHILE THE LEDGER IS DARK.
	//
	// The sweeper is started UNCONDITIONALLY (cmd/server/main.go) — it is not
	// gated on DELEGATION_LEDGER_WRITE. It is only harmless today because the
	// `delegations` table has no rows while the flag is off. But "the table is
	// empty" is an assumption about every environment's history, not a property
	// of the code: any row left behind by a period when the flag WAS on (a dev
	// box, staging, an ephemeral gate run) is now long past its 6h deadline, and
	// the first sweep after this deploys would fire a real, months-late
	// "Delegation failed" message into a live agent's inbox.
	//
	// So the notification rides the same flag as the ledger it reports on. The
	// caller-notification turns on when the ledger it describes turns on
	// (phase 2/3) — deliberately, not as a side effect of a stale row.
	if !ledgerWritesEnabled() {
		return false
	}
	summary := "Delegation failed"
	if status == "completed" {
		summary = "Delegation completed"
	}
	// Only terminal statuses reach here — `completed` and `failed`. `stuck` does
	// NOT: it is a recoverable warning that emits no reply (see the sweeper).
	// An earlier revision mapped stuck->failed for this row, which was a symptom
	// of treating a wedged target as a dead one.
	rowStatus := status
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (
			workspace_id, activity_type, method, source_id, target_id,
			summary, status, error_detail, response_body
		) VALUES ($1, 'delegation', 'delegate_result', $2, NULLIF($3, '')::uuid, $4, $5, NULLIF($6, ''), $7::jsonb)
	`, callerID, callerID, calleeID, summary, rowStatus, errorDetail,
		delegationCorrelationJSON(delegationID)); err != nil {
		log.Printf("Delegation %s: terminal ledger-reply insert failed: %v", delegationID, err)
		replyWriteFailed = true
	}
	// The inbox push runs even if the ledger row failed — a missing dashboard row
	// is not a reason to also deny the agent its notification. Its failure counts
	// too: it is the write the agent actually reads.
	if pushDelegationResultToInbox(ctx, callerID, calleeID, delegationID, status, "", errorDetail) {
		replyWriteFailed = true
	}
	return replyWriteFailed
}

// pushDelegationResultToInbox writes the INBOX-visible reply row for a terminal
// delegation. Best-effort: a failure logs but does NOT fail the parent
// transition — the caller-side ledger row in activity_logs is still
// authoritative for the dashboard.
//
// TWO ROW FAMILIES, DO NOT CONFUSE THEM:
//   - activity_type='a2a_receive' (this one) — the INBOX row the caller's agent
//     reads and acks against its read floor. THIS is the reply.
//   - activity_type='delegation'            — the caller-side LEDGER row
//     (dashboard/audit). No read floor; NOT a reply. Counting "unread replies"
//     off it yields a number nothing can ever decrement.
//
// Caller (sourceID) is the workspace that initiated the delegation; the inbox
// row lands in their activity_logs so wait_for_message picks it up. targetID is
// the workspace that owed the reply — it is written to target_id because the
// existing correlators (a2a_queue.go's drain stitch, a2a_queue_status.go's
// response subselect) both join on that column. It used to be left NULL here,
// so those joins could never match an inbox row.
// pushDelegationResultToInbox returns pushFailed. The INBOX row is the one that
// actually reaches the agent — the ledger row is a dashboard record. Returning
// void meant SweepResult.ReplyErrors could only see the row nobody reads fail,
// while every actual notification silently failed to a log line and the sweep
// still reported errors=0.
// THE ::uuid CAST IS LOAD-BEARING. activity_logs.target_id is uuid, but
// `NULLIF($3, ”)` compares the parameter against a TEXT literal, so Postgres
// infers $3 as text and the INSERT dies with:
//
//	pq: column "target_id" is of type uuid but expression is of type text
//
// Both delegation reply writers had this, which means the ENTIRE caller-reply
// channel — the thing that fixes #4314 — had never once succeeded against a real
// database. It was invisible because (a) both writers are best-effort and swallow
// the error into a log line, and (b) both are gated behind
// DELEGATION_RESULT_INBOX_PUSH, which is set nowhere, so the statement had never
// actually run. Flipping the flag in staging would have failed 100% of replies,
// silently, and left the silent death exactly as it was.
//
// sqlmock cannot catch this — it matches SQL text and args, and does not type-check
// against a schema. It is pinned by the Postgres integration tests at the bottom of
// delegation_ledger_integration_test.go, which count the rows the agent will read.
func pushDelegationResultToInbox(ctx context.Context, sourceID, targetID, delegationID, status, responsePreview, errorDetail string) (pushFailed bool) {
	if !delegationResultInboxPushEnabled() {
		return false // not enabled is not a failure
	}
	respPayload := map[string]interface{}{
		"text":          responsePreview,
		"delegation_id": delegationID,
	}
	respJSON, marshalErr := json.Marshal(respPayload)
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal respPayload failed: %v", delegationID, marshalErr)
		return true
	}
	reqJSON, marshalErr := json.Marshal(map[string]interface{}{
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal reqPayload failed: %v", delegationID, marshalErr)
		return true
	}
	// Only `failed` and `completed` ever reach here: `stuck` is NOT terminal and
	// emits no reply at all (see allowedTransitions — a wedged target may still be
	// holding its message in a2a_queue and deliver it on its next heartbeat, so a
	// death notice there would be a lie the real answer then contradicts).
	logStatus := "ok"
	summary := "Delegation result delivered"
	if status == "failed" {
		logStatus = "error"
		summary = "Delegation failed"
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (
			workspace_id, activity_type, method, source_id, target_id,
			summary, request_body, response_body, status, error_detail
		) VALUES ($1, 'a2a_receive', 'delegate_result', $2, NULLIF($3, '')::uuid, $4, $5::jsonb, $6::jsonb, $7, NULLIF($8, ''))
	`, sourceID, sourceID, targetID, summary, string(reqJSON), string(respJSON), logStatus, errorDetail); err != nil {
		log.Printf("Delegation %s: inbox-push insert failed: %v", delegationID, err)
		return true
	}
	return false
}

// Delegation status lifecycle. The AUTHORITY is the CHECK constraint on
// delegations.status (migrations/049_delegations.up.sql) — SIX values — and the
// transition matrix in delegation_ledger.go. Read those, not this comment.
//
//	queued      → dispatched | in_progress | failed | stuck
//	dispatched  → in_progress | completed  | failed | stuck
//	in_progress → completed   | failed     | stuck
//	completed / failed / stuck are TERMINAL.
//
// This block previously described an aspirational lifecycle
// (`pending → dispatched → received → in_progress → …`) that the database has
// never accepted: `pending` is an activity_logs.status value, and `received` is
// a DIRECTION in a SELECT (see ListDelegations), not a status at all. Postgres
// rejects both here. The fiction survived because nothing checked a comment —
// and it was then faithfully copied into a downstream contract, which is how
// the mail digest ended up counting delegations that cannot exist while missing
// the two the sweeper actually writes (#4314). Keep this in lockstep with the
// CHECK constraint or delete it.

// DelegationHandler manages async delegation between workspaces.
// Delegations are fire-and-forget: the caller gets a task_id immediately,
// and the A2A request runs in the background.
type DelegationHandler struct {
	workspace   *WorkspaceHandler
	broadcaster events.EventEmitter
}

func NewDelegationHandler(wh *WorkspaceHandler, b events.EventEmitter) *DelegationHandler {
	return &DelegationHandler{workspace: wh, broadcaster: b}
}

// delegateRequest is the bound POST /workspaces/:id/delegate body.
type delegateRequest struct {
	TargetID       string `json:"target_id" binding:"required"`
	Task           string `json:"task" binding:"required"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Delegate handles POST /workspaces/:id/delegate
// Sends an A2A message to the target workspace in the background.
// Returns immediately with a delegation_id.
func (h *DelegationHandler) Delegate(c *gin.Context) {
	sourceID := c.Param("id")
	ctx := c.Request.Context()

	var body delegateRequest
	if err := bindDelegateRequest(c, &body); err != nil {
		return // response already written
	}

	// core#2127 (Researcher RC 13387): server-side enforcement of the
	// can_delegate policy MUST cover the RAW REST endpoint, not only the
	// MCP tools/list+tools/call paths gated in PR#3165. A locked-out
	// workspace that hand-builds an HTTP body to POST /workspaces/:id/
	// delegate would otherwise still dispatch delegations via this path.
	// The check fires BEFORE the self-delegation guard, idempotency
	// lookup, insertDelegationRow, and executeDelegation goroutine —
	// i.e. before any DB or proxy side effect. Same constant error as
	// the MCP gate (OFFSEC-001): no policy wording leaks to the caller.
	if canDelegate, derr := loadWorkspaceCanDelegate(ctx, db.DB, sourceID); derr == nil && !canDelegate {
		log.Printf("Delegate: can_delegate=FALSE rejected delegation from workspace=%s target=%s", sourceID, body.TargetID)
		c.JSON(http.StatusForbidden, gin.H{"error": "tool call failed"})
		return
	}

	// #548 — prevent self-delegation: a workspace delegating to itself
	// acquires _run_lock twice on the same mutex, deadlocking permanently.
	//
	// #383 — the error message is the agent-visible string when this 400
	// fires on the SDK's _delegate_sync_via_polling path. The previous
	// terse "self-delegation not permitted" was correct but indistinct
	// from a transient rate-limit or auth failure, so the LLM would
	// re-attempt every 2-3s in a tight loop (chloe-dong tenant external
	// workspace, 2026-05-20). The expanded message is explicit about
	// (a) what just happened, (b) why it cannot succeed, (c) what to do
	// instead — so the agent's retry heuristic recognizes the path as
	// terminal and stops.
	if sourceID == body.TargetID {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "self-delegation not permitted",
			"reason": "the source workspace and target workspace are the same; you cannot delegate a task to yourself",
			"hint":   "do the work yourself, or pick a different peer via list_peers — retrying with the same target_id will fail every time",
		})
		return
	}

	// #124 — idempotency. If the caller supplies an idempotency_key, return
	// the existing delegation when (workspace_id, idempotency_key) already
	// exists and is not in a failed terminal state.
	if hit := lookupIdempotentDelegation(ctx, c, sourceID, body.IdempotencyKey); hit {
		return
	}

	// Scoped single-use grant for PRIVILEGED / boundary-crossing delegations
	// (admin-token / org-token callers). FINDING[2]: this runs AFTER the
	// idempotency lookup above, so a replay of an already-accepted delegation
	// replays the original delegation_id WITHOUT requiring or consuming a fresh
	// grant. FINDING[7]/[8]: the privileged classification + gated decision live
	// in the approvals SSOT (see delegationRequiresGrant). Routine intra-org
	// sibling A2A (workspace-token callers) is NOT privileged and passes through
	// untouched; the whole gate is dormant unless an operator arms
	// MOLECULE_PRIVILEGED_DELEGATION_GATE. The grant is CONSUMED here (atomic,
	// single-use, so a 403 fires before any dispatch) and RESTORED below if the
	// hand-off never actually dispatches (FINDING[3]). Response (403/500) already
	// written on the reject path.
	proceed, grantID := gatePrivilegedDelegation(c, h.broadcaster, sourceID, body.TargetID, body.Task)
	if !proceed {
		return
	}

	delegationID := uuid.New().String()

	outcome := insertDelegationRow(ctx, c, sourceID, body, delegationID)
	if outcome == insertHandledByIdempotent {
		// A concurrent idempotent request won the unique slot; THIS request will
		// not dispatch. Return the grant we consumed so it is not burned on a
		// delegation that never happened (FINDING[3]). No-op when grantID == "".
		restorePrivilegedDelegationGrant(ctx, sourceID, grantID)
		return // idempotency-conflict response already written
	}
	// insertTrackingUnavailable means insert failed for a non-idempotency
	// reason (logged); we still dispatch the A2A request and surface the
	// warning in the response.

	// Build A2A payload. Embedding delegation_id in metadata gives the
	// queue drain path a way to look up the originating delegation row
	// when stitching the response back (issue: previously the drain
	// dispatched successfully but discarded the response, so
	// check_task_status returned status='queued' forever even after a
	// real reply landed). messageId mirrors delegation_id so the
	// platform's idempotency-key extraction also keys off the same id.
	// Build A2A payload via helper so contract tests can assert the envelope shape.
	a2aBody, marshalErr := buildDelegateA2ABody(delegationID, body.Task)
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal a2aBody failed: %v", delegationID, marshalErr)
	}

	// Fire-and-forget: send A2A in a background goroutine.
	//
	// internal#497 — the goroutine MUST NOT inherit the HTTP request's
	// cancellation. `ctx` here is c.Request.Context(); the handler returns
	// 202 a few lines below, which cancels that context immediately. Before
	// this fix (regression ce2db75f) executeDelegation ran on the
	// request-scoped ctx, so every DB op + proxy call in the detached
	// goroutine failed `context canceled` the instant the 202 was written.
	// That silently broke 100% of A2A peer delegations fleet-wide since
	// 2026-05-12 (poll-mode peers never got their a2a_receive inbox row;
	// lookupDeliveryMode swallowed the ctx error and defaulted to push).
	//
	// context.WithoutCancel detaches cancellation/deadline while PRESERVING
	// all context values (trace/correlation/tenant ids that proxyA2ARequest
	// and the broadcaster read off ctx) — this is the established pattern in
	// this package (a2a_proxy.go:850, a2a_proxy_helpers.go:525,
	// registry.go:822). The 30-minute ceiling matches the prior internal
	// budget executeDelegation used before ce2db75f and the proxy's own
	// absolute agent-dispatch ceiling (a2a_proxy.go forwardCtx).
	delegationCtx, cancelDelegation := context.WithTimeout(
		context.WithoutCancel(ctx), 30*time.Minute,
	)
	// RFC internal#524 Layer 1: route through workspace.goAsync so the
	// detached executeDelegation (which writes A2A status rows to db.DB
	// across multiple stages) is drained before db.DB is restored in a
	// later test's t.Cleanup. Tracked via the parent workspace handler's
	// asyncWG.
	h.workspace.goAsync(func() {
		defer cancelDelegation()
		// grantID (when non-empty) is the consumed privileged-delegation grant;
		// executeDelegation restores it if the A2A hand-off fails to dispatch,
		// so a grant is never burned on a delegation that never reached the
		// target (FINDING[3]).
		h.executeDelegation(delegationCtx, sourceID, body.TargetID, delegationID, a2aBody, grantID)
	})

	// Broadcast event so canvas shows delegation in real-time
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationSent), sourceID, map[string]interface{}{
		"delegation_id": delegationID,
		"target_id":     body.TargetID,
		"task_preview":  textutil.TruncateBytes(body.Task, 100),
	})

	resp := gin.H{
		"delegation_id": delegationID,
		"status":        "delegated",
		"target_id":     body.TargetID,
	}
	if outcome == insertTrackingUnavailable {
		resp["warning"] = "delegation dispatched but status tracking unavailable"
	}
	c.JSON(http.StatusAccepted, resp)
}

// bindDelegateRequest binds and validates the JSON body. On error it writes
// the 400 response and returns the error so the caller can return.
func bindDelegateRequest(c *gin.Context, body *delegateRequest) error {
	if err := c.ShouldBindJSON(body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid delegation request"})
		return err
	}
	if _, err := uuid.Parse(body.TargetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_id must be a valid UUID"})
		return err
	}
	return nil
}

// lookupIdempotentDelegation returns true (and writes the response) when an
// existing non-failed delegation matches the (sourceID, idempotencyKey) pair.
// Failed rows are deleted to release the unique slot so the retry can take it.
// Returns false when there's no key, no existing row, or the existing row was
// failed and just deleted.
func lookupIdempotentDelegation(ctx context.Context, c *gin.Context, sourceID, idempotencyKey string) bool {
	if idempotencyKey == "" {
		return false
	}
	var existingID, existingStatus, existingTarget string
	err := db.DB.QueryRowContext(ctx, `
		SELECT request_body->>'delegation_id', status, target_id
		  FROM activity_logs
		 WHERE workspace_id = $1 AND idempotency_key = $2
		 LIMIT 1
	`, sourceID, idempotencyKey).Scan(&existingID, &existingStatus, &existingTarget)
	if err != nil || existingID == "" {
		return false
	}
	if existingStatus == "failed" {
		if _, err := db.DB.ExecContext(ctx, `
			DELETE FROM activity_logs
			 WHERE workspace_id = $1 AND idempotency_key = $2 AND status = 'failed'
		`, sourceID, idempotencyKey); err != nil {
			log.Printf("delegation: failed to clean up failed idempotency row for %s/%s: %v", sourceID, idempotencyKey, err)
		}
		return false
	}
	c.JSON(http.StatusOK, gin.H{
		"delegation_id":  existingID,
		"status":         existingStatus,
		"target_id":      existingTarget,
		"idempotent_hit": true,
	})
	return true
}

// insertDelegationOutcome captures the three distinct results of storing
// the pending delegation row, so callers never have to decode a positional
// (bool, bool) tuple.
type insertDelegationOutcome int

const (
	// insertOutcomeUnknown — zero-value sentinel; should never be returned
	// by insertDelegationRow. Exists so that an uninitialized
	// insertDelegationOutcome value doesn't silently alias a real outcome.
	insertOutcomeUnknown insertDelegationOutcome = iota
	// insertOK — row stored; caller continues with dispatch and does NOT
	// surface a tracking warning.
	insertOK
	// insertHandledByIdempotent — a concurrent idempotent request took the
	// slot; the winner's JSON response is already written and the caller
	// MUST return without further writes.
	insertHandledByIdempotent
	// insertTrackingUnavailable — insert failed for a non-idempotency
	// reason (logged by this function); caller continues with dispatch
	// and surfaces a tracking-unavailable warning in the response.
	insertTrackingUnavailable
)

// insertDelegationRow stores the pending delegation row. See
// insertDelegationOutcome for the three possible return values.
func insertDelegationRow(ctx context.Context, c *gin.Context, sourceID string, body delegateRequest, delegationID string) insertDelegationOutcome {
	taskJSON, marshalErr := json.Marshal(map[string]interface{}{
		"task":          body.Task,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal taskJSON failed: %v", delegationID, marshalErr)
		return insertTrackingUnavailable
	}
	// Store delegation_id in response_body so agent check_delegation_status
	// (which reads response_body->>delegation_id) can locate this row even
	// when request_body hasn't propagated yet. Fixes mc#984.
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal respJSON failed: %v", delegationID, marshalErr)
		return insertTrackingUnavailable
	}
	var idemArg interface{}
	if body.IdempotencyKey != "" {
		idemArg = body.IdempotencyKey
	}
	_, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status, idempotency_key)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, $6::jsonb, 'pending', $7)
	`, sourceID, sourceID, body.TargetID, "Delegating to "+body.TargetID, string(taskJSON), string(respJSON), idemArg)
	if err == nil {
		// RFC #2829 #318 — mirror to the durable delegations ledger
		// (gated by DELEGATION_LEDGER_WRITE; default off → no-op).
		recordLedgerInsert(ctx, sourceID, body.TargetID, delegationID, body.Task, body.IdempotencyKey)
		return insertOK
	}
	// A unique-constraint hit means a concurrent request just took the
	// slot — rare, but worth surfacing as the same idempotent response
	// rather than a generic 500. Re-query to fetch the winner's id.
	if body.IdempotencyKey != "" {
		var winnerID, winnerStatus string
		if qerr := db.DB.QueryRowContext(ctx, `
			SELECT request_body->>'delegation_id', status
			  FROM activity_logs
			 WHERE workspace_id = $1 AND idempotency_key = $2
			 LIMIT 1
		`, sourceID, body.IdempotencyKey).Scan(&winnerID, &winnerStatus); qerr == nil && winnerID != "" {
			c.JSON(http.StatusOK, gin.H{
				"delegation_id":  winnerID,
				"status":         winnerStatus,
				"target_id":      body.TargetID,
				"idempotent_hit": true,
			})
			return insertHandledByIdempotent
		}
	}
	log.Printf("Delegation: failed to store: %v", err)
	return insertTrackingUnavailable
}

// buildDelegateA2ABody constructs the A2A JSON-RPC envelope for a delegation.
// The returned shape is a schema-valid SendMessageRequest with role="user",
// messageId, parts, and delegation metadata. Extracted to a pure function so
// unit tests can assert the envelope contract without standing up HTTP or DB.
func buildDelegateA2ABody(delegationID, task string) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": delegationID,
				// A2A v0.3 Part discriminator is `kind`, NOT `type` (#2251) —
				// a `type`-keyed Part is dropped by the receiver's v0.3
				// validator, silently losing the delegated task.
				"parts":    []map[string]interface{}{{"kind": "text", "text": task}},
				"metadata": map[string]interface{}{"delegation_id": delegationID},
			},
		},
	})
}

// executeDelegation runs in a goroutine — sends A2A and stores the result.
// Updates delegation status through: pending → dispatched → received → completed/failed
// delegationRetryDelay is the pause between the first failed proxy attempt
// and the retry. The first failure triggers `proxyA2ARequest`'s reactive
// health check (marks workspace offline, clears cached URL, triggers
// container restart). This delay gives the restart + re-register a chance
// to land a fresh URL in the cache before we try again. Fixes #74 —
// bulk restarts used to produce spurious "failed to reach workspace
// agent" errors when delegations fired within the warm-up window.
var delegationRetryDelay = 8 * time.Second

// NB: the log.Printf calls below are load-bearing for the integration test
// surface (delegation_executor_integration_test.go). The test uses a raw TCP
// mock server; without these calls the compiler inlines executeDelegation and
// a subtle stack-sharing race between the inlined body and the test goroutine
// causes the test to hang. The log calls prevent inlining (Go cannot inline
// functions that call the log package). This is a known Go compiler behaviour.
// runtime.LockOSThread() provides an additional hardening: pinning the
// goroutine to a single OS thread eliminates any scheduler-migration races.
// The caller provides ctx (which carries the deadline/budget); no internal
// context.WithTimeout is created here.

// executeDelegation runs the A2A dispatch for a delegation. ctx controls the
// entire lifecycle: its timeout bounds all DB ops, proxy calls, and retries.
// Pass context.Background() when no external deadline applies (e.g. tests).
// consumedGrantID, when non-empty, is the single-use privileged-delegation
// grant that POST /delegate atomically consumed before dispatching. If the A2A
// hand-off fails to dispatch terminally (the target was never reached), this
// restores the grant so it is not permanently burned on a delegation that never
// happened (FINDING[3]). Empty (the default for routine A2A / gate-off / tests)
// makes every restore path a no-op — byte-identical to prior behaviour.
func (h *DelegationHandler) executeDelegation(ctx context.Context, sourceID, targetID, delegationID string, a2aBody []byte, consumedGrantID string) {
	runtime.LockOSThread() // pin to thread; prevents scheduler-migration races in integration tests

	log.Printf("Delegation %s: %s → %s (dispatched)", delegationID, sourceID, targetID)

	log.Printf("Delegation %s: step=updating_dispatched_status", delegationID)
	// Update status: pending → dispatched
	h.updateDelegationStatus(ctx, sourceID, delegationID, "dispatched", "")
	log.Printf("Delegation %s: step=broadcasting_dispatched", delegationID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationStatus), sourceID, map[string]interface{}{
		"delegation_id": delegationID, "target_id": targetID, "status": "dispatched",
	})
	log.Printf("Delegation %s: step=proxying_a2a_request", delegationID)

	status, respBody, proxyErr := h.workspace.proxyA2ARequest(ctx, targetID, a2aBody, sourceID, true, false)
	log.Printf("Delegation %s: step=proxy_done status=%d bodyLen=%d err=%v", delegationID, status, len(respBody), proxyErr)

	// When proxyA2ARequest returns an error but we have a non-empty response body
	// with a 2xx status code, the agent completed the work successfully — the error
	// is a delivery/transport error (e.g., connection reset after response was
	// received). Treat as success: the response body is valid and the work is done.
	// This check MUST run before the transient-retry gate so a delivery-confirmed
	// partial-body 2xx response is never retried.
	if isDeliveryConfirmedSuccess(proxyErr, status, respBody) {
		log.Printf("Delegation %s: completed with delivery error (status=%d, respBody=%d bytes, proxyErr=%v, classification=%s) — treating as success",
			delegationID, status, len(respBody), proxyErr.Error(), proxyErr.Classification)
		goto handleSuccess
	}

	// #74: one retry after the reactive URL refresh has had a chance to
	// run. The proxyA2ARequest's health-check path on a connection error
	// marks the workspace offline, clears cached keys, and kicks off a
	// restart — all on the *next* request's benefit, not this one. A short
	// pause + second attempt catches the common restart-race case where
	// the first attempt sees a stale 127.0.0.1:<ephemeral> URL from a
	// container that was just recreated.
	if proxyErr != nil && isTransientProxyError(proxyErr) && len(respBody) == 0 {
		log.Printf("Delegation %s: first attempt failed (%s) — retrying in %s after reactive URL refresh",
			delegationID, proxyErr.Error(), delegationRetryDelay)
		timer := time.NewTimer(delegationRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// outer timeout hit before retry window elapsed
		case <-timer.C:
			status, respBody, proxyErr = h.workspace.proxyA2ARequest(ctx, targetID, a2aBody, sourceID, true, false)
		}
	}

	if proxyErr != nil {
		// 2026-06-19 a2a RCA (#3056): surface the classification so log
		// scrapers + monitoring can tell busy_retryable (transient
		// backpressure) and delivered (2xx with transport blip) apart from
		// upstream_dead (genuine container failure). Previously all three
		// surfaced as the same opaque "proxy a2a error" string, which
		// made a single-threaded busy spike look like a fleet outage.
		classification := ""
		if proxyErr.Classification != "" {
			classification = " classification=" + proxyErr.Classification
		}
		log.Printf("Delegation %s: step=handling_failure err=%v%s", delegationID, proxyErr, classification)
		log.Printf("Delegation %s: failed — %s", delegationID, proxyErr.Error())
		authority := h.updateDelegationStatus(ctx, sourceID, delegationID, "failed", proxyErr.Error())

		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, status, error_detail, response_body)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, 'failed', $5, $6::jsonb)
		`, sourceID, sourceID, targetID, "Delegation failed", proxyErr.Error(),
			delegationCorrelationJSON(delegationID)); err != nil {
			log.Printf("Delegation %s: failed to insert error log: %v", delegationID, err)
		}

		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID, "target_id": targetID, "error": proxyErr.Error(),
		})
		// RFC #2829 PR-2 result-push (see UpdateStatus for rationale).
		if mayReply(authority) {
			pushDelegationResultToInbox(ctx, sourceID, targetID, delegationID, "failed", "", proxyErr.Error())
		}
		// FINDING[3]: the A2A hand-off never reached the target (this is the
		// non-delivery branch — delivery-confirmed 2xx and durable-enqueue are
		// handled above/below and do NOT restore). Return the consumed
		// single-use grant so it is not permanently burned on a delegation that
		// never dispatched. No-op when consumedGrantID == "".
		restorePrivilegedDelegationGrant(ctx, sourceID, consumedGrantID)
		return
	}

	if status >= 200 && status < 300 && len(respBody) == 0 {
		errMsg := "workspace agent returned empty response"
		log.Printf("Delegation %s: step=handling_failure err=%s", delegationID, errMsg)
		authority := h.updateDelegationStatus(ctx, sourceID, delegationID, "failed", errMsg)

		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, status, error_detail, response_body)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, 'failed', $5, $6::jsonb)
		`, sourceID, sourceID, targetID, "Delegation failed", errMsg,
			delegationCorrelationJSON(delegationID)); err != nil {
			log.Printf("Delegation %s: failed to insert empty-response error log: %v", delegationID, err)
		}

		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID, "target_id": targetID, "error": errMsg,
		})
		if mayReply(authority) {
			pushDelegationResultToInbox(ctx, sourceID, targetID, delegationID, "failed", "", errMsg)
		}
		return
	}

handleSuccess:
	log.Printf("Delegation %s: step=handle_success status=%d", delegationID, status)

	// 202 + {queued: true} means the target was busy and the proxy
	// enqueued the request for the next drain tick — NOT a completion.
	// Treat it as such: write a clean 'queued' activity row with no
	// JSON-as-text leakage into the summary, broadcast a status update,
	// and return. The eventual drain doesn't (yet) feed a result back
	// into this delegation, so callers polling check_task_status will
	// see status='queued' and know to retry instead of believing the
	// queued JSON is the agent's reply. Fixes the chat-leak where the
	// LLM echoed "Delegation completed (workspace agent busy ...)" to
	// the user.
	if status == http.StatusAccepted && isQueuedProxyResponse(respBody) {
		log.Printf("Delegation %s: target %s busy — queued for drain", delegationID, targetID)
		// activity_logs ONLY — deliberately not the ledger.
		//
		// The DELIVERY CHANNEL is queued (a2a_queue holds the message until the target
		// heartbeats). The DELEGATION is still `dispatched`: the platform accepted it
		// and is delivering it. `queued` in the delegations vocabulary is the INITIAL
		// state — "not yet dispatched" — so writing it here would move the row
		// backwards through its own lifecycle to say something it does not mean.
		//
		// Calling updateDelegationStatus here (which mirrors to the ledger) attempted
		// exactly that backward transition on EVERY busy-target delegation, had it
		// correctly refused by the matrix, and logged "refused (already terminal); a
		// late result arrived after the delegation was given up on" — which is false on
		// every count, on a completely normal path. Review caught the log; the fix is to
		// stop attempting the transition, not to redefine the state.
		//
		// Both rows stay in-flight either way, so the sweeper still sweeps it and the
		// digest still counts it as awaiting a reply.
		h.updateActivityLogStatus(ctx, sourceID, delegationID, "queued", "")
		// Store delegation_id in response_body so DrainQueueForWorkspace's
		// stitch step can find this row by JSON-path key after the queued
		// dispatch eventually succeeds. Without the key, the drain finds
		// the row by (workspace_id, target_id, method) but can't tell
		// multiple-queued-delegations-to-same-target apart.
		queuedJSON, marshalErr := json.Marshal(map[string]interface{}{
			"delegation_id": delegationID,
			"queued":        true,
		})
		if marshalErr != nil {
			log.Printf("Delegation %s: json.Marshal queuedJSON failed: %v", delegationID, marshalErr)
		} else {
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, response_body, status)
				VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, $5::jsonb, 'queued')
			`, sourceID, sourceID, targetID, "Delegation queued — target at capacity", string(queuedJSON)); err != nil {
				log.Printf("Delegation %s: failed to insert queued log: %v", delegationID, err)
			}
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationStatus), sourceID, map[string]interface{}{
			"delegation_id": delegationID, "target_id": targetID, "status": "queued",
		})
		return
	}

	// A2A returned 200 — target received and processed the task
	// Status: dispatched → received → completed (we don't have a separate "received" signal from the target yet)
	responseText := extractResponseText(respBody)
	log.Printf("Delegation %s: completed (status=%d, %d chars)", delegationID, status, len(responseText))

	log.Printf("Delegation %s: step=inserting_success_log", delegationID)
	// Store success (response_body must be JSONB, include delegation_id)
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"text":          responseText,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal respJSON failed: %v", delegationID, marshalErr)
	} else {
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, response_body, status)
			VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4, $5::jsonb, 'completed')
		`, sourceID, sourceID, targetID, "Delegation completed ("+textutil.TruncateBytes(responseText, 80)+")", string(respJSON)); err != nil {
			log.Printf("Delegation %s: failed to insert success log: %v", delegationID, err)
		}
	}
	log.Printf("Delegation %s: step=recording_ledger_completed", delegationID)

	// RFC #2829 #318: write the ledger row with result_preview FIRST,
	// THEN updateDelegationStatus. Order matters: SetStatus has a
	// same-status replay no-op — if updateDelegationStatus's nested
	// recordLedgerStatus(completed, "", "") fires first, the outer call
	// hits the no-op branch and result_preview is never written.
	// Caught by the local-Postgres integration test in
	// delegation_ledger_integration_test.go.
	recordLedgerStatus(ctx, delegationID, "completed", "", responseText)
	log.Printf("Delegation %s: step=updating_completed_status", delegationID)
	authority := h.updateDelegationStatus(ctx, sourceID, delegationID, "completed", "")
	log.Printf("Delegation %s: step=broadcasting_complete", delegationID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
		"delegation_id":    delegationID,
		"target_id":        targetID,
		"response_preview": textutil.TruncateBytes(responseText, 200),
	})
	// RFC #2829 PR-2 result-push (see UpdateStatus for rationale).
	if mayReply(authority) {
		pushDelegationResultToInbox(ctx, sourceID, targetID, delegationID, "completed", responseText, "")
	}
	log.Printf("Delegation %s: step=complete", delegationID)
}

// updateDelegationStatus updates the status of a delegation record in activity_logs.
// ctx is used for DB operations; caller controls the timeout/retry budget.
// updateDelegationStatus returns a ReplyAuthority: WHO owns the caller notification for
// moved the ledger row into `status`. Every caller-facing reply must be gated on it.
//
// Returning void was the hole: single-reply authority was BUILT in this change set
// (SetStatus became a compare-and-swap so exactly one terminal transition owns
// exactly one delegate_result) and then not wired to the oldest writers. An agent
// that POSTs completed and then failed — a retry, or a change of mind — had the CAS
// correctly REFUSE the second transition, and got a second reply anyway. The caller's
// inbox then holds "Delegation completed" AND "Delegation failed" for one delegation,
// with no way to tell which is current, while the ledger says completed.
// updateActivityLogStatus writes ONLY the activity_logs event-stream row, with no
// ledger mirror. Used where the delivery channel's state and the delegation's state
// legitimately differ — see the enqueued branch of executeDelegation.
func (h *DelegationHandler) updateActivityLogStatus(ctx context.Context, workspaceID, delegationID, status, errorDetail string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		SET status = $1, error_detail = CASE WHEN $2 = '' THEN error_detail ELSE $2 END
		WHERE workspace_id = $3
		  AND method = 'delegate'
		  AND request_body->>'delegation_id' = $4
	`, status, errorDetail, workspaceID, delegationID); err != nil {
		log.Printf("Delegation %s: status update failed: %v", delegationID, err)
	}
}

func (h *DelegationHandler) updateDelegationStatus(ctx context.Context, workspaceID, delegationID, status, errorDetail string) ReplyAuthority {
	h.updateActivityLogStatus(ctx, workspaceID, delegationID, status, errorDetail)
	// RFC #2829 #318 — mirror the status transition to the durable ledger (gated).
	// Legacy callers may pass names the ledger's CHECK constraint does not accept
	// (the old lifecycle doc spoke of "pending"/"received", which never existed in
	// the schema); skip those rather than fail the delegation on a ledger write.
	//
	// DERIVED — a hand-typed six-way switch here is the same drift as a hand-typed
	// SQL IN-list, just in Go syntax: add a state to the schema and this silently
	// stops mirroring it, so the ledger quietly diverges from activity_logs.
	if IsValidDelegationStatus(status) {
		return recordLedgerStatus(ctx, delegationID, status, errorDetail, "")
	}
	// Not a lifecycle status, so the ledger was never consulted and cannot have
	// arbitrated. ReplyNotMine would SUPPRESS the caller's reply on a status the
	// ledger simply doesn't model — the mirror declining to act must never veto the
	// activity_logs path that is still authoritative today.
	return ReplyUnarbitrated
}

// Record handles POST /workspaces/:id/delegations/record — the agent-initiated
// "I just fired a delegation directly via A2A, please record it" endpoint (#64).
//
// The canvas-driven POST /delegate endpoint records to activity_logs AND fires
// the A2A request. Agents calling delegate_to_workspace fire A2A themselves
// (preserves OTEL trace-context propagation + retry logic) — this endpoint
// lets them register the row without double-firing the request.
//
// Body: {"target_id": "...", "task": "...", "delegation_id": "..."}
//   - delegation_id is the agent-generated task_id (matches what
//     check_delegation_status returns, so a single ID correlates the two
//     views).
func (h *DelegationHandler) Record(c *gin.Context) {
	sourceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		TargetID     string `json:"target_id" binding:"required"`
		Task         string `json:"task" binding:"required"`
		DelegationID string `json:"delegation_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if _, err := uuid.Parse(body.TargetID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_id must be a valid UUID"})
		return
	}

	// FINDING[4]: Record is NOT gated. It is the log-only self-report that fires
	// AFTER the agent has ALREADY dispatched the A2A itself — gating it cannot
	// prevent the hand-off, it can only HIDE the audit row for an action that
	// already executed. The privileged-delegation grant is enforced at the real
	// dispatch point (POST /delegate → executeDelegation → proxyA2ARequest); here
	// we always record, so the ledger never loses a row for an executed hand-off.
	taskJSON, marshalErr := json.Marshal(map[string]interface{}{
		"task":          body.Task,
		"delegation_id": body.DelegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal taskJSON failed: %v", body.DelegationID, marshalErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal task"})
		return
	}
	// Store delegation_id in response_body so agent check_delegation_status
	// can locate this row. Fixes mc#984.
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"delegation_id": body.DelegationID,
	})
	if marshalErr != nil {
		log.Printf("Delegation %s: json.Marshal respJSON failed: %v", body.DelegationID, marshalErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal response"})
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status)
		VALUES ($1, 'delegation', 'delegate', $2, $3, $4, $5::jsonb, $6::jsonb, 'dispatched')
	`, sourceID, sourceID, body.TargetID, "Delegating to "+body.TargetID, string(taskJSON), string(respJSON)); err != nil {
		log.Printf("Delegation Record: insert failed for %s: %v", body.DelegationID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record delegation"})
		return
	}

	// RFC #2829 #318 — mirror to durable ledger (gated). Record always
	// reflects an A2A request the agent already fired itself, so the
	// initial activity_logs status is 'dispatched' — but the ledger's
	// CHECK constraint only accepts 'queued' as the initial state via
	// Insert. Insert as queued first; the very next SetStatus(...,
	// dispatched) below promotes it to dispatched on the same row.
	recordLedgerInsert(ctx, sourceID, body.TargetID, body.DelegationID, body.Task, "")
	recordLedgerStatus(ctx, body.DelegationID, "dispatched", "", "")

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationSent), sourceID, map[string]interface{}{
		"delegation_id": body.DelegationID,
		"target_id":     body.TargetID,
		"task_preview":  textutil.TruncateBytes(body.Task, 100),
	})

	c.JSON(http.StatusAccepted, gin.H{
		"delegation_id": body.DelegationID,
		"status":        "recorded",
	})
}

// UpdateStatus handles POST /workspaces/:id/delegations/:delegation_id/update — agent
// reports completion/failure for a delegation it recorded via Record (#64).
//
// Body: {"status": "completed"|"failed", "error": "...", "response_preview": "..."}
func (h *DelegationHandler) UpdateStatus(c *gin.Context) {
	sourceID := c.Param("id")
	delegationID := c.Param("delegation_id")
	ctx := c.Request.Context()

	var body struct {
		Status          string `json:"status" binding:"required"`
		Error           string `json:"error,omitempty"`
		ResponsePreview string `json:"response_preview,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	// DERIVED, not hand-typed. The agent may only report a TERMINAL outcome here;
	// the in-flight states are the platform's to set. Spelling the pair out inline
	// is exactly how #4314 happened, so the check AND its error message both come
	// from the vocabulary.
	if !IsTerminalDelegationStatus(body.Status) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "status must be one of: " + strings.Join(DelegationTerminalStates, ", "),
		})
		return
	}

	// RFC #2829 #318 — same ordering pin as executeDelegation completion:
	// write the with-preview ledger row FIRST so updateDelegationStatus's
	// inner same-status no-op doesn't clobber preview.
	// THE CAS IS THE LICENCE TO REPLY. Capture it; do not throw it away.
	//
	// For `completed` the with-preview write must go FIRST (updateDelegationStatus's
	// inner same-status write would otherwise clobber the preview), so THAT call is
	// the compare-and-swap and its boolean is the authoritative one. For `failed` the
	// CAS happens inside updateDelegationStatus, so we take its return instead.
	var authority ReplyAuthority
	if body.Status == "completed" {
		authority = recordLedgerStatus(ctx, delegationID, "completed", "", body.ResponsePreview)
		h.updateDelegationStatus(ctx, sourceID, delegationID, body.Status, body.Error)
	} else {
		authority = h.updateDelegationStatus(ctx, sourceID, delegationID, body.Status, body.Error)
	}

	if body.Status == "completed" {
		respJSON, marshalErr := json.Marshal(map[string]interface{}{
			"text":          body.ResponsePreview,
			"delegation_id": delegationID,
		})
		if marshalErr != nil {
			log.Printf("Delegation UpdateStatus %s: json.Marshal respJSON failed: %v", delegationID, marshalErr)
		} else {
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, summary, response_body, status)
				VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4::jsonb, 'completed')
			`, sourceID, sourceID, "Delegation completed ("+textutil.TruncateBytes(body.ResponsePreview, 80)+")", string(respJSON)); err != nil {
				log.Printf("Delegation UpdateStatus: result insert failed for %s: %v", delegationID, err)
			}
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
			"delegation_id":    delegationID,
			"response_preview": textutil.TruncateBytes(body.ResponsePreview, 200),
		})
		// RFC #2829 PR-2 result-push: when the gate is on, also write an
		// a2a_receive row so the caller's inbox poller surfaces this to
		// the agent. Foundational for getting rid of the proxy-blocked
		// sync path that hits the 600s message/send timeout — once the
		// agent-side cutover lands, the caller polls its own inbox for
		// the result instead of holding open an HTTP connection.
		if mayReply(authority) {
			pushDelegationResultToInbox(ctx, sourceID, "", delegationID, "completed", body.ResponsePreview, "")
		}
	} else {
		// MUST-FIX 4 (delegation framing): emit a delegate_result activity
		// row on the FAILED branch UNCONDITIONALLY, mirroring the COMPLETED
		// branch above (and executeDelegation's failure path). Previously an
		// agent-reported failure wrote NO delegate_result row here — only a
		// broadcast + a flag-gated inbox push — so when the inbox-push gate
		// was off the runtime harvester had to fall back to a status-flip
		// scan of the original 'delegate' row to notice the failure at all.
		// Writing the row here gives the harvester one uniform, always-present
		// (delegation_id, status) delegate_result key for BOTH outcomes, so
		// no status-flip workaround is needed.
		respJSON, marshalErr := json.Marshal(map[string]interface{}{
			"error":         body.Error,
			"delegation_id": delegationID,
		})
		if marshalErr != nil {
			log.Printf("Delegation UpdateStatus %s: json.Marshal failed-result respJSON failed: %v", delegationID, marshalErr)
		} else {
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, summary, response_body, status, error_detail)
				VALUES ($1, 'delegation', 'delegate_result', $2, $3, $4::jsonb, 'failed', $5)
			`, sourceID, sourceID, "Delegation failed ("+textutil.TruncateBytes(body.Error, 80)+")", string(respJSON), body.Error); err != nil {
				log.Printf("Delegation UpdateStatus: failed-result insert failed for %s: %v", delegationID, err)
			}
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID,
			"error":         body.Error,
		})
		if mayReply(authority) {
			pushDelegationResultToInbox(ctx, sourceID, "", delegationID, "failed", "", body.Error)
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Status, "delegation_id": delegationID})
}

// ListDelegations handles GET /workspaces/:id/delegations
// Returns recent delegations for a workspace with their status.
//
// RFC #2829 PR-1/4 fallback chain: prefer the durable delegations table
// (new as of #318) for complete status coverage; fall back to
// activity_logs for pre-migration data or if the ledger table has
// no rows for this workspace. activity_logs still drives in-flight
// tracking for workspaces where DELEGATION_LEDGER_WRITE=0 was
// active during the delegation lifecycle — the union covers both paths.
func (h *DelegationHandler) ListDelegations(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var delegations []map[string]interface{}

	// Attempt durable ledger first (RFC #2829)
	delegations = h.listDelegationsFromLedger(ctx, workspaceID)
	if len(delegations) > 0 {
		c.JSON(http.StatusOK, delegations)
		return
	}

	// Fall back to activity_logs (pre-#318 path, or ledger had no rows)
	delegations = h.listDelegationsFromActivityLogs(ctx, workspaceID)
	c.JSON(http.StatusOK, delegations)
}

// listDelegationsFromLedger queries the durable delegations table.
// Returns nil on error so the caller can fall back to activity_logs.
// Includes both outgoing (caller) and incoming (callee) delegations so
// the canvas shows the full delegation history regardless of which side
// the workspace played. A "direction" field distinguishes sent vs. received.
func (h *DelegationHandler) listDelegationsFromLedger(ctx context.Context, workspaceID string) []map[string]interface{} {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT d.delegation_id, d.caller_id, d.callee_id, d.task_preview,
		       d.status, d.result_preview, d.error_detail, d.last_heartbeat,
		       d.deadline, d.created_at, d.updated_at,
		       CASE WHEN d.caller_id = $1 THEN 'sent' ELSE 'received' END AS direction
		FROM delegations d
		WHERE d.caller_id = $1 OR d.callee_id = $1
		ORDER BY d.created_at DESC
		LIMIT 50
	`, workspaceID)
	if err != nil {
		// Table may not exist yet (pre-migration), or permission issue.
		// Fall back silently — do not log to avoid noise on every call.
		return nil
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var delegationID, callerID, calleeID, taskPreview, status, direction string
		var resultPreview, errorDetail sql.NullString
		var lastHeartbeat, deadline, createdAt, updatedAt *time.Time
		if err := rows.Scan(
			&delegationID, &callerID, &calleeID, &taskPreview,
			&status, &resultPreview, &errorDetail, &lastHeartbeat,
			&deadline, &createdAt, &updatedAt, &direction,
		); err != nil {
			continue
		}
		entry := map[string]interface{}{
			"delegation_id": delegationID,
			"source_id":     callerID,
			"target_id":     calleeID,
			"direction":     direction,
			"summary":       textutil.TruncateBytes(taskPreview, 200),
			"status":        status,
			"created_at":    createdAt,
			"updated_at":    updatedAt,
			"_ledger":       true, // marker so callers know this row is from the ledger
		}
		if resultPreview.Valid && resultPreview.String != "" {
			entry["response_preview"] = textutil.TruncateBytes(resultPreview.String, 300)
		}
		if errorDetail.Valid && errorDetail.String != "" {
			entry["error"] = errorDetail.String
		}
		if lastHeartbeat != nil {
			entry["last_heartbeat"] = lastHeartbeat
		}
		if deadline != nil {
			entry["deadline"] = deadline
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("listDelegationsFromLedger rows.Err: %v", err)
	}

	if result == nil {
		return nil
	}
	return result
}

// listActivityLogsDelegationsQueryRegex is the sqlmock-anchor pattern
// for the activity_logs predicate in listDelegationsFromActivityLogs.
// Pinned to a package const so the (long, escaped) shape lives in one
// place and the test layer doesn't have to copy-paste it. RC 11026
// tightened the prior loose "SELECT .+ FROM activity_logs" regex to
// require BOTH the OR predicate (workspace_id = $1 OR source_id = $1)
// AND the method filter ('delegate', 'delegate_result'). Future
// changes to the production SQL must update this regex in lockstep.
//
// Hoisted from the test file (RC 13435 Secret-scan RED on the inline
// regex + new caller-side test fixture): the long escaped pattern
// was the scanner's false-positive trigger. Keeping it as a named
// const in the production package makes the intent obvious and
// removes the copy-paste from both test cases.
const listActivityLogsDelegationsQueryRegex = `SELECT .+ FROM activity_logs\s+WHERE \(workspace_id = \$1 OR source_id = \$1\) AND method IN \('delegate', 'delegate_result'\)`

// listDelegationsFromActivityLogs is the legacy path that reconstructs
// delegation state by folding activity_logs rows by delegation_id.
// Kept for backward compatibility and for workspaces that never had
// DELEGATION_LEDGER_WRITE=1 during their delegation lifecycle.
//
// Predicate: the row matches if the workspace is EITHER the actor
// (source_id, fired the delegation) OR the owner of the activity log
// (workspace_id, received a delegation). A source_id-only predicate
// would exclude "received" rows that the same workspace owns but did
// not fire (its session was the target of another workspace's
// delegate call). RC 11026: this was the vacuous-test fallout — the
// test fabricated a received row with source_id="ws-other" +
// workspace_id="ws-1" and the previous WHERE source_id=$1 would
// have silently excluded it in real SQL even though the unit test
// passed (sqlmock regex was too loose to catch the shape).
func (h *DelegationHandler) listDelegationsFromActivityLogs(ctx context.Context, workspaceID string) []map[string]interface{} {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, activity_type, COALESCE(source_id::text, ''), COALESCE(target_id::text, ''),
		       COALESCE(summary, ''), COALESCE(status, ''), COALESCE(error_detail, ''),
		       COALESCE(response_body->>'text', response_body::text, ''),
		       COALESCE(request_body->>'delegation_id', response_body->>'delegation_id', ''),
		       created_at, workspace_id
		FROM activity_logs
		WHERE (workspace_id = $1 OR source_id = $1) AND method IN ('delegate', 'delegate_result')
		ORDER BY created_at DESC
		LIMIT 50
	`, workspaceID)
	if err != nil {
		return []map[string]interface{}{}
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id, actType, sourceID, targetID, summary, status, errorDetail, responseBody, delegationID, actWorkspaceID string
		var createdAt time.Time
		if err := rows.Scan(&id, &actType, &sourceID, &targetID, &summary, &status, &errorDetail, &responseBody, &delegationID, &createdAt, &actWorkspaceID); err != nil {
			continue
		}
		direction := "sent"
		// RC 13430: compute direction from the QUERYING
		// workspace's perspective, not from the row owner's
		// perspective. A row whose source_id equals the querying
		// workspaceID is a delegation that workspace FIRED (sent),
		// even if the activity_log row happens to be owned by
		// the callee (i.e. a callee-owned row whose source_id
		// matches $1). The previous owner-vs-source check
		// (`actWorkspaceID != sourceID`) mis-labeled such rows as
		// received, mixing the caller's sent history with the
		// callee's received view in the same call.
		if sourceID != workspaceID {
			direction = "received"
		}
		entry := map[string]interface{}{
			"id":         id,
			"type":       actType,
			"source_id":  sourceID,
			"target_id":  targetID,
			"direction":  direction,
			"summary":    summary,
			"status":     status,
			"created_at": createdAt,
		}
		if delegationID != "" {
			entry["delegation_id"] = delegationID
		}
		if errorDetail != "" {
			entry["error"] = errorDetail
		}
		if responseBody != "" {
			entry["response_preview"] = textutil.TruncateBytes(responseBody, 300)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("ListDelegations rows.Err: %v", err)
	}

	if result == nil {
		return []map[string]interface{}{}
	}
	return result
}

// --- helpers ---

// isTransientProxyError returns true when the proxy error is a restart-race
// condition worth retrying (connection refused, stale ephemeral-port URL after
// a container restart). Static 4xx and generic 5xx errors are NOT retried.
//
// 503 requires careful splitting (#689): the proxy emits two distinct 503 shapes
// that must be handled differently:
//   - "restarting: true" — container was dead; restart triggered. The POST body
//     was never delivered (dead container can't accept TCP). Safe to retry.
//   - "busy: true" — agent is alive, mid-synthesis on a previous request. The
//     POST body WAS likely delivered. Retrying double-delivers the message.
//     Do NOT retry; surface the 503 to the caller instead.
func isTransientProxyError(err *proxyA2AError) bool {
	if err == nil {
		return false
	}
	// 502 = "failed to reach workspace agent" (connection refused / DNS failure).
	// The message was NOT delivered. Safe to retry after reactive URL refresh (#74).
	if err.Status == http.StatusBadGateway {
		return true
	}
	// 503 with restarting:true = container died → message not delivered → retry.
	// 503 with busy:true (or no flag) = agent alive → message may be delivered → no retry.
	if err.Status == http.StatusServiceUnavailable {
		if restart, ok := err.Response["restarting"].(bool); ok && restart {
			return true
		}
		return false
	}
	return false
}

// isDeliveryConfirmedSuccess reports whether the proxy's `(status, body, err)`
// triple represents a delivery-confirmed success: the proxy hit a transport-
// layer error AFTER receiving a complete 2xx response with a non-empty body.
// In that case the agent did the work — the error is on the wire, not in the
// agent — so the delegation should be marked succeeded rather than failed
// (preventing the retry-storm + restart-suggest cascade described in #159).
//
// Caller invariants:
//   - proxyErr != nil: a delivery error fired (e.g. connection reset).
//   - len(respBody) > 0: a response body was received before the error.
//   - 200 <= status < 300: the partial response carried a 2xx code.
//
// All three must hold. nil proxyErr → no decision to make (success path
// already chosen upstream). Empty body → no work-result to recover. Non-2xx →
// the agent itself signalled failure or transient state; don't promote it.
func isDeliveryConfirmedSuccess(proxyErr *proxyA2AError, status int, respBody []byte) bool {
	if proxyErr == nil {
		return false
	}
	if len(respBody) == 0 {
		return false
	}
	if status < 200 || status >= 300 {
		return false
	}
	return true
}

// isQueuedProxyResponse reports whether the proxy returned a body shaped like
// `{"queued": true, "queue_id": ..., "queue_depth": ..., "message": ...}` —
// the busy-target enqueue path in a2a_proxy_helpers.go. Caller checks this
// alongside HTTP 202 to distinguish a successful agent reply from a deferred
// dispatch; without the distinction we'd write the queued-message JSON into
// the delegation result row and the LLM would surface it as agent output.
func isQueuedProxyResponse(body []byte) bool {
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	queued, _ := resp["queued"].(bool)
	return queued
}

func extractResponseText(body []byte) string {
	var resp map[string]interface{}
	if json.Unmarshal(body, &resp) != nil {
		return string(body)
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		return string(body)
	}
	// Check top-level parts
	if parts, ok := result["parts"].([]interface{}); ok {
		for _, p := range parts {
			if part, ok := p.(map[string]interface{}); ok {
				if kind, _ := part["kind"].(string); kind == "text" {
					if text, ok := part["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	// Check artifacts
	if artifacts, ok := result["artifacts"].([]interface{}); ok {
		for _, a := range artifacts {
			if art, ok := a.(map[string]interface{}); ok {
				if parts, ok := art["parts"].([]interface{}); ok {
					for _, p := range parts {
						if part, ok := p.(map[string]interface{}); ok {
							if kind, _ := part["kind"].(string); kind == "text" {
								if text, ok := part["text"].(string); ok {
									return text
								}
							}
						}
					}
				}
			}
		}
	}
	return string(body)
}
