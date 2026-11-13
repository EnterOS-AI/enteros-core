package handlers

// light_enable_test.go — POST /workspaces/:id/light/enable, /light/disable and
// GET /workspaces/:id/light.

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

const lightLoadSQL = `SELECT name, status, COALESCE\(runtime, 'claude-code'\), COALESCE\(kind, 'workspace'\),\s+COALESCE\(light_state, ''\), COALESCE\(active_tasks, 0\), light_run_token_id, light_retired_token_id,\s+COALESCE\(is_busy, false\), light_state_at, COALESCE\(light_park_trigger, ''\),\s+COALESCE\(light_park_watermark, 0\), light_pending_token_id\s+FROM workspaces WHERE id = \$1 AND status != 'removed'`

// lightLoadExtra is the part-2 columns of a scripted loadLightWorkspace row.
type lightLoadExtra struct {
	isBusy         bool
	stateAt        interface{}
	parkTrigger    string
	parkWatermark  int64
	pendingTokenID interface{}
}

// expectLightLoad scripts loadLightWorkspace.
func expectLightLoad(mock sqlmock.Sqlmock, status, runtime, kind, lightState string, activeTasks int, runToken, retiredToken interface{}) {
	expectLightLoadWith(mock, status, runtime, kind, lightState, activeTasks, runToken, retiredToken, lightLoadExtra{})
}

// expectLightLoadWith scripts loadLightWorkspace with the part-2 columns.
func expectLightLoadWith(mock sqlmock.Sqlmock, status, runtime, kind, lightState string, activeTasks int, runToken, retiredToken interface{}, x lightLoadExtra) {
	mock.ExpectQuery(lightLoadSQL).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "runtime", "kind", "light_state", "active_tasks", "light_run_token_id", "light_retired_token_id",
			"is_busy", "light_state_at", "light_park_trigger", "light_park_watermark", "light_pending_token_id"}).
			AddRow("Light agent", status, runtime, kind, lightState, activeTasks, runToken, retiredToken,
				x.isBusy, x.stateAt, x.parkTrigger, x.parkWatermark, x.pendingTokenID))
}

const lightSecretReadSQL = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`

const lightEnableClaimSQL = `UPDATE workspaces SET light_state = 'open', light_state_at = now\(\), light_opened_by = 'owner',\s+light_continuous_before = \$2::jsonb, updated_at = now\(\)\s+WHERE id = \$1 AND light_state IS NULL`

// continuousBeforeArg matches the light_continuous_before JSON enable records:
// whether the workspace had its own MOLECULE_CONTINUOUS row, and (when it did)
// that row's value, decrypted from the recorded ciphertext.
type continuousBeforeArg struct {
	present bool
	plain   string
}

func (a continuousBeforeArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var b lightSecretBefore
	if json.Unmarshal([]byte(s), &b) != nil || b.Present != a.present {
		return false
	}
	if !a.present {
		return len(b.EncryptedValue) == 0
	}
	plain, err := crypto.DecryptVersioned(b.EncryptedValue, b.EncryptionVersion)
	return err == nil && string(plain) == a.plain
}

func expectLightSecretMissingAndWritten(mock sqlmock.Sqlmock, key, value string) {
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, key).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO workspace_secrets \(workspace_id, key, encrypted_value, encryption_version\)`).
		WithArgs(lightTestWS, key, encryptedAs(value), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func newLightTestHandler(t *testing.T, api provisioner.LightAPI) (*LightHandler, *WorkspaceHandler) {
	t.Helper()
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	if api != nil {
		wh.SetLightAPI(api)
	}
	waitForHandlerAsyncBeforeDBCleanup(t, wh)
	return NewLightHandler(wh), wh
}

func lightCall(handle func(*gin.Context), method, path, body string, setup func(*gin.Context)) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	if setup != nil {
		setup(c)
	}
	handle(c)
	return w
}

func lightBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, w.Body.String())
	}
	return out
}

func TestLightEnableWritesBothSecretsAndRestarts(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _ := newLightTestHandler(t, api)

	expectLightLoad(mock, "online", "hermes", "workspace", "", 0, nil, nil)
	mock.ExpectBegin()
	expectLightSecretMissingAndWritten(mock, "MOLECULE_LIGHT", "1")
	expectLightSecretMissingAndWritten(mock, "MOLECULE_CONTINUOUS", "false")
	mock.ExpectExec(lightEnableClaimSQL).
		WithArgs(lightTestWS, continuousBeforeArg{present: false}).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := lightCall(lh.Enable, http.MethodPost, "/workspaces/"+lightTestWS+"/light/enable", "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("enable = %d %s", w.Code, w.Body.String())
	}
	want := map[string]interface{}{"status": "enabled", "light_state": "open", "restart": "started"}
	if got := lightBody(t, w); !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want %v", got, want)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"jobs " + lightTestWS}) {
		t.Errorf("CP calls = %v, want the capability probe only", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLightEnableWakesAHibernatedAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})

	expectLightLoad(mock, "hibernated", "hermes", "workspace", "", 0, nil, nil)
	mock.ExpectBegin()
	expectLightSecretMissingAndWritten(mock, "MOLECULE_LIGHT", "1")
	expectLightSecretMissingAndWritten(mock, "MOLECULE_CONTINUOUS", "false")
	mock.ExpectExec(lightEnableClaimSQL).WithArgs(lightTestWS, continuousBeforeArg{present: false}).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusOK || lightBody(t, w)["restart"] != "woken" {
		t.Fatalf("enable = %d %s, want restart woken", w.Code, w.Body.String())
	}
}

func TestLightEnableRefusesOtherRuntimes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoad(mock, "online", "claude-code", "workspace", "", 0, nil, nil)

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	body := lightBody(t, w)
	if w.Code != http.StatusUnprocessableEntity || body["code"] != "LIGHT_RUNTIME_UNSUPPORTED" || body["runtime"] != "claude-code" ||
		!strings.Contains(body["error"].(string), "claude-code") {
		t.Fatalf("enable = %d %v, want 422 naming the runtime", w.Code, body)
	}
	if len(api.Calls()) != 0 {
		t.Errorf("CP called: %v", api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (nothing may be written): %v", err)
	}
}

func TestLightEnableRefusesThePlatformAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})
	expectLightLoad(mock, "online", "hermes", "platform", "", 0, nil, nil)

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusUnprocessableEntity || lightBody(t, w)["code"] != "LIGHT_PLATFORM_AGENT" {
		t.Fatalf("enable = %d %s, want 422 LIGHT_PLATFORM_AGENT", w.Code, w.Body.String())
	}
}

func TestLightEnableRefusesAPausedAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})
	expectLightLoad(mock, "paused", "hermes", "workspace", "", 0, nil, nil)

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_AGENT_PAUSED" {
		t.Fatalf("enable = %d %s, want 409 LIGHT_AGENT_PAUSED", w.Code, w.Body.String())
	}
}

func TestLightEnableNeedsTheKubernetesSubstrate(t *testing.T) {
	cases := []struct {
		name       string
		api        provisioner.LightAPI
		wantStatus int
		wantCode   string
		retryAfter string
	}{
		{"no control plane", nil, http.StatusNotImplemented, "LIGHT_UNSUPPORTED", ""},
		{"docker substrate", &fakeLightAPI{jobsErr: &provisioner.LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED"}}, http.StatusNotImplemented, "LIGHT_UNSUPPORTED", ""},
		{"control plane down", &fakeLightAPI{jobsErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE", RetryAfter: "10"}}, http.StatusServiceUnavailable, "LIGHT_BACKEND_UNAVAILABLE", "10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			lh, _ := newLightTestHandler(t, tc.api)
			expectLightLoad(mock, "online", "hermes", "workspace", "", 0, nil, nil)

			w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
			if w.Code != tc.wantStatus || lightBody(t, w)["code"] != tc.wantCode || w.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("enable = %d %s (Retry-After %q)", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (nothing may be written): %v", err)
			}
		})
	}
}

func TestLightEnableOfALightAgentIsUnchangedAndRestoresAMissingSecret(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _ := newLightTestHandler(t, api)

	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	expectLightSecretMissingAndWritten(mock, "MOLECULE_CONTINUOUS", "false")
	mock.ExpectCommit()

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	want := map[string]interface{}{"status": "unchanged", "light_state": "open", "restart": "started"}
	if got := lightBody(t, w); w.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("enable = %d %v, want %v", w.Code, got, want)
	}
	if len(api.Calls()) != 0 {
		t.Errorf("an already-Light agent needs no capability probe: %v", api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLightEnableWithBothSecretsInPlaceRestartsNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})

	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "true"))
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnRows(encryptedRow(t, "off"))
	mock.ExpectCommit()

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	want := map[string]interface{}{"status": "unchanged", "light_state": "parked", "restart": "none"}
	if got := lightBody(t, w); w.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("enable = %d %v, want %v", w.Code, got, want)
	}
}

// A parked agent whose Light secret went missing gets it back without being
// woken: the change rides a queued refresh.
func TestLightEnableOfAParkedAgentQueuesARefreshInsteadOfWaking(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})

	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	mock.ExpectBegin()
	expectLightSecretMissingAndWritten(mock, "MOLECULE_LIGHT", "1")
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnRows(encryptedRow(t, "false"))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO light_pending_actions \(workspace_id, kind, payload, source\)\s+SELECT \$1, \$2, \$3::jsonb, \$4\s+WHERE NOT EXISTS`).
		WithArgs(lightTestWS, "refresh", "{}", "enable").
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := lightCall(lh.Enable, http.MethodPost, "/", "", nil)
	want := map[string]interface{}{"status": "unchanged", "light_state": "parked", "restart": "refresh_queued"}
	if got := lightBody(t, w); w.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("enable = %d %v, want %v", w.Code, got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestLightManagementRefusesTheWorkspaceItself: a run pod holds the agent's
// own token (an API-kind run token); it must not manage the agent's Light
// state. The token resolves to the workspace through callerWorkspaceID.
func TestLightManagementRefusesTheWorkspaceItself(t *testing.T) {
	routes := map[string]func(*LightHandler) func(*gin.Context){
		"enable":  func(h *LightHandler) func(*gin.Context) { return h.Enable },
		"disable": func(h *LightHandler) func(*gin.Context) { return h.Disable },
		"park":    func(h *LightHandler) func(*gin.Context) { return h.Park },
		"open":    func(h *LightHandler) func(*gin.Context) { return h.Open },
		"run":     func(h *LightHandler) func(*gin.Context) { return h.RunNow },
	}
	// The identity WorkspaceAuth proved and recorded (any token of the
	// workspace, a run token included), and the X-Workspace-ID header.
	callers := map[string]func(*gin.Context){
		"workspace token": func(c *gin.Context) {
			c.Request.Header.Set("Authorization", "Bearer the-run-token")
			c.Set("authenticated_workspace_id", lightTestWS)
		},
		"X-Workspace-ID": func(c *gin.Context) { c.Request.Header.Set("X-Workspace-ID", lightTestWS) },
	}
	for name, route := range routes {
		for callerName, caller := range callers {
			t.Run(name+"/"+callerName, func(t *testing.T) {
				mock := setupTestDB(t)
				setupTestRedis(t)
				api := &fakeLightAPI{}
				lh, _ := newLightTestHandler(t, api)
				rec := recordSQL(t)

				w := lightCall(route(lh), http.MethodPost, "/", "", caller)
				if w.Code != http.StatusForbidden || lightBody(t, w)["code"] != "LIGHT_SELF_FORBIDDEN" {
					t.Fatalf("%s by the workspace itself = %d %s, want 403 LIGHT_SELF_FORBIDDEN", name, w.Code, w.Body.String())
				}
				if len(api.Calls()) != 0 {
					t.Errorf("CP called: %v", api.Calls())
				}
				// Fail-closed: the refusal rests on the recorded identity, not on a
				// second token lookup that a database error could turn into "".
				if stmts := rec(); len(stmts) != 0 {
					t.Errorf("the self check touched the database: %v", stmts)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Errorf("unmet: %v", err)
				}
			})
		}
	}
}

// Another workspace's bearer never reaches these handlers (WorkspaceAuth binds
// a workspace token to :id), and owner credentials record no workspace: the
// self check lets them through.
func TestLightManagementLetsTheOwnerThrough(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})
	expectLightLoad(mock, "online", "hermes", "workspace", "", 0, nil, nil)
	w := lightCall(lh.Disable, http.MethodPost, "/", "", func(c *gin.Context) {
		c.Set("org_token_id", "tok-org")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("disable by the owner = %d %s", w.Code, w.Body.String())
	}
}

func TestLightRoutesRejectANonCanonicalID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})
	for _, handle := range []func(*gin.Context){lh.Enable, lh.Disable, lh.Get, lh.ListRuns, lh.ReportRun, lh.Park, lh.Open, lh.RunNow} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		handle(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("non-UUID id: %d %s, want 400", w.Code, w.Body.String())
		}
	}
}

const (
	lightDisableBeforeSQL     = `SELECT light_continuous_before FROM workspaces WHERE id = \$1 FOR UPDATE`
	lightDeleteSecretSQL      = `DELETE FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`
	lightRestoreContinuousSQL = `INSERT INTO workspace_secrets \(workspace_id, key, encrypted_value, encryption_version\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+ON CONFLICT \(workspace_id, key\) DO UPDATE\s+SET encrypted_value = EXCLUDED\.encrypted_value, encryption_version = EXCLUDED\.encryption_version, updated_at = now\(\)`
)

// expectLightDisableWrites scripts disable's transaction. before is the
// recorded light_continuous_before ("" = none recorded); restore scripts what
// it puts back.
func expectLightDisableWrites(mock sqlmock.Sqlmock, tokens []string, before string, restore func(sqlmock.Sqlmock)) {
	mock.ExpectBegin()
	var b interface{}
	if before != "" {
		b = []byte(before)
	}
	mock.ExpectQuery(lightDisableBeforeSQL).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"light_continuous_before"}).AddRow(b))
	if len(tokens) > 0 {
		mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\)\s+WHERE workspace_id = \$1 AND id::text = ANY\(\$2\) AND revoked_at IS NULL`).
			WithArgs(lightTestWS, pqArrayArg(tokens)).WillReturnResult(sqlmock.NewResult(0, int64(len(tokens))))
	}
	mock.ExpectExec(lightDeleteSecretSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnResult(sqlmock.NewResult(0, 1))
	if restore != nil {
		restore(mock)
	}
	mock.ExpectExec(`UPDATE workspaces\s+SET light_state = NULL, light_state_at = now\(\), light_opened_by = NULL,\s+light_run_token_id = NULL, light_retired_token_id = NULL, light_continuous_before = NULL,`).
		WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM light_schedules WHERE workspace_id = \$1`).WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM light_pending_actions WHERE workspace_id = \$1 AND applied_at IS NULL`).WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
}

func TestLightDisableDeletesCPObjectsThenEverythingElse(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{deleted: provisioner.LightDeleteResult{CronJobs: 2, Jobs: 1, Secrets: 1}}
	lh, _ := newLightTestHandler(t, api)

	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, "tok-current", "tok-retired")
	expectLightDisableWrites(mock, []string{"tok-current", "tok-retired"}, `{"present":false}`, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(lightDeleteSecretSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnResult(sqlmock.NewResult(0, 1))
	})

	w := lightCall(lh.Disable, http.MethodPost, "/", "", nil)
	want := map[string]interface{}{
		"status": "disabled", "light_state": nil, "restart": "woken",
		"deleted": map[string]interface{}{"cronjobs": float64(2), "jobs": float64(1), "secrets": float64(1)},
	}
	if got := lightBody(t, w); w.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("disable = %d %v, want %v", w.Code, got, want)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"delete " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A run token a park minted but never recorded (it was interrupted before its
// commit) is revoked by disable with the others (review 21718 N1).
func TestLightDisableRevokesAnUnrecordedParkToken(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoadWith(mock, "hibernated", "hermes", "workspace", "parking", 0, "tok-current", nil, lightLoadExtra{pendingTokenID: "tok-pending"})
	expectLightDisableWrites(mock, []string{"tok-current", "tok-pending"}, `{"present":false}`, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(lightDeleteSecretSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnResult(sqlmock.NewResult(0, 1))
	})
	w := lightCall(lh.Disable, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusOK || lightBody(t, w)["status"] != "disabled" {
		t.Fatalf("disable = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (the pending token is revoked with the others): %v", err)
	}
}

func TestLightDisableStopsAtACPFailure(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{deleteErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, "tok-current", nil)
	// No transaction: the agent stays parked with its runs intact; retry.

	w := lightCall(lh.Disable, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusServiceUnavailable || lightBody(t, w)["code"] != "LIGHT_BACKEND_UNAVAILABLE" {
		t.Fatalf("disable = %d %s, want 503", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLightDisableTreatsUnsupportedAndNoTenantAsDeleted(t *testing.T) {
	for _, cpErr := range []error{
		&provisioner.LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED"},
		&provisioner.LightCPError{HTTPStatus: 404, Code: "LIGHT_TENANT_NOT_FOUND"},
	} {
		mock := setupTestDB(t)
		setupTestRedis(t)
		lh, _ := newLightTestHandler(t, &fakeLightAPI{deleteErr: cpErr})
		expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
		expectLightDisableWrites(mock, nil, "", nil)

		w := lightCall(lh.Disable, http.MethodPost, "/", "", nil)
		if w.Code != http.StatusOK || lightBody(t, w)["restart"] != "started" {
			t.Fatalf("%v: disable = %d %s, want 200 and a restart", cpErr, w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	}
}

// Enable records the agent's own 24/7 row as it was — on, off or absent — so
// disable can put exactly that back (review 21716, R3).
func TestLightEnableRecordsThe247SettingAsItWas(t *testing.T) {
	for _, prior := range []string{"true", "false"} {
		t.Run(prior, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			lh, _ := newLightTestHandler(t, &fakeLightAPI{})
			expectLightLoad(mock, "online", "hermes", "workspace", "", 0, nil, nil)
			mock.ExpectBegin()
			expectLightSecretMissingAndWritten(mock, "MOLECULE_LIGHT", "1")
			mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnRows(encryptedRow(t, prior))
			if prior == "true" {
				mock.ExpectExec(`INSERT INTO workspace_secrets \(workspace_id, key, encrypted_value, encryption_version\)`).
					WithArgs(lightTestWS, "MOLECULE_CONTINUOUS", encryptedAs("false"), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(1, 1))
			}
			mock.ExpectExec(lightEnableClaimSQL).WithArgs(lightTestWS, continuousBeforeArg{present: true, plain: prior}).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			if w := lightCall(lh.Enable, http.MethodPost, "/", "", nil); w.Code != http.StatusOK {
				t.Fatalf("enable = %d %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (the record must hold the row as it was before enable): %v", err)
			}
		})
	}
}

func TestLightDisableRestoresThe247SettingAsItWas(t *testing.T) {
	cipher, err := crypto.Encrypt([]byte("true"))
	if err != nil {
		t.Fatal(err)
	}
	present, _ := json.Marshal(lightSecretBefore{Present: true, EncryptedValue: cipher, EncryptionVersion: 7})
	cases := []struct {
		name, before string
		restore      func(sqlmock.Sqlmock)
	}{
		{"no workspace row before enable: removed again", `{"present":false}`, func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(lightDeleteSecretSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS").WillReturnResult(sqlmock.NewResult(0, 1))
		}},
		{"a workspace row before enable: put back byte for byte", string(present), func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(lightRestoreContinuousSQL).WithArgs(lightTestWS, "MOLECULE_CONTINUOUS", cipher, 7).WillReturnResult(sqlmock.NewResult(1, 1))
		}},
		// No record (not written by enable): the owner's setting is left alone.
		{"no record", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			lh, _ := newLightTestHandler(t, &fakeLightAPI{})
			expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
			expectLightDisableWrites(mock, nil, tc.before, tc.restore)

			// The transaction is scripted statement by statement: any other
			// write to the 24/7 row fails it, and disable then answers 500.
			if w := lightCall(lh.Disable, http.MethodPost, "/", "", nil); w.Code != http.StatusOK {
				t.Fatalf("disable = %d %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestLightDisableOfANonLightAgentIsUnchanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoad(mock, "online", "hermes", "workspace", "", 0, nil, nil)

	w := lightCall(lh.Disable, http.MethodPost, "/", "", nil)
	want := map[string]interface{}{"status": "unchanged", "light_state": nil}
	if got := lightBody(t, w); w.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("disable = %d %v", w.Code, got)
	}
	if len(api.Calls()) != 0 {
		t.Errorf("CP called: %v", api.Calls())
	}
}

func TestLightGetReportsStateSchedulesAndQueuedWork(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COALESCE\(light_state, ''\), light_state_at, light_last_error, light_last_error_at\s+FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"light_state", "light_state_at", "light_last_error", "light_last_error_at"}).
			AddRow("parked", at, []byte(`{"code":"LIGHT_AGENT_BUSY","message":"busy"}`), at))
	mock.ExpectQuery(`FROM light_schedules WHERE workspace_id = \$1 ORDER BY name`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "cron", "timezone", "prompt", "enabled", "suspended_reason", "suspended_at", "consecutive_failures", "cronjob"}).
			AddRow("daily", "0 9 * * *", "UTC", "write the report", true, nil, nil, 0, "light-be7540cfd160-1a2b3c4d").
			AddRow("nightly", "0 1 * * *", "", "p", true, "three failed runs in a row", at, 3, nil))
	mock.ExpectQuery(`FROM light_pending_actions\s+WHERE workspace_id = \$1 AND applied_at IS NULL ORDER BY id`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "source", "created_at"}).AddRow("refresh", "restart", at))

	w := lightCall(lh.Get, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	body := lightBody(t, w)
	if body["light_state"] != "parked" || body["refresh_pending"] != true || body["light_state_at"] != "2026-11-13T09:00:00Z" {
		t.Errorf("state fields = %v", body)
	}
	if le := body["last_error"].(map[string]interface{}); le["code"] != "LIGHT_AGENT_BUSY" || le["at"] != "2026-11-13T09:00:00Z" {
		t.Errorf("last_error = %v", le)
	}
	schedules := body["schedules"].([]interface{})
	if len(schedules) != 2 {
		t.Fatalf("schedules = %v", schedules)
	}
	nightly := schedules[1].(map[string]interface{})
	if nightly["suspended"] != true || nightly["suspended_reason"] != "three failed runs in a row" || nightly["consecutive_failures"] != float64(3) || nightly["cronjob"] != nil {
		t.Errorf("nightly = %v", nightly)
	}
	if daily := schedules[0].(map[string]interface{}); daily["suspended"] != false || daily["cronjob"] != "light-be7540cfd160-1a2b3c4d" {
		t.Errorf("daily = %v", daily)
	}
	pending := body["pending_actions"].([]interface{})
	if len(pending) != 1 || pending[0].(map[string]interface{})["kind"] != "refresh" {
		t.Errorf("pending_actions = %v", pending)
	}
}

func TestLightGetOfAnOrdinaryWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	mock.ExpectQuery(`SELECT COALESCE\(light_state, ''\), light_state_at`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"light_state", "light_state_at", "light_last_error", "light_last_error_at"}).AddRow("", nil, nil, nil))
	mock.ExpectQuery(`FROM light_schedules`).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"name"}))
	mock.ExpectQuery(`FROM light_pending_actions`).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"kind"}))

	w := lightCall(lh.Get, http.MethodGet, "/", "", nil)
	body := lightBody(t, w)
	if w.Code != http.StatusOK || body["light_state"] != nil || body["refresh_pending"] != false ||
		len(body["schedules"].([]interface{})) != 0 || len(body["pending_actions"].([]interface{})) != 0 {
		t.Fatalf("get = %d %v", w.Code, body)
	}
}

func TestLightGetOfAMissingWorkspaceIs404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	mock.ExpectQuery(`SELECT COALESCE\(light_state, ''\), light_state_at`).WithArgs(lightTestWS).WillReturnError(sql.ErrNoRows)

	w := lightCall(lh.Get, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusNotFound || lightBody(t, w)["code"] != "WORKSPACE_NOT_FOUND" {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
}

func TestWriteLightCPErrorMapsTheContract(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		code       string
		extraKey   string
		extraValue interface{}
	}{
		{&provisioner.LightCPError{HTTPStatus: 422, Code: "LIGHT_INVALID", Invalid: []provisioner.LightFieldError{{Schedule: "a", Field: "cron", Reason: "r"}}}, 422, "LIGHT_INVALID", "invalid", []interface{}{map[string]interface{}{"schedule": "a", "field": "cron", "reason": "r"}}},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_ENV_UNSUPPORTED", Key: "POD_IP"}, 409, "LIGHT_ENV_UNSUPPORTED", "key", "POD_IP"},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_RUN_ACTIVE", RunID: "r1"}, 409, "LIGHT_RUN_ACTIVE", "run_id", "r1"},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_NOT_ENABLED"}, 409, "LIGHT_RESTART_REQUIRED", "", nil},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_RUNTIME_UNSUPPORTED"}, 422, "LIGHT_RUNTIME_UNSUPPORTED", "", nil},
		{&provisioner.LightCPError{HTTPStatus: 413, Code: "LIGHT_TOO_LARGE"}, 413, "LIGHT_TOO_LARGE", "", nil},
		{&provisioner.LightCPError{HTTPStatus: 404, Code: "LIGHT_SCHEDULE_NOT_FOUND"}, 404, "LIGHT_SCHEDULE_NOT_FOUND", "", nil},
		{&provisioner.LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED"}, 501, "LIGHT_UNSUPPORTED", "", nil},
		{provisioner.ErrLightNotConfigured, 501, "LIGHT_UNSUPPORTED", "", nil},
		{&provisioner.LightCPError{HTTPStatus: 500, Code: "LIGHT_FAILED"}, 502, "LIGHT_FAILED", "", nil},
		{errors.New("boom"), 500, "LIGHT_FAILED", "", nil},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		writeLightCPError(c, "test", lightTestWS, tc.err)
		body := lightBody(t, w)
		if w.Code != tc.status || body["code"] != tc.code {
			t.Errorf("%v: %d %v, want %d %s", tc.err, w.Code, body, tc.status, tc.code)
			continue
		}
		if tc.extraKey != "" && !reflect.DeepEqual(body[tc.extraKey], tc.extraValue) {
			t.Errorf("%v: %s = %v, want %v", tc.err, tc.extraKey, body[tc.extraKey], tc.extraValue)
		}
	}
}
