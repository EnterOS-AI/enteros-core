package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// #33 (RCA#2970 deadlock-break): a kind=platform concierge whose runtime
// reports mcp_server_present=false is marked failed and the heartbeat returns
// BEFORE the recovery branches that fire the declared-plugin reconcile. On
// SaaS that reconcile is the ONLY path that installs the management MCP into
// the running container, so without firing it here the concierge is stuck
// failed forever (mcp_server_present can never become true). This asserts the
// fix: the mcp-missing heartbeat STILL fails closed (markWorkspaceFailed) AND
// fires the recovery reconcile so the MCP can be delivered and a later
// heartbeat can climb failed→online.
func TestHeartbeatHandler_PlatformMCPMissing_FiresRecoveryReconcile(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	reconcileFired := make(chan string, 4)
	handler.SetReconcileFunc(func(_ context.Context, workspaceID string) {
		reconcileFired <- workspaceID
	})

	// prevTask/status read (status=online → not provisioning, so the
	// prevStatus==provisioning reconcile fire does NOT match; only the
	// deadlock-break fire can fire here).
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-fail").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	// Main heartbeat UPDATE.
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-fail", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus=online, kind=platform.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-mcp-fail").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	// RCA#2970 gate: model secret present (so we fall to the !hasMCP branch).
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-fail").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// markWorkspaceFailed: broadcast (structure_events) then the failed UPDATE.
	mcpMissingMsg := "platform agent heartbeat denied: /opt/molecule-mcp-server missing; refusing to mark online (RCA #2970 FAIL-CLOSED)"
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs("ws-mcp-fail", mcpMissingMsg, models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-mcp-fail","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":false}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	// The deadlock-break reconcile is fire-and-forget via globalGoAsync.
	select {
	case got := <-reconcileFired:
		if got != "ws-mcp-fail" {
			t.Errorf("recovery reconcile fired for wrong workspace: got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("#33 regression: mcp-missing heartbeat did NOT fire the recovery reconcile (concierge would stay failed forever)")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// A MODEL-missing platform agent must STILL fail closed but must NOT fire the
// plugin reconcile — a missing MODEL secret is not something a declared-plugin
// reconcile can fix, so the recovery fire is scoped to the !hasMCP branch only.
func TestHeartbeatHandler_PlatformModelMissing_DoesNotFireReconcile(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	reconcileFired := make(chan string, 4)
	handler.SetReconcileFunc(func(_ context.Context, workspaceID string) {
		reconcileFired <- workspaceID
	})

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-model-fail").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-model-fail", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-model-fail").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))
	// Model secret ABSENT → the switch picks the !hasModel branch first.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-model-fail").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	modelMissingMsg := "platform agent heartbeat denied: no seeded MODEL workspace_secret; refusing to mark online (RCA #2970 FAIL-CLOSED)"
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs("ws-model-fail", modelMissingMsg, models.StatusFailed).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// mcp_server_present=true so hasMCP=true; only the model is missing.
	body := `{"workspace_id":"ws-model-fail","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-reconcileFired:
		t.Fatalf("model-missing must NOT fire the plugin reconcile, but it fired for %q", got)
	case <-time.After(300 * time.Millisecond):
		// good — no reconcile fired
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The recovery reconcile is guarded so a concierge that fails the gate on
// every heartbeat (until the MCP lands) fires AT MOST one concurrent reconcile
// — otherwise each blocked beat would spawn another clone+deliver.
func TestFireReconcileMCPRecovery_InflightGuard(t *testing.T) {
	handler := NewRegistryHandler(newTestBroadcaster())
	started := make(chan string, 8)
	release := make(chan struct{})
	handler.SetReconcileFunc(func(_ context.Context, workspaceID string) {
		started <- workspaceID
		<-release // hold the goroutine "in flight"
	})

	ctx := context.Background()
	handler.fireReconcileMCPRecovery(ctx, "ws-x") // spawns reconcile goroutine
	handler.fireReconcileMCPRecovery(ctx, "ws-x") // guarded — must NOT spawn a second

	select {
	case got := <-started:
		if got != "ws-x" {
			t.Fatalf("reconcile fired for wrong workspace: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first recovery reconcile did not fire")
	}
	select {
	case <-started:
		t.Fatal("inflight guard failed: a second concurrent reconcile fired for the same workspace")
	case <-time.After(200 * time.Millisecond):
		// good — guard held
	}
	close(release) // let the goroutine complete + clear the inflight key (drained at cleanup)
}

// nil reconcile func / empty id must be no-ops (never panic).
func TestFireReconcileMCPRecovery_NilSafe(t *testing.T) {
	h := &RegistryHandler{} // reconcilePlugins is nil
	h.fireReconcileMCPRecovery(context.Background(), "ws-x")

	h2 := NewRegistryHandler(newTestBroadcaster())
	h2.SetReconcileFunc(func(_ context.Context, _ string) { t.Fatal("must not fire for empty workspace id") })
	h2.fireReconcileMCPRecovery(context.Background(), "")
}
