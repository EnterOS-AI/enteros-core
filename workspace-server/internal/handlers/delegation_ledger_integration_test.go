//go:build integration
// +build integration

// delegation_ledger_integration_test.go — REAL Postgres integration tests
// for the RFC #2829 ledger writes.
//
// Run with:
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   sleep 4
//   psql ... < workspace-server/migrations/049_delegations.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/handlers/**.
//
// Why these are NOT plain unit tests
// ----------------------------------
// The strict-sqlmock unit tests in delegation_ledger_writes_test.go pin
// which SQL statements fire — they are fast and let us iterate without
// a DB. But sqlmock CANNOT detect bugs that depend on the ROW STATE
// after the SQL runs. The result_preview-lost bug shipped to staging in
// PR #2854 because every unit test was satisfied with "an UPDATE
// statement fired" — none verified the row's preview field landed.
//
// These integration tests close that gap by booting a real Postgres,
// running the production helpers, and SELECTing the row to verify the
// observable state matches the expected outcome.

package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	mdb "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	_ "github.com/lib/pq"
)

// integrationDB returns the configured integration-test connection or
// skips the test if INTEGRATION_DB_URL is unset. Local devs run the
// docker-postgres incantation in the file header; CI's workflow sets the
// env var via a service container.
//
// NOT SAFE FOR `t.Parallel()`. Each call hot-swaps the package-level
// `mdb.DB` and restores via `t.Cleanup`. If two tests using this helper
// run in parallel they race on the global; tests that need parallelism
// should drive a local `*sql.DB` they own and pass it into helpers
// directly rather than going through the package global.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// Each test gets a fresh table state — fail loud if cleanup fails so
	// a bad test doesn't pollute the next one.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, err := conn.ExecContext(ctx2, `DELETE FROM delegations`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// Wire the package-level db.DB so production helpers (recordLedgerInsert,
	// recordLedgerStatus) see the same connection.
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() {
		mdb.DB = prev
		conn.Close()
	})
	return conn
}

// readLedgerRow returns (status, result_preview, error_detail) for the
// given delegation_id, or fails the test on miss.
func readLedgerRow(t *testing.T, conn *sql.DB, id string) (status, preview, errorDetail string) {
	t.Helper()
	var prev, errDet sql.NullString
	err := conn.QueryRowContext(context.Background(),
		`SELECT status, result_preview, error_detail FROM delegations WHERE delegation_id = $1`, id,
	).Scan(&status, &prev, &errDet)
	if err != nil {
		t.Fatalf("readLedgerRow(%s): %v", id, err)
	}
	return status, prev.String, errDet.String
}

// TestIntegration_ResultPreviewPreservedThroughCompletion is the
// regression gate for the bug that shipped in PR #2854 + was caught in
// self-review: when both the inner SetStatus(completed, "", "") (from
// updateDelegationStatus) and an outer SetStatus(completed, "", preview)
// fire, the SECOND one is a same-status no-op — order matters.
//
// The fix in delegation.go calls the WITH-PREVIEW SetStatus FIRST so the
// outer write lands the preview, and the inner becomes the no-op.
//
// This test fires the call sequence in the corrected order and asserts
// the row's result_preview matches.
//
// If a future refactor reverses the order, this test fails on a real
// Postgres — which sqlmock would have missed.
func TestIntegration_ResultPreviewPreservedThroughCompletion(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-preview-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"
	expectedPreview := "the long-running task's final answer"

	// Mirror the production call sequence the FIXED code path uses.
	// executeDelegation flow:
	//   1. insertDelegationRow → recordLedgerInsert (status=queued)
	//   2. updateDelegationStatus("dispatched", "") at the start of execute,
	//      so the row is at status=dispatched by completion time
	//   3. recordLedgerStatus("completed", "", preview)   ← outer FIRST (the fix)
	//   4. updateDelegationStatus("completed", "") inside, which calls
	//      recordLedgerStatus("completed", "", "")        ← inner same-status no-op
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	recordLedgerStatus(context.Background(), id, "completed", "", expectedPreview)
	recordLedgerStatus(context.Background(), id, "completed", "", "")

	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview != expectedPreview {
		t.Errorf("result_preview lost: want %q, got %q", expectedPreview, preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty: got %q", errDet)
	}
}

// Same-status terminal replays remain idempotent, but if the first terminal
// write lacked result_preview, a later same-status replay carrying the preview
// should fill that missing field once. This protects legacy call ordering and
// mirrors the failure-path error_detail repair.
func TestIntegration_ResultPreviewSameStatusReplayFillsMissingPreview(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-preview-2"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	// Legacy sequence: queued → dispatched → completed (no preview) →
	// completed (preview). The second completed replay should repair the
	// missing preview without changing status.
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	recordLedgerStatus(context.Background(), id, "completed", "", "")
	recordLedgerStatus(context.Background(), id, "completed", "", "the answer")

	_, preview, _ := readLedgerRow(t, conn, id)
	if preview != "the answer" {
		t.Errorf("same-status replay should fill missing preview; got %q", preview)
	}
}

// TestIntegration_FailedTransitionCapturesErrorDetail — error_detail is
// the failure-path equivalent of result_preview. The legacy path calls
// SetStatus(failed, errorDetail, "") via updateDelegationStatus; no
// outer call exists today (no observed bug). This test pins that
// error_detail lands as expected, so a future refactor adding an outer
// call must consciously preserve the field — same lesson as the preview
// bug, just on the failure path.
func TestIntegration_FailedTransitionCapturesErrorDetail(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-fail-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"
	expectedError := "callee unreachable: connection refused"

	// queued → failed is allowed by allowedTransitions (the failure-on-
	// dispatch case) so this exercises a real production path.
	recordLedgerInsert(context.Background(), caller, callee, id, "the question", "")
	recordLedgerStatus(context.Background(), id, "failed", expectedError, "")

	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet != expectedError {
		t.Errorf("error_detail: want %q, got %q", expectedError, errDet)
	}
	if preview != "" {
		t.Errorf("result_preview should be empty on failure: got %q", preview)
	}
}

// TestIntegration_Sweeper_DeadlineExceededIsMarkedFailed — real-Postgres
// gate for the RFC #2829 PR-3 stuck-task sweeper. Inserts a row with a
// past deadline, runs Sweep, asserts the row is now `failed` with
// `deadline exceeded by sweeper` in error_detail.
//
// sqlmock unit tests pinned the SQL fired but couldn't observe the
// real ON CONFLICT / index-scan behavior on the partial inflight
// index. Real Postgres catches:
//   - deadline timestamp comparison is correct under tz boundaries
//   - the partial index actually serves the WHERE clause
//   - SetStatus's terminal forward-only protection holds across the
//     sweep + concurrent-write race
func TestIntegration_Sweeper_DeadlineExceededIsMarkedFailed(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-sweeper-deadline-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	// Insert + transition to dispatched (otherwise queued→failed is
	// allowed but doesn't exercise the in-flight scan accurately).
	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")

	// Force the deadline into the past — Insert defaults to now+6h, so
	// we override. We don't touch last_heartbeat: the sweeper checks
	// deadline FIRST (it's the stronger statement) and short-circuits
	// before evaluating heartbeat staleness, so a NULL or stale beat is
	// irrelevant for the deadline-failure path.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET deadline = now() - interval '1 minute' WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("backdate deadline: %v", err)
	}

	res := newTestSweeper(nil).Sweep(context.Background())
	if res.DeadlineFailures != 1 {
		t.Errorf("expected 1 deadline failure, got %+v", res)
	}
	status, _, errDet := readLedgerRow(t, conn, id)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet != "deadline exceeded by sweeper" {
		t.Errorf("error_detail: %q", errDet)
	}
}

// TestIntegration_Sweeper_StaleHeartbeatIsMarkedStuck — heartbeat
// staleness path. Insert + dispatch + backdate last_heartbeat past the
// 10× threshold; Sweep should mark the row stuck.
func TestIntegration_Sweeper_StaleHeartbeatIsMarkedStuck(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	// Tighten threshold so the test is deterministic + fast.
	t.Setenv("DELEGATION_STUCK_THRESHOLD_S", "10")

	id := "integ-sweeper-stuck-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	recordLedgerStatus(context.Background(), id, "in_progress", "", "")

	// Backdate last_heartbeat past the 10s threshold; deadline still in
	// future so deadline check shouldn't fire.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET last_heartbeat = now() - interval '60 seconds' WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	res := newTestSweeper(nil).Sweep(context.Background())
	if res.StuckMarked != 1 {
		t.Errorf("expected 1 stuck mark, got %+v", res)
	}
	status, _, errDet := readLedgerRow(t, conn, id)
	if status != "stuck" {
		t.Errorf("status: want stuck, got %q", status)
	}
	if !strings.Contains(errDet, "no heartbeat for") {
		t.Errorf("error_detail should contain 'no heartbeat for'; got %q", errDet)
	}
}

// TestIntegration_Sweeper_HealthyRowsNotTouched — sanity: rows with a
// fresh heartbeat AND a future deadline are left alone. Confirms the
// partial inflight index scan + per-row branching don't false-positive
// against well-behaved delegations.
func TestIntegration_Sweeper_HealthyRowsNotTouched(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-sweeper-healthy-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task", "")
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	// Fresh heartbeat = now()
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET last_heartbeat = now() WHERE delegation_id = $1`, id,
	); err != nil {
		t.Fatalf("set heartbeat: %v", err)
	}

	res := newTestSweeper(nil).Sweep(context.Background())
	if res.DeadlineFailures != 0 || res.StuckMarked != 0 {
		t.Errorf("healthy row touched; result: %+v", res)
	}
	status, _, _ := readLedgerRow(t, conn, id)
	if status != "dispatched" {
		t.Errorf("status changed unexpectedly: %q", status)
	}
}

// TestIntegration_FullLifecycle_QueuedToDispatchedToCompleted — pins the
// happy-path lifecycle. INSERT lands the row at queued; SetStatus moves
// it through dispatched and into completed with preview. After the
// terminal transition, no further state change is possible via
// SetStatus (forward-only protection).
func TestIntegration_FullLifecycle_QueuedToDispatchedToCompleted(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	id := "integ-deleg-lifecycle-1"
	caller := "11111111-1111-1111-1111-111111111111"
	callee := "22222222-2222-2222-2222-222222222222"

	recordLedgerInsert(context.Background(), caller, callee, id, "task body", "")
	if status, _, _ := readLedgerRow(t, conn, id); status != "queued" {
		t.Errorf("after Insert: status want queued, got %q", status)
	}
	recordLedgerStatus(context.Background(), id, "dispatched", "", "")
	if status, _, _ := readLedgerRow(t, conn, id); status != "dispatched" {
		t.Errorf("after dispatched: status want dispatched, got %q", status)
	}
	recordLedgerStatus(context.Background(), id, "completed", "", "the result")
	status, preview, _ := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("after completed: status want completed, got %q", status)
	}
	if preview != "the result" {
		t.Errorf("preview after completed: want %q, got %q", "the result", preview)
	}

	// Forward-only: trying to revise to failed should silently no-op
	// (recordLedgerStatus swallows ErrInvalidTransition).
	recordLedgerStatus(context.Background(), id, "failed", "post-hoc revision", "")
	status, preview, errDet := readLedgerRow(t, conn, id)
	if status != "completed" {
		t.Errorf("forward-only broken: status changed to %q", status)
	}
	if preview != "the result" {
		t.Errorf("preview clobbered by failed revision: %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail clobbered by failed revision: %q", errDet)
	}
}

// ---------------------------------------------------------------------------
// SINGLE-REPLY AUTHORITY (the drain path)
//
// The contract: every TERMINAL delegation transition emits exactly one
// delegate_result into the caller's inbox. Not zero, not two.
//
// These live here, against a real Postgres, and not in sqlmock, on purpose.
// sqlmock's ExpectationsWereMet() asserts that EXPECTED calls fired — it can
// never detect an EXTRA one. "The drain must not reply a second time" is
// therefore unobservable in sqlmock: the assertion passes whether or not the
// bug is present. Counting the rows the agent will actually read is a number
// that MOVES when the fix is reverted.

// seedDrainScenario creates the delegation ledger row plus the `queued`
// activity_logs placeholder that the drain's stitch UPDATE matches on.
func seedDrainScenario(t *testing.T, conn *sql.DB, caller, callee, delegationID string) {
	t.Helper()
	// HERMETIC BY CONSTRUCTION. These tests COUNT rows, and the integration DB is
	// not reset between runs — CI reuses a service container across a job, and a
	// local `go test` run against a long-lived Postgres accumulates. Seeded with
	// fixed ids and no cleanup, the first run passes and every run after it counts
	// the previous run's replies too: "want 1, got 5". A test that is only correct
	// on a virgin database is a test that will lie to the next person who runs it.
	// So each scenario deletes its own delegation's rows before seeding them.
	for _, q := range []string{
		`DELETE FROM activity_logs WHERE response_body->>'delegation_id' = $1`,
		`DELETE FROM delegations WHERE delegation_id = $1`,
	} {
		if _, err := conn.ExecContext(context.Background(), q, delegationID); err != nil {
			t.Fatalf("clean prior state for %s: %v", delegationID, err)
		}
	}
	// activity_logs.workspace_id is FK -> workspaces(id), so the two peers must
	// exist before any row can reference them.
	//
	// AND THEY MUST BE REMOVED AGAIN. The integration DB is long-lived (CI reuses a
	// service container across a job; a local run reuses one for days), so a fixture
	// that is only ever inserted ACCUMULATES. Leaving these behind made unrelated tests
	// — TestIntegration_SameOrg_RealCTE_ResolvesAncestorChain,
	// TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError — fail in the full
	// suite while passing in isolation, which is the classic shape of a leak being
	// misread as a flake. Cleaned up at the SOURCE, per-test, rather than swept later.
	for _, ws := range []string{caller, callee} {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')
			 ON CONFLICT (id) DO NOTHING`, ws, "ws-"+ws[:8]); err != nil {
			t.Fatalf("seed workspace %s: %v", ws, err)
		}
	}
	t.Cleanup(func() {
		// ON DELETE CASCADE takes activity_logs with it; delegations is cleaned by
		// delegation_id above and again here for the row this scenario created.
		for _, ws := range []string{caller, callee} {
			if _, err := conn.ExecContext(context.Background(),
				`DELETE FROM workspaces WHERE id = $1`, ws); err != nil {
				t.Logf("cleanup workspace %s: %v", ws, err)
			}
		}
		if _, err := conn.ExecContext(context.Background(),
			`DELETE FROM delegations WHERE delegation_id = $1`, delegationID); err != nil {
			t.Logf("cleanup delegation %s: %v", delegationID, err)
		}
	})
	NewDelegationLedger(conn).Insert(context.Background(), InsertOpts{
		DelegationID: delegationID,
		CallerID:     caller,
		CalleeID:     callee,
		TaskPreview:  "do the thing",
	})
	// The caller-side placeholder row the async delegate flow writes.
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (
			workspace_id, activity_type, method, source_id, target_id,
			summary, status, response_body
		) VALUES ($1, 'delegation', 'delegate_result', $1, $2, 'Delegation queued', 'queued', $3::jsonb)
	`, caller, callee, `{"delegation_id":"`+delegationID+`"}`); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}
}

// countInboxReplies counts the rows the CALLER'S AGENT actually reads — the
// a2a_receive family. The activity_type='delegation' rows are the dashboard
// ledger and are NOT replies; counting those instead is the exact confusion that
// produced an "unread replies" number nothing could ever decrement.
func countInboxReplies(t *testing.T, conn *sql.DB, delegationID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(), `
		SELECT count(*) FROM activity_logs
		 WHERE activity_type = 'a2a_receive'
		   AND method        = 'delegate_result'
		   AND response_body->>'delegation_id' = $1
	`, delegationID).Scan(&n); err != nil {
		t.Fatalf("count inbox replies: %v", err)
	}
	return n
}

// TestIntegration_DrainReplies_ExactlyOnceOnTransition — the happy path this whole
// branch turns on. The target was settling, the platform HELD the message in
// a2a_queue, and the answer is only now being delivered. Before this change the
// drain terminalized the ledger and told nobody: on main the row simply stayed
// `queued` (wrong, but at least still visible as "awaiting reply"); with the
// ledger fixed and no reply, it would leave the in-flight set and become INVISIBLE.
func TestIntegration_DrainReplies_ExactlyOnceOnTransition(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	conn := integrationDB(t)

	caller := "33333333-3333-3333-3333-333333333333"
	callee := "44444444-4444-4444-4444-444444444444"
	id := "deleg-drain-once"
	seedDrainScenario(t, conn, caller, callee, id)

	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(), caller, callee, id,
		[]byte(`{"result":{"parts":[{"text":"the answer"}]}}`))

	if status, _, _ := readLedgerRow(t, conn, id); status != "completed" {
		t.Fatalf("drain left the ledger row %q, want completed", status)
	}
	if n := countInboxReplies(t, conn, id); n != 1 {
		t.Fatalf("the caller received %d delegate_result replies, want exactly 1. "+
			"A drained delegation whose answer never reaches the caller's inbox is a "+
			"delegation that silently vanished (#4314) — and this is the settling-target "+
			"path, the case the caller is most likely to be hanging on.", n)
	}
}

// TestIntegration_DrainDoesNotReply_WhenTheTransitionWasRefused — the double-notify
// this gate exists to prevent.
//
// The sweeper deadline-failed the delegation at 6h and told the caller
// ("Delegation failed"). It writes a NEW terminal row; the original `queued`
// placeholder is untouched. So when the late answer finally drains, the stitch
// UPDATE still matches that placeholder — but the LEDGER row is already terminal,
// the CAS is refused, and this drain transitioned nothing.
//
// It therefore owns no reply. Push one anyway and the caller's agent reads BOTH
// "Delegation failed" and "Delegation completed" for a single delegation, with no
// way to tell which is current.
func TestIntegration_DrainDoesNotReply_WhenTheTransitionWasRefused(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	conn := integrationDB(t)

	caller := "55555555-5555-5555-5555-555555555555"
	callee := "66666666-6666-6666-6666-666666666666"
	id := "deleg-late-drain"
	seedDrainScenario(t, conn, caller, callee, id)

	// The sweeper already gave up on it and notified the caller.
	if _, err := NewDelegationLedger(conn).SetStatus(context.Background(),
		id, "failed", "deadline exceeded", ""); err != nil {
		t.Fatalf("seed the sweeper's terminal transition: %v", err)
	}
	before := countInboxReplies(t, conn, id)

	// ...and NOW the answer arrives.
	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(), caller, callee, id,
		[]byte(`{"result":{"parts":[{"text":"sorry I am late"}]}}`))

	if after := countInboxReplies(t, conn, id); after != before {
		t.Fatalf("a drain that transitioned NOTHING still notified the caller: "+
			"%d replies before, %d after. The sweeper already told this caller the "+
			"delegation FAILED; a second, contradictory 'completed' reply for the same "+
			"delegation breaks single-reply authority and the agent cannot tell which "+
			"is current.", before, after)
	}
	if status, _, _ := readLedgerRow(t, conn, id); status != "failed" {
		t.Errorf("the late drain overwrote a TERMINAL ledger row: now %q, want failed", status)
	}
}

// TestIntegration_DrainStitch_UnattributableDrain_LeavesLedgerAlone — the stitch
// matched NO activity_logs placeholder, so this drain cannot be attributed to that
// delegation. Terminalizing the ledger anyway would mark a delegation completed on
// the strength of a response we could not tie to it.
//
// Real Postgres, not sqlmock: this is a "must NOT happen" assertion, and
// ExpectationsWereMet() can never detect an EXTRA call, so the sqlmock version of
// this test passed with the `rows == 0` early return DELETED. Row state is a witness
// that moves.
func TestIntegration_DrainStitch_UnattributableDrain_LeavesLedgerAlone(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	conn := integrationDB(t)

	caller := "77777777-7777-7777-7777-777777777777"
	callee := "88888888-8888-8888-8888-888888888888"
	id := "deleg-unattributable"
	seedDrainScenario(t, conn, caller, callee, id)

	// Remove the placeholder: nothing for the stitch UPDATE to match.
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM activity_logs WHERE response_body->>'delegation_id' = $1`, id); err != nil {
		t.Fatalf("drop placeholder: %v", err)
	}

	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(), caller, callee, id,
		[]byte(`{"result":{"parts":[{"text":"whose answer is this?"}]}}`))

	if status, _, _ := readLedgerRow(t, conn, id); status != "queued" {
		t.Fatalf("an unattributable drain terminalized the ledger anyway: status=%q, want queued. "+
			"It would be marking a delegation completed on the strength of a response it "+
			"could not attribute to that delegation.", status)
	}
	if n := countInboxReplies(t, conn, id); n != 0 {
		t.Fatalf("an unattributable drain sent the caller %d replies, want 0", n)
	}
}

// TestIntegration_DrainStitch_FlagOff_TouchesNothing — THE NO-OP CLAIM.
//
// This PR's central safety argument is that Phase 1 changes nothing on the fleet
// while DELEGATION_LEDGER_WRITE is dark. The sqlmock test that used to back that
// claim was VACUOUS: deleting the ledgerWritesEnabled() gate outright left it green.
// The most important claim in the change set was pinned by a test that could not fail.
func TestIntegration_DrainStitch_FlagOff_TouchesNothing(t *testing.T) {
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1") // even with the reply flag ON...
	conn := integrationDB(t)

	caller := "99999999-9999-9999-9999-999999999999"
	callee := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	id := "deleg-flag-off"

	t.Setenv("DELEGATION_LEDGER_WRITE", "1") // seed the row...
	seedDrainScenario(t, conn, caller, callee, id)
	t.Setenv("DELEGATION_LEDGER_WRITE", "") // ...then go dark for the drain itself.

	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(), caller, callee, id,
		[]byte(`{"result":{"parts":[{"text":"done"}]}}`))

	if status, _, _ := readLedgerRow(t, conn, id); status != "queued" {
		t.Fatalf("with the ledger DARK the drain still wrote to `delegations`: status=%q, "+
			"want queued (untouched). Phase 1 is only safe to merge because it is a no-op "+
			"while the flag is off — that is the whole argument.", status)
	}
}

// postUpdateStatus drives the agent-facing status-update endpoint the way an agent
// does: POST /workspaces/:id/delegations/:delegation_id/update.
func postUpdateStatus(t *testing.T, caller, delegationID, body string) {
	t.Helper()
	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, _ := newTestGinContext()
	c.Params = gin.Params{
		{Key: "id", Value: caller},
		{Key: "delegation_id", Value: delegationID},
	}
	c.Request = httptest.NewRequest(http.MethodPost,
		"/workspaces/"+caller+"/delegations/"+delegationID+"/update",
		strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	dh.UpdateStatus(c)
}

// TestIntegration_UpdateStatus_SecondTerminalPostDoesNotReplyAgain — SINGLE-REPLY
// AUTHORITY ON THE OLDEST WRITER.
//
// The compare-and-swap was BUILT in this change set so that exactly one terminal
// transition owns exactly one delegate_result... and then was not wired to the
// agent-facing status endpoint. UpdateStatus computed the CAS and DISCARDED it, then
// pushed unconditionally.
//
// So: an agent POSTs completed (CAS wins, reply #1), then POSTs failed — a retry, or
// it changed its mind. The CAS correctly REFUSES (completed is terminal), and the old
// code replied anyway. The caller's inbox then holds BOTH "Delegation completed" and
// "Delegation failed" for one delegation, contradicting each other, while the ledger
// says completed. A plain HTTP retry of the same POST does it too — the endpoint has
// no idempotency key.
//
// This PR's own TestIntegration_DrainDoesNotReply_WhenTheTransitionWasRefused calls
// exactly this outcome fatal, for the drain path. Same defect, same file, one writer
// over.
func TestIntegration_UpdateStatus_SecondTerminalPostDoesNotReplyAgain(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	conn := integrationDB(t)

	caller := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	callee := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	id := "deleg-double-reply"
	seedDrainScenario(t, conn, caller, callee, id)

	postUpdateStatus(t, caller, id, `{"status":"completed","response_preview":"the answer"}`)
	afterFirst := countInboxReplies(t, conn, id)
	if afterFirst != 1 {
		t.Fatalf("the first terminal POST produced %d replies, want exactly 1", afterFirst)
	}

	// ...and now the agent contradicts itself (or simply retries).
	postUpdateStatus(t, caller, id, `{"status":"failed","error":"boom"}`)

	if n := countInboxReplies(t, conn, id); n != 1 {
		t.Fatalf("a SECOND terminal POST produced %d replies, want 1. The CAS refused the "+
			"transition (completed is terminal) — so this call owns no reply. The caller's "+
			"agent now reads both 'Delegation completed' and 'Delegation failed' for one "+
			"delegation and cannot tell which is current, while the ledger says completed.", n)
	}
	if status, _, _ := readLedgerRow(t, conn, id); status != "completed" {
		t.Errorf("the refused transition mutated the ledger anyway: %q, want completed", status)
	}
}

// TestIntegration_UpdateStatus_PlainRetryDoesNotReplyTwice — same defect, the shape
// that needs no misbehaviour at all: the endpoint has no idempotency key, so a
// network retry of an identical POST is ordinary and must not double-notify.
func TestIntegration_UpdateStatus_PlainRetryDoesNotReplyTwice(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	conn := integrationDB(t)

	caller := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	callee := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	id := "deleg-retry"
	seedDrainScenario(t, conn, caller, callee, id)

	body := `{"status":"completed","response_preview":"the answer"}`
	postUpdateStatus(t, caller, id, body)
	postUpdateStatus(t, caller, id, body) // the retry

	if n := countInboxReplies(t, conn, id); n != 1 {
		t.Fatalf("an identical retried POST produced %d replies, want 1 — the endpoint has "+
			"no idempotency key, so a retry is ordinary, not misbehaviour", n)
	}
}

// TestIntegration_UpdateStatus_UnledgeredDelegationStillRepliesExactlyOnce — THE FLIP.
//
// This is the state of EVERY delegation that is in flight at the moment
// DELEGATION_LEDGER_WRITE is turned on: it was created while the ledger was dark, so
// it has no row, and then it terminalizes with the flag ON. The ledger is asked to
// arbitrate a reply for a delegation it has never heard of.
//
// The first cut of single-reply authority gated the push on a `didTransition bool`,
// and a missing row returned false — so the agent reported `failed`, and NOBODY told
// the caller. The caller waits forever for a delegation that already ended. That is
// #4314, reintroduced by the fix for #4314, and it would have fired on every in-flight
// delegation at the exact moment of the flip — the worst possible time for a silent
// death, and one that would have looked like "the flip broke delegation".
//
// The rule: with no arbiter, nobody else will speak, so we must. Exactly once.
func TestIntegration_UpdateStatus_UnledgeredDelegationStillRepliesExactlyOnce(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-unledgered-flip"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	// ...and now make it look like it was born before the flip: the placeholder and
	// the workspaces exist (the agent is really waiting on this), but the ledger has
	// no row for it, because the ledger was dark when it was created.
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM delegations WHERE delegation_id = $1`, delegationID); err != nil {
		t.Fatalf("simulate pre-flip delegation: %v", err)
	}

	postUpdateStatus(t, caller, delegationID,
		`{"status":"failed","error":"target exploded"}`)

	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("caller received %d delegate_result replies, want exactly 1.\n"+
			"    0 = SILENT DEATH: the agent reported `failed`, the ledger had no row to "+
			"arbitrate with, and the reply was suppressed. The caller waits forever. Every "+
			"delegation in flight at the DELEGATION_LEDGER_WRITE flip is in this state.\n"+
			"    >1 = double notification.", got)
	}
}

// TestIntegration_UpdateStatus_UnledgeredRetryDoesNotReplyTwice — the honest cost of
// the rule above, held to exactly one duplicate-free case and no more.
//
// With no ledger row there is no arbiter, so we cannot dedupe a genuine double-POST
// from the agent — and that is the ACCEPTED trade: a duplicate beats a caller waiting
// forever. But the ledger's OWN insert-on-first-touch must not make this worse. This
// pins that a second POST after the first one healed the row is refused, so the
// unarbitrated window is exactly one reply wide, not unbounded.
func TestIntegration_UpdateStatus_UnledgeredThenLedgeredDoesNotReplyTwice(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-unledgered-then-ledgered"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	// A row EXISTS and is already terminal — the agent's first POST landed and was
	// replied to. A retry of that POST must be arbitrated and refused.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET status = 'failed', error_detail = 'target exploded'
		  WHERE delegation_id = $1`, delegationID); err != nil {
		t.Fatalf("seed terminal row: %v", err)
	}

	postUpdateStatus(t, caller, delegationID,
		`{"status":"failed","error":"target exploded"}`)

	if got := countInboxReplies(t, conn, delegationID); got != 0 {
		t.Fatalf("a retried POST against an ALREADY-TERMINAL ledger row produced %d "+
			"replies, want 0. The ledger CAN arbitrate here (the row exists and is "+
			"terminal), so ReplyUnarbitrated must not leak into this path — otherwise "+
			"the fix for the silent death becomes an unbounded double-notify.", got)
	}
}

// TestIntegration_AsyncMCPFailure_RepliesToTheCallerExactlyOnce — N4.
//
// delegate_task_async hands the agent a task_id and RETURNS. The agent is not
// blocking; it goes and does other things. When the detached goroutine's A2A call
// then fails — target offline, proxy 5xx, unmarshalable body — the delegation goes
// queued -> failed, which is TERMINAL: it drops straight out of the caller's
// awaiting-reply count, so the digest stops showing it too.
//
// Before this fix, NOTHING was sent to the caller. The agent asked a peer to do
// something, the request died, it vanished from the digest, and the platform never
// said a word. Review proved it against a real database:
//
//	delegations.status         = "failed"   (terminal — a transition that owes a reply)
//	inbox delegate_result rows = 0          (want 1)
//	still in sent_awaiting_reply = 0        (dropped from the digest too)
//
// That is #4314 verbatim, in the newest code of the change set that exists to remove
// #4314. It is also NOT the gap #4338 tracks: #4338 is the missing COMPLETION writer
// (the happy path that never leaves in_progress). This is the FAILURE path, which
// terminalizes correctly and says nothing. Closing #4338 by wiring only the
// completion side would leave this live.
func TestIntegration_AsyncMCPFailure_RepliesToTheCallerExactlyOnce(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-fail"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	failAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID,
		"target_offline: dial tcp: connection refused")

	var status string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT status FROM delegations WHERE delegation_id = $1`, delegationID).Scan(&status); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if status != "failed" {
		t.Fatalf("ledger status = %q, want failed — the async failure must terminalize", status)
	}

	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("the caller received %d delegate_result replies, want exactly 1.\n"+
			"    0 = SILENT DEATH: the delegation is TERMINAL ('failed') and therefore gone "+
			"from the caller's awaiting-reply count, and nothing was ever sent to it. The "+
			"agent is not blocking on this call, so an inbox reply is the ONLY way it can "+
			"ever learn the request died. It never finds out.", got)
	}
}

// TestIntegration_AsyncMCPFailure_RetryDoesNotReplyTwice — the other half of the
// single-reply rule on the same path. The detached goroutine can be re-entered (a
// retry, a redelivery); the ledger's compare-and-swap must arbitrate, so the second
// failure is not a second notification.
func TestIntegration_AsyncMCPFailure_RetryDoesNotReplyTwice(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-async-mcp-fail-twice"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)

	detail := "target_offline: dial tcp: connection refused"
	failAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, detail)
	failAsyncMCPDelegation(context.Background(), conn, caller, callee, delegationID, detail)

	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("two failure writes produced %d replies, want exactly 1. The ledger's CAS "+
			"is what makes the second one a no-op; without it the caller's agent is told "+
			"twice that one delegation died.", got)
	}
}

// TestIntegration_Sweeper_DBBlipDoesNotDoubleReply — N5, against real Postgres.
//
// The reviewer broke my first N5 fix here: the deadline arm treated EVERY ledger
// error as ReplyUnarbitrated and replied. But a failed UPDATE means the row is still
// `queued`, still in the in-flight SELECT — and the sweeper is its OWN retrier. So the
// blip sweep replied on a transition that had provably not happened, and the recovery
// sweep, terminalizing the row for real, replied AGAIN: two "Delegation failed" for one
// delegation, plus a DeadlineFailures count for a row left queued.
//
// We simulate the blip the same way the reviewer did — a BEFORE UPDATE trigger that
// raises (a stand-in for a lock timeout / statement timeout / reset pooled conn) — then
// drop it and let the sweeper recover. The property is: exactly one reply across BOTH
// sweeps, and no deadline-failure counted for the sweep that did not transition.
func TestIntegration_Sweeper_DBBlipDoesNotDoubleReply(t *testing.T) {
	conn := integrationDB(t)
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")

	const (
		caller       = "33333333-3333-3333-3333-333333333333"
		callee       = "44444444-4444-4444-4444-444444444444"
		delegationID = "deleg-sweeper-dbblip"
	)
	seedDrainScenario(t, conn, caller, callee, delegationID)
	// Past-deadline, in-flight: exactly what the deadline arm terminalizes.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE delegations SET status='queued', deadline = now() - interval '1 minute'
		  WHERE delegation_id = $1`, delegationID); err != nil {
		t.Fatalf("arm deadline: %v", err)
	}

	// THE BLIP. A BEFORE UPDATE trigger that always raises, so the deadline arm's
	// UPDATE errors and no write lands. Torn down in t.Cleanup, NOT inline: if an
	// assertion below fails before the inline DROP runs, the trigger would survive into
	// the NEXT test on this shared DB and make every UPDATE on `delegations` raise —
	// a leaked fixture masquerading as a cascade of unrelated failures.
	mustExec(t, conn, `
		CREATE OR REPLACE FUNCTION deleg_blip() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'simulated db blip'; END; $$ LANGUAGE plpgsql;`)
	mustExec(t, conn, `
		CREATE TRIGGER deleg_blip_trg BEFORE UPDATE ON delegations
		FOR EACH ROW WHEN (NEW.delegation_id = 'deleg-sweeper-dbblip')
		EXECUTE FUNCTION deleg_blip();`)
	blipDropped := false
	dropBlip := func() {
		if blipDropped {
			return
		}
		blipDropped = true
		_, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS deleg_blip_trg ON delegations;`)
		_, _ = conn.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS deleg_blip();`)
	}
	t.Cleanup(dropBlip)

	res1 := newTestSweeper(nil).Sweep(context.Background())

	// The blip sweep must have touched nobody: no reply, and — critically — no
	// DeadlineFailures counted for a row it could not move.
	if got := countInboxReplies(t, conn, delegationID); got != 0 {
		t.Fatalf("after the BLIP sweep the caller had %d replies, want 0 — the UPDATE "+
			"errored so no transition happened; replying here double-notifies once the "+
			"sweeper retries.", got)
	}
	if res1.DeadlineFailures != 0 {
		t.Fatalf("the blip sweep counted %d DeadlineFailures for a row it left `queued` — "+
			"the metric lies in the same breath the reply would have. want 0.",
			res1.DeadlineFailures)
	}
	if st, _, _ := readLedgerRow(t, conn, delegationID); st != "queued" {
		t.Fatalf("row is %q after the blip, want queued (no write should have landed)", st)
	}

	// RECOVERY. Drop the trigger; the sweeper's next tick terminalizes for real.
	dropBlip()
	res2 := newTestSweeper(nil).Sweep(context.Background())

	if res2.DeadlineFailures != 1 {
		t.Fatalf("recovery sweep counted %d DeadlineFailures, want exactly 1", res2.DeadlineFailures)
	}
	if st, _, _ := readLedgerRow(t, conn, delegationID); st != "failed" {
		t.Fatalf("row is %q after recovery, want failed", st)
	}
	// THE PROPERTY: exactly one reply across BOTH sweeps.
	if got := countInboxReplies(t, conn, delegationID); got != 1 {
		t.Fatalf("across the blip sweep AND the recovery sweep the caller received %d "+
			"replies, want exactly 1. >1 is the single-reply-authority violation the "+
			"deadline arm's fall-through caused: it replied on a non-transition, then the "+
			"recovery replied on the real one.", got)
	}
}

// mustExec runs a statement or fails the test — for test-scaffolding DDL (triggers).
func mustExec(t *testing.T, conn *sql.DB, q string, args ...interface{}) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %.40q: %v", q, err)
	}
}
