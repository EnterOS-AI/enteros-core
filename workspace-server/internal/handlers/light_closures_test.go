package handlers

// light_closures_test.go — Enter OS Light (C5): while a Light agent is parking
// or parked, every handler-side wake path is closed. Each path is pinned twice:
// the guard read answers 409 and writes nothing, and the write the path would
// make carries models.LightWakeAllowedSQL so a failed guard read still cannot
// wake the agent.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// lightPredicateRe is the wake predicate as a regexp fragment.
var lightPredicateRe = regexp.QuoteMeta(models.LightWakeAllowedSQL)

func lightPredicateReFor(alias string) string {
	return regexp.QuoteMeta(models.LightWakeAllowedSQLFor(alias))
}

const lightMsgSend = `{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"m-light-1","parts":[{"kind":"text","text":"hi"}]}}}`

func TestProxyA2AToAParkedLightAgentIs409AndWritesNothing(t *testing.T) {
	for _, tc := range []struct{ state, code, msg string }{
		{"parked", "LIGHT_AGENT_PARKED", "This agent is a Light agent and is parked. Open it to chat."},
		{"parking", "LIGHT_AGENT_PARKING", "This agent is a Light agent and is being parked. Open it to chat."},
	} {
		t.Run(tc.state, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			h.SetCPProvisioner(&trackingCPProv{})
			waitForHandlerAsyncBeforeDBCleanup(t, h)

			expectBudgetCheck(mock, lightTestWS)
			mock.ExpectQuery(`SELECT kind, status FROM workspaces WHERE id = \$1`).WithArgs(lightTestWS).
				WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow("workspace", "hibernated"))
			expectLightState(mock, lightTestWS, tc.state)
			// Nothing after the guard: no ingest INSERT, no delivery-mode read, no
			// URL resolve, no wake claim. recordSQL sees a write even where a
			// best-effort writer swallows sqlmock's "unexpected" error.
			rec := recordSQL(t)

			_, _, perr := h.proxyA2ARequest(context.Background(), lightTestWS, []byte(lightMsgSend), "", true, true)
			h.waitAsyncForTest()
			assertNoWrites(t, rec())
			if perr == nil || perr.Status != http.StatusConflict {
				t.Fatalf("proxy error = %+v, want a 409", perr)
			}
			if perr.Response["code"] != tc.code || perr.Response["error"] != tc.msg {
				t.Errorf("body = %v", perr.Response)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestProxyA2ALightGateAppliesToSystemAndSelfCallers(t *testing.T) {
	for _, caller := range []string{"system:restart-context", "webhook:github", lightTestWS} {
		t.Run(caller, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			waitForHandlerAsyncBeforeDBCleanup(t, h)

			expectBudgetCheck(mock, lightTestWS)
			expectLightState(mock, lightTestWS, "parked")

			_, _, perr := h.proxyA2ARequest(context.Background(), lightTestWS, []byte(lightMsgSend), caller, true, false)
			if perr == nil || perr.Status != http.StatusConflict || perr.Response["code"] != "LIGHT_AGENT_PARKED" {
				t.Fatalf("caller %q: proxy error = %+v, want 409 LIGHT_AGENT_PARKED", caller, perr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestProxyA2AToAnOpenLightAgentPassesTheGate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectBudgetCheck(mock, lightTestWS)
	expectLightState(mock, lightTestWS, "open")
	// The next step after the gate is the delivery-mode read; a context error
	// there ends the call with its own fail-closed 503, which proves the gate
	// let the message through.
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).WithArgs(lightTestWS).WillReturnError(context.Canceled)

	_, _, perr := h.proxyA2ARequest(context.Background(), lightTestWS, []byte(lightMsgSend), "system:test", true, false)
	if perr == nil || perr.Status != http.StatusServiceUnavailable {
		t.Fatalf("proxy error = %+v, want the delivery-mode 503 (the gate must pass an open agent)", perr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestWakeWorkspaceClaimCarriesTheLightPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &trackingCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	// The parked row does not match: zero rows, and WakeWorkspace stops there.
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, url = '', updated_at = now\(\) WHERE id = \$2 AND status = 'hibernated' AND `+lightPredicateRe).
		WithArgs(models.StatusProvisioning, lightTestWS).
		WillReturnResult(sqlmock.NewResult(0, 0))

	h.WakeWorkspace(lightTestWS)
	h.waitAsyncForTest()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if calls := trackedCPCalls(cp); len(calls) != 0 {
		t.Errorf("a parked Light agent was provisioned: CP calls %v", calls)
	}
}

func lightRestartCall(h *WorkspaceHandler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+lightTestWS+"/restart", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Restart(c)
	return w
}

func expectRestartLookup(mock sqlmock.Sqlmock, status string) {
	mock.ExpectQuery("SELECT status, name, tier, COALESCE").WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime", "template"}).
			AddRow(status, "Light agent", 1, "hermes", ""))
}

func TestRestartOfAParkedLightAgentIs409AndDoesNotProvision(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &trackingCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectRestartLookup(mock, "hibernated")
	expectLightState(mock, lightTestWS, "parked")
	rec := recordSQL(t)

	w := lightRestartCall(h)
	h.waitAsyncForTest()
	assertNoWrites(t, rec())

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"LIGHT_AGENT_PARKED"`) {
		t.Fatalf("restart = %d %s, want 409 LIGHT_AGENT_PARKED", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if calls := trackedCPCalls(cp); len(calls) != 0 {
		t.Errorf("restart reached the CP: %v", calls)
	}
}

func TestRestartClaimRefusesALightRowWhenTheGuardReadFails(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &trackingCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectRestartLookup(mock, "hibernated")
	mock.ExpectQuery(lightStateQuery).WithArgs(lightTestWS).WillReturnError(errors.New("db blip"))
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	// The claim's predicate refuses the parked row: zero rows.
	mock.ExpectExec(restartClaimRe).
		WithArgs(models.StatusProvisioning, lightTestWS).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// The answer names why: the row is a parked Light agent.
	expectLightState(mock, lightTestWS, "parked")

	w := lightRestartCall(h)
	h.waitAsyncForTest()

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"LIGHT_AGENT_PARKED"`) {
		t.Fatalf("restart = %d %s, want 409", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if calls := trackedCPCalls(cp); len(calls) != 0 {
		t.Errorf("an unclaimed restart was dispatched: CP calls %v", calls)
	}
}

const restartClaimRe = `UPDATE workspaces SET status = \$1, mcp_unloaded_since = NULL, updated_at = now\(\) WHERE id = \$2 AND `

// A claim that misses for a row that is not a Light agent (deleted, or changed
// state concurrently) must not answer with Light's wording (review N2).
func TestRestartClaimMissOfANonLightRowSaysWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		name       string
		read       func(sqlmock.Sqlmock)
		wantStatus int
	}{
		{"row deleted", func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(lightStateQuery).WithArgs(lightTestWS).WillReturnError(sql.ErrNoRows)
		}, http.StatusNotFound},
		{"not a Light agent", func(mock sqlmock.Sqlmock) { expectLightState(mock, lightTestWS, "") }, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			cp := &trackingCPProv{}
			h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			h.SetCPProvisioner(cp)
			waitForHandlerAsyncBeforeDBCleanup(t, h)

			expectRestartLookup(mock, "online")
			expectLightState(mock, lightTestWS, "")
			mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").WithArgs(lightTestWS).
				WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
			mock.ExpectExec(restartClaimRe).WithArgs(models.StatusProvisioning, lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
			tc.read(mock)

			w := lightRestartCall(h)
			h.waitAsyncForTest()
			if w.Code != tc.wantStatus || strings.Contains(w.Body.String(), "LIGHT") || strings.Contains(w.Body.String(), "Light agent") {
				t.Fatalf("restart = %d %s, want %d without Light wording", w.Code, w.Body.String(), tc.wantStatus)
			}
			if calls := trackedCPCalls(cp); len(calls) != 0 {
				t.Errorf("an unclaimed restart was dispatched: CP calls %v", calls)
			}
		})
	}
}

const runRestartCycleSelectRe = `SELECT name, status, tier, COALESCE\(runtime, 'claude-code'\), COALESCE\(template, ''\), COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id = \$1 AND status NOT IN \('removed', 'paused', 'hibernated'\) AND `

func TestRunRestartCycleSkipsAParkedLightAgentAndQueuesARefresh(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &trackingCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(runRestartCycleSelectRe + lightPredicateRe).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows(nil))
	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectExec(`INSERT INTO light_pending_actions`).
		WithArgs(lightTestWS, "refresh", "{}", "restart").
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.runRestartCycle(lightTestWS, "") // a mutation-driven restart (secret, plugin, config)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if calls := trackedCPCalls(cp); len(calls) != 0 {
		t.Errorf("a parked Light agent was restarted: CP calls %v", calls)
	}
}

func TestRunRestartCycleCrashProbeOnALightAgentQueuesNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(runRestartCycleSelectRe + lightPredicateRe).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows(nil))

	h.runRestartCycle(lightTestWS, "liveness-monitor")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPauseOfAParkedLightAgentIs409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &pauseCPStop{errByID: map[string]error{}}
	h := newPauseHandler(t, cp)

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("hibernated", "Light agent"))
	expectLightState(mock, lightTestWS, "parked")

	w := pauseCall(h, lightTestWS, "", nil)

	if w.Code != http.StatusConflict || pauseBody(t, w)["code"] != "LIGHT_AGENT_PARKED" {
		t.Fatalf("pause = %d %s, want 409 LIGHT_AGENT_PARKED", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	if len(cp.stopped) != 0 {
		t.Errorf("a parked Light agent was stopped: %v", cp.stopped)
	}
}

func TestPauseCascadeAndClaimCarryTheLightPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &pauseCPStop{errByID: map[string]error{}}
	h := newPauseHandler(t, cp)

	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", "Lead"))
	expectLightState(mock, lightTestWS, "")
	mock.ExpectQuery(`WITH RECURSIVE descendants .* SELECT d\.id, d\.name FROM descendants d JOIN workspaces w ON w\.id = d\.id WHERE ` + lightPredicateReFor("w")).
		WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectExec(`AND status NOT IN \('removed', 'paused'\) AND `+lightPredicateRe).
		WithArgs(models.StatusPaused, lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").WillReturnResult(sqlmock.NewResult(0, 1))

	if w := pauseCall(h, lightTestWS, "", nil); w.Code != http.StatusOK {
		t.Fatalf("pause = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func lightResumeCall(h *WorkspaceHandler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+lightTestWS+"/resume", nil)
	h.Resume(c)
	return w
}

func TestResumeOfAParkedLightAgentIs409AndDoesNotProvision(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	cp := &trackingCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectLightState(mock, lightTestWS, "parked")
	rec := recordSQL(t)
	provLog := captureProvLog(t)

	w := lightResumeCall(h)
	h.waitAsyncForTest()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"LIGHT_AGENT_PARKED"`) {
		t.Fatalf("resume = %d %s, want 409", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	// A provision is dispatched synchronously (provisionWorkspaceAuto logs
	// provision.start before it spawns), so nothing may have been logged,
	// written or sent to the control plane.
	if strings.Contains(provLog(), "provision.start") {
		t.Error("resume of a parked Light agent dispatched a provision")
	}
	if calls := trackedCPCalls(cp); len(calls) != 0 {
		t.Errorf("resume reached the CP: %v", calls)
	}
	assertNoWrites(t, rec())
}

func TestResumeCascadeAndClaimCarryTheLightPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectLightState(mock, lightTestWS, "")
	mock.ExpectQuery(`SELECT name, tier, COALESCE\(runtime, 'claude-code'\), COALESCE\(template, ''\) FROM workspaces WHERE id = \$1 AND status = 'paused'`).
		WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "runtime", "template"}).AddRow("Lead", 1, "hermes", ""))
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectQuery(`WITH RECURSIVE descendants .* SELECT d\.id, d\.name, d\.tier, d\.runtime, d\.template FROM descendants d JOIN workspaces w ON w\.id = d\.id WHERE ` + lightPredicateReFor("w")).
		WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "tier", "runtime", "template"}))
	// A refused claim (zero rows) provisions nothing.
	mock.ExpectExec(`WHERE id = \$2 AND status = 'paused' AND `+lightPredicateRe).
		WithArgs(models.StatusProvisioning, lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))

	w := lightResumeCall(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("resume = %d %s, want the unclaimed 500", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestHibernateOfALightAgentIs409(t *testing.T) {
	for _, state := range []string{"open", "parked"} {
		t.Run(state, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
			waitForHandlerAsyncBeforeDBCleanup(t, h)
			expectLightState(mock, lightTestWS, state)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
			c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+lightTestWS+"/hibernate", nil)
			h.Hibernate(c)

			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusConflict || body["code"] != "LIGHT_ENABLED" || body["light_state"] != state {
				t.Fatalf("hibernate = %d %s, want 409 LIGHT_ENABLED", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestSwitchProviderOfALightAgentIs409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(`SELECT status, name, tier, COALESCE\(runtime, 'claude-code'\)`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime", "template", "provider", "data_persistence", "instance_id"}).
			AddRow("online", "Light agent", 1, "hermes", "", "", "", nil))
	expectLightState(mock, lightTestWS, "open")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+lightTestWS+"/switch-provider", strings.NewReader(`{"provider":"hetzner"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.SwitchProvider(c)

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"LIGHT_ENABLED"`) {
		t.Fatalf("switch-provider = %d %s, want 409 LIGHT_ENABLED", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The provider-switch pre-claim is pinned at the source level, like its
// existing ordering pins (workspace_switch_provider_test.go).
func TestSwitchProviderPreClaimRefusesALightRow(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	stripped := stripGoComments(src)
	start := bytes.Index(stripped, []byte("preClaim, err := db.DB.ExecContext(ctx, `"))
	if start < 0 {
		t.Fatal("pre-claim not found")
	}
	end := bytes.Index(stripped[start:], []byte("id, models.StatusProvisioning, oldProvider)"))
	if end < 0 {
		t.Fatal("pre-claim args not found")
	}
	if !bytes.Contains(stripped[start:start+end], []byte("AND light_state IS NULL")) {
		t.Fatal("the provider-switch pre-claim must refuse a Light agent (AND light_state IS NULL)")
	}
}

func TestDrainQueueSkipsAParkedLightAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	expectLightState(mock, lightTestWS, "parked")
	// No DequeueNext: the queue is left untouched.

	h.DrainQueueForWorkspace(context.Background(), lightTestWS, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestA2AQueueSweeperExcludesParkedLightAgents(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(`AND w\.status IN \('online', 'degraded'\)\s+AND ` + lightPredicateReFor("w") + `\s+GROUP BY`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "capacity"}))

	h.sweepA2AQueue(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
