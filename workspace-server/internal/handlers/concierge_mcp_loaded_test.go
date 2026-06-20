package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestHeartbeatHandler_PlatformManagementMCPMissing_FlipsOnlineToDegraded
// verifies core#3082: a platform concierge that reports loaded_mcp_tools but
// does NOT include the declared management MCP is marked degraded.
func TestHeartbeatHandler_PlatformManagementMCPMissing_FlipsOnlineToDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Initial heartbeat UPDATE.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-missing", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus=online, kind=platform.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	// platformAgentHasModelSecret: model secret exists.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// platformAgentManagementMCPLoaded: listDeclaredPlugins returns management MCP.
	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-missing").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	// Degraded UPDATE.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent management MCP declared but not loaded; marking degraded (core#3082)", "ws-mcp-missing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_DEGRADED broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-mcp-missing","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","some_other_tool"]}`
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

// TestHeartbeatHandler_PlatformManagementMCPLoaded_StaysOnline verifies that
// a platform concierge reporting the management MCP in loaded_mcp_tools stays
// online.
func TestHeartbeatHandler_PlatformManagementMCPLoaded_StaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-ok", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-ok").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-mcp-ok","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPName + `"]}`
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

// TestHeartbeatHandler_OldRuntimeNoLoadedTools_StaysOnline verifies backward
// compat: a platform concierge whose runtime does not report loaded_mcp_tools
// (nil field) is not degraded by the core#3082 check.
func TestHeartbeatHandler_OldRuntimeNoLoadedTools_StaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-old-runtime").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-old-runtime", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-old-runtime").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-old-runtime").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-old-runtime","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
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
