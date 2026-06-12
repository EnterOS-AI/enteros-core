package handlers

// Tests for the core#2608 create-boundary BYOK hard-reject + the SSOT
// model-discovery endpoint (CTO 2026-06-11).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestCreate_BYOKModelNoCredential_422 pins the hard-fail: a registered
// BYOK-form model with no credential anywhere must be rejected at create
// with MISSING_BYOK_CREDENTIAL and NO workspace row.
func TestCreate_BYOKModelNoCredential_422(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", "/tmp/configs")

	// Only the global-keys scan runs; no INSERT may follow.
	mock.ExpectQuery("SELECT key FROM global_secrets").
		WillReturnRows(sqlmock.NewRows([]string{"key"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Doomed","model":"claude-sonnet-4-6"}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "MISSING_BYOK_CREDENTIAL" {
		t.Errorf("code = %v", resp["code"])
	}
	if !strings.Contains(resp["error"].(string), "moonshot/kimi-k2.6") {
		t.Errorf("error must steer to the platform default: %v", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (workspace INSERT must not run): %v", err)
	}
}

// TestCreate_BYOKModelGlobalCredential_Passes: a global-scope credential the
// derived provider accepts satisfies the gate (the Reno-Stars / #711-revert
// contract — global creds count for byok).
func TestCreate_BYOKModelGlobalCredential_Passes(t *testing.T) {
	ok, why := func() (bool, string) {
		mock := setupTestDB(t)
		mock.ExpectQuery("SELECT key FROM global_secrets").
			WillReturnRows(sqlmock.NewRows([]string{"key"}).AddRow("ANTHROPIC_API_KEY"))
		return validateBYOKCredentialSatisfiable(context.Background(), "claude-code", "claude-sonnet-4-6", nil)
	}()
	if !ok {
		t.Fatalf("global ANTHROPIC_API_KEY must satisfy the anthropic-api arm: %s", why)
	}
}

// TestCreate_PlatformSlashModel_NoQueries: the SSOT platform default resolves
// without touching the DB at all.
func TestCreate_PlatformSlashModel_NoQueries(t *testing.T) {
	mock := setupTestDB(t)
	ok, why := validateBYOKCredentialSatisfiable(context.Background(), "claude-code", "moonshot/kimi-k2.6", nil)
	if !ok {
		t.Fatalf("platform model must pass keyless: %s", why)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("platform path must be query-free: %v", err)
	}
}

// TestListOfferedModels_ClaudeCode pins the discovery payload: platform
// entries keyless, BYOK entries carrying auth_env — derived through the same
// registry the create gate enforces.
func TestListOfferedModels_ClaudeCode(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/llm/offered-models?runtime=claude-code", nil)

	ListOfferedModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Runtime string         `json:"runtime"`
		Models  []OfferedModel `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := map[string]OfferedModel{}
	for _, m := range resp.Models {
		byID[m.Model] = m
	}
	kimi, ok := byID["moonshot/kimi-k2.6"]
	if !ok || !kimi.PlatformBilled || len(kimi.AuthEnv) != 0 {
		t.Errorf("moonshot/kimi-k2.6 must be platform-billed keyless: %+v (present=%v)", kimi, ok)
	}
	byok, ok := byID["claude-sonnet-4-6"]
	if !ok || byok.PlatformBilled || len(byok.AuthEnv) == 0 {
		t.Errorf("claude-sonnet-4-6 must be BYOK with auth_env: %+v (present=%v)", byok, ok)
	}
}
