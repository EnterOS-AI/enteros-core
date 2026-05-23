package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

func TestValidateWorkspaceCompute_AcceptsPhase1SizingAndDisplayNone(t *testing.T) {
	compute := models.WorkspaceCompute{
		InstanceType: "m6i.xlarge",
		Volume:       models.WorkspaceComputeVolume{RootGB: 100},
		Display:      models.WorkspaceComputeDisplay{Mode: "none"},
	}

	if err := validateWorkspaceCompute(compute); err != nil {
		t.Fatalf("validateWorkspaceCompute returned error for valid compute: %v", err)
	}
}

func TestValidateWorkspaceCompute_RejectsUnknownInstanceType(t *testing.T) {
	compute := models.WorkspaceCompute{InstanceType: "p4d.24xlarge"}

	if err := validateWorkspaceCompute(compute); err == nil {
		t.Fatal("validateWorkspaceCompute accepted unsupported instance type")
	}
}

func TestValidateWorkspaceCompute_RejectsOutOfRangeRootVolume(t *testing.T) {
	for _, rootGB := range []int{29, 501} {
		compute := models.WorkspaceCompute{Volume: models.WorkspaceComputeVolume{RootGB: rootGB}}
		if err := validateWorkspaceCompute(compute); err == nil {
			t.Fatalf("validateWorkspaceCompute accepted root_gb=%d", rootGB)
		}
	}
}

func TestWorkspaceComputeJSON_OmitsEmptyNestedSections(t *testing.T) {
	got, err := workspaceComputeJSON(models.WorkspaceCompute{
		InstanceType: "m6i.xlarge",
		Volume:       models.WorkspaceComputeVolume{RootGB: 100},
	})
	if err != nil {
		t.Fatalf("workspaceComputeJSON returned error: %v", err)
	}

	if strings.Contains(got, `"display"`) {
		t.Fatalf("workspaceComputeJSON included empty display section: %s", got)
	}
	if got != `{"instance_type":"m6i.xlarge","volume":{"root_gb":100}}` {
		t.Fatalf("workspaceComputeJSON = %s", got)
	}
}

func TestWorkspaceCreate_WithCompute_PersistsComputeJSON(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET compute = \$2::jsonb`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{
		"name":"Sized Agent",
		"external":true,
		"runtime":"external",
		"compute":{
			"instance_type":"m6i.xlarge",
			"volume":{"root_gb":100},
			"display":{"mode":"none"}
		}
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceCreate_WithInvalidCompute_ReturnsBadRequest(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{
		"name":"Oversized Agent",
		"compute":{"instance_type":"p4d.24xlarge"}
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBuildProvisionerConfig_CopiesComputeSizingFromPayload(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(workspace_dir`).
		WithArgs("ws-compute").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_dir", "workspace_access"}).AddRow("", "none"))

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cfg := handler.buildProvisionerConfig(
		context.Background(),
		"ws-compute",
		"",
		nil,
		models.CreateWorkspacePayload{
			Tier:    4,
			Runtime: "claude-code",
			Compute: models.WorkspaceCompute{
				InstanceType: "m6i.xlarge",
				Volume:       models.WorkspaceComputeVolume{RootGB: 100},
			},
		},
		nil,
		t.TempDir(),
		"workspace:ws-compute",
	)

	if cfg.InstanceType != "m6i.xlarge" {
		t.Errorf("cfg.InstanceType = %q, want m6i.xlarge", cfg.InstanceType)
	}
	if cfg.DiskGB != 100 {
		t.Errorf("cfg.DiskGB = %d, want 100", cfg.DiskGB)
	}
}

func TestWithStoredCompute_LoadsComputeForRestartPayloads(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-restart-compute").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"instance_type":"m6i.xlarge","volume":{"root_gb":100}}`))

	payload := models.CreateWorkspacePayload{Name: "Restart Me", Tier: 4, Runtime: "claude-code"}
	got := withStoredCompute(context.Background(), "ws-restart-compute", payload)

	if got.Compute.InstanceType != "m6i.xlarge" {
		t.Errorf("stored compute instance_type = %q, want m6i.xlarge", got.Compute.InstanceType)
	}
	if got.Compute.Volume.RootGB != 100 {
		t.Errorf("stored compute root_gb = %d, want 100", got.Compute.Volume.RootGB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_NonDisplayWorkspaceReturnsUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-no-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-no-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != false {
		t.Fatalf("available = %v, want false", resp["available"])
	}
	if resp["reason"] != "display_not_enabled" {
		t.Fatalf("reason = %v, want display_not_enabled", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_DisplayConfiguredReturnsSessionUnavailableContract(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["available"] != false {
		t.Fatalf("available = %v, want false", resp["available"])
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if resp["status"] != "not_configured" {
		t.Fatalf("status = %v, want not_configured", resp["status"])
	}
	if resp["mode"] != "desktop-control" || resp["protocol"] != "dcv" {
		t.Fatalf("mode/protocol = %v/%v, want desktop-control/dcv", resp["mode"], resp["protocol"])
	}
	if resp["width"] != float64(1920) || resp["height"] != float64(1080) {
		t.Fatalf("width/height = %v/%v, want 1920/1080", resp["width"], resp["height"])
	}
	if _, ok := resp["url"]; ok {
		t.Fatalf("display response exposed url before session infra exists: %v", resp["url"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_IgnoresUnrelatedStoredComputeSizingDrift(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display-sizing-drift").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"instance_type":"old.large","display":{"mode":"desktop-control","protocol":"dcv","width":1920,"height":1080}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-display-sizing-drift"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display-sizing-drift/display", nil)

	handler.Display(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_InvalidStoredDisplayConfigReturnsServerError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-invalid-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"vnc"}}`))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-invalid-display"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-invalid-display/display", nil)

	handler.Display(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse display response: %v", err)
	}
	if resp["error"] != "invalid display config" {
		t.Fatalf("error = %v, want invalid display config", resp["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
