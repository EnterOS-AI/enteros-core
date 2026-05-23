package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func attachDisplayControlAdminToken(t *testing.T, c *gin.Context) {
	t.Helper()
	t.Setenv("ADMIN_TOKEN", "test-admin-secret")
	c.Request.Header.Set("Authorization", "Bearer test-admin-secret")
}

func TestWorkspaceDisplayControl_NoActiveLockReturnsNone(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display/control", nil)

	handler.DisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "none" {
		t.Fatalf("controller = %v, want none", resp["controller"])
	}
	if _, ok := resp["expires_at"]; ok {
		t.Fatalf("none response included expires_at: %#v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_ClaimsUnlockedDisplay(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))
	mock.ExpectQuery(`INSERT INTO workspace_display_control_locks`).
		WithArgs("ws-display", "user", "admin-token", 300).
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", "admin-token", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "user" || resp["controlled_by"] != "admin-token" {
		t.Fatalf("lock response = %#v, want user/admin-token", resp)
	}
	if resp["expires_at"] == "" {
		t.Fatalf("expires_at missing in response: %#v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_ActiveLockReturnsConflict(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))
	mock.ExpectQuery(`INSERT INTO workspace_display_control_locks`).
		WithArgs("ws-display", "user", "admin-token", 300).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("agent", "sidecar", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control already held" {
		t.Fatalf("error = %v, want display control already held", resp["error"])
	}
	current, ok := resp["current"].(map[string]interface{})
	if !ok || current["controller"] != "agent" || current["controlled_by"] != "sidecar" {
		t.Fatalf("current lock = %#v, want agent/sidecar", resp["current"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsDisplayDisabledWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-no-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display not enabled" {
		t.Fatalf("error = %v, want display not enabled", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsCoarseSessionActor(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"user","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Cookie", "molecule_session=present")

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control requires admin-token or org-token auth" {
		t.Fatalf("error = %v, want display control requires admin-token or org-token auth", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_RemovesCallerLock(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectExec(`DELETE FROM workspace_display_control_locks WHERE workspace_id = \$1 AND controlled_by = \$2`).
		WithArgs("ws-display", "admin-token").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", nil)
	attachDisplayControlAdminToken(t, c)

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["controller"] != "none" {
		t.Fatalf("controller = %v, want none", resp["controller"])
	}
	if _, ok := resp["expires_at"]; ok {
		t.Fatalf("none response included expires_at: %#v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_ConflictWhenCallerDoesNotOwnLock(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expiresAt := time.Date(2026, 5, 23, 18, 30, 0, 0, time.UTC)

	mock.ExpectExec(`DELETE FROM workspace_display_control_locks WHERE workspace_id = \$1 AND controlled_by = \$2`).
		WithArgs("ws-display", "admin-token").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).
			AddRow("user", "org-token:abcd1234", expiresAt))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", nil)
	attachDisplayControlAdminToken(t, c)

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "display control held by another caller" {
		t.Fatalf("error = %v, want display control held by another caller", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplayControlRelease_RejectsOrgTokenForceRelease(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Set("org_token_prefix", "abcd1234")
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/release", bytes.NewBufferString(`{"force":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ReleaseDisplayControl(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "force release requires admin-token auth" {
		t.Fatalf("error = %v, want force release requires admin-token auth", resp["error"])
	}
}

func TestWorkspaceDisplayControlAcquire_RejectsAgentImpersonation(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-display/display/control/acquire", bytes.NewBufferString(`{"controller":"agent","ttl_seconds":300}`))
	c.Request.Header.Set("Content-Type", "application/json")
	attachDisplayControlAdminToken(t, c)

	handler.AcquireDisplayControl(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "browser callers may only acquire user display control" {
		t.Fatalf("error = %v, want browser callers may only acquire user display control", resp["error"])
	}
}
