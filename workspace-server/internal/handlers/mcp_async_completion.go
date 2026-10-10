package handlers

// mcp_async_completion.go — #4338, THE ASYNC MCP COMPLETION WRITER.
//
// delegate_task_async is the route agents actually use, and until this file existed
// it had no completion writer at all. Its lifecycle ended at
//
//	insert          -> activity_logs 'queued'    (ledger: queued)
//	proxy succeeds  -> activity_logs 'delivered' (ledger: in_progress)  <- LAST WRITE
//
// Nothing moved it to `completed`. The a2a_queue drain stitch could not: it matches
// activity_logs rows with method='delegate_result', and MCP rows are method='delegate'.
// The agent-facing /delegations/:id/update endpoint could, but nothing on this path
// calls it.
//
// So with DELEGATION_LEDGER_WRITE on, every async MCP delegation would sit at
// in_progress until its 6h deadline and the sweeper would deadline-fail it — INCLUDING
// every one whose target finished the work an hour in — and push a false "Delegation
// failed" into each caller's inbox. A fleet-wide false-failure event on the busiest
// delegation route, six hours after a flag flip that appeared to go fine, with nothing
// connecting the two. That is what asyncMCPCompletionWired refuses to boot over, and
// this file is what lets it be flipped.
//
// THE HARD PART IS NOT WRITING `completed`. It is deciding whether the response in
// hand IS AN ANSWER — see asyncMCPAnswer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
)

// nonFinalA2ATaskStates — A2A Task states in which the target has NOT produced its
// answer. A Task in one of these is still owed one; terminalizing it is a lie.
//
// `input-required` and `auth-required` are the sharp ones: the target is BLOCKED
// asking the caller something, and its status.message carries that question as text.
// A text-only check would lift the question out and file it as the RESULT.
var nonFinalA2ATaskStates = map[string]bool{
	"submitted":      true,
	"working":        true,
	"input-required": true,
	"auth-required":  true,
	"unknown":        true,
}

// asyncMCPAnswer reports whether an A2A response body carries the target's FINAL
// ANSWER, and returns it.
//
// THIS PREDICATE IS THE SAFETY ARGUMENT OF THE WHOLE FEATURE, and it is deliberately
// POSITIVE — "prove we are holding an answer" — rather than "2xx and no error member".
// On this platform a 2xx very often is not an answer:
//
//	poll-mode target        -> 200 {"status":"queued","delivery_mode":"poll",...}
//	platform boot turn      -> 200 {"status":"queued","queued":true,"queue_id":...}
//	target busy / settling  -> 202 {"queued":true,"queue_id":...}
//
// Every one of those is a synthetic ACK minted by proxyA2ARequest. The target has not
// read the message; the real answer arrives later through the a2a_queue drain. A
// completion writer that accepted them would fire "Delegation completed" with an EMPTY
// result at the caller before the work had started, drop the delegation out of the
// awaiting-reply count, and leave the agent proceeding on a result it never received.
// That is the same failure ledgerStatusForMCP's `route` parameter was introduced to
// prevent — arriving through the front door instead of via a careless edit.
//
// The tell is structural and cheap: a JSON-RPC ANSWER has a `result` member; the
// synthetic acks have no `result` at all. On top of that, a Task whose status.state is
// non-final is explicitly not done even though it does have one.
//
// Anything this returns false for keeps today's byte-identical `delivered` ->
// in_progress behaviour, which the 6h sweeper already handles correctly (see the
// never-answers negative control). So the classifier can only ADD correct
// terminalizations; it cannot invent one.
func asyncMCPAnswer(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		// Not JSON at all (an HTML error page that reached a 2xx arm, a truncated
		// body). It proves nothing about the delegation.
		return "", false
	}
	raw, hasResult := env["result"]
	if !hasResult {
		// No `result` member: an ack envelope, not a JSON-RPC answer.
		return "", false
	}
	// A Task that says it is still working is not an answer, whatever text it carries.
	var result struct {
		Status *struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err == nil &&
		result.Status != nil && nonFinalA2ATaskStates[result.Status.State] {
		return "", false
	}
	// extractA2AText walks the same a2aresp shape matrix the SYNC route uses, plus the
	// error precedence and raw-JSON diagnostic fallback — so both routes report the
	// same answer for the same body rather than drifting into two readings of one
	// protocol.
	return extractA2AText(body), true
}

// completeAsyncMCPDelegation terminalizes a SUCCESSFUL async MCP delegation AND TELLS
// THE CALLER — the exact mirror of failAsyncMCPDelegation, and for the same reason:
// doing only the first half is a silent success, doing only the second is unbacked.
//
// #4338's scope note is explicit that the failure half (core#4316's
// failAsyncMCPDelegation) and this completion half are opposite directions, and that
// closing the issue by wiring only one would leave the other silent. Both now live
// side by side so an edit to one is next to the other.
//
// THE CALLER IS NOT BLOCKING. delegate_task_async handed the agent a task_id and
// returned; the agent went off and did other things. An inbox reply is the ONLY way it
// can ever learn its delegation finished — and the only way it receives the answer it
// delegated FOR.
//
// Reply gated on mayReply(): the ledger arbitrates who owns the single notification,
// and when it cannot arbitrate (dark, or no row) nobody else will speak, so we do.
func completeAsyncMCPDelegation(ctx context.Context, db *sql.DB, callerID, targetID, delegationID, resultText string) {
	authority := updateMCPDelegationStatusWithResult(ctx, db, mcpAsyncRoute,
		callerID, delegationID, "completed", "", resultText)
	if !mayReply(authority) {
		return
	}
	if emitTerminalDelegationReplyWithResult(ctx, callerID, targetID, delegationID,
		"completed", "", resultText) {
		log.Printf("MCP delegate_task_async %s: terminal reply to caller FAILED — the "+
			"caller will never learn this delegation finished, or what it returned",
			delegationID)
	}
}

// recordAsyncMCPResultBody stores the target's answer on the delegation's activity
// row. check_task_status — the async agent's OWN polling route — reads
// activity_logs.response_body, so a completion that flips the status and drops the
// answer leaves that tool reporting `completed` with nothing in it.
//
// Separate from the status UPDATE on purpose: that statement is pinned byte-for-byte
// by the strict-sqlmock suite and is shared with the sync and failure paths, so
// widening it to carry a result would churn every one of those expectations for a
// column only this path writes.
func recordAsyncMCPResultBody(ctx context.Context, db *sql.DB, workspaceID, delegationID, resultText string) {
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"text":          resultText,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("MCP delegation %s: json.Marshal result body failed: %v", delegationID, marshalErr)
		return
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE activity_logs
		SET response_body = $1::jsonb
		WHERE workspace_id = $2
		  AND method = 'delegate'
		  AND request_body->>'delegation_id' = $3
	`, string(respJSON), workspaceID, delegationID); err != nil {
		// Best-effort like every other write on this path: the ledger row and the inbox
		// reply both already carry the answer, so losing this one costs
		// check_task_status its result, not the caller its notification.
		log.Printf("MCP delegation %s: result-body update failed: %v", delegationID, err)
	}
}
