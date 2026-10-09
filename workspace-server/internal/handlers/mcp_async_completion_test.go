package handlers

// mcp_async_completion_test.go — #4338, the ASYNC MCP COMPLETION WRITER.
//
// delegate_task_async is the route agents actually use, and until this change it
// had no completion writer at all: `delivered` -> in_progress was the last status
// any code wrote for it. The 6h sweeper deadline was the only thing that ever
// terminalized one, and it terminalizes as FAILED — so flipping
// DELEGATION_LEDGER_WRITE would have pushed "Delegation failed" into the inbox of
// every caller whose delegation SUCCEEDED. That is why asyncMCPCompletionWired
// exists and why the workspace-server refuses to boot while it is false.
//
// The completion writer's hard part is NOT "write completed when the proxy returns
// 2xx". It is deciding WHETHER A 2xx IS AN ANSWER AT ALL — because on this platform
// it very often is not:
//
//	poll-mode target        -> 200 {"status":"queued","delivery_mode":"poll",...}
//	boot turn in flight     -> 200 {"status":"queued","queued":true,"queue_id":...}
//	target busy / settling  -> 202 {"queued":true,"queue_id":...}
//
// None of those carry the target's answer; the work has not started. Terminalizing
// them as `completed` would fire "Delegation completed" with an EMPTY result at the
// caller before the target had read the message — the exact trap
// ledgerStatusForMCP's route parameter was introduced to prevent, arriving through
// the front door instead. So the classifier is a POSITIVE test: terminalize only
// when we can prove we are holding a final answer, and otherwise fall through to
// today's byte-identical `delivered` behaviour.

import (
	"context"
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// TestAsyncMCPAnswer_OnlyAcceptsARealAnswer is the whole safety argument of the
// completion writer, as a table.
//
// Every `false` row here is a case where the async goroutine sees a 2xx with no
// error member and would, under a naive "2xx means done" completion writer,
// terminalize a delegation whose target has not answered — and then tell the caller
// it completed. The three `queued` envelopes are not hypothetical: they are what
// proxyA2ARequest literally returns for poll-mode targets, for a workspace whose
// platform-owned boot turn is in flight, and for a busy/settling target whose
// message was enqueued for drain.
func TestAsyncMCPAnswer_OnlyAcceptsARealAnswer(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantOK  bool
		because string
	}{
		{
			name:    "bare message reply",
			body:    `{"jsonrpc":"2.0","id":"1","result":{"kind":"message","role":"agent","parts":[{"kind":"text","text":"the answer"}]}}`,
			want:    "the answer",
			wantOK:  true,
			because: "the shape every real runtime returns from message/send — this is THE happy path #4338 exists to wire",
		},
		{
			name:    "task with artifacts",
			body:    `{"result":{"kind":"task","status":{"state":"completed"},"artifacts":[{"parts":[{"kind":"text","text":"artifact answer"}]}]}}`,
			want:    "artifact answer",
			wantOK:  true,
			because: "a completed Task carrying its output in artifacts is an answer",
		},
		{
			name:    "poll-mode queued ack",
			body:    `{"status":"queued","delivery_mode":"poll","method":"message/send"}`,
			wantOK:  false,
			because: "the target has no public URL; the platform recorded the request for the agent to PICK UP later. Nothing has run. Terminalizing here tells the caller a delegation completed before the target has even seen it",
		},
		{
			name:    "boot-turn-in-flight queued ack",
			body:    `{"status":"queued","method":"message/send","queued":true,"queue_id":"q-1","queue_depth":2}`,
			wantOK:  false,
			because: "the caller's turn was durably enqueued to avoid interleaving into a platform boot turn — the drain delivers it later",
		},
		{
			name:    "busy/settling enqueue ack",
			body:    `{"queued":true,"queue_id":"q-2","queue_depth":1,"message":"workspace agent busy — request queued, will dispatch when capacity available"}`,
			wantOK:  false,
			because: "202 + queued: the target is mid-restart. The answer arrives via the a2a_queue drain, not here",
		},
		{
			name:    "task still working",
			body:    `{"result":{"kind":"task","status":{"state":"working","message":{"parts":[{"kind":"text","text":"on it, give me a minute"}]}}}}`,
			wantOK:  false,
			because: "THE SUBTLE ONE. a2aresp.Text() happily returns the interim status line, so a text-only check would terminalize with 'on it, give me a minute' as the RESULT and drop the delegation out of awaiting-reply while the target is still working",
		},
		{
			name:    "task submitted",
			body:    `{"result":{"kind":"task","id":"t-1","status":{"state":"submitted"}}}`,
			wantOK:  false,
			because: "accepted, not started",
		},
		{
			name:    "task input-required",
			body:    `{"result":{"kind":"task","status":{"state":"input-required","message":{"parts":[{"kind":"text","text":"which environment?"}]}}}}`,
			wantOK:  false,
			because: "the target is BLOCKED asking a question. It is not done, and 'completed' with a question as the result is the worst possible reading",
		},
		{
			name:    "not json",
			body:    `<html>502 Bad Gateway</html>`,
			wantOK:  false,
			because: "an HTML error page reaching a 2xx arm proves nothing about the delegation",
		},
		{
			name:    "empty body",
			body:    ``,
			wantOK:  false,
			because: "no body is not an answer",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := asyncMCPAnswer([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("asyncMCPAnswer() ok = %v, want %v — %s\n  body: %s",
					ok, c.wantOK, c.because, c.body)
			}
			if c.wantOK && got != c.want {
				t.Fatalf("asyncMCPAnswer() text = %q, want %q — %s", got, c.want, c.because)
			}
		})
	}
}

// TestLedgerStatusForMCP_AsyncCompletedIsTerminal — the mapping half.
//
// The completion writer is worthless if its status does not reach the ledger as a
// TERMINAL state: a row left in-flight is still swept and still false-failed at 6h,
// which is the entire bug.
func TestLedgerStatusForMCP_AsyncCompletedIsTerminal(t *testing.T) {
	got, ok := ledgerStatusForMCP(mcpAsyncRoute, "completed")
	if !ok {
		t.Fatal("the async route's completion status does not map to the ledger at all, so " +
			"the completion writer cannot terminalize anything and the 6h sweeper still " +
			"false-fails every successful async delegation")
	}
	if !IsTerminalDelegationStatus(got) {
		t.Fatalf("async completion maps to %q, which is NOT terminal — the row stays in the "+
			"sweeper's in-flight SELECT and is deadline-failed at 6h anyway", got)
	}
	if got != "completed" {
		t.Fatalf("async completion maps to %q, want %q", got, "completed")
	}
}

// TestAsyncMCPCompletionWired_IsTrue — the interlock constant.
//
// This is the replacement for TestDelegationRolloutInterlock_IsActuallyEngagedToday,
// which asserted the OPPOSITE and was deleted in this same commit by design (it was
// written to fail the moment the constant flipped, so that flipping it required
// reading why). Now that the writer exists, the assertion inverts: the constant must
// be true, or WarnOnPartialDelegationRollout still refuses to boot with
// DELEGATION_LEDGER_WRITE=1 and Phase 2 stays blocked forever.
func TestAsyncMCPCompletionWired_IsTrue(t *testing.T) {
	if !asyncMCPCompletionWired {
		t.Fatal("asyncMCPCompletionWired is false, so the workspace-server still REFUSES TO " +
			"BOOT with DELEGATION_LEDGER_WRITE=1 and Phase 2 remains blocked. #4338's " +
			"completion writer is in this package (completeAsyncMCPDelegation); if it is " +
			"still here, this constant must be true.")
	}
	if reason := delegationRolloutFatalReason(true, asyncMCPCompletionWired); reason != "" {
		t.Fatalf("with the writer wired the interlock still refuses the Phase-2 end state: %s", reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The goroutine, end to end, on sqlmock.
// ─────────────────────────────────────────────────────────────────────────────

// TestMCPHandler_DelegateTaskAsync_AnswerWritesCompleted — the happy path at the
// call site, not just in the classifier. Pins that the goroutine's terminal write is
// `completed` (not `delivered`) and that the answer is persisted to the activity row
// so check_task_status can hand it back.
func TestMCPHandler_DelegateTaskAsync_AnswerWritesCompleted(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("queued", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("completed", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs(sqlmock.AnyArg(), callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		return http.StatusOK, []byte(`{"result":{"kind":"message","parts":[{"kind":"text","text":"done: 42"}]}}`), nil
	}

	if _, err := h.toolDelegateTaskAsync(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "async work",
	}); err != nil {
		t.Fatalf("delegate_task_async returned error: %v", err)
	}
	waitGlobalAsyncForTest()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the async goroutine did not write the expected statements: %v\n"+
			"    A `delivered` write here instead of `completed` means the delegation is "+
			"left in_progress forever and the 6h sweeper false-fails it.", err)
	}
}

// TestMCPHandler_DelegateTaskAsync_QueuedAckStaysDelivered — the negative control at
// the call site. A poll-mode target acks `queued`; the goroutine MUST NOT
// terminalize. This is the regression gate on the trap: if a future edit relaxes the
// classifier to "2xx means done", every poll-mode delegation on the fleet completes
// instantly with an empty answer.
func TestMCPHandler_DelegateTaskAsync_QueuedAckStaysDelivered(t *testing.T) {
	h, mock := newMCPHandler(t)
	callerID := "11111111-1111-1111-1111-111111111111"
	targetID := "22222222-2222-2222-2222-222222222222"
	parentID := "33333333-3333-3333-3333-333333333333"

	expectCanCommunicateSiblings(mock, callerID, targetID, parentID)
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.*'delegation'.*'delegate'`).
		WithArgs(callerID, callerID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("queued", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("delivered", "", callerID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.a2aProxy = func(ctx context.Context, workspaceID string, body []byte, proxyCallerID string, logActivity bool) (int, []byte, error) {
		return http.StatusOK, []byte(`{"status":"queued","delivery_mode":"poll","method":"message/send"}`), nil
	}

	if _, err := h.toolDelegateTaskAsync(context.Background(), callerID, map[string]interface{}{
		"workspace_id": targetID,
		"task":         "async work",
	}); err != nil {
		t.Fatalf("delegate_task_async returned error: %v", err)
	}
	waitGlobalAsyncForTest()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a poll-mode `queued` ack did not leave the delegation at `delivered`: %v\n"+
			"    Terminalizing an ack fires 'Delegation completed' with an EMPTY result at "+
			"the caller before the target has read the message.", err)
	}
}

// TestCompleteAsyncMCPDelegation_PersistsTheAnswerForCheckTaskStatus — check_task_status
// is the async agent's own polling route and it reads activity_logs.response_body.
// A completion that flips the status but drops the answer leaves that tool reporting
// `completed` with nothing in it.
func TestCompleteAsyncMCPDelegation_PersistsTheAnswerForCheckTaskStatus(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("completed", "", "ws-src", "del-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE activity_logs.*response_body`).
		WithArgs(sqlmock.AnyArg(), "ws-src", "del-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	completeAsyncMCPDelegation(context.Background(), mockDB, "ws-src", "ws-tgt", "del-1", "the answer")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("completion did not persist the answer to the activity row: %v\n"+
			"    check_task_status reads activity_logs.response_body — a completion that "+
			"drops the answer leaves the async agent's own polling route reporting "+
			"`completed` with nothing in it.", err)
	}
}
