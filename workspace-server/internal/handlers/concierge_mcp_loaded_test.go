package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestHeartbeatHandler_PlatformManagementMCPMissing_FlipsOnlineToDegraded
// verifies core#3082 (CR2 #12653 fix): a platform concierge that reports
// loaded_mcp_tools but does NOT include the literal required tool identifier
// `mcp__molecule-platform__create_workspace` is marked degraded. The old
// check compared the loaded tools against the plugin NAME
// (`molecule-ai-plugin-molecule-platform-mcp`) which never matches the
// namespaced tool ids Claude Code dispatches — that was the false-green.
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

	// Degraded UPDATE — required tool absent.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent management MCP declared but not loaded; marking degraded (core#3082)", "ws-mcp-missing").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_DEGRADED broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// loaded_mcp_tools has plenty of tools but NOT the literal required one.
	body := `{"workspace_id":"ws-mcp-missing","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other-server__other-tool"]}`
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
// a platform concierge reporting the literal required create_workspace tool
// in loaded_mcp_tools stays online. (The previous test loaded the plugin
// NAME as a fake tool — that was a no-op false-green; this test pins the
// real contract.)
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

	// loaded_mcp_tools carries the literal required tool identifier.
	body := `{"workspace_id":"ws-mcp-ok","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPCreateWorkspaceTool + `"]}`
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

// TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_Degraded
// pins the CR2+Researcher fail-loud behavior: a runtime that speaks the
// #147 contract (mcp_server_present=true) but does NOT report the new
// loaded_mcp_tools producer cannot prove the management MCP is actually
// loaded — flip to degraded instead of silent-skip. The previous
// "old-runtime stays online" test was the false-green #3082 exists to
// catch; the new contract says: if you can prove server-up, prove tools
// too, or fail loud.
func TestHeartbeatHandler_RuntimeEmitsServerPresentButNoLoadedTools_Degraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-server-present-no-tools", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-server-present-no-tools").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// Degraded UPDATE — runtime spoke server-present but omitted loaded_mcp_tools.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent runtime did not report loaded_mcp_tools on a mcp_server_present=true heartbeat; cannot verify create_workspace tool is loaded — marking degraded (core#3082)", "ws-server-present-no-tools").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// mcp_server_present=true but loaded_mcp_tools absent — runtime needs a
	// loaded_mcp_tools producer. Until it does, every platform concierge
	// will be flagged degraded (which is the honest signal).
	body := `{"workspace_id":"ws-server-present-no-tools","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true}`
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

// TestHeartbeatHandler_Pre147RuntimeNoMCPServerPresent_StaysOnline pins the
// backward-compat path: a runtime that predates the #147 contract (neither
// mcp_server_present nor loaded_mcp_tools) does NOT trigger the #3082 gate.
// The earlier platformAgentMCPServerPresent nil-tolerance keeps legacy
// runtimes serving until the runtime-side loaded_mcp_tools producer lands.
func TestHeartbeatHandler_Pre147RuntimeNoMCPServerPresent_StaysOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-pre-147").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-pre-147", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-pre-147").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-pre-147").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// No listDeclaredPlugins query — the #3082 gate is skipped entirely for
	// pre-#147 runtimes (mcp_server_present nil ⇒ platformAgentMCPServerPresent
	// returns true under nil-tolerance; the new gate requires
	// mcp_server_present != nil && *mcp_server_present).

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	body := `{"workspace_id":"ws-pre-147","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60}`
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

// TestHeartbeatHandler_PlatformManagementMCPLookupError_FlipsOnlineToDegraded
// verifies that a failure to read workspace_declared_plugins is fail-loud:
// the workspace is marked degraded rather than staying online with an
// unverified management MCP. This closes the false-green path where a broken
// lookup silently looked healthy (CR2 #12653 follow-up).
func TestHeartbeatHandler_PlatformManagementMCPLookupError_FlipsOnlineToDegraded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	// Initial heartbeat UPDATE.
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-mcp-lookup-err", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// evaluateStatus: currentStatus=online, kind=platform.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at FROM workspaces WHERE id =").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at"}).AddRow("online", "platform", nil))

	// platformAgentHasModelSecret: model secret exists.
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-mcp-lookup-err").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	// platformAgentManagementMCPLoaded: listDeclaredPlugins fails.
	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-mcp-lookup-err").
		WillReturnError(errors.New("connection refused"))

	// Degraded UPDATE — lookup failure must not silently look healthy.
	mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
		WithArgs(models.StatusDegraded, "platform agent declared management MCP lookup failed: declared-plugin lookup: listDeclaredPlugins: query: connection refused; marking degraded (core#3082)", "ws-mcp-lookup-err").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// WORKSPACE_DEGRADED broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Even though loaded_mcp_tools contains the required tool, the lookup error
	// takes precedence and the workspace must degrade.
	body := `{"workspace_id":"ws-mcp-lookup-err","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["` + conciergePlatformMCPCreateWorkspaceTool + `"]}`
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
