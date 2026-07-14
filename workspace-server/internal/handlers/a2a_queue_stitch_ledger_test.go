package handlers

// a2a_queue_stitch_ledger_test.go — the DRAIN path must terminalize the LEDGER,
// not just the activity_logs row.
//
// This is the bug that would have turned the sweeper's new caller-notification
// (#4316) from a feature into an incident. The async delegate flow is:
//
//	executeDelegation   → delegations row 'queued', activity_logs row 'queued'
//	  ...target works...
//	drain (Heartbeat)   → activity_logs row 'completed'   ← and NOTHING ELSE
//
// The `delegations` row stayed 'queued' forever. The sweeper selects
// status IN ('queued','dispatched','in_progress'), so a SUCCESSFUL delegation
// remained in its candidate set until the 6h deadline elapsed — and then got
// marked 'failed'. Silently, while the ledger was dark. The moment the sweeper
// starts telling the caller (this PR), that becomes: "Delegation failed",
// delivered to a live agent's inbox, about a delegation that completed six hours
// earlier and whose result the agent has already acted on.
//
// The forward-only terminal guard does not save us: it only protects rows some
// path already terminalized, and this path never did — so the guard was vacuous
// in exactly the case it was written for.

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// NOTE — TestDrainStitch_NoActivityRow_LeavesLedgerAlone and
// TestDrainStitch_FlagOff_TouchesNoLedger USED TO LIVE HERE, and both were VACUOUS.
// Review proved it: delete the `rows == 0` early return, or delete the
// ledgerWritesEnabled() gate, and each still passed.
//
// They are "must NOT happen" assertions, and sqlmock cannot make those:
// ExpectationsWereMet() asserts that EXPECTED calls fired and can NEVER detect an
// EXTRA one. Declaring no ledger expectations and then checking them proves nothing
// — the mock is equally happy whether or not the ledger was touched.
//
// The second of those was the test backing this PR's central safety claim, that
// Phase 1 is a no-op while the flags are dark. It was proving nothing at all.
//
// Both are now real-Postgres tests that assert on ROW STATE — a witness that
// actually moves — in delegation_ledger_integration_test.go:
//
//	TestIntegration_DrainStitch_UnattributableDrain_LeavesLedgerAlone
//	TestIntegration_DrainStitch_FlagOff_TouchesNothing
//
// drainMock wires the stitch UPDATE that the drain path always performed.
func drainMock(t *testing.T, rowsAffected int64) sqlmock.Sqlmock {
	t.Helper()
	mock := setupTestDB(t)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE activity_logs`)).
		WillReturnResult(sqlmock.NewResult(0, rowsAffected))
	return mock
}

func TestDrainStitch_TerminalizesTheLedgerNotJustActivityLogs(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	mock := drainMock(t, 1)

	// THE PIN: the ledger row must move queued → completed. Without this the
	// sweeper deadline-fails a delegation that succeeded.
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(),
		"caller-ws", "callee-ws", "deleg-x", []byte(`{"result":{"parts":[{"text":"done"}]}}`))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a drained (successful) delegation left its ledger row non-terminal — "+
			"the sweeper will deadline-fail it in 6h and tell the caller it FAILED: %v", err)
	}
}

// TestLedgerTransitions_CoverEveryRealWriter pins allowedTransitions to the
// paths that actually exist, so it cannot drift back into a presumed lifecycle.
//
// The matrix used to forbid `queued → completed` — the single most common
// transition in the system (sync delegate, agent status-update, and async drain
// all do exactly that). It survived review because NOTHING called SetStatus on
// those paths while the ledger was dark, so the rejection was unobservable. A
// matrix is only defense-in-depth if it permits the real lifecycle; one that
// rejects the happy path is just a silent failure with a comment on it.
func TestLedgerTransitions_CoverEveryRealWriter(t *testing.T) {
	realWriters := []struct {
		from, to, writer string
	}{
		{"queued", "completed", "delegation.go sync delegate returned"},
		{"queued", "failed", "delegation.go proxy/empty-response error"},
		{"queued", "dispatched", "delegation.go agent status-update"},
		{"queued", "stuck", "sweeper: target never heartbeat"},
		{"dispatched", "completed", "a2a_queue.go async drain"},
		{"dispatched", "failed", "sweeper: deadline exceeded"},
		{"dispatched", "stuck", "sweeper: target went silent"},
		{"in_progress", "completed", "delegation.go agent status-update"},
		{"in_progress", "failed", "sweeper: deadline exceeded"},
		{"in_progress", "stuck", "sweeper: heartbeat went stale"},
		{"stuck", "completed", "a2a_queue drain: the wedged target came back"},
		{"stuck", "failed", "sweeper: deadline exceeded on a wedged target"},
	}
	for _, w := range realWriters {
		if !allowedTransitions[w.from][w.to] {
			t.Errorf("%s → %s is REJECTED but a real writer performs it (%s)", w.from, w.to, w.writer)
		}
	}

	// TERMINAL = {completed, failed}. NOT stuck.
	//
	// stuck is a recoverable WARNING: the target may be settling with its message
	// still held in a2a_queue (infinite TTL by default) and deliver it on its next
	// heartbeat. Making stuck terminal meant the drain's completed-transition
	// bounced off ErrInvalidTransition and the ledger permanently misreported a
	// SUCCESSFUL delegation as stuck — while the caller had been told it failed.
	for _, terminal := range []string{"completed", "failed"} {
		if len(allowedTransitions[terminal]) != 0 {
			t.Errorf("%s is terminal but has outbound transitions %v", terminal, allowedTransitions[terminal])
		}
	}
	// ...and a stuck delegation MUST be able to recover, or be killed by the deadline.
	if !allowedTransitions["stuck"]["completed"] {
		t.Error("stuck -> completed must be legal: a wedged target can come back and " +
			"the queued message still deliver (this is the #4316 ledger-corruption bug)")
	}
	if !allowedTransitions["stuck"]["failed"] {
		t.Error("stuck -> failed must be legal: the 6h deadline is the only thing that kills it")
	}

	// ...and no transition targets a status outside the CHECK constraint's six.
	valid := map[string]bool{"queued": true, "dispatched": true, "in_progress": true,
		"completed": true, "failed": true, "stuck": true}
	for from, tos := range allowedTransitions {
		if !valid[from] {
			t.Errorf("transition source %q is not in the schema CHECK constraint", from)
		}
		for to := range tos {
			if !valid[to] {
				t.Errorf("%s → %q targets a status the schema CHECK forbids", from, to)
			}
		}
	}
}

// TestDrainStitch_OnlyMatchesTheQueuedPlaceholder pins the `AND status='queued'`
// guard on the stitch UPDATE.
//
// Without it, the UPDATE's WHERE (workspace_id + activity_type + method +
// target_id + delegation_id) ALSO matches the sweeper's own terminal
// delegate_result row — they differ only in status. So a delegation that the
// sweeper deadline-failed at 6h, whose answer then arrives late, would have its
// "Delegation failed" notice silently rewritten to "completed" in activity_logs,
// while `delegations` still reads `failed`. The two tables disagree permanently,
// and the caller has been told both stories.
//
// It also makes a replayed drain idempotent: it matches zero rows and skips the
// ledger terminalization rather than double-applying it.
func TestDrainStitch_OnlyMatchesTheQueuedPlaceholder(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	mock := setupTestDB(t)

	// The UPDATE must constrain on the placeholder's status. A regex, because the
	// hazard lives in the SQL text: drop the clause and this expectation stops
	// matching — which is exactly the regression we are guarding.
	mock.ExpectExec(`UPDATE activity_logs(?s).*AND status\s+=\s+'queued'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).WillReturnResult(sqlmock.NewResult(0, 1))

	h := &WorkspaceHandler{}
	h.stitchDrainResponseToDelegation(context.Background(),
		"caller-ws", "callee-ws", "deleg-x", []byte(`{"result":{"parts":[{"text":"done"}]}}`))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the stitch UPDATE must be constrained to the 'queued' placeholder row, "+
			"or a late drain rewrites the sweeper's terminal failure notice: %v", err)
	}
}
