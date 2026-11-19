package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// A name-keyed grid entry maps to the stable Canvas shape with id==name,
// cron->cron_expr, and a computed next_run_at.
func TestToScheduleResponse_Translation(t *testing.T) {
	e := volumeEntry{
		Name: "standup", Cron: "*/15 * * * *", Timezone: "UTC",
		Prompt: "Summarise PRs.", Enabled: true, Source: "runtime",
	}
	got := toScheduleResponse("ws-9", e)
	if got.ID != "standup" || got.Name != "standup" {
		t.Errorf("id/name must equal the grid name; got id=%q name=%q", got.ID, got.Name)
	}
	if got.CronExpr != "*/15 * * * *" {
		t.Errorf("cron must map to cron_expr; got %q", got.CronExpr)
	}
	if got.WorkspaceID != "ws-9" || got.Source != "runtime" || !got.Enabled {
		t.Errorf("field passthrough wrong: %+v", got)
	}
	if got.NextRunAt == nil {
		t.Error("next_run_at should be computed from the cron so the UI stays populated")
	}
	// A malformed timezone must not panic — next_run_at stays nil, rest intact.
	bad := toScheduleResponse("ws-9", volumeEntry{Name: "x", Cron: "* * * * *", Timezone: "Mars/Phobos"})
	if bad.NextRunAt != nil {
		t.Error("bad timezone should leave next_run_at nil, not fabricate one")
	}
}

// The create body the proxy sends must be exactly the runtime grid contract
// (name/cron/timezone/prompt/enabled/source) — NOT core's cron_expr.
func TestCreateBodyShape(t *testing.T) {
	e := volumeEntry{Name: "n", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "p", Enabled: true, Source: "runtime"}
	raw, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["cron"]; !ok {
		t.Error("runtime contract uses 'cron', proxy must not send 'cron_expr'")
	}
	if _, ok := m["cron_expr"]; ok {
		t.Error("proxy leaked core's 'cron_expr' field into the runtime body")
	}
	for _, k := range []string{"name", "timezone", "prompt", "enabled", "source"} {
		if _, ok := m[k]; !ok {
			t.Errorf("create body missing %q", k)
		}
	}
}

// relayScheduleError maps runtime status codes onto the codes the Canvas client
// already handles, and never leaks a raw auth failure as a client 4xx.
func TestRelayScheduleError_StatusMapping(t *testing.T) {
	cases := []struct {
		runtime int
		want    int
	}{
		{http.StatusNotFound, http.StatusNotFound},
		{http.StatusBadRequest, http.StatusBadRequest},
		{http.StatusConflict, http.StatusBadRequest}, // disabled run-now → 400 for the client
		{http.StatusUnauthorized, http.StatusBadGateway},
		{http.StatusForbidden, http.StatusBadGateway},
		{http.StatusInternalServerError, http.StatusBadGateway},
		// A gateway status: the runtime (or what is in front of it) is not
		// serving yet. A retryable 503, which a proxy passes through.
		{http.StatusBadGateway, http.StatusServiceUnavailable},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		relayScheduleError(c, tc.runtime, []byte(`{"error":"boom"}`))
		if w.Code != tc.want {
			t.Errorf("runtime %d → want %d, got %d", tc.runtime, tc.want, w.Code)
		}
		notReady := strings.Contains(w.Body.String(), `"code":"WORKSPACE_NOT_READY"`) && w.Header().Get("Retry-After") != ""
		if notReady != (tc.want == http.StatusServiceUnavailable) {
			t.Errorf("runtime %d → body %s Retry-After %q; only a not-ready 503 carries WORKSPACE_NOT_READY", tc.runtime, w.Body.String(), w.Header().Get("Retry-After"))
		}
	}
}

// A forward that gets no answer (the agent is starting, or a restart is
// replacing its pod) is a retryable 503 WORKSPACE_NOT_READY with
// Retry-After, not a 502: Cloudflare replaces an origin's 502 body.
func TestForwardScheduleAPI_UnreachableIs503NotReady(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	const wsID = "44e59f01-0009-4000-8000-000000000009"
	expectURLAndMode(mock, wsID, deadURL, "push")
	expectInboundSecret(mock, wsID, "sched-secret")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/schedules", nil)
	NewScheduleHandler().List(c)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"WORKSPACE_NOT_READY"`) ||
		!strings.Contains(w.Body.String(), "schedule backend unreachable") || w.Header().Get("Retry-After") != workspaceNotReadyRetryAfter {
		t.Fatalf("list of an unreachable agent = %d %s (Retry-After %q), want 503 WORKSPACE_NOT_READY", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
