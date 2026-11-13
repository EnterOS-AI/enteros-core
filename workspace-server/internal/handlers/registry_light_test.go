package handlers

// registry_light_test.go — Enter OS Light (C5): /registry/heartbeat,
// /registry/register and /registry/update-card of a parking or parked Light
// agent are answered 409 and write nothing (no DB column, no Redis liveness key,
// no URL cache), and the writes they would make carry the wake predicate.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// expectNoLiveInstanceToken scripts requireWorkspaceToken's bootstrap branch
// (no live instance token: the request is admitted without a bearer).
func expectNoLiveInstanceToken(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
}

// expectValidWorkspaceBearer scripts requireWorkspaceToken accepting bearer.
func expectValidWorkspaceBearer(mock sqlmock.Sqlmock, workspaceID, bearer string) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM workspace_auth_tokens`).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	sum := sha256.Sum256([]byte(bearer))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id`).WithArgs(sum[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", workspaceID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at = now\(\) WHERE id = \$1`).WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func registryCall(t *testing.T, handle func(*gin.Context), path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		c.Request.Header.Set("Authorization", "Bearer "+bearer)
	}
	handle(c)
	return w
}

func assertLightParked409(t *testing.T, w *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusConflict || body["code"] != wantCode {
		t.Fatalf("got %d %s, want 409 %s", w.Code, w.Body.String(), wantCode)
	}
}

func TestHeartbeatFromAParkedLightAgentIs409AndWritesNothing(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	expectValidWorkspaceBearer(mock, lightTestWS, "run-or-instance-token")
	expectLightState(mock, lightTestWS, "parked")
	rec := recordSQL(t)

	body := `{"workspace_id":"` + lightTestWS + `","error_rate":0,"sample_error":"","active_tasks":1,"uptime_seconds":60,"monthly_spend":500}`
	w := registryCall(t, h.Heartbeat, "/registry/heartbeat", body, "run-or-instance-token")

	assertLightParked409(t, w, "LIGHT_AGENT_PARKED")
	assertNoWrites(t, rec(), "workspace_auth_tokens")
	// No write beyond auth's own token last_used_at bump: no spend ledger row,
	// no heartbeat UPDATE, no drain.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	for _, key := range []string{"ws:" + lightTestWS, "ws:" + lightTestWS + ":url", "ws:" + lightTestWS + ":internal_url"} {
		if mr.Exists(key) {
			t.Errorf("Redis key %s written for a parked agent", key)
		}
	}
}

func TestHeartbeatFromAParkingLightAgentIs409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	expectNoLiveInstanceToken(mock, lightTestWS)
	expectLightState(mock, lightTestWS, "parking")
	rec := recordSQL(t)

	body := `{"workspace_id":"` + lightTestWS + `","active_tasks":0}`
	assertLightParked409(t, registryCall(t, h.Heartbeat, "/registry/heartbeat", body, ""), "LIGHT_AGENT_PARKING")
	assertNoWrites(t, rec())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRegisterFromAParkedLightAgentIs409AndWritesNothing(t *testing.T) {
	mock := setupTestDB(t)
	mr := setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	// fetchExistingWorkspaceStateForDiagnostics reads the row first (best-effort).
	mock.ExpectQuery(`FROM workspaces WHERE id = \$1`).WithArgs(lightTestWS).
		WillReturnError(errors.New("diagnostics only"))
	expectNoLiveInstanceToken(mock, lightTestWS)
	expectLightState(mock, lightTestWS, "parked")
	// Nothing after: no upsert, no SetOnline, no URL cache, and — because the
	// guard runs before authOK — no deferred last_register_failure_at UPDATE.
	rec := recordSQL(t)

	body := `{"id":"` + lightTestWS + `","url":"http://ws.example:8000","agent_card":{"name":"x"}}`
	w := registryCall(t, h.Register, "/registry/register", body, "")

	assertLightParked409(t, w, "LIGHT_AGENT_PARKED")
	assertNoWrites(t, rec())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if mr.Exists("ws:"+lightTestWS) || mr.Exists("ws:"+lightTestWS+":url") {
		t.Error("register of a parked agent wrote a Redis key")
	}
}

func TestUpdateCardForAParkedLightAgentIs409AndWritesNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	expectNoLiveInstanceToken(mock, lightTestWS)
	expectLightState(mock, lightTestWS, "parked")
	rec := recordSQL(t)

	body := `{"workspace_id":"` + lightTestWS + `","agent_card":{"name":"rewritten by a run"}}`
	assertLightParked409(t, registryCall(t, h.UpdateCard, "/registry/update-card", body, ""), "LIGHT_AGENT_PARKED")
	assertNoWrites(t, rec())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestHeartbeatUpdateCarriesTheLightPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	expectNoLiveInstanceToken(mock, lightTestWS)
	mock.ExpectQuery(lightStateQuery).WithArgs(lightTestWS).WillReturnError(errors.New("db blip"))
	mock.ExpectQuery(`SELECT COALESCE\(current_task`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "hibernated", 0))
	// The guard read failed open; the UPDATE's predicate refuses the parked row.
	mock.ExpectExec(`UPDATE workspaces SET[\s\S]*WHERE id = \$1 AND status != 'removed' AND ` + lightPredicateRe).
		WillReturnResult(sqlmock.NewResult(0, 0))

	body := `{"workspace_id":"` + lightTestWS + `","active_tasks":0}`
	registryCall(t, h.Heartbeat, "/registry/heartbeat", body, "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (the heartbeat UPDATE must carry the predicate): %v", err)
	}
}

func TestRegisterUpsertAndCardUpdateCarryTheLightPredicate(t *testing.T) {
	// Both statements are long and their remaining flow is covered elsewhere;
	// pin the predicate on the statements themselves at the source level.
	src := readLightSource(t, "registry.go")
	for _, want := range []string{
		"WHERE workspaces.status NOT IN ('removed', 'paused', 'hibernated') AND `+models.LightWakeAllowedSQLFor(\"workspaces\")+`",
		"UPDATE workspaces SET agent_card = $2::jsonb, updated_at = now() WHERE id = $1 AND `+models.LightWakeAllowedSQL+`",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("registry.go lacks %q", want)
		}
	}
}

// The restart cycle's provisioning write carries the predicate like every
// other revive write: its SELECT saw the agent open, but a park can claim it
// during the stop (review 21716, R4; behaviour on real Postgres in
// TestIntegration_LightGuards_RestartInFlightCannotReprovision).
func TestRestartCycleProvisioningWriteCarriesTheLightPredicate(t *testing.T) {
	src := readLightSource(t, "workspace_restart.go")
	want := "`UPDATE workspaces SET status = $1, url = '', mcp_unloaded_since = NULL, updated_at = now() WHERE id = $2 AND `+models.LightWakeAllowedSQL"
	if !strings.Contains(src, want) {
		t.Errorf("workspace_restart.go's restart-cycle provisioning write lacks the Light predicate: want %q", want)
	}
}

// readLightSource reads a file of this package by its relative path (go test
// runs in the package directory), so the read also works for a test binary
// built on another OS.
func readLightSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
