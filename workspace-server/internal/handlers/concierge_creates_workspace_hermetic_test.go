package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestConciergeCreatesWorkspace_Hermetic is the in-repo, mock-driven complement
// to the staging E2E test_staging_concierge_creates_workspace_e2e.sh. It proves
// the same three deterministic claims without any live staging dependency:
//
//  1. The concierge heartbeat's loaded_mcp_tools field contains the exact
//     management tool id mcp__molecule-platform__provision_workspace.
//  2. When that tool is reported, the concierge stays status=online (the
//     core#3082 gate is satisfied, not degraded).
//  3. The workspace create handler actually creates a workspace row — the real
//     side-effect that the staging E2E polls for.
func TestConciergeCreatesWorkspace_Hermetic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("heartbeat_reports_required_tool_stays_online", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		broadcaster := newTestBroadcaster()
		handler := NewRegistryHandler(broadcaster)

		// Base heartbeat UPDATE.
		mock.ExpectQuery("SELECT COALESCE\\(current_task").
			WithArgs("ws-concierge-ok").
			WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

		mock.ExpectExec("UPDATE workspaces SET").
			WithArgs("ws-concierge-ok", 0.0, "", 0, 60, "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		// core#3082 / molecule-core#3256: persist loaded_mcp_tools to the row.
		mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
			WithArgs(sqlmock.AnyArg(), "ws-concierge-ok").
			WillReturnResult(sqlmock.NewResult(0, 1))

		// evaluateStatus: currently online, kind=platform, no outstanding unload stamp.
		mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
			WithArgs("ws-concierge-ok").
			WillReturnRows(evalStatusRows("online", "platform", nil, nil))

		mock.ExpectQuery("SELECT EXISTS").
			WithArgs("ws-concierge-ok").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

		mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
			WithArgs("ws-concierge-ok").
			WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
				AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

		// Management MCP is loaded and there is no stale mcp_unloaded_since stamp,
		// so the handler has nothing to clear. NO degrade UPDATE and NO
		// structure_events broadcast should fire.

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		body := `{"workspace_id":"ws-concierge-ok","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
		c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.Heartbeat(c)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("heartbeat_missing_required_tool_degrades", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		broadcaster := newTestBroadcaster()
		handler := NewRegistryHandler(broadcaster)

		mock.ExpectQuery("SELECT COALESCE\\(current_task").
			WithArgs("ws-concierge-missing").
			WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

		mock.ExpectExec("UPDATE workspaces SET").
			WithArgs("ws-concierge-missing", 0.0, "", 0, 60, "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		// core#3082 / molecule-core#3256: persist loaded_mcp_tools to the row.
		mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
			WithArgs(sqlmock.AnyArg(), "ws-concierge-missing").
			WillReturnResult(sqlmock.NewResult(0, 1))

		sustained := time.Now().Add(-5 * time.Minute)
		mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
			WithArgs("ws-concierge-missing").
			WillReturnRows(evalStatusRows("online", "platform", nil, sustained))

		mock.ExpectQuery("SELECT EXISTS").
			WithArgs("ws-concierge-missing").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

		mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
			WithArgs("ws-concierge-missing").
			WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
				AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))

		mock.ExpectExec("UPDATE workspaces SET status =.*status = 'online'").
			WithArgs(models.StatusDegraded, "platform agent management MCP declared but not loaded; marking degraded (core#3082)", "ws-concierge-missing").
			WillReturnResult(sqlmock.NewResult(0, 1))

		mock.ExpectExec("INSERT INTO structure_events").
			WillReturnResult(sqlmock.NewResult(0, 1))

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		body := `{"workspace_id":"ws-concierge-missing","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other-server__other-tool"]}`
		c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.Heartbeat(c)

		if w.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("create_workspace_handler_creates_real_row", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		broadcaster := newTestBroadcaster()
		handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

		payload := models.CreateWorkspacePayload{
			Name:    "hermetic-concierge-worker",
			Role:    "engineer",
			Runtime: "claude-code",
			Model:   "anthropic:claude-opus-4-7",
			Tier:    3,
			Canvas: struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			}{X: 120, Y: 240},
		}

		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO workspaces").
			WithArgs(sqlmock.AnyArg(), payload.Name, sqlmock.AnyArg(), payload.Tier, payload.Runtime, "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		// Model secret is persisted for the non-empty model.
		mock.ExpectExec("INSERT INTO workspace_secrets").
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Canvas layout for the requested position.
		mock.ExpectExec("INSERT INTO canvas_layouts").
			WithArgs(sqlmock.AnyArg(), payload.Canvas.X, payload.Canvas.Y).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// WORKSPACE_PROVISIONING broadcast.
		mock.ExpectExec("INSERT INTO structure_events").
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Auth token minted for the new workspace.
		mock.ExpectExec("INSERT INTO workspace_auth_tokens").
			WillReturnResult(sqlmock.NewResult(0, 1))

		body, _ := json.Marshal(payload)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBuffer(body))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.Create(c)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		if resp["id"] == "" || resp["id"] == nil {
			t.Errorf("expected created workspace to have a non-empty id")
		}
		if resp["status"] != "provisioning" {
			t.Errorf("expected status 'provisioning', got %v", resp["status"])
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("get_workspace_returns_loaded_mcp_tools", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		broadcaster := newTestBroadcaster()
		handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

		wsID := "cccccccc-000f-0000-0000-000000000000"
		columns := []string{
			"id", "name", "role", "tier", "status", "agent_card", "url",
			"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
			"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
			"budget_limit", "monthly_spend",
			"broadcast_enabled", "talk_to_user_enabled", "compute", "kind",
			"loaded_mcp_tools",
		}
		mock.ExpectQuery("SELECT w.id, w.name").
			WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows(columns).
				AddRow(wsID, "Concierge", "concierge", 1, "online", []byte(`null`),
					"http://localhost:8001", nil, 0, 1, 0.0, "", 60, "", "claude-code",
					"", 0.0, 0.0, false,
					nil, 0, false, true, []byte(`{}`), "workspace",
					[]byte(`["a2a","`+conciergePlatformMCPProvisionWorkspaceTool+`"]`)))

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: wsID}}
		c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID, nil)

		handler.Get(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to parse response: %v", err)
		}
		loaded, ok := resp["loaded_mcp_tools"].([]interface{})
		if !ok || len(loaded) != 2 {
			t.Errorf("expected loaded_mcp_tools to be a 2-element array, got %v", resp["loaded_mcp_tools"])
		}
		if ok && (loaded[0] != "a2a" || loaded[1] != conciergePlatformMCPProvisionWorkspaceTool) {
			t.Errorf("expected loaded_mcp_tools [a2a %s], got %v", conciergePlatformMCPProvisionWorkspaceTool, loaded)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}
