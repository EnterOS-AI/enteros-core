package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
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

// internal#734: data_persistence enum. "" (auto), "persist", "ephemeral" are
// the only accepted values; anything else is a clear 400 before the CP call.
func TestValidateWorkspaceCompute_DataPersistence(t *testing.T) {
	for _, ok := range []string{"", "persist", "ephemeral"} {
		c := models.WorkspaceCompute{DataPersistence: ok}
		if err := validateWorkspaceCompute(c); err != nil {
			t.Errorf("data_persistence=%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"persistent", "off", "none", "Ephemeral", "true"} {
		c := models.WorkspaceCompute{DataPersistence: bad}
		if err := validateWorkspaceCompute(c); err == nil {
			t.Errorf("data_persistence=%q must be rejected", bad)
		}
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

func TestValidateWorkspaceCompute_RejectsOutOfRangeDisplayDimensions(t *testing.T) {
	for _, display := range []models.WorkspaceComputeDisplay{
		{Mode: "desktop-control", Protocol: "novnc", Width: 799, Height: 1080},
		{Mode: "desktop-control", Protocol: "novnc", Width: 3841, Height: 1080},
		{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 599},
		{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 2161},
	} {
		compute := models.WorkspaceCompute{Display: display}
		if err := validateWorkspaceCompute(compute); err == nil {
			t.Fatalf("validateWorkspaceCompute accepted display size %dx%d", display.Width, display.Height)
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
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").
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
		"model":"claude-opus-4-7",
		"compute":{"instance_type":"p4d.24xlarge"}
	}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkspaceUpdate_WithCompute_PersistsComputeJSONAndRequiresRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wsID := "00000000-0000-0000-0000-000000000123"

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM workspaces WHERE id = \$1\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE workspaces SET compute = \$2::jsonb, updated_at = now\(\) WHERE id = \$1`).
		WithArgs(wsID, `{"display":{"height":1080,"mode":"desktop-control","protocol":"novnc","width":1920},"instance_type":"t3.xlarge","volume":{"root_gb":80}}`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{
		"compute":{
			"instance_type":"t3.xlarge",
			"volume":{"root_gb":80},
			"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}
		}
	}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["needs_restart"] != true {
		t.Fatalf("needs_restart = %v, want true", resp["needs_restart"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceUpdate_WithInvalidCompute_ReturnsBadRequest(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wsID := "00000000-0000-0000-0000-000000000124"

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"compute":{"instance_type":"p4d.24xlarge"}}`
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+wsID, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

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
				Display:      models.WorkspaceComputeDisplay{Mode: "desktop-control", Protocol: "novnc", Width: 1920, Height: 1080},
			},
		},
		nil,
		t.TempDir(),
	)

	if cfg.InstanceType != "m6i.xlarge" {
		t.Errorf("cfg.InstanceType = %q, want m6i.xlarge", cfg.InstanceType)
	}
	if cfg.DiskGB != 100 {
		t.Errorf("cfg.DiskGB = %d, want 100", cfg.DiskGB)
	}
	if cfg.Display.Mode != "desktop-control" || cfg.Display.Protocol != "novnc" {
		t.Errorf("cfg.Display mode/protocol = %q/%q, want desktop-control/novnc", cfg.Display.Mode, cfg.Display.Protocol)
	}
	if cfg.Display.Width != 1920 || cfg.Display.Height != 1080 {
		t.Errorf("cfg.Display size = %dx%d, want 1920x1080", cfg.Display.Width, cfg.Display.Height)
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

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{}`, ""))

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

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

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
	if resp["mode"] != "desktop-control" || resp["protocol"] != "novnc" {
		t.Fatalf("mode/protocol = %v/%v, want desktop-control/novnc", resp["mode"], resp["protocol"])
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

func TestWorkspaceDisplay_DisplayConfiguredWithInstanceReturnsAvailableSession(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, "i-display123"))

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
	if resp["available"] != true {
		t.Fatalf("available = %v, want true", resp["available"])
	}
	if resp["viewer_url"] != nil {
		t.Fatalf("viewer_url = %v, want omitted; stream URL is minted by Take control", resp["viewer_url"])
	}
	if resp["reason"] != nil {
		t.Fatalf("reason = %v, want omitted", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_DisplayConfiguredWithoutInstanceReturnsUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	workspaceID := "ws-display"
	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+workspaceID+"/display", nil)

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
	if resp["viewer_url"] != nil {
		t.Fatalf("viewer_url = %v, want omitted for invalid viewer base", resp["viewer_url"])
	}
	if resp["reason"] != "display_session_unavailable" {
		t.Fatalf("reason = %v, want display_session_unavailable", resp["reason"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplay_IgnoresUnrelatedStoredComputeSizingDrift(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display-sizing-drift").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"instance_type":"old.large","display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`, ""))

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

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-invalid-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{"display":{"mode":"desktop-control","protocol":"vnc"}}`, ""))

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

func TestWorkspaceDisplaySession_ProxiesThroughDisplayForward(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "display-session-test-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	var upstreamAuth, upstreamCookie, upstreamProtocol, gotInstanceID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/websockify" {
			t.Errorf("upstream path = %q, want /websockify", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("upstream raw query = %q, want stripped", r.URL.RawQuery)
		}
		upstreamAuth = r.Header.Get("Authorization")
		upstreamCookie = r.Header.Get("Cookie")
		upstreamProtocol = r.Header.Get("Sec-WebSocket-Protocol")
		_, _ = w.Write([]byte("websockify"))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	prevForward := displayForward
	displayForward = func(_ context.Context, instanceID string, fn func(target *url.URL) error) error {
		gotInstanceID = instanceID
		return fn(upstreamURL)
	}
	t.Cleanup(func() { displayForward = prevForward })

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(
			`{"display":{"mode":"desktop-control","protocol":"novnc","width":1920,"height":1080}}`,
			"i-display123",
		))
	expiresAt := time.Now().Add(5 * time.Minute).UTC()
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws-display").
		WillReturnRows(sqlmock.NewRows([]string{"controller", "controlled_by", "expires_at"}).AddRow("user", "admin-token", expiresAt))
	token := signDisplaySessionToken("ws-display", "admin-token", expiresAt)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-display"},
		{Key: "proxyPath", Value: "/websockify"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-display/display/session/websockify", nil)
	c.Request.Header.Set("Authorization", "Bearer should-not-reach-upstream")
	c.Request.Header.Set("Cookie", "session=should-not-reach-upstream")
	c.Request.Header.Set("Sec-WebSocket-Protocol", "binary, molecule-display-token."+token)

	handler.DisplaySession(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotInstanceID != "i-display123" {
		t.Fatalf("displayForward instanceID = %q, want i-display123", gotInstanceID)
	}
	if w.Body.String() != "websockify" {
		t.Fatalf("body = %q, want websockify", w.Body.String())
	}
	if upstreamAuth != "" || upstreamCookie != "" {
		t.Fatalf("proxied credentials leaked upstream: auth=%q cookie=%q", upstreamAuth, upstreamCookie)
	}
	if upstreamProtocol != "binary" {
		t.Fatalf("upstream websocket protocol = %q, want binary without display token", upstreamProtocol)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceDisplaySession_NonDisplayWorkspaceDoesNotProxy(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	prevForward := displayForward
	displayForward = func(_ context.Context, _ string, _ func(target *url.URL) error) error {
		t.Fatal("displayForward must not run for non-display workspaces")
		return nil
	}
	t.Cleanup(func() { displayForward = prevForward })

	mock.ExpectQuery(`SELECT COALESCE\(compute, '\{\}'::jsonb\), COALESCE\(instance_id, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs("ws-no-display").
		WillReturnRows(sqlmock.NewRows([]string{"compute", "instance_id"}).AddRow(`{}`, "i-display123"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-no-display"},
		{Key: "proxyPath", Value: "/websockify"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-no-display/display/session/websockify", nil)

	handler.DisplaySession(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
