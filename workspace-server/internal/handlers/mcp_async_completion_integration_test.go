//go:build integration
// +build integration

// mcp_async_completion_integration_test.go — #4338, THE ASYNC MCP COMPLETION
// WRITER, against a real Postgres.
//
// These are the acceptance tests the issue asks for by name: "fire an async MCP
// delegation, let the target complete it, and assert the `delegations` row reaches
// `completed` and the caller receives exactly one `delegate_result`. Negative-control
// it (a target that never answers must still deadline-fail and notify once)."
//
// They run against a real database and not sqlmock, for a reason this subsystem has
// already paid for once. The caller-reply channel's `NULLIF($3,'')::uuid` cast bug
// meant EVERY reply insert would have failed against real Postgres — and every
// sqlmock test passed anyway, because sqlmock matches SQL text and arguments and
// does not type-check against a schema. The reply path these tests exercise is that
// same one, and until this suite ran it, it had never once executed successfully in
// any environment: the flag that gates it is set nowhere, so the statement had never
// been sent to a real server. countInboxReplies() is a SELECT count(*) against the
// actual table, so a reply that fails to insert counts as 0 and FAILS the test
// instead of scrolling past as a best-effort log line.
//
// Helpers (integrationDB, seedDrainScenario, countInboxReplies, readLedgerRow) live
// in delegation_ledger_integration_test.go; newTestSweeper in
// delegation_sweeper_test.go.

package handlers

import (
	"context"
	"testing"
)

// TestIntegration_AsyncMCPCompletion_TerminalizesAndRepliesExactlyOnce — #4338's
// stated acceptance criterion, verbatim.
//
// Before this change the async route's last write was `delivered` -> in_progress and
// nothing on earth moved it forward. With DELEGATION_LEDGER_WRITE on, that row sits
// in the sweeper's in-flight SELECT until its 6h deadline and is then marked FAILED —
// and the caller, whose target finished the work an hour in, is told "Delegation
// failed". On the fleet's most-used delegation route, all of them, at once, six hours
// after a flag flip that appeared to go fine.
func TestIntegration_AsyncMCPCompletion_TerminalizesAndRepliesExactlyOnce(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-complete"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)
	// The state the goroutine is in when the target's answer arrives: dispatched,
	// accepted, ledger says in_progress. This is exactly where every async MCP
	// delegation used to stop, permanently.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET status = 'in_progress' WHERE delegation_id = $1`,
		delegationID); err != nil {
		t.Fatalf("seed in_progress: %v", err)
	}

	completeAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID,
		"the target's actual answer")

	status, preview, _ := readLedgerRow(t, conn, delegationID)
	if status != "completed" {
		t.Fatalf("ledger status = %q, want completed.\n"+
			"    An async MCP delegation that is not terminalized stays in the sweeper's "+
			"in-flight SELECT, and six hours later the sweeper marks it failed and pushes "+
			"'Delegation failed' to a caller whose delegation SUCCEEDED. That single "+
			"outcome is the entire reason asyncMCPCompletionWired exists.", status)
	}
	if preview != "the target's actual answer" {
		t.Fatalf("result_preview = %q, want the target's answer. A terminal row with no "+
			"result lets the dashboard and the digest see that a delegation ended but not "+
			"what it ended WITH.", preview)
	}
	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("the caller received %d delegate_result replies, want exactly 1.\n"+
			"    0 = the delegation is now TERMINAL, so it has dropped out of the caller's "+
			"awaiting-reply count, and nothing told the caller anything. delegate_task_async "+
			"does NOT block — an inbox reply is the only channel that exists. The agent "+
			"asked, the peer answered, and the platform swallowed it.\n"+
			"    >1 = the caller's agent is told twice about one delegation.", got)
	}
	// The answer must actually be IN the reply, not merely the fact of one.
	var replyText string
	if err := conn.QueryRowContext(context.Background(), `
		SELECT COALESCE(response_body->>'text','') FROM activity_logs
		 WHERE activity_type = 'a2a_receive' AND method = 'delegate_result'
		   AND response_body->>'delegation_id' = $1
	`, delegationID).Scan(&replyText); err != nil {
		t.Fatalf("read reply body: %v", err)
	}
	if replyText != "the target's actual answer" {
		t.Fatalf("the delegate_result the caller reads carries %q, want the target's "+
			"answer. A completion notice with an empty body tells the agent its delegation "+
			"ended and withholds the thing it delegated FOR.", replyText)
	}
}

// TestIntegration_AsyncMCPCompletion_RetryDoesNotReplyTwice — the single-reply rule on
// the new path. The detached goroutine can be re-entered (a retry, a redelivery); the
// ledger's compare-and-swap is what makes the second completion a no-op rather than a
// second notification.
func TestIntegration_AsyncMCPCompletion_RetryDoesNotReplyTwice(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-complete-twice"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	completeAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, "answer")
	completeAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, "answer")

	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("two completion writes produced %d replies, want exactly 1. Without the "+
			"ledger's CAS the caller's agent is told twice that one delegation ended.", got)
	}
}

// TestIntegration_AsyncMCPCompletion_DoesNotOverrideAFailureAlreadyReported — the
// ordering guard between #4338's half and core#4316's half.
//
// The failure writer (failAsyncMCPDelegation) and the completion writer added here now
// both terminalize the same row. If a completion could revise a row the failure path
// already reported, the caller would read BOTH "Delegation failed" and "Delegation
// completed" for one delegation, with `delegations` permanently disagreeing with what
// the caller was told and no way to know which is true.
func TestIntegration_AsyncMCPCompletion_DoesNotOverrideAFailureAlreadyReported(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-fail-then-complete"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	failAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, "target_offline")
	completeAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, "late answer")

	if status, _, _ := readLedgerRow(t, conn, delegationID); status != "failed" {
		t.Fatalf("ledger status = %q after fail-then-complete, want the failure to stand — "+
			"terminal states are forward-only", status)
	}
	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("fail-then-complete produced %d replies, want exactly 1", got)
	}
}

// TestIntegration_AsyncMCPTargetNeverAnswers_DeadlineFailsAndNotifiesOnce — #4338's
// NEGATIVE CONTROL, named in the issue.
//
// The completion writer must not be a rubber stamp. A delegation whose target never
// answers — a poll-mode peer that never picks the message up, a container that dies
// mid-task — has to reach the caller as a FAILURE, exactly once, and must NOT be
// quietly terminalized as `completed` by the writer this change adds.
//
// It is also what proves the writer is not vacuous: it drives the same async path with
// the same helpers and changes only the target's behaviour, and gets the opposite
// outcome.
func TestIntegration_AsyncMCPTargetNeverAnswers_DeadlineFailsAndNotifiesOnce(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-never-answers"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	// The target ACCEPTED the task — the goroutine got its 2xx — and then went
	// silent. The classifier saw a `queued` ack, not an answer, so no completion was
	// written and the row is exactly where the async route leaves an unanswered
	// delegation.
	if _, ok := asyncMCPAnswer([]byte(`{"status":"queued","delivery_mode":"poll","method":"message/send"}`)); ok {
		t.Fatal("a poll-mode queued ACK was classified as an ANSWER. The completion writer " +
			"would terminalize a delegation the target has not even read, tell the caller " +
			"it completed, and hand it an empty result. This control exists precisely " +
			"because a completion writer that accepts everything is worse than no " +
			"completion writer at all.")
	}
	if _, err := conn.ExecContext(context.Background(), `
		UPDATE delegations SET status = 'in_progress',
		                       deadline = now() - interval '1 minute'
		 WHERE delegation_id = $1`, delegationID); err != nil {
		t.Fatalf("arm deadline: %v", err)
	}

	res := newTestSweeper(nil).Sweep(context.Background())

	if res.DeadlineFailures != 1 {
		t.Fatalf("the sweep counted %d deadline failures, want exactly 1 — a target that "+
			"never answers must still be given up on", res.DeadlineFailures)
	}
	status, _, errDetail := readLedgerRow(t, conn, delegationID)
	if status != "failed" {
		t.Fatalf("ledger status = %q, want failed. An unanswered delegation that the "+
			"completion writer had silently marked `completed` would be the #4314 lie with "+
			"a nicer label on it.", status)
	}
	if errDetail == "" {
		t.Error("the failure carries no error_detail — the caller is told it died and not why")
	}
	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("the caller received %d delegate_result replies for a delegation that was "+
			"never answered, want exactly 1. 0 is the silent death (#4314): the row is "+
			"terminal, so it is gone from the awaiting-reply count, and nobody said "+
			"anything.", got)
	}
}
