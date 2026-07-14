package handlers

// Integration tests for the workspace hibernation feature (issue #711 / PR #724).
// Updated for the atomic TOCTOU fix (issue #819).
//
// Coverage:
//   - HibernateWorkspace(): atomic claim, container stop, DB status update, Redis key clear, event broadcast
//   - POST /workspaces/:id/hibernate HTTP handler: online→200, not-eligible→404, DB error→500
//   - resolveAgentURL(): hibernated workspace → 503 + Retry-After: 15 + waking: true
//
// The A2A auto-wake path (resolveAgentURL) is tested via TestResolveAgentURL_HibernatedWorkspace_*
// added to a2a_proxy_test.go to keep related resolveAgentURL tests co-located.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ──────────────────────────────────────────────────────────────────────────────
// HibernateWorkspace unit tests
// ──────────────────────────────────────────────────────────────────────────────

// TestHibernateWorkspace_OnlineWorkspace_Success verifies the happy-path with
// the 3-step atomic pattern (#819):
//   - Atomic claim UPDATE returns rowsAffected=1 (workspace was online/degraded + active_tasks=0)
//   - Name/tier SELECT runs after the claim
//   - Final UPDATE sets status='hibernated', url=”
//   - Redis keys ws:{id}, ws:{id}:url, ws:{id}:internal_url are deleted
//   - WORKSPACE_HIBERNATED event is broadcast (INSERT INTO structure_events)
func TestHibernateWorkspace_OnlineWorkspace_Success(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-idle-online"

	// Pre-populate Redis keys that ClearWorkspaceKeys should remove.
	mr.Set(fmt.Sprintf("ws:%s", wsID), "some-value")
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), "http://agent.internal:8000")
	mr.Set(fmt.Sprintf("ws:%s:internal_url", wsID), "http://172.17.0.5:8000")

	// Step 1: atomic claim UPDATE succeeds.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT for name/tier.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Idle Agent", 1))

	// Step 3: final UPDATE to 'hibernated'.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster inserts a structure_events row.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.HibernateWorkspace(context.Background(), wsID)

	// All DB expectations were exercised.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}

	// Redis keys must all be gone.
	for _, suffix := range []string{"", ":url", ":internal_url"} {
		key := fmt.Sprintf("ws:%s%s", wsID, suffix)
		if _, err := mr.Get(key); err == nil {
			t.Errorf("expected Redis key %q to be deleted, but it still exists", key)
		}
	}
}

// TestHibernateWorkspace_RoutesStopToCPWhenOnlyCPWired is the fail-before/
// pass-after regression guard for the local-docker-vs-EC2 hibernate gap.
//
// On a SaaS / molecules-server tenant the CP provisioner is wired and the local
// Docker provisioner is nil. Step 2 of HibernateWorkspace MUST route the
// container stop through StopWorkspaceAuto (which dispatches to cpProv), NOT the
// old inlined `else if h.provisioner != nil { Stop }` — that branch silently
// no-op'd here, flipping the row to 'hibernated' + url=” while the container
// KEPT RUNNING and serving A2A (verified live on staging: container stayed "Up"
// while GET reported hibernated). This mirrors TestStopWorkspaceAuto_RoutesToCPWhenSet
// but exercises the HIBERNATE call site specifically. Before the fix cpProv.Stop
// is never invoked and this fails; after, it is invoked exactly once.
func TestHibernateWorkspace_RoutesStopToCPWhenOnlyCPWired(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	rec := &trackingCPProv{}
	handler.SetCPProvisioner(rec) // cpProv wired, local Docker provisioner nil (SaaS/molecules-server shape)

	wsID := "ws-hibernate-cp-stop"

	// Step 1: atomic claim UPDATE succeeds (online/degraded + active_tasks=0).
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Post-claim SELECT for name/tier.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("CP Agent", 2))
	// Step 3: final UPDATE to 'hibernated' (fires regardless of stop outcome).
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Broadcaster inserts a structure_events row.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	handler.HibernateWorkspace(context.Background(), wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
	got := rec.stoppedSnapshot()
	if len(got) != 1 || got[0] != wsID {
		t.Fatalf("hibernate must stop the container via cpProv when only CP is wired; "+
			"expected cpProv.Stop([%q]), got %v — the container would keep running (local-docker-vs-EC2 gap)", wsID, got)
	}
}

// TestWakeWorkspace_ClaimsHibernated verifies the auto-wake re-provision path
// ACTS on a hibernated workspace — the exact case RestartByID/runRestartCycle
// SELECT-EXCLUDE (`status NOT IN (...,'hibernated')`) and no-op on, which left a
// genuinely-stopped hibernated ws unable to wake (verified live). WakeWorkspace
// must issue the atomic hibernated→provisioning claim, then proceed to load the
// stored provision inputs. We return an error on the load SELECT to stop before
// the async re-provision, keeping the assertion deterministic — the claim firing
// against `status = 'hibernated'` is the load-bearing proof.
func TestWakeWorkspace_ClaimsHibernated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(&trackingCPProv{}) // a backend must be wired or WakeWorkspace returns before any DB touch

	wsID := "ws-wake-hib"
	// Atomic claim: hibernated → provisioning (rowsAffected=1 = we won the claim).
	mock.ExpectExec(`AND status = 'hibernated'`).
		WithArgs(models.StatusProvisioning, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Load stored provision inputs — return an error to halt before the async
	// provisionWorkspaceAuto dispatch (deterministic; the claim already proved it
	// acts on hibernated instead of early-returning like RestartByID).
	mock.ExpectQuery(`SELECT name, tier, COALESCE\(runtime`).
		WithArgs(wsID).
		WillReturnError(fmt.Errorf("halt-after-claim"))

	handler.WakeWorkspace(wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (WakeWorkspace must claim the hibernated row): %v", err)
	}
}

// TestWakeWorkspace_NoOpWhenNotHibernated verifies the atomic claim dedupe: when
// the row is no longer hibernated (a concurrent wake already claimed it, or it
// was resumed/removed), rowsAffected=0 and WakeWorkspace returns immediately with
// no load SELECT, no broadcast, no provision dispatch.
func TestWakeWorkspace_NoOpWhenNotHibernated(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(&trackingCPProv{})

	wsID := "ws-wake-nothib"
	// Claim matches nothing (not hibernated) → rowsAffected=0.
	mock.ExpectExec(`AND status = 'hibernated'`).
		WithArgs(models.StatusProvisioning, wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// No further DB expectations — WakeWorkspace must return after a zero-row claim.

	handler.WakeWorkspace(wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations (WakeWorkspace must no-op on a non-hibernated row): %v", err)
	}
}

// TestHibernateWorkspace_NotEligible_NoOp verifies that when the atomic claim
// UPDATE returns rowsAffected=0 (workspace not in online/degraded state, or
// active_tasks > 0), HibernateWorkspace returns immediately — no Stop, no
// final UPDATE, no Redis clear, no broadcast.
func TestHibernateWorkspace_NotEligible_NoOp(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-already-offline"

	// Atomic claim finds nothing matching WHERE (workspace offline, paused, etc.).
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Set a Redis key to confirm it is NOT cleared by early return.
	mr.Set(fmt.Sprintf("ws:%s:url", wsID), "http://still-here:8000")

	handler.HibernateWorkspace(context.Background(), wsID)

	// Only the one ExecContext expectation; no further DB operations.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}

	// Redis key must still exist — HibernateWorkspace returned early.
	if _, err := mr.Get(fmt.Sprintf("ws:%s:url", wsID)); err != nil {
		t.Errorf("expected Redis key to still exist after no-op, but it was deleted: %v", err)
	}
}

// TestHibernateWorkspace_DBUpdateFails_NoCrash verifies that a DB error on the
// final status UPDATE does not panic — the function logs and returns silently.
func TestHibernateWorkspace_DBUpdateFails_NoCrash(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-update-fail"

	// Step 1: atomic claim succeeds.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Flaky Agent", 2))

	// Step 3: final UPDATE fails.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnError(fmt.Errorf("db: connection refused"))

	// Must not panic — test will catch a panic via t.Fatal.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HibernateWorkspace panicked on UPDATE error: %v", r)
		}
	}()

	handler.HibernateWorkspace(context.Background(), wsID)

	// Claim + SELECT + failing UPDATE; no INSERT INTO structure_events expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /workspaces/:id/hibernate HTTP handler tests
// ──────────────────────────────────────────────────────────────────────────────

// hibernateRequest fires POST /workspaces/{id}/hibernate against the handler
// and returns the response recorder.
func hibernateRequest(t *testing.T, handler *WorkspaceHandler, wsID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/hibernate", nil)
	handler.Hibernate(c)
	return w
}

// hibernateRequestWithQuery is like hibernateRequest but appends a query string.
func hibernateRequestWithQuery(t *testing.T, handler *WorkspaceHandler, wsID, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/hibernate?"+query, nil)
	handler.Hibernate(c)
	return w
}

// TestHibernateHandler_Online_Returns200 verifies that an online workspace
// that is eligible for hibernation returns 200 {"status":"hibernated"}.
// With the 3-step fix: handler SELECT → atomic claim UPDATE → name/tier SELECT
// → final UPDATE → broadcaster INSERT.
func TestHibernateHandler_Online_Returns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-online"

	// Hibernate() handler eligibility SELECT — checks status IN ('online','degraded').
	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Online Bot", 1, 0))

	// HibernateWorkspace() step 1: atomic claim.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT for name/tier.
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Online Bot", 1, 0))

	// Step 3: final UPDATE.
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster INSERT.
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "hibernated" {
		t.Errorf(`expected {"status":"hibernated"}, got %v`, resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_NotActive_Returns404 verifies that a workspace not in
// online/degraded state (e.g. offline, paused, already hibernated) returns 404.
func TestHibernateHandler_NotActive_Returns404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-paused"

	// Handler's eligibility SELECT returns no rows — workspace is not online/degraded.
	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !strings.Contains(fmt.Sprint(resp["error"]), "not found") {
		t.Errorf("expected error mentioning 'not found', got %v", resp)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_ActiveTasks_Returns409 verifies that hibernating a
// workspace with active_tasks > 0 returns 409 unless ?force=true is passed.
// (#822)
func TestHibernateHandler_ActiveTasks_Returns409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-busy"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Busy Bot", 1, 3))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if active, _ := resp["active_tasks"].(float64); active != 3 {
		t.Errorf("expected active_tasks=3 in response, got %v", resp["active_tasks"])
	}
}

// TestHibernateHandler_ActiveTasks_ForceTrue_Returns200 verifies that
// ?force=true overrides the 409 guard and proceeds with hibernation. (#822)
func TestHibernateHandler_ActiveTasks_ForceTrue_Returns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-force-hibernate"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Force Bot", 1, 2))

	// HibernateWorkspace claim
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Post-claim SELECT
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Force Bot", 1))

	// Final UPDATE to hibernated
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Broadcaster
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := hibernateRequestWithQuery(t, handler, wsID, "force=true")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHibernateHandler_DBError_Returns500 verifies that an unexpected DB error
// on the eligibility SELECT returns 500.
func TestHibernateHandler_DBError_Returns500(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	wsID := "ws-handler-dberror"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnError(fmt.Errorf("db: connection reset"))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ?force=true must actually force (staging e2e 10b, run 494525)
//
// The bug: the HTTP handler skipped its own 409 when ?force=true, logged
// "force-hibernating ... with N active tasks", then called HibernateWorkspace —
// whose atomic claim still demanded `active_tasks = 0`. The claim matched no row,
// HibernateWorkspace returned silently (it returned nothing at all), and the
// handler answered 200 {"status":"hibernated"} regardless. The workspace stayed
// online and kept running, and the API said it hadn't.
//
// Why the pre-existing unit test (TestHibernateHandler_ActiveTasks_ForceTrue_Returns200)
// stayed green through all of it: sqlmock does not evaluate a WHERE clause. It
// returned rowsAffected=1 for the claim no matter what the predicate said, so the
// test mocked away the exact thing that was broken. The only property sqlmock CAN
// observe here is the claim's SQL text — so that is what these tests assert on.
// ──────────────────────────────────────────────────────────────────────────────

// TestHibernateWorkspace_Force_ClaimDropsActiveTasksPredicate pins the fix: with
// force=true the atomic claim must NOT carry `AND active_tasks = 0`. The regex is
// anchored at the end of the (whitespace-stripped) query, so the pre-fix claim —
// which always appended `AND active_tasks = 0` — fails to match and the test fails.
func TestHibernateWorkspace_Force_ClaimDropsActiveTasksPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	handler.stopFnOverride = func(_ context.Context, _ string) {}

	wsID := "ws-force-claim"

	// Anchored: the claim must END at the status predicate. No active_tasks gate.
	mock.ExpectExec(`WHERE id = \$1 AND status IN \('online', 'degraded'\)$`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Forced", 1))
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := handler.hibernateWorkspace(context.Background(), wsID, true); err != nil {
		t.Fatalf("force hibernate returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateWorkspace_NonForce_ClaimKeepsActiveTasksPredicate is the other half
// of the guard: force=false must STILL refuse to interrupt a running task. Without
// this, "make force work" could be mis-fixed by dropping the predicate outright and
// every idle-timer hibernation would start killing live agents mid-task (#822).
func TestHibernateWorkspace_NonForce_ClaimKeepsActiveTasksPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) { atomic.AddInt32(&stopCalls, 1) }

	wsID := "ws-noforce-claim"

	mock.ExpectExec(`AND status IN \('online', 'degraded'\) AND active_tasks = 0$`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0)) // busy workspace: claim matches nothing

	err := handler.hibernateWorkspace(context.Background(), wsID, false)
	if !errors.Is(err, errHibernateNotClaimed) {
		t.Fatalf("want errHibernateNotClaimed for an unclaimable workspace, got %v", err)
	}
	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Errorf("stop called %d times; want 0 — a non-forced hibernate must never interrupt a running task", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateWorkspace_ZeroesActiveTasks pins the second-order leak: Step 2 stops
// the container, so a force-hibernated row must not keep a stale active_tasks > 0.
// If it did, the idle monitor (which only ever selects active_tasks = 0) could never
// auto-hibernate that workspace again after its next wake — silently forfeiting the
// cost saving hibernation exists for.
func TestHibernateWorkspace_ZeroesActiveTasks(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	handler.stopFnOverride = func(_ context.Context, _ string) {}

	wsID := "ws-zero-active"

	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Zeroed", 1))
	// The mark-hibernated UPDATE must clear active_tasks.
	mock.ExpectExec(`SET status = \$1, url = '', active_tasks = 0`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := handler.hibernateWorkspace(context.Background(), wsID, true); err != nil {
		t.Fatalf("force hibernate returned error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_ClaimNoOp_Returns409_NotFalse200 pins the lie itself. When the
// claim lands on no row (the workspace left online/degraded under us — a concurrent
// pause, restart, or hibernation), the handler must say so. Pre-fix it answered
// 200 {"status":"hibernated"} on this exact path, so a caller — human or e2e — was
// told a workspace had been hibernated while it was still online and still billing.
func TestHibernateHandler_ClaimNoOp_Returns409_NotFalse200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) { atomic.AddInt32(&stopCalls, 1) }

	wsID := "ws-claim-noop"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Racy", 1, 0))
	// Between the eligibility SELECT and the claim, someone paused the workspace.
	mock.ExpectExec(`UPDATE workspaces`).
		WithArgs(wsID, models.StatusHibernating).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := hibernateRequest(t, handler, wsID)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 when the claim matched no row, got %d: %s — a no-op hibernation must never report success", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"status":"hibernated"`) {
		t.Errorf("body claims the workspace is hibernated when nothing was stopped: %s", w.Body.String())
	}
	if got := atomic.LoadInt32(&stopCalls); got != 0 {
		t.Errorf("stop called %d times; want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestHibernateHandler_StopThenDBError_Returns500_NotFalse409 pins the other
// direction of the same lie. If Step 3's mark-hibernated UPDATE fails, Step 2 has
// ALREADY stopped the container — the workspace really is down, with its row wedged
// mid-transition at 'hibernating'. Collapsing that into the 409 ("it left the
// online state before the claim landed — nothing happened") would assert the exact
// opposite of the truth and misdirect whoever debugs it next. Only
// errHibernateNotClaimed may become a 409; everything else is a 500.
func TestHibernateHandler_StopThenDBError_Returns500_NotFalse409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	var stopCalls int32
	handler.stopFnOverride = func(_ context.Context, _ string) { atomic.AddInt32(&stopCalls, 1) }

	wsID := "ws-step3-dberr"

	mock.ExpectQuery(`SELECT name, tier, active_tasks FROM workspaces WHERE id = .* AND status IN`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "active_tasks"}).AddRow("Wedged", 1, 0))
	mock.ExpectExec(`UPDATE workspaces`). // claim succeeds
						WithArgs(wsID, models.StatusHibernating).
						WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Wedged", 1))
	// Step 3 blows up — but the container is already stopped by now.
	mock.ExpectExec(`SET status = \$1, url = '', active_tasks = 0`).
		WithArgs(models.StatusHibernated, wsID).
		WillReturnError(fmt.Errorf("connection reset by peer"))

	w := hibernateRequest(t, handler, wsID)

	if got := atomic.LoadInt32(&stopCalls); got != 1 {
		t.Fatalf("stop called %d times; want 1 — this test is only meaningful if the container was actually stopped", got)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 after a post-stop failure, got %d: %s — a stopped workspace must never be reported as 'nothing happened'", w.Code, w.Body.String())
	}
}
