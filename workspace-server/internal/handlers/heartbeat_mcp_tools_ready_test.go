package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// EV2 mcp_tools_ready heartbeat readiness — negative-control suite.
//
// These pin the POSITIVE half of the tools-loaded heartbeat signal
// (runtime#273 landed the negative half, launch_failure): core flips a
// kind=platform concierge provisioning->online on the FIRST beat carrying
// mcp_tools_ready=true, WITHOUT the retired wall-clock fireConciergeWarmup
// nudge and WITHOUT the runtime having to emit loaded_mcp_tools (turn-
// independent). The tri-state (absent != false != true) is load-bearing, so
// each state gets its own case:
//
//   - true    -> provisioning flips to online (the verified-ready UPDATE fires).
//   - absent  -> HELD in provisioning (warming stamp only; NO online UPDATE).
//   - false   -> HELD in provisioning (probed-not-ready; NO online UPDATE).
//
// The "absent"/"false" cases are the anti-vacuous negative controls: they prove
// the flip is CAUSED by mcp_tools_ready=true and not by merely reaching the
// handler. There is deliberately NO warmup A2A expectation anywhere — the warmup
// sender was removed with fireConciergeWarmup, so any resurrected synthetic-turn
// dispatch would have no wiring to fire through.

// TestHeartbeat_EV2_MCPToolsReadyTrue_FlipsProvisioningOnline is the core EV2
// proof: a provisioning platform concierge that reports mcp_tools_ready=true
// (and NO loaded_mcp_tools — proving the flip does not depend on the under-
// emitting loaded_mcp_tools producer, runtime#181) is promoted to online by the
// verified-ready UPDATE. No warmup turn is involved.
func TestHeartbeat_EV2_MCPToolsReadyTrue_FlipsProvisioningOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Pre-read (prevStatus=provisioning): fireReconcileOnline is nil-safe here.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-ev2-ready").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))

	// Main heartbeat UPDATE (the inline CASE excludes platform, so the row stays
	// provisioning until the verified-ready flip below).
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-ev2-ready", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// NO loaded_mcp_tools persist — the body omits the list.

	// evaluateStatus: currentStatus=provisioning, kind=platform, no stamp.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-ev2-ready").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))

	// platformAgentHasModelSecret: model secret exists (fail-closed gate passes).
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-ev2-ready").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// THE FLIP: verified-ready UPDATE promotes provisioning->online and clears any
	// warming stamp. This is the load-bearing assertion — it fires ONLY because
	// mcp_tools_ready=true made readyForOnline true.
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL.*WHERE id = .* AND status =").
		WithArgs(models.StatusOnline, "ws-ev2-ready", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_ONLINE broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-ev2-ready","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"mcp_tools_ready":true,"first_ready_at":"2026-07-17T17:00:00Z"}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeat_EV2_MCPToolsReadyAbsent_HoldsProvisioning is the anti-vacuous
// negative control: the SAME provisioning platform concierge, but with
// mcp_tools_ready ABSENT (nil = unknown / prober has not yet succeeded) and no
// loaded_mcp_tools, is NOT promoted. It hits the warming-hold branch, which only
// stamps mcp_unloaded_since — there is NO online UPDATE and NO WORKSPACE_ONLINE
// broadcast. Proves the flip is caused by the readiness event, not by reaching
// the handler.
func TestHeartbeat_EV2_MCPToolsReadyAbsent_HoldsProvisioning(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-ev2-absent").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-ev2-absent", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-ev2-absent").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-ev2-absent").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Warming-hold: first observation stamps mcp_unloaded_since. NO online flip,
	// NO broadcast. (If EV2 wrongly promoted on absence, this stamp expectation
	// would be unmet and an unexpected online UPDATE would fire.)
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = COALESCE").
		WithArgs("ws-ev2-absent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-ev2-absent","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestHeartbeat_EV2_MCPToolsReadyFalse_HoldsProvisioning proves the tri-state
// distinction: an EXPLICIT mcp_tools_ready=false (probed-not-ready — distinct
// from absent/unknown) also holds provisioning. false must NEVER be treated as
// true; the row stays warming (stamp only, no online flip, no broadcast).
func TestHeartbeat_EV2_MCPToolsReadyFalse_HoldsProvisioning(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Provide a pre-existing stamp so the warming-hold takes the no-stamp-needed
	// path (mcp_unloaded_since already set) — proves there is STILL no online
	// UPDATE even when the grace window is already open but not elapsed.
	recentStamp := time.Now().Add(-10 * time.Second)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-ev2-false").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-ev2-false", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-ev2-false").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, recentStamp))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-ev2-false").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// mcp_unloaded_since already valid → warming-hold logs but does NOT re-stamp
	// and does NOT flip online. No further DB writes.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-ev2-false","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"mcp_tools_ready":false}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
