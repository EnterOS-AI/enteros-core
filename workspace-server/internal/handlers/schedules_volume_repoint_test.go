package handlers

// schedules_volume_repoint_test.go — the webhook event-poke, History, and Health
// (peer + admin) surfaces must serve schedules from the runtime
// /internal/schedules API. Core no longer stores or fires schedules (the legacy
// dual-path core-DB backend was retired in P4b), so these tests assert the
// volume path end-to-end and the exact Canvas JSON shapes it returns.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// markVolumeScheduler registers wsID as a volume-native scheduler workspace for
// the duration of the test: the `scheduler` capability is declared
// (heartbeat-equivalent), so schedule CRUD is served from the runtime volume.
func markVolumeScheduler(t *testing.T, wsID string) {
	t.Helper()
	runtimeOverrides.SetCapabilities(wsID, map[string]bool{"scheduler": true})
	t.Cleanup(func() { runtimeOverrides.SetCapabilities(wsID, nil) })
}

// expectFanoutURL stubs the single-column URL SELECT the quiet fan-out resolver
// performs (armSchedulerPlugin-style; distinct from the two-column
// resolveWorkspaceForwardCreds query that expectURL stubs).
func expectFanoutURL(mock sqlmock.Sqlmock, workspaceID, url string) {
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(url))
}

// volumeRuntimeStub is a minimal /internal/schedules* runtime API that records
// every request (method + decoded path + auth) it receives.
type volumeRuntimeStub struct {
	mu       sync.Mutex
	requests []string
	auths    []string
	grid     string
	health   string
	history  string
	srv      *httptest.Server
}

func newVolumeRuntimeStub(t *testing.T, grid, health, history string) *volumeRuntimeStub {
	t.Helper()
	s := &volumeRuntimeStub{grid: grid, health: health, history: history}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules":
			fmt.Fprint(w, s.grid)
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules/health":
			fmt.Fprint(w, s.health)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/history"):
			fmt.Fprint(w, s.history)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"poked":"x"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such route"}`)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *volumeRuntimeStub) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// jsonKeys returns the sorted key set of a decoded JSON object.
func jsonKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func assertExactKeys(t *testing.T, m map[string]interface{}, want []string, label string) {
	t.Helper()
	if len(m) != len(want) {
		t.Errorf("%s: key set drift — want exactly %v, got %v", label, want, jsonKeys(m))
		return
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s: missing key %q (got %v)", label, k, jsonKeys(m))
		}
	}
}

// ==================== Webhook event-poke ====================

// TestWebhookCronPoke_VolumeNative_PokesRuntimeNotDB is the webhook re-point
// gate: on issues/opened, a volume-native workspace's matching ENABLED grid
// entry must be poked via POST /internal/schedules/{name}/run (the only live
// "fire now" channel post-#4399), while disabled/non-matching entries must NOT
// be poked. Core writes no DB table.
func TestWebhookCronPoke_VolumeNative_PokesRuntimeNotDB(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWebhookHandler(newTestBroadcaster())

	wsID := "11e59f01-0001-4000-8000-000000000001"
	markVolumeScheduler(t, wsID)

	stub := newVolumeRuntimeStub(t,
		`{"schedules":[
			{"name":"pick-up-work engineer","cron":"*/30 * * * *","timezone":"UTC","prompt":"p","enabled":true},
			{"name":"pick-up-work paused","cron":"*/30 * * * *","timezone":"UTC","prompt":"p","enabled":false},
			{"name":"unrelated standup","cron":"*/30 * * * *","timezone":"UTC","prompt":"p","enabled":true}
		]}`,
		`{"last_tick":null,"armed":3,"errors":{}}`,
		`{"history":[]}`,
	)

	// Fan-out resolution: URL + inbound secret for the volume workspace.
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "poke-secret-1")

	secret := "test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	body := []byte(`{
		"action": "opened",
		"repository": {"full_name": "acme/repo"},
		"sender": {"login": "alice"},
		"issue": {"number": 7, "title": "T", "html_url": "https://example.com/7"}
	}`)
	w, c := newWebhookTestContext(t, "", body)
	c.Request.Header.Set("X-GitHub-Event", "issues")
	c.Request.Header.Set("X-Hub-Signature-256", githubSignature(secret, body))

	handler.GitHub(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Exactly one poke, for the enabled matching entry only.
	reqs := stub.got()
	var pokes []string
	for _, r := range reqs {
		if strings.HasPrefix(r, "POST ") {
			pokes = append(pokes, r)
		}
	}
	if len(pokes) != 1 || pokes[0] != "POST /internal/schedules/pick-up-work engineer/run" {
		t.Errorf("want exactly one poke for the enabled matching schedule, got %v (all: %v)", pokes, reqs)
	}
	// The poke must carry the workspace's inbound secret.
	for _, a := range stub.auths {
		if a != "Bearer poke-secret-1" {
			t.Errorf("runtime forward missing inbound-secret bearer, got %q", a)
		}
	}
	// Only the volume poke is counted now — core no longer writes a DB table.
	if !strings.Contains(w.Body.String(), `"schedules_affected":1`) {
		t.Errorf("expected schedules_affected 1 (1 poked), got: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== History ====================

// TestScheduleHistory_VolumeNative_ProxiesRunLog_ShapeParity — the volume
// History surface must proxy GET /internal/schedules/{name}/history (the
// per-name filter is a PATH segment; the runtime ignores query params) and
// return the run log in the EXACT legacy HistoryEntry JSON shape: same field
// names, newest-first, daemon "fired" mapped to the legacy success value "ok",
// newer daemon states passed through.
func TestScheduleHistory_VolumeNative_ProxiesRunLog_ShapeParity(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "11e59f01-0003-4000-8000-000000000003"
	markVolumeScheduler(t, wsID)

	// Daemon appends chronologically: index 0 is the OLDER row.
	stub := newVolumeRuntimeStub(t, `{"schedules":[]}`, `{}`,
		`{"history":[
			{"name":"standup","at":"2026-07-16T09:00:00+00:00","scheduled_for":"2026-07-16T09:00:00+00:00","poked":false,"status":"fired"},
			{"name":"standup","at":"2026-07-16T10:00:00+00:00","scheduled_for":"2026-07-16T10:00:00+00:00","poked":true,"status":"unknown"}
		]}`,
	)
	expectURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "hist-secret-3")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "scheduleId", Value: "standup"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules/standup/history", nil)

	handler.History(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// The runtime must have been asked via the path-segment form.
	if reqs := stub.got(); len(reqs) != 1 || reqs[0] != "GET /internal/schedules/standup/history" {
		t.Errorf("want one GET /internal/schedules/standup/history, got %v", reqs)
	}

	var entries []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %s", len(entries), w.Body.String())
	}
	// Legacy shape: exactly these fields, per HistoryEntry.
	wantKeys := []string{"timestamp", "duration_ms", "status", "error_detail", "request"}
	for i, e := range entries {
		assertExactKeys(t, e, wantKeys, fmt.Sprintf("history[%d]", i))
	}
	// Newest first (legacy ORDER BY created_at DESC parity).
	if entries[0]["status"] != "unknown" || entries[1]["status"] != "ok" {
		t.Errorf("want newest-first with fired→ok mapping; got statuses %v, %v",
			entries[0]["status"], entries[1]["status"])
	}
	ts0, err := time.Parse(time.RFC3339, entries[0]["timestamp"].(string))
	if err != nil {
		t.Fatalf("timestamp not RFC3339: %v", err)
	}
	if ts0.UTC().Hour() != 10 {
		t.Errorf("newest entry should be the 10:00 run, got %v", ts0)
	}
	if entries[0]["duration_ms"] != nil {
		t.Errorf("duration_ms must be null (daemon log has no durations), got %v", entries[0]["duration_ms"])
	}
	req0, ok := entries[0]["request"].(map[string]interface{})
	if !ok || req0["schedule_id"] != "standup" {
		t.Errorf("request must be an object carrying schedule_id, got %v", entries[0]["request"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// ==================== Health (peer) ====================

// TestScheduleHealth_VolumeNative_ShapeParity_AndRedaction — the volume Health
// surface must map grid + daemon-health into the EXACT legacy
// ScheduleHealthResponse shape, preserving the issue-#249 redaction contract:
// no prompt, no cron_expr, no timezone — ever.
func TestScheduleHealth_VolumeNative_ShapeParity_AndRedaction(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()
	t.Setenv("ADMIN_TOKEN", "vol-health-admin-token")

	wsID := "11e59f01-0004-4000-8000-000000000004"
	markVolumeScheduler(t, wsID)

	stub := newVolumeRuntimeStub(t,
		`{"schedules":[
			{"name":"nightly","cron":"0 3 * * *","timezone":"UTC","prompt":"secret prompt text","enabled":true},
			{"name":"broken","cron":"0 4 * * *","timezone":"UTC","prompt":"p2","enabled":true}
		]}`,
		`{"last_tick":"2026-07-16T10:00:00+00:00","armed":2,"errors":{"broken":"unschedulable cron"}}`,
		`{"history":[]}`,
	)
	// Two forwards (grid, then health); the inbound secret is cached after the
	// first read, so: URL, secret, URL.
	expectURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "health-secret-4")
	expectURL(mock, wsID, stub.srv.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", "11e59f01-00ca-4000-8000-0000000000ca")
	req.Header.Set("Authorization", "Bearer vol-health-admin-token")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %s", len(entries), w.Body.String())
	}
	// Legacy shape: exactly the ScheduleHealthResponse fields.
	wantKeys := []string{"id", "name", "enabled", "last_run_at", "next_run_at", "run_count", "last_status", "last_error"}
	byName := map[string]map[string]interface{}{}
	for i, e := range entries {
		assertExactKeys(t, e, wantKeys, fmt.Sprintf("health[%d]", i))
		byName[e["name"].(string)] = e
	}
	if s := byName["nightly"]; s == nil || s["last_status"] != "ok" || s["next_run_at"] == nil {
		t.Errorf("nightly: want last_status=ok + computed next_run_at, got %v", byName["nightly"])
	}
	if s := byName["broken"]; s == nil || s["last_status"] != "error" || s["last_error"] != "unschedulable cron" {
		t.Errorf("broken: want last_status=error + daemon reason, got %v", byName["broken"])
	}
	// Redaction contract (issue #249): no task content, no cron internals.
	raw := w.Body.String()
	for _, forbidden := range []string{"prompt", "cron_expr", "timezone", "secret prompt text"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("health response must not contain %q: %s", forbidden, raw)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestScheduleHealth_VolumeNative_PeerWithoutCanCommunicate_Rejected — the
// CanCommunicate gate must fire BEFORE any runtime forward on the volume
// branch: a non-peer caller gets 403 and the workspace runtime sees zero
// traffic. (Mutation-verified: moving the volume branch above the gate makes
// this fail with a 200 + forwarded requests.)
func TestScheduleHealth_VolumeNative_PeerWithoutCanCommunicate_Rejected(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "11e59f01-0005-4000-8000-000000000005"
	callerID := "11e59f01-0006-4000-8000-000000000006"
	markVolumeScheduler(t, wsID)

	stub := newVolumeRuntimeStub(t, `{"schedules":[]}`, `{}`, `{}`)

	// Caller authenticates, then fails the hierarchy check. No forward-cred
	// expectations exist: any runtime forward would trip sqlmock AND stub.
	expectScheduleHealthWorkspaceAuth(mock, callerID)
	mockCanCommunicate(mock, callerID, wsID, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", callerID)
	req.Header.Set("Authorization", "Bearer health-token")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-peer on volume workspace, got %d: %s", w.Code, w.Body.String())
	}
	if got := stub.got(); len(got) != 0 {
		t.Errorf("auth rejection must precede any runtime forward, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestScheduleHealth_VolumeNative_SelfCall_Allowed — the callerID==workspaceID
// self-read must bypass the CanCommunicate gate and reach the volume forward.
// (The negative side is covered by _PeerWithoutCanCommunicate_Rejected; this
// guards the ALLOW branch so an over-restrictive auth regression that dropped
// the self-bypass — making agents unable to read their OWN schedule health —
// would be caught. Deliberately sets NO CanCommunicate expectation: if the
// self-bypass is removed, the handler hits the unmocked CanCommunicate query
// and sqlmock fails the test. Mutation-verified.)
func TestScheduleHealth_VolumeNative_SelfCall_Allowed(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "11e59f01-0007-4000-8000-000000000007"
	markVolumeScheduler(t, wsID)
	stub := newVolumeRuntimeStub(t, `{"schedules":[]}`, `{"last_tick":null,"armed":0,"errors":{}}`, `{"history":[]}`)

	// Bearer resolves to wsID → callerID == workspaceID (self). No
	// CanCommunicate query is expected; the two forwards (grid, health) follow.
	expectScheduleHealthWorkspaceAuth(mock, wsID)
	expectURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "self-secret")
	expectURL(mock, wsID, stub.srv.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", wsID)
	req.Header.Set("Authorization", "Bearer health-token")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("self-read must be allowed to the volume forward, got %d: %s", w.Code, w.Body.String())
	}
	if got := stub.got(); len(got) != 2 {
		t.Errorf("self-read must reach the runtime (grid+health), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestScheduleHealth_VolumeNative_CanCommunicatePeer_Allowed — a peer caller
// that passes CanCommunicate must reach the volume forward. Guards the ALLOW
// arm of the hierarchy gate (its DENY arm is _PeerWithoutCanCommunicate_
// Rejected); a regression that inverted or hard-denied the gate would be caught
// here rather than silently blocking all cross-agent health reads.
func TestScheduleHealth_VolumeNative_CanCommunicatePeer_Allowed(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	wsID := "11e59f01-0008-4000-8000-000000000008"
	callerID := "11e59f01-0009-4000-8000-000000000009"
	markVolumeScheduler(t, wsID)
	stub := newVolumeRuntimeStub(t, `{"schedules":[]}`, `{"last_tick":null,"armed":0,"errors":{}}`, `{"history":[]}`)

	expectScheduleHealthWorkspaceAuth(mock, callerID)
	mockCanCommunicate(mock, callerID, wsID, true)
	expectURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "peer-secret")
	expectURL(mock, wsID, stub.srv.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	req := httptest.NewRequest("GET", "/workspaces/"+wsID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", callerID)
	req.Header.Set("Authorization", "Bearer health-token")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("CanCommunicate peer must be allowed to the volume forward, got %d: %s", w.Code, w.Body.String())
	}
	if got := stub.got(); len(got) != 2 {
		t.Errorf("allowed peer read must reach the runtime (grid+health), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}

// TestScheduleHealthResponse_HasNoPromptOrCronFields is the structural half of
// the issue #249 redaction contract: _ShapeParity_AndRedaction proves the live
// response omits task content; this proves the RESPONSE TYPE cannot carry it, so
// a future field addition (e.g. re-adding Prompt for a UI convenience) is caught
// at compile-adjacent time rather than leaking through the volume proxy.
func TestScheduleHealthResponse_HasNoPromptOrCronFields(t *testing.T) {
	rt := reflect.TypeOf(ScheduleHealthResponse{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.ToLower(rt.Field(i).Tag.Get("json"))
		name := strings.ToLower(rt.Field(i).Name)
		for _, banned := range []string{"prompt", "cron", "timezone"} {
			if strings.Contains(tag, banned) || strings.Contains(name, banned) {
				t.Errorf("ScheduleHealthResponse must not expose %q (field %s / tag %q) — redaction contract #249",
					banned, rt.Field(i).Name, rt.Field(i).Tag.Get("json"))
			}
		}
	}
}

// ==================== Health (admin aggregate) ====================

// TestAdminSchedulesHealth_VolumeNative_UsesRuntimeProxy — the admin aggregate
// serves a capability-advertising workspace from the runtime proxy (grid +
// daemon last_tick as the liveness signal).
func TestAdminSchedulesHealth_VolumeNative_UsesRuntimeProxy(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewAdminSchedulesHealthHandler()

	volWS := "11e59f01-0007-4000-8000-000000000007"
	markVolumeScheduler(t, volWS)

	lastTick := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
	stub := newVolumeRuntimeStub(t,
		`{"schedules":[{"name":"vol-sched","cron":"*/5 * * * *","timezone":"UTC","prompt":"p","enabled":true}]}`,
		`{"last_tick":"`+lastTick+`","armed":1,"errors":{}}`,
		`{"history":[]}`,
	)

	// Volume loop: name lookup, then quiet fan-out creds.
	mock.ExpectQuery(`SELECT name FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(volWS).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("Vol WS"))
	expectFanoutURL(mock, volWS, stub.srv.URL)
	expectInboundSecret(mock, volWS, "admin-secret-7")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []adminScheduleHealth
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 volume entry, got %d: %s", len(resp), w.Body.String())
	}
	vol := resp[0]
	if vol.WorkspaceName != "Vol WS" || vol.ScheduleID != "vol-sched" || vol.CronExpr != "*/5 * * * *" {
		t.Errorf("volume entry fields wrong: %+v", vol)
	}
	// Daemon ticked 1 min ago, threshold 2×5min=600s → alive → ok.
	if vol.Status != "ok" || vol.StaleThresholdSeconds != 600 {
		t.Errorf("volume entry liveness wrong (want ok/600): %+v", vol)
	}
	if vol.LastRunAt == nil {
		t.Errorf("volume entry must carry the daemon last_tick as last_run_at, got nil")
	}
	// Runtime saw exactly the grid + health reads.
	reqs := stub.got()
	if len(reqs) != 2 || reqs[0] != "GET /internal/schedules" || reqs[1] != "GET /internal/schedules/health" {
		t.Errorf("want [grid, health] reads, got %v", reqs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
}
