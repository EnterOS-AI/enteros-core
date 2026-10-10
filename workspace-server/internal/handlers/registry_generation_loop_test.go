package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// registry_generation_loop_test.go — coverage for the versioned-heartbeat
// GENERATION LOOP wiring in the Heartbeat handler (PR-C):
//
//   (a) the response echoes generation = the row's desired_generation, on EVERY
//       beat (including beats that carry no observed_generation);
//   (b) a beat with observed_generation >= desired settles (MarkWakeSettled hook
//       is invoked with the observed value); one below desired does NOT settle;
//   (c) observed_generation is persisted via GREATEST (monotonic write);
//   (d) a beat from a runtime PREDATING the contract (observed_generation absent)
//       is a structural no-op: no observed write, no settle.
//
// These are sqlmock unit tests: the convergence settle is exercised through a
// recorded SetWakeSettler hook so the assertions stay at the handler boundary
// (the real MarkWakeSettled DB transition is proven in
// wake_lifecycle_integration_test.go). The heartbeat DB-op sequence mirrors the
// stable-online template (TestHeartbeatHandler_OnlineStaysOnline).

// recordedWakeSettle captures the convergence hook invocations.
type recordedWakeSettle struct {
	calls       int
	workspaceID string
	observedGen int64
}

func (r *recordedWakeSettle) settle(_ context.Context, workspaceID string, observedGen int64) error {
	r.calls++
	r.workspaceID = workspaceID
	r.observedGen = observedGen
	return nil
}

// respGeneration extracts the numeric "generation" field from a heartbeat ack.
func respGeneration(t *testing.T, body []byte) (float64, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal heartbeat response %q: %v", string(body), err)
	}
	v, ok := m["generation"]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("generation field is %T, want number", v)
	}
	return f, true
}

// --- (a) response carries generation = desired_generation ---

func TestHeartbeat_EmitsDesiredGeneration(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	const ws = "ws-gen-emit"
	// prevStatus row carries desired_generation = 7.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).
			AddRow("", 0, "online", int64(7)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(ws, 0.0, "", 0, 0, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-gen-emit","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	gen, ok := respGeneration(t, w.Body.Bytes())
	if !ok {
		t.Fatalf("response missing generation field: %s", w.Body.String())
	}
	if gen != 7 {
		t.Errorf("response generation = %v, want 7 (the row's desired_generation)", gen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- (d) old runtime (no observed_generation) is a no-op ---

func TestHeartbeat_AbsentObservedGeneration_IsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())
	settle := &recordedWakeSettle{}
	handler.SetWakeSettler(settle.settle)

	const ws = "ws-gen-oldrt"
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).
			AddRow("", 0, "online", int64(4)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(ws, 0.0, "", 0, 0, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO observed_generation UPDATE expected — the payload omits it.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// A runtime predating the contract sends no observed_generation.
	body := `{"workspace_id":"ws-gen-oldrt","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if settle.calls != 0 {
		t.Errorf("old-runtime beat must NOT claim convergence; settle called %d times", settle.calls)
	}
	// generation still emitted so the runtime can eventually learn it.
	if gen, ok := respGeneration(t, w.Body.Bytes()); !ok || gen != 4 {
		t.Errorf("generation = %v (present=%v), want 4 emitted even for an old runtime", gen, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- (c) observed_generation written monotonically (GREATEST) + (b) settle when observed >= desired ---

func TestHeartbeat_ObservedAtOrAboveDesired_WritesMonotonicAndSettles(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())
	settle := &recordedWakeSettle{}
	handler.SetWakeSettler(settle.settle)

	const ws = "ws-gen-converge"
	// desired = 3; runtime reports observed = 5 (>= desired → converged).
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).
			AddRow("", 0, "online", int64(3)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(ws, 0.0, "", 0, 0, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The monotonic observed write — GREATEST guards against a regressing beat.
	mock.ExpectExec("observed_generation = GREATEST\\(observed_generation, \\$1\\)").
		WithArgs(int64(5), ws).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-gen-converge","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0,"observed_generation":5}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if settle.calls != 1 {
		t.Fatalf("observed>=desired must settle exactly once; got %d", settle.calls)
	}
	if settle.workspaceID != ws || settle.observedGen != 5 {
		t.Errorf("settle args = (%s, %d), want (%s, 5)", settle.workspaceID, settle.observedGen, ws)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- (b) observed below desired: monotonic write still happens, but NO settle ---

func TestHeartbeat_ObservedBelowDesired_WritesButDoesNotSettle(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())
	settle := &recordedWakeSettle{}
	handler.SetWakeSettler(settle.settle)

	const ws = "ws-gen-unconverged"
	// desired = 9; runtime reports observed = 2 (< desired → still in flight).
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).
			AddRow("", 0, "online", int64(9)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(ws, 0.0, "", 0, 0, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The observed write STILL fires (persisting the runtime's progress is
	// unconditional when observed_generation is present); only the settle is gated.
	mock.ExpectExec("observed_generation = GREATEST\\(observed_generation, \\$1\\)").
		WithArgs(int64(2), ws).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).
			AddRow("online", "", nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-gen-unconverged","error_rate":0,"sample_error":"","active_tasks":0,"uptime_seconds":0,"observed_generation":2}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if settle.calls != 0 {
		t.Errorf("observed<desired must NOT settle; got %d call(s)", settle.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
