package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== List secrets ====================

func TestSecretsList_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"key", "created_at", "updated_at"}).
			AddRow("API_KEY", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z").
			AddRow("DB_PASSWORD", "2024-01-02T00:00:00Z", "2024-01-03T00:00:00Z"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 2 {
		t.Errorf("expected 2 secrets, got %d", len(resp))
	}
	if resp[0]["key"] != "API_KEY" {
		t.Errorf("expected first key 'API_KEY', got %v", resp[0]["key"])
	}
	if resp[0]["has_value"] != true {
		t.Errorf("expected has_value true, got %v", resp[0]["has_value"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsList_Empty(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"key", "created_at", "updated_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(resp))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsList_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/not-a-uuid/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "invalid workspace ID" {
		t.Errorf("expected error 'invalid workspace ID', got %v", resp["error"])
	}
}

func TestSecretsList_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery("SELECT key, created_at, updated_at FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", nil)

	handler.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Set secret ====================

func TestSecretsSet_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "bad-id"}}

	body := `{"key":"API_KEY","value":"secret123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/bad-id/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_MissingKey(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"value":"secret123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_MissingValue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsSet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	// The crypto.Encrypt will use plaintext mode if SECRETS_ENCRYPTION_KEY is not set
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY","value":"sk-test123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("expected status 'saved', got %v", resp["status"])
	}
	if resp["key"] != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSet_AutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Track whether restart was called via channel (replaces time.Sleep)
	done := make(chan string, 1)
	restartFunc := func(wsID string) {
		done <- wsID
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// autoRestartAllowed (core#2573) checks the target's kind before firing.
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case wsID := <-done:
		if wsID != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("expected restart to be called with workspace ID, got %q", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSet_NoAutoRestart_SelfWrite asserts core#2573 / #2605: when the
// caller IS the target workspace, the secret write succeeds but auto-restart is
// suppressed — restarting would tear down the writing agent mid-turn.
func TestSecretsSet_NoAutoRestart_SelfWrite(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	done := make(chan string, 1)
	restartFunc := func(wsID string) { done <- wsID }
	handler := NewSecretsHandler(restartFunc)

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs(wsID, "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// autoRestartAllowed returns false on callerID == workspaceID before any
	// DB kind lookup, so no SELECT COALESCE(kind...) expectation here.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", wsID) // caller == target

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-done:
		t.Fatal("auto-restart MUST be skipped for a self-write")
	case <-time.After(200 * time.Millisecond):
		// expected: no restart
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSet_NoAutoRestart_PlatformRoot asserts core#2573 / #2605: the
// concierge (kind='platform') is the org root; secret writes/deletes there must
// NOT auto-restart it, because that terminates the org root's box. This is the
// code-side enforcement of the no-self-secret-ops / safe-approval rule.
func TestSecretsSet_NoAutoRestart_PlatformRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	done := make(chan string, 1)
	restartFunc := func(wsID string) { done <- wsID }
	handler := NewSecretsHandler(restartFunc)

	wsID := "550e8400-e29b-41d4-a716-446655440000"
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs(wsID, "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Caller is unauthenticated admin-style, so autoRestartAllowed falls through
	// to the kind lookup and must see 'platform'.
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}

	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	// No X-Workspace-ID and no bearer token => callerID == "" (concierge MCP path).

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-done:
		t.Fatal("auto-restart MUST be skipped for the platform root")
	case <-time.After(200 * time.Millisecond):
		// expected: no restart
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSet_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"API_KEY","value":"secret"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Set(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== Delete secret ====================

func TestSecretsDelete_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("expected status 'deleted', got %v", resp["status"])
	}
	if resp["key"] != "API_KEY" {
		t.Errorf("expected key 'API_KEY', got %v", resp["key"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "MISSING_KEY").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "MISSING_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/MISSING_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "invalid"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/invalid/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsDelete_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "API_KEY").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "API_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/API_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsDelete_AutoRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	done := make(chan string, 1)
	restartFunc := func(wsID string) {
		done <- wsID
	}
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// autoRestartAllowed (core#2573) checks the target's kind before firing.
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "OLD_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/OLD_KEY", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case wsID := <-done:
		if wsID != "550e8400-e29b-41d4-a716-446655440000" {
			t.Errorf("expected restart called for workspace, got %q", wsID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart callback not called within timeout")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== GetModel ====================

func TestSecretsGetModel_Unresolved(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	// No MODEL secret (formerly MODEL_PROVIDER — see 2026-05-19 rename
	// migration). Pin the WHERE clause so a regression that reads the
	// wrong column-name shows up here.
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs("ws-model").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-model"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-model/model", nil)

	handler.GetModel(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["model"] != "" {
		t.Errorf("expected empty model, got %v", resp["model"])
	}
	// core#2594: an absent MODEL secret is "unresolved", not "default" — the
	// platform no longer substitutes a default model, so the empty state is
	// reported truthfully (the workspace will fail closed at provision).
	if resp["source"] != "unresolved" {
		t.Errorf("expected source 'unresolved', got %v", resp["source"])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsGetModel_DBError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs("ws-model-err").
		WillReturnError(sql.ErrConnDone)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-model-err"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-model-err/model", nil)

	handler.GetModel(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== SetModel ====================

func TestSecretsSetModel_Upsert(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restartCalled := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restartCalled <- id })

	// Runtime lookup (issue #2172) — model is non-empty so validation fires.
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs("00000000-0000-0000-0000-000000000001").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	// Pin the literal 'MODEL' key in the SQL so a regression to the
	// pre-2026-05-19 'MODEL_PROVIDER' column name shows up here.
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs("00000000-0000-0000-0000-000000000001", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000001"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000001/model",
		strings.NewReader(`{"model":"minimax/MiniMax-M2.7"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-restartCalled:
		if id != "00000000-0000-0000-0000-000000000001" {
			t.Errorf("restart called with wrong id: %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetModel_EmptyClears(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(func(string) {})

	// Pin the literal 'MODEL' key — see TestSecretsSetModel_Upsert.
	mock.ExpectExec(`DELETE FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs("00000000-0000-0000-0000-000000000002").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000002"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000002/model",
		strings.NewReader(`{"model":""}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsSetModel_InvalidID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/not-a-uuid/model",
		strings.NewReader(`{"model":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad UUID, got %d", w.Code)
	}
}

// TestSecretsSetModel_UnregisteredModel_422 guards that a model not in the
// runtime's native set is rejected at save (issue #2172 continuation).
func TestSecretsSetModel_UnregisteredModel_422(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs("00000000-0000-0000-0000-000000000003").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000003"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000003/model",
		strings.NewReader(`{"model":"totally-made-up-model-xyz"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "UNREGISTERED_MODEL_FOR_RUNTIME") {
		t.Errorf("expected code UNREGISTERED_MODEL_FOR_RUNTIME in body, got: %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSetModel_UnknownRuntimeFailOpen_200 verifies the federation
// contract: a runtime absent from the registry (langgraph) passes through
// without validation so non-first-party runtimes are not blocked.
func TestSecretsSetModel_UnknownRuntimeFailOpen_200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs("00000000-0000-0000-0000-000000000004").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("langgraph"))

	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs("00000000-0000-0000-0000-000000000004", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000004"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000004/model",
		strings.NewReader(`{"model":"any-arbitrary-model"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSetModel_WorkspaceNotFound_404 verifies 404 when the runtime
// lookup finds no workspace row.
func TestSecretsSetModel_WorkspaceNotFound_404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs("00000000-0000-0000-0000-000000000005").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000005"}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000005/model",
		strings.NewReader(`{"model":"claude-sonnet-4-6"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetModel(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsModel_RoundTrip_KeyIsMODELNotMODEL_PROVIDER pins the
// 2026-05-19 rename: writes via SetModel land under workspace_secrets
// key='MODEL', and reads via GetModel hit the same key. A regression
// that reverts either side to 'MODEL_PROVIDER' will mismatch sqlmock's
// query-regex anchor and fail loudly here. Combined integration-shape
// guard for the secrets.go half of fix/workspace-server-rename-
// MODEL_PROVIDER-to-MODEL.
func TestSecretsModel_RoundTrip_KeyIsMODELNotMODEL_PROVIDER(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(func(string) {})

	// 1. SetModel — must hit key='MODEL' in the INSERT.
	// Runtime lookup (issue #2172) — model is non-empty so validation fires.
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs("00000000-0000-0000-0000-000000000099").
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("codex"))
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'[\s\S]*ON CONFLICT`).
		WithArgs("00000000-0000-0000-0000-000000000099", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	c1.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000099"}}
	c1.Request = httptest.NewRequest("PUT", "/workspaces/00000000-0000-0000-0000-000000000099/model",
		strings.NewReader(`{"model":"gpt-5.5"}`))
	c1.Request.Header.Set("Content-Type", "application/json")
	handler.SetModel(c1)
	if w1.Code != http.StatusOK {
		t.Fatalf("SetModel: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}

	// 2. GetModel — must hit key='MODEL' in the SELECT. Return raw
	//    bytes; the handler will run them through DecryptVersioned.
	//    crypto is disabled in the test env (no MASTER_KEY), so the
	//    raw bytes pass through unchanged. We assert the SELECT
	//    fires against key='MODEL' (the rename pin); the decoded
	//    value isn't load-bearing for this contract test.
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs("00000000-0000-0000-0000-000000000099").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
			AddRow([]byte("gpt-5.5"), 0))

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Params = gin.Params{{Key: "id", Value: "00000000-0000-0000-0000-000000000099"}}
	c2.Request = httptest.NewRequest("GET", "/workspaces/00000000-0000-0000-0000-000000000099/model", nil)
	handler.GetModel(c2)
	if w2.Code != http.StatusOK {
		t.Fatalf("GetModel: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// We don't assert resp["model"] equals "gpt-5.5" because crypto
	// state in this package varies by build tag; the load-bearing
	// contract is the workspace_secrets key, pinned by the sqlmock
	// regex above. If a future change adds encryption to the test
	// env, the round-trip value check can move to an integration
	// test that owns the crypto state.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations — Model round-trip did not hit key='MODEL' on both sides: %v", err)
	}
}

// ==================== GetProvider / SetProvider — RETIRED ====================
//
// internal#718 P4 closure: the GetProvider/SetProvider suite covered the
// LLM_PROVIDER workspace_secret round-trip. Both handlers and the
// shared setProviderSecret helper were removed when the secret itself
// was retired. The replacement endpoint behavior (410 Gone with a
// structured body) is covered by
// `llm_provider_removal_p4_test.go::TestPutProvider_410Gone`,
// `TestGetProvider_410Gone`, and
// `TestProviderEndpointGone_BodyShape`.

// ==================== Values — Phase 30.2 decrypted pull ====================

// These tests target the secrets.Values handler (GET /workspaces/:id/secrets/values)
// which returns decrypted key→value pairs so remote agents can bootstrap their env
// without the provisioner pushing at container-create time. Auth follows the
// Phase 30.1 lazy-bootstrap contract: workspaces with any live token MUST present
// a matching Bearer, legacy workspaces (no tokens yet) are grandfathered through.

const testWsID = "550e8400-e29b-41d4-a716-446655440000"

// secretsValuesRequest builds a GET request with the given Authorization header.
func secretsValuesRequest(w http.ResponseWriter, auth string) *gin.Context {
	c, _ := gin.CreateTestContext(w.(*httptest.ResponseRecorder))
	c.Params = gin.Params{{Key: "id", Value: testWsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+testWsID+"/secrets/values", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	c.Request = req
	return c
}

func TestSecretsValues_LegacyWorkspaceGrandfathered(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	// No tokens on file → grandfather path
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("GLOBAL_KEY", []byte("plainvalue"), 0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("WS_KEY", []byte("ws_plainvalue"), 0))
	// internal#711: Values now resolves billing mode to gate the global LLM-cred
	// merge. Neither key here is a platform-managed LLM bypass key, so the mode
	// is immaterial to the assertions — but the resolver query must be mocked.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModePlatformManaged))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "") // no auth — grandfathered
	handler.Values(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body["GLOBAL_KEY"] != "plainvalue" || body["WS_KEY"] != "ws_plainvalue" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestSecretsValues_MissingTokenWhenOnFile(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "")
	handler.Values(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsValues_WrongToken(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// ValidateToken lookup returns nothing
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "Bearer wrong-token")
	handler.Values(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSecretsValues_ValidTokenReturnsDecryptedMerge(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", testWsID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Global and workspace secrets — workspace overrides SHARED_KEY
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("ONLY_GLOBAL", []byte("global_val"), 0).
			AddRow("SHARED_KEY", []byte("global_loses"), 0))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("ONLY_WS", []byte("ws_val"), 0).
			AddRow("SHARED_KEY", []byte("ws_wins"), 0))
	// internal#711: billing-mode resolver query. None of these keys is a
	// platform-managed LLM bypass key, so the resolved mode does not affect the
	// merge assertions; platform_managed keeps the existing pass-through.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModePlatformManaged))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "Bearer good-token")
	handler.Values(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["ONLY_GLOBAL"] != "global_val" {
		t.Errorf("global missing: %v", body)
	}
	if body["ONLY_WS"] != "ws_val" {
		t.Errorf("ws missing: %v", body)
	}
	if body["SHARED_KEY"] != "ws_wins" {
		t.Errorf("workspace should override global: got %q", body["SHARED_KEY"])
	}
}

// TestSecretsValues_ByokServesTenantGlobalLLMCred is the molecule-core#1994
// (corrected-model) regression guard for the remote-pull path. `global_secrets`
// is the TENANT's store, so a byok workspace's pull MUST include the tenant's
// own global-scope LLM credential — that is exactly what byok runs on, direct.
//
// Pre-fix (internal#711) this path STRIPPED the global-origin oauth on byok,
// resting on the inverted premise that a global LLM cred was "the platform's
// own"; that killed legitimate byok workspaces whose oauth lived at global
// scope. The strip is removed: the merged bundle (tenant globals + workspace
// overrides) is served verbatim.
//
// Mutation: re-add the byok global-LLM-cred strip in secrets.go Values() →
// CLAUDE_CODE_OAUTH_TOKEN disappears from the body → this test RED.
func TestSecretsValues_ByokServesTenantGlobalLLMCred(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewSecretsHandler(nil)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", testWsID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// global_secrets holds the TENANT's own global-scope OAuth token (shared
	// across all the tenant's workspaces) + a non-LLM global.
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("CLAUDE_CODE_OAUTH_TOKEN", []byte("TENANT-OWN-GLOBAL-OAUTH"), 0).
			AddRow("SENTRY_DSN", []byte("https://sentry.example/123"), 0))
	// This workspace set no LLM secret of its own — it relies on the tenant
	// global-scope oauth.
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id`).
		WithArgs(testWsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("MODEL", []byte("opus"), 0))

	w := httptest.NewRecorder()
	c := secretsValuesRequest(w, "Bearer good-token")
	handler.Values(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	// 1. The tenant's own global-scope OAuth token SURVIVES — byok runs on it.
	if body["CLAUDE_CODE_OAUTH_TOKEN"] != "TENANT-OWN-GLOBAL-OAUTH" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the tenant's own global-scope token served for byok pull", body["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	// 2. The workspace's own non-LLM secret survives.
	if body["MODEL"] != "opus" {
		t.Fatalf("MODEL = %q, want opus preserved", body["MODEL"])
	}
	// 3. Unrelated non-LLM global secrets are untouched.
	if body["SENTRY_DSN"] != "https://sentry.example/123" {
		t.Fatalf("SENTRY_DSN = %q, want non-LLM globals untouched", body["SENTRY_DSN"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSecretsValues_InvalidWorkspaceID(t *testing.T) {
	setupTestDB(t)
	handler := NewSecretsHandler(nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/not-a-uuid/secrets/values", nil)
	handler.Values(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ==================== Global secret auto-restart (issue #15) ====================

// TestSetGlobal_AutoRestartsAffectedWorkspaces documents the fix for #15:
// rotating a global secret (e.g. CLAUDE_CODE_OAUTH_TOKEN) must propagate to
// every running workspace without a manual restart loop. The handler should
// fire RestartByID for each non-paused/non-removed workspace that does NOT
// have a workspace-level override of the same key.
func TestSetGlobal_AutoRestartsAffectedWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 4)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// INSERT ... ON CONFLICT for the global secret itself.
	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Query for affected workspaces — ws-A inherits, ws-B overrides (excluded).
	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-a").
			AddRow("ws-c"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"CLAUDE_CODE_OAUTH_TOKEN","value":"sk-ant-oat01-new"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Collect both expected restarts (order not guaranteed).
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-restarted:
			seen[id] = true
		case <-deadline:
			t.Fatalf("auto-restart not fired for all affected workspaces; got %v", seen)
		}
	}
	if !seen["ws-a"] || !seen["ws-c"] {
		t.Errorf("expected ws-a and ws-c restarted, got %v", seen)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSetGlobal_AllowsTenantOwnedVendorKeyDespiteLegacyOrgEnv pins the
// internal#718 correction: the org-level LLM billing rung is RETIRED (billing
// is resolved per-workspace, not per-org). A global secret is the tenant's OWN
// shared credential and is always writable at global scope; the provision-time
// provider-matched strip (workspace_provision) keeps any platform-managed
// workspace from USING a non-matching global cred, and per-workspace secret
// writes still enforce the strip-list via the per-workspace guard. So even with
// the legacy MOLECULE_LLM_BILLING_MODE env still set to platform_managed, a
// global vendor/oauth key write MUST SUCCEED (200) and persist — the retired
// org rung no longer gates it.
//
// Mutation: re-add the org-level rejectPlatformManagedDirectLLMBypass guard to
// SetGlobal → the write 400s before the INSERT → this test RED.
func TestSetGlobal_AllowsTenantOwnedVendorKeyDespiteLegacyOrgEnv(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 2)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	// Legacy org env still platform_managed — it must no longer gate the write.
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)

	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-a"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"CLAUDE_CODE_OAUTH_TOKEN","value":"sk-ant-oat01-tenant-own"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (global write allowed; org rung retired), got %d: %s", w.Code, w.Body.String())
	}
	// Wait on the async restart fan-out so its SELECT drains before db swap.
	select {
	case id := <-restarted:
		if id != "ws-a" {
			t.Errorf("expected ws-a restarted, got %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-restart not fired for affected workspace")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDeleteGlobal_AutoRestartsAffectedWorkspaces covers the delete branch of #15.
func TestDeleteGlobal_AutoRestartsAffectedWorkspaces(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 2)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	mock.ExpectExec("DELETE FROM global_secrets").
		WithArgs("OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("OLD_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-x"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "key", Value: "OLD_KEY"}}
	c.Request = httptest.NewRequest("DELETE", "/admin/secrets/OLD_KEY", nil)

	handler.DeleteGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "ws-x" {
			t.Errorf("expected ws-x, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-restart not fired")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// (core#2574) Regression test: a caller presenting the tenant ADMIN_TOKEN
// (the concierge's MCP credential) MUST be gated when writing a workspace
// secret. The live incident (2026-06-11) had the gate INERT on the
// admin-token path — TEST_APPROVAL_SECRET + TEST_APPROVAL_DUMMY_KEY were
// written with zero pending approvals and the secret-change auto-restart
// fired (core#2573). The fix: gateDestructive on ActionSecretWrite
// now checks callerIsAdminToken (caller_is_admin_token context key set
// by AdminAuth on the Tier 2b ADMIN_TOKEN path) and ALWAYS gates,
// regardless of the rollout flag.
func TestSecretsSet_AdminToken_GatedByApproval(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewSecretsHandler(nil)

	// requireApproval sequence for an admin-token caller (gated action,
	// no pre-existing approval). The gate's requireApproval runs THREE
	// queries: UPDATE (consume), INSERT (new pending), SELECT (parent_id).
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-core2574-secret-write"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// NOTE: deliberately NO `INSERT INTO workspace_secrets` mock setup. If
	// the gate is bypassed (the bug), the handler reaches the INSERT and
	// sqlmock returns an error → test fails. The gate firing = no INSERT
	// = test passes.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}

	body := `{"key":"TEST_APPROVAL_SECRET","value":"should-have-required-approval"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// core#2574: the auth middleware sets caller_is_admin_token when
	// the request authenticates via Tier 2b ADMIN_TOKEN.
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")

	// Rollout flag is OFF (default) — this is the regression assertion:
	// even WITHOUT MOLECULE_PLATFORM_APPROVAL_GATE=1, the admin-token
	// path MUST gate.
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	handler.Set(c)

	// Gate fires → 202 Accepted with a pending approval_id.
	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token + secret_write MUST return 202 (Phase-4 approval gate), got %d: %s",
			w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "pending_approval" {
		t.Errorf("status = %v, want \"pending_approval\"", resp["status"])
	}
	if resp["approval_id"] != "appr-core2574-secret-write" {
		t.Errorf("approval_id = %v, want \"appr-core2574-secret-write\"", resp["approval_id"])
	}
	if resp["action"] != "secret_write" {
		t.Errorf("action = %v, want \"secret_write\"", resp["action"])
	}
}

// TestSecretsSet_SkipSelfRestart_WhenCallerIsTarget (#2573): when an agent
// writes a secret to its own workspace, auto-restart must NOT fire — otherwise
// the restart kills the writing agent mid-turn.
func TestSecretsSet_SkipSelfRestart_WhenCallerIsTarget(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", "550e8400-e29b-41d4-a716-446655440000")

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-restarted:
		t.Fatal("restart must NOT fire when caller is the target workspace")
	case <-time.After(200 * time.Millisecond):
		// Expected — no restart.
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsDelete_SkipSelfRestart_WhenCallerIsTarget (#2573): symmetric
// skip for the DELETE path.
func TestSecretsDelete_SkipSelfRestart_WhenCallerIsTarget(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "OLD_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "OLD_KEY"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/OLD_KEY", nil)
	c.Request.Header.Set("X-Workspace-ID", "550e8400-e29b-41d4-a716-446655440000")

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-restarted:
		t.Fatal("restart must NOT fire when caller is the target workspace")
	case <-time.After(200 * time.Millisecond):
		// Expected — no restart.
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSet_SkipAutoRestart_PlatformRoot (#2573): a secret write
// targeting the org's kind='platform' concierge must NOT auto-restart it,
// regardless of who the caller is. The management MCP authenticates with the
// tenant ADMIN token (callerID == ""), so the self-write skip never fires for
// the concierge — this kind check is what actually protects the org root.
func TestSecretsSet_SkipAutoRestart_PlatformRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "TEST_SECRET", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	body := `{"key":"TEST_SECRET","value":"v"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	// No workspace bearer / X-Workspace-ID: admin-token caller, callerID == "".

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-restarted:
		t.Fatal("restart must NOT fire for a kind='platform' target")
	case <-time.After(200 * time.Millisecond):
		// Expected — no restart.
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsDelete_SkipAutoRestart_PlatformRoot (#2573): symmetric skip for
// the DELETE path — cleaning up secrets on the concierge must not kill it.
func TestSecretsDelete_SkipAutoRestart_PlatformRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("DELETE FROM workspace_secrets WHERE workspace_id").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "TEST_SECRET").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"},
		{Key: "key", Value: "TEST_SECRET"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/TEST_SECRET", nil)

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case <-restarted:
		t.Fatal("restart must NOT fire for a kind='platform' target")
	case <-time.After(200 * time.Millisecond):
		// Expected — no restart.
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSetGlobal_SkipSelfRestart_WhenCallerIsAffected (#2573): when the org
// platform agent (caller) sets a global secret, its own workspace must be
// excluded from the fan-out restart so it isn't killed mid-turn.
func TestSetGlobal_SkipSelfRestart_WhenCallerIsAffected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 4)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	callerWS := "ws-caller-550e8400-e29b-41d4-a716-446655440000"

	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Query returns ws-a, ws-b, and the caller's workspace.
	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("CLAUDE_CODE_OAUTH_TOKEN").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-a").
			AddRow(callerWS).
			AddRow("ws-b"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"CLAUDE_CODE_OAUTH_TOKEN","value":"sk-ant-oat01-new"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Workspace-ID", callerWS)

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-restarted:
			seen[id] = true
		case <-deadline:
			t.Fatalf("auto-restart not fired for all affected workspaces; got %v", seen)
		}
	}
	if !seen["ws-a"] || !seen["ws-b"] {
		t.Errorf("expected ws-a and ws-b restarted, got %v", seen)
	}
	if seen[callerWS] {
		t.Errorf("caller workspace %q must NOT be restarted", callerWS)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSetGlobal_FanOutQuery_ExcludesPlatformRoot (#2573): the fan-out query
// itself must exclude kind='platform' workspaces — the org root must never be
// auto-restarted by a global-secret rotation. The regex pins the SQL filter;
// rows returned already reflect it, so the assertion lives in the query match.
func TestSetGlobal_FanOutQuery_ExcludesPlatformRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 2)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("ROTATED_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`(?s)SELECT id FROM workspaces.*COALESCE\(kind, 'workspace'\) <> 'platform'`).
		WithArgs("ROTATED_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-a"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"ROTATED_KEY","value":"v2"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "ws-a" {
			t.Errorf("expected ws-a restarted, got %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart not fired for the affected regular workspace")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsSet_SpoofedHeader_DoesNotSuppressRestart (core#2584 regression):
// a request presenting a valid workspace bearer token MUST have its identity
// derived from the token, NOT from a spoofed X-Workspace-ID header. If the
// token says the caller is workspace A but the header claims workspace B
// (the target), restart MUST still fire — the unsigned header must not
// override authenticated identity.
func TestSecretsSet_SpoofedHeader_DoesNotSuppressRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// #2584: production order is INSERT (mutation) THEN callerWorkspaceID
	// (the bearer-token lookup) — sqlmock is ordered, so expect INSERT first.
	mock.ExpectExec("INSERT INTO workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "DB_PASS", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token lookup: the bearer resolves to ws-caller, NOT the target.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-caller"))
	// autoRestartAllowed (core#2573) checks the target's kind before firing.
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}}
	body := `{"key":"DB_PASS","value":"password123"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer fake-workspace-token")
	c.Request.Header.Set("X-Workspace-ID", "550e8400-e29b-41d4-a716-446655440000") // spoofed to match target

	handler.Set(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "550e8400-e29b-41d4-a716-446655440000" {
			t.Fatalf("expected restart of target workspace, got %s", id)
		}
		// restart fired — spoofed header did NOT suppress it
	case <-time.After(200 * time.Millisecond):
		t.Fatal("restart MUST fire when token-derived caller differs from target; spoofed header must not suppress")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSecretsDelete_SpoofedHeader_DoesNotSuppressRestart (core#2584):
// symmetric spoof-test for the DELETE path.
func TestSecretsDelete_SpoofedHeader_DoesNotSuppressRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 1)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// #2584: production order is DELETE (mutation) THEN callerWorkspaceID
	// (the bearer-token lookup) — sqlmock is ordered, so expect DELETE first.
	mock.ExpectExec("DELETE FROM workspace_secrets").
		WithArgs("550e8400-e29b-41d4-a716-446655440000", "DB_PASS").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token lookup: the bearer resolves to ws-caller, NOT the target.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-caller"))
	// autoRestartAllowed (core#2573) checks the target's kind before firing.
	mock.ExpectQuery(`SELECT COALESCE\(kind`).
		WithArgs("550e8400-e29b-41d4-a716-446655440000").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "550e8400-e29b-41d4-a716-446655440000"}, {Key: "key", Value: "DB_PASS"}}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/550e8400-e29b-41d4-a716-446655440000/secrets/DB_PASS", nil)
	c.Request.Header.Set("Authorization", "Bearer fake-workspace-token")
	c.Request.Header.Set("X-Workspace-ID", "550e8400-e29b-41d4-a716-446655440000") // spoofed

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "550e8400-e29b-41d4-a716-446655440000" {
			t.Fatalf("expected restart of target workspace, got %s", id)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("restart MUST fire on DELETE when token-derived caller differs from target")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSetGlobal_SpoofedHeader_DoesNotSuppressRestart (core#2584):
// global secret write with a spoofed X-Workspace-ID must use the token-derived
// caller for the fan-out exclusion, not the header. ws-caller (from token) is
// excluded; ws-target (from spoofed header) is NOT excluded and must be restarted.
func TestSetGlobal_SpoofedHeader_DoesNotSuppressRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 4)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// #2584: production order is INSERT (mutation) THEN callerWorkspaceID
	// (the bearer-token lookup), THEN the affected-workspaces fan-out query.
	mock.ExpectExec("INSERT INTO global_secrets").
		WithArgs("GLOBAL_KEY", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token lookup: bearer resolves to ws-caller (the exclusion), NOT the spoofed header.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-caller"))

	// Affected workspaces: ws-target (does NOT have an overriding workspace secret).
	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("GLOBAL_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-target"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"key":"GLOBAL_KEY","value":"global-val"}`
	c.Request = httptest.NewRequest("POST", "/admin/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer fake-workspace-token")
	c.Request.Header.Set("X-Workspace-ID", "ws-target") // spoofed

	handler.SetGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "ws-target" {
			t.Fatalf("expected restart of ws-target, got %s", id)
		}
		// ws-target was restarted — it was NOT excluded (the exclusion was ws-caller from token)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ws-target MUST be restarted; spoofed header must not be used as exclusion")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDeleteGlobal_SpoofedHeader_DoesNotSuppressRestart (core#2584):
// symmetric spoof-test for the global DELETE path.
func TestDeleteGlobal_SpoofedHeader_DoesNotSuppressRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	restarted := make(chan string, 4)
	restartFunc := func(wsID string) { restarted <- wsID }
	handler := NewSecretsHandler(restartFunc)

	// #2584: production order is DELETE (mutation) THEN callerWorkspaceID
	// (the bearer-token lookup), THEN the affected-workspaces fan-out query.
	mock.ExpectExec("DELETE FROM global_secrets").
		WithArgs("GLOBAL_KEY").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Token lookup: bearer resolves to ws-caller (the exclusion), NOT the spoofed header.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-caller"))

	mock.ExpectQuery("SELECT id FROM workspaces").
		WithArgs("GLOBAL_KEY").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-target"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "key", Value: "GLOBAL_KEY"}}
	c.Request = httptest.NewRequest("DELETE", "/admin/secrets/GLOBAL_KEY", nil)
	c.Request.Header.Set("Authorization", "Bearer fake-workspace-token")
	c.Request.Header.Set("X-Workspace-ID", "ws-target") // spoofed

	handler.DeleteGlobal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case id := <-restarted:
		if id != "ws-target" {
			t.Fatalf("expected restart of ws-target, got %s", id)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ws-target MUST be restarted on global DELETE; spoofed header must not be used as exclusion")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
