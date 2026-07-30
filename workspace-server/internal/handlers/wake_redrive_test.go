package handlers

// wake_redrive_test.go — fast, DB-less unit coverage of the generation-loop
// re-drive (wake_redrive.go). sqlmock pins the SELECT/UPDATE shapes and the
// selection + bounding decisions; the real-engine behaviours (the status filter
// actually excluding settled rows, the attempt cap crossing over runs against a
// live counter) are proven end-to-end in wake_redrive_integration_test.go.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// recordedReEmit captures re-emit hook invocations so a test can assert re-drive
// re-emitted exactly the intents it should, through the exact idempotency keys.
type reEmitCall struct {
	workspaceID string
	kind        WakeKind
	key         string
	generation  int64
}

type recordedReEmit struct {
	calls []reEmitCall
}

func (r *recordedReEmit) emit(_ context.Context, workspaceID string, kind WakeKind, key string, gen int64) error {
	r.calls = append(r.calls, reEmitCall{workspaceID, kind, key, gen})
	return nil
}

// redriveSelectCols is the row shape ReDriveStuckWakes scans.
func redriveSelectRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "kind", "idempotency_key", "generation", "redrive_attempts"})
}

// TestReDriveStuckWakes_ReEmitsBelowCap proves a stuck intent whose attempt
// count is below the cap is re-emitted once — the attempt is counted (bump
// UPDATE) and the re-emit hook fires with the intent's EXISTING idempotency key.
// A "delivered-but-unsettled" intent is exactly this shape (status='delivered',
// selected by the non-terminal filter), so this is the required
// "stuck delivered-but-unsettled intent is re-emitted once" proof at the unit
// level.
func TestReDriveStuckWakes_ReEmitsBelowCap(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}
	rec := &recordedReEmit{}
	h.SetWakeReEmitter(rec.emit)

	const ws = "ws-redrive-1"

	// One stuck delivered intent, attempts=2 (< cap) → re-emit.
	mock.ExpectQuery(`SELECT id, kind, idempotency_key, generation, redrive_attempts\s+FROM wake_intents`).
		WithArgs(ws, int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), redriveBatchCap).
		WillReturnRows(redriveSelectRows().
			AddRow(int64(11), "first-boot-greet", "first-boot-greet:"+ws, int64(1), 2))

	// Attempt counted BEFORE the re-emit fires.
	mock.ExpectExec(`UPDATE wake_intents\s+SET redrive_attempts = redrive_attempts \+ 1, last_redriven_at = now\(\)\s+WHERE id = \$1`).
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := h.ReDriveStuckWakes(context.Background(), ws)
	if err != nil {
		t.Fatalf("ReDriveStuckWakes: %v", err)
	}
	if res.Redriven != 1 || res.Dropped != 0 {
		t.Errorf("result = %+v, want Redriven=1 Dropped=0", res)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("re-emit called %d times, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.kind != WakeFirstBootGreet || got.key != "first-boot-greet:"+ws || got.generation != 1 || got.workspaceID != ws {
		t.Errorf("re-emit call = %+v, want the intent's own kind/key/gen through the SAME key", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestReDriveStuckWakes_DropsAtCap proves the bound: an intent whose
// redrive_attempts has reached the cap is marked 'dropped' (terminal) and is NOT
// re-emitted — the never-re-emitted-forever guarantee.
func TestReDriveStuckWakes_DropsAtCap(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}
	rec := &recordedReEmit{}
	h.SetWakeReEmitter(rec.emit)

	const ws = "ws-redrive-cap"

	// attempts == cap → drop, no re-emit.
	mock.ExpectQuery(`SELECT id, kind, idempotency_key, generation, redrive_attempts\s+FROM wake_intents`).
		WithArgs(ws, int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), redriveBatchCap).
		WillReturnRows(redriveSelectRows().
			AddRow(int64(77), "lifecycle", "lifecycle:"+ws+":seed", int64(4), redriveMaxAttempts))

	mock.ExpectExec(`UPDATE wake_intents\s+SET status = 'dropped', last_redriven_at = now\(\)\s+WHERE id = \$1 AND status IN`).
		WithArgs(int64(77)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := h.ReDriveStuckWakes(context.Background(), ws)
	if err != nil {
		t.Fatalf("ReDriveStuckWakes: %v", err)
	}
	if res.Dropped != 1 || res.Redriven != 0 {
		t.Errorf("result = %+v, want Dropped=1 Redriven=0", res)
	}
	if len(rec.calls) != 0 {
		t.Errorf("re-emit fired %d times for a capped intent; want 0 (must not re-emit past the cap)", len(rec.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestReDriveStuckWakes_MixedBatch proves a single pass re-emits the below-cap
// intents AND drops the at-cap ones, in one scan, with per-intent UPDATEs in
// candidate order.
func TestReDriveStuckWakes_MixedBatch(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}
	rec := &recordedReEmit{}
	h.SetWakeReEmitter(rec.emit)

	const ws = "ws-redrive-mixed"

	mock.ExpectQuery(`SELECT id, kind, idempotency_key, generation, redrive_attempts\s+FROM wake_intents`).
		WithArgs(ws, int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), redriveBatchCap).
		WillReturnRows(redriveSelectRows().
			AddRow(int64(1), "first-boot-greet", "first-boot-greet:"+ws, int64(1), 0).           // below cap → re-emit
			AddRow(int64(2), "restart-context", "restart-context:"+ws, int64(2), redriveMaxAttempts). // at cap → drop
			AddRow(int64(3), "nudge", "nudge:"+ws+":42", int64(3), 1))                            // below cap → re-emit

	// id=1 bump
	mock.ExpectExec(`UPDATE wake_intents\s+SET redrive_attempts = redrive_attempts \+ 1`).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	// id=2 drop
	mock.ExpectExec(`UPDATE wake_intents\s+SET status = 'dropped'`).
		WithArgs(int64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	// id=3 bump
	mock.ExpectExec(`UPDATE wake_intents\s+SET redrive_attempts = redrive_attempts \+ 1`).
		WithArgs(int64(3)).WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := h.ReDriveStuckWakes(context.Background(), ws)
	if err != nil {
		t.Fatalf("ReDriveStuckWakes: %v", err)
	}
	if res.Redriven != 2 || res.Dropped != 1 {
		t.Errorf("result = %+v, want Redriven=2 Dropped=1", res)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("re-emit called %d times, want 2 (the two below-cap intents)", len(rec.calls))
	}
	if rec.calls[0].key != "first-boot-greet:"+ws || rec.calls[1].key != "nudge:"+ws+":42" {
		t.Errorf("re-emit keys = [%q, %q], want the two below-cap keys in order", rec.calls[0].key, rec.calls[1].key)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestReDriveStuckWakes_UnwiredIsNoOp proves the nil-safe contract: with no
// re-emitter wired, ReDriveStuckWakes does not even scan — a bare handler (every
// deployment that doesn't wire the loop) is a true no-op. The mock expects NO
// queries, so any DB touch fails the test.
func TestReDriveStuckWakes_UnwiredIsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{} // wakeReEmit left nil

	res, err := h.ReDriveStuckWakes(context.Background(), "ws-unwired")
	if err != nil {
		t.Fatalf("ReDriveStuckWakes (unwired): %v", err)
	}
	if res.Redriven != 0 || res.Dropped != 0 {
		t.Errorf("unwired result = %+v, want zero", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unwired re-drive touched the DB; want no queries: %v", err)
	}
}

// TestWakeReEmitter_GreetVsOther proves the production dispatcher: it re-emits a
// stuck greet by invoking the greeter (respecting has_greeted's greet-once via
// the greeter itself) and treats every non-greet kind as a no-op skip.
func TestWakeReEmitter_GreetVsOther(t *testing.T) {
	var greetCalls int
	greeter := func(workspaceID string, toolCount int) { greetCalls++ }
	reEmit := WakeReEmitter(greeter)

	// A greet re-emit invokes the greeter (async via globalGoAsync); drain it.
	if err := reEmit(context.Background(), "ws-g", WakeFirstBootGreet, "first-boot-greet:ws-g", 1); err != nil {
		t.Fatalf("greet re-emit: %v", err)
	}
	waitGlobalAsyncForTest()
	if greetCalls != 1 {
		t.Errorf("greet re-emit invoked greeter %d times, want 1", greetCalls)
	}

	// A non-greet kind is a no-op skip — the greeter is never touched.
	for _, kind := range []WakeKind{WakeRestartContext, WakeStall, WakeNudge, WakeIdle, WakeLifecycle} {
		if err := reEmit(context.Background(), "ws-g", kind, string(kind)+":ws-g", 2); err != nil {
			t.Fatalf("re-emit %s: %v", kind, err)
		}
	}
	waitGlobalAsyncForTest()
	if greetCalls != 1 {
		t.Errorf("non-greet kinds must not invoke the greeter; greetCalls=%d, want still 1", greetCalls)
	}
}

// TestWakeReEmitter_NilGreeterSkips proves a nil greeter degrades greet re-emit
// to a safe skip rather than a panic.
func TestWakeReEmitter_NilGreeterSkips(t *testing.T) {
	reEmit := WakeReEmitter(nil)
	if err := reEmit(context.Background(), "ws-x", WakeFirstBootGreet, "first-boot-greet:ws-x", 1); err != nil {
		t.Errorf("nil-greeter greet re-emit should be a no-op, got %v", err)
	}
	waitGlobalAsyncForTest()
}
