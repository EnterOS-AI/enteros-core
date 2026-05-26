package handlers

// llm_billing_mode_handler_test.go — admin route coverage for the per-workspace
// LLM billing mode endpoint (internal#691).
//
// What this guards:
//   - GET path validates UUID + returns the BillingModeResolution shape
//   - PUT distinguishes "omitted mode" (400) from "explicit null" (clear)
//     from "string value" (set), so a typo'd field name can't silently no-op
//   - Unknown mode strings 400 from the validator, not from a PG CHECK
//     constraint round-trip (matters because the error message must be
//     actionable to a canvas user)
//   - 404 propagates when the workspace row is missing on a set/clear

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

const testWSID = "44444444-4444-4444-4444-444444444444"

func TestGetWorkspaceLLMBillingMode_HappyPath_InheritsOrgDefault(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModeBYOK)
	mock := setupTestDB(t)
	// Workspace has no override → resolver returns org_default = byok.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+testWSID+"/llm-billing-mode", nil)

	GetWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Errorf("resolved mode: got %q want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if res.Source != BillingModeSourceOrgDefault {
		t.Errorf("source: got %q want %q", res.Source, BillingModeSourceOrgDefault)
	}
	if res.WorkspaceOverride != nil {
		t.Errorf("expected nil override, got %v", *res.WorkspaceOverride)
	}
}

func TestGetWorkspaceLLMBillingMode_BadUUID_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/not-a-uuid/llm-billing-mode", nil)
	GetWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestPutWorkspaceLLMBillingMode_SetByok(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
		WithArgs(LLMBillingModeBYOK, testWSID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Readback after write.
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(LLMBillingModeBYOK))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"byok"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	PutWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Errorf("post-write resolved: got %q want %q", res.ResolvedMode, LLMBillingModeBYOK)
	}
	if res.Source != BillingModeSourceWorkspaceOverride {
		t.Errorf("post-write source: got %q want %q", res.Source, BillingModeSourceWorkspaceOverride)
	}
}

func TestPutWorkspaceLLMBillingMode_ExplicitNullClearsOverride(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BILLING_MODE", LLMBillingModePlatformManaged)
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT llm_billing_mode FROM workspaces WHERE id = \$1`).
		WithArgs(testWSID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_billing_mode"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":null}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	PutWorkspaceLLMBillingMode(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var res BillingModeResolution
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Errorf("post-clear resolved: got %q want %q", res.ResolvedMode, LLMBillingModePlatformManaged)
	}
	if res.Source != BillingModeSourceOrgDefault {
		t.Errorf("post-clear source: got %q want %q", res.Source, BillingModeSourceOrgDefault)
	}
	if res.WorkspaceOverride != nil {
		t.Errorf("post-clear override should be nil, got %v", *res.WorkspaceOverride)
	}
}

func TestPutWorkspaceLLMBillingMode_MissingModeField_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestPutWorkspaceLLMBillingMode_UnknownMode_400(t *testing.T) {
	setupTestDB(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"totally-bogus"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestPutWorkspaceLLMBillingMode_NoSuchWorkspace_404(t *testing.T) {
	mock := setupTestDB(t)
	// SET path: rows affected = 0 → SetWorkspaceLLMBillingMode returns sql.ErrNoRows
	// → handler maps to 404.
	mock.ExpectExec(`UPDATE workspaces SET llm_billing_mode = \$1 WHERE id = \$2`).
		WithArgs(LLMBillingModeBYOK, testWSID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: testWSID}}
	body := `{"mode":"byok"}`
	c.Request = httptest.NewRequest("PUT",
		"/admin/workspaces/"+testWSID+"/llm-billing-mode",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	PutWorkspaceLLMBillingMode(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404, body=%s", w.Code, w.Body.String())
	}
}
