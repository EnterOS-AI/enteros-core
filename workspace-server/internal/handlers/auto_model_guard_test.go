package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// AUTO_REQUIRES_PLATFORM_CREDITS guard (ruling R19): selecting platform/auto
// (or bare auto) is refused when the workspace holds a direct vendor key. The
// guard is model-id based and does not depend on the SDK registry pin.

const autoGuardWS = "00000000-0000-0000-0000-0000000000a1"

func TestIsAutoModel(t *testing.T) {
	for m, want := range map[string]bool{
		"platform/auto": true, " platform/auto ": true, "auto": true, " auto": true,
		"": false, "automatic": false, "platform/autox": false, "anthropic/claude-opus-4-8": false,
	} {
		if got := isAutoModel(m); got != want {
			t.Errorf("isAutoModel(%q) = %v, want %v", m, got, want)
		}
	}
}

func putModel(t *testing.T, handler *SecretsHandler, model string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: autoGuardWS}}
	c.Request = httptest.NewRequest("PUT", "/workspaces/"+autoGuardWS+"/model",
		strings.NewReader(`{"model":"`+model+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.SetModel(c)
	return w
}

func expectRuntimeAndKeys(mock sqlmock.Sqlmock, keys ...string) {
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).WithArgs(autoGuardWS).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
	rows := sqlmock.NewRows([]string{"key"})
	for _, k := range keys {
		rows.AddRow(k)
	}
	mock.ExpectQuery(`SELECT key FROM workspace_secrets WHERE workspace_id = \$1`).WithArgs(autoGuardWS).WillReturnRows(rows)
}

func TestSetModel_AutoRefusedWhenVendorKeyStored(t *testing.T) {
	for _, model := range []string{"platform/auto", "auto"} {
		t.Run(model, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			expectRuntimeAndKeys(mock, "ANTHROPIC_API_KEY") // no INSERT expected: nothing may be written
			w := putModel(t, NewSecretsHandler(nil), model)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
			}
			for _, want := range []string{`"code":"AUTO_REQUIRES_PLATFORM_CREDITS"`,
				"Auto uses platform credits. Remove your provider API key or choose a specific model."} {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body missing %q: %s", want, w.Body.String())
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock: %v", err)
			}
		})
	}
}

func TestSetModel_AutoAllowedWithoutVendorKey(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	expectRuntimeAndKeys(mock, "SOME_OTHER_SECRET") // a non-vendor key does not trigger the guard
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs(autoGuardWS, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	w := putModel(t, NewSecretsHandler(nil), "platform/auto")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// Unchanged behavior: a concrete model on a BYOK workspace never consults the
// key table (no extra query), so it proceeds exactly as before.
func TestSetModel_ConcreteModelOnByokWorkspaceUnchanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).WithArgs(autoGuardWS).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
	mock.ExpectExec(`INSERT INTO workspace_secrets[\s\S]*'MODEL'`).
		WithArgs(autoGuardWS, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	w := putModel(t, NewSecretsHandler(nil), "minimax/MiniMax-M2.7")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestWorkspaceCreate_AutoWithVendorKeyPayload422(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Auto Byok","runtime":"claude-code","model":"platform/auto","secrets":{"ANTHROPIC_API_KEY":"sk-x"}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Create(c)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"code":"AUTO_REQUIRES_PLATFORM_CREDITS"`) {
		t.Fatalf("want 422 AUTO_REQUIRES_PLATFORM_CREDITS, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil { // refused before any DB work
		t.Errorf("unexpected DB activity: %v", err)
	}
}

// After a workspace is on platform/auto it is platform-managed, so raw
// vendor-key writes are rejected: platformManagedLLMModeForWorkspace is true and
// rejectPlatformManagedDirectLLMBypassForWorkspace answers 400.
func TestPlatformAuto_VendorKeyWritesRejectedAfterSwitch(t *testing.T) {
	runtimeQ := `SELECT runtime FROM workspaces WHERE id = \$1`
	secretsQ := `SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`
	mock := setupTestDB(t)
	const wsID = "33333333-3333-3333-3333-333333333333"
	for i := 0; i < 2; i++ { // the mode check runs once directly and once inside the reject helper
		mock.ExpectQuery(runtimeQ).WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
		mock.ExpectQuery(secretsQ).WithArgs(wsID).
			WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
				AddRow("MODEL", []byte("platform/auto"), 0).
				AddRow("ANTHROPIC_API_KEY", []byte("sk-not-decrypted"), 0))
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/workspaces/"+wsID+"/secrets", nil)
	if !platformManagedLLMModeForWorkspace(c, wsID) {
		t.Fatal("workspace on platform/auto must be platform-managed")
	}
	if !rejectPlatformManagedDirectLLMBypassForWorkspace(c, wsID, "ANTHROPIC_API_KEY") {
		t.Fatal("rejectPlatformManagedDirectLLMBypassForWorkspace must return true for ANTHROPIC_API_KEY")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// Provisioning a platform/auto workspace that still carries a vendor key strips
// it and injects the proxy env instead (billing never uses the user key).
func TestApplyPlatformManagedLLMEnv_PlatformAutoStripsVendorKey(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	envVars := map[string]string{"ANTHROPIC_API_KEY": "sk-user-vendor-key", "XAI_API_KEY": "xai-user-key"}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-auto", "claude-code", "platform/auto", nil)
	if !res.RoutedToPlatform {
		t.Fatal("platform/auto must route to the platform proxy")
	}
	if _, ok := envVars["XAI_API_KEY"]; ok {
		t.Error("XAI_API_KEY must be stripped")
	}
	for k, v := range envVars {
		if v == "sk-user-vendor-key" || v == "xai-user-key" {
			t.Errorf("user vendor key still present under %s", k)
		}
	}
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.example.test/api/v1/internal/llm/anthropic/v1" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the proxy URL", got)
	}
	if envVars["MOLECULE_LLM_USAGE_TOKEN"] != "tenant-admin-token" {
		t.Error("proxy usage token not injected")
	}
}
