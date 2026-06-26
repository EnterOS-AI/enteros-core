package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// warmupRecorder is a stub WarmupSendFunc that records every call so a test
// can assert how many times — and with which workspace id / caller — the
// concierge warmup fired. err is returned from every call to exercise the
// fail-safe path. Concurrency-safe: the warmup runs on a detached goroutine.
type warmupRecorder struct {
	mu      sync.Mutex
	calls   int32
	wsIDs   []string
	callers []string
	bodies  [][]byte
	err     error
}

func (r *warmupRecorder) send(_ context.Context, workspaceID string, body []byte, callerID string) (int, []byte, error) {
	atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	r.wsIDs = append(r.wsIDs, workspaceID)
	r.callers = append(r.callers, callerID)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	if r.err != nil {
		return http.StatusBadGateway, nil, r.err
	}
	return http.StatusOK, []byte(`{"ok":true}`), nil
}

func (r *warmupRecorder) count() int { return int(atomic.LoadInt32(&r.calls)) }

// expectHealthyOnlinePlatformHeartbeat sets up the sqlmock expectations for a
// kind=platform concierge heartbeat that reports the management MCP loaded and
// stays online (clearing any stale stamp). This is the happy "freshly online
// concierge" path that triggers the warmup. Mirrors the expectations in
// TestHeartbeatHandler_PlatformManagementMCPLoaded_ClearsStampStaysOnline.
func expectHealthyOnlinePlatformHeartbeat(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(wsID, 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// core#3082 / molecule-core#3256: persist loaded_mcp_tools to the row.
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))
	// mcp_unloaded_since is NULL in this path, so the gate's default branch does
	// NOT issue a "clear stamp" UPDATE (it only clears when a stamp exists).
}

func newHealthyPlatformHeartbeatRequest(wsID string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"` + wsID + `","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

// TestConciergeWarmup_FiresOnceForPlatformOnline verifies (a): when a
// kind=platform concierge transitions to / is observed online with its model +
// management MCP present, the warmup A2A fires exactly once, targeting the
// concierge's own id with a benign system-caller turn.
func TestConciergeWarmup_FiresOnceForPlatformOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	rec := &warmupRecorder{}
	handler.SetWarmupSendFunc(rec.send)

	expectHealthyOnlinePlatformHeartbeat(mock, "ws-warmup-fires")

	c, w := newHealthyPlatformHeartbeatRequest("ws-warmup-fires")
	handler.Heartbeat(c)

	// Drain the detached warmup goroutine deterministically before asserting.
	waitGlobalAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("expected warmup to fire exactly once, got %d", got)
	}
	if rec.wsIDs[0] != "ws-warmup-fires" {
		t.Errorf("warmup fired for wrong workspace: got %q want %q", rec.wsIDs[0], "ws-warmup-fires")
	}
	if rec.callers[0] != conciergeWarmupCaller {
		t.Errorf("warmup used wrong caller: got %q want %q", rec.callers[0], conciergeWarmupCaller)
	}
	// The body must be a valid A2A message/send turn carrying the warmup text.
	if !bytes.Contains(rec.bodies[0], []byte("message/send")) || !bytes.Contains(rec.bodies[0], []byte(conciergeWarmupText)) {
		t.Errorf("warmup body not a benign message/send turn: %s", rec.bodies[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmup_DoesNotFireForNonPlatform verifies (b): a regular
// (kind=workspace) workspace never gets a warmup, even when it goes online.
func TestConciergeWarmup_DoesNotFireForNonPlatform(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	rec := &warmupRecorder{}
	handler.SetWarmupSendFunc(rec.send)

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-regular").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))

	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-regular", 0.0, "", 0, 60, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// kind=workspace → the whole platform block (and the warmup) is skipped.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-regular").
		WillReturnRows(evalStatusRows("online", models.KindWorkspace, nil, nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-regular","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)
	waitGlobalAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("expected NO warmup for a non-platform workspace, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmup_DoesNotFireTwiceAcrossHeartbeats verifies (c): across two
// online heartbeats for the SAME concierge, the warmup fires only once (the
// in-process one-shot guard).
func TestConciergeWarmup_DoesNotFireTwiceAcrossHeartbeats(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	rec := &warmupRecorder{}
	handler.SetWarmupSendFunc(rec.send)

	// Two identical healthy-online heartbeats for the same workspace.
	expectHealthyOnlinePlatformHeartbeat(mock, "ws-warmup-once")
	expectHealthyOnlinePlatformHeartbeat(mock, "ws-warmup-once")

	c1, w1 := newHealthyPlatformHeartbeatRequest("ws-warmup-once")
	handler.Heartbeat(c1)
	waitGlobalAsyncForTest()

	c2, w2 := newHealthyPlatformHeartbeatRequest("ws-warmup-once")
	handler.Heartbeat(c2)
	waitGlobalAsyncForTest()

	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("expected both heartbeats to 200, got %d and %d", w1.Code, w2.Code)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("expected warmup to fire exactly once across two heartbeats, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmup_SenderErrorDoesNotAffectStatus verifies (d): a warmup
// send error must NOT change the resulting status and must NOT error the
// heartbeat handler. The heartbeat still returns 200 and the concierge stays
// online (no degrade UPDATE is issued because of the warmup failure).
func TestConciergeWarmup_SenderErrorDoesNotAffectStatus(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)

	rec := &warmupRecorder{err: errors.New("warmup boom: connection refused")}
	handler.SetWarmupSendFunc(rec.send)

	// Same healthy-online expectations. CRUCIALLY: no extra degrade UPDATE and
	// no extra broadcast are expected — a warmup failure leaves the status path
	// untouched. ExpectationsWereMet would fail if the warmup error triggered
	// an unexpected DB write.
	expectHealthyOnlinePlatformHeartbeat(mock, "ws-warmup-err")

	c, w := newHealthyPlatformHeartbeatRequest("ws-warmup-err")
	handler.Heartbeat(c)
	waitGlobalAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 despite warmup error, got %d: %s", w.Code, w.Body.String())
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("expected the (failing) warmup to have been attempted once, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (a warmup error must not cause extra DB writes): %v", err)
	}
}

// TestConciergeWarmup_NilSenderIsNoOp verifies the nil-safe path: with no
// warmup sender wired (e.g. CP/SaaS without a workspace handler, or unit
// tests), the heartbeat path runs unchanged and does not panic.
func TestConciergeWarmup_NilSenderIsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewRegistryHandler(broadcaster)
	// Intentionally do NOT call SetWarmupSendFunc — warmupSend stays nil.

	expectHealthyOnlinePlatformHeartbeat(mock, "ws-warmup-nil")

	c, w := newHealthyPlatformHeartbeatRequest("ws-warmup-nil")
	handler.Heartbeat(c)
	waitGlobalAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 with nil warmup sender, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmup_BodyIsValidA2A pins the warmup body shape: a v0.3
// message/send turn with a `kind:text` part (NOT `type`, which v0.3 receivers
// drop) carrying the benign warmup text.
func TestConciergeWarmup_BodyIsValidA2A(t *testing.T) {
	body, err := buildConciergeWarmupBody("ws-body-check")
	if err != nil {
		t.Fatalf("buildConciergeWarmupBody errored: %v", err)
	}
	for _, want := range []string{
		`"jsonrpc":"2.0"`,
		`"method":"message/send"`,
		`"role":"user"`,
		`"kind":"text"`,
		conciergeWarmupText,
		`"concierge_warmup":true`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("warmup body missing %q; body=%s", want, body)
		}
	}
	// Must NOT use the `type`-keyed Part discriminator (v0.3 drops it).
	if bytes.Contains(body, []byte(`"type":"text"`)) {
		t.Errorf("warmup body used `type`-keyed part (dropped by v0.3); body=%s", body)
	}
}

// TestConciergeWarmup_MessageIDUniquePerFire is the regression for CR2 #14189:
// the warmup messageId must be UNIQUE per fire. The in-process one-shot guard
// resets on a CP restart, so a post-restart warmup legitimately re-fires; a
// deterministic messageId would collide with the pre-restart fire and be deduped
// downstream → the re-warmup no-ops → the concierge stays degraded (the exact
// failure this warmup fixes). It must also keep the recognizable prefix.
func TestConciergeWarmup_MessageIDUniquePerFire(t *testing.T) {
	b1, err := buildConciergeWarmupBody("ws-dup")
	if err != nil {
		t.Fatalf("build1: %v", err)
	}
	b2, err := buildConciergeWarmupBody("ws-dup")
	if err != nil {
		t.Fatalf("build2: %v", err)
	}
	id1, id2 := warmupMessageID(t, b1), warmupMessageID(t, b2)
	if id1 == id2 {
		t.Errorf("two warmup fires produced the SAME messageId %q — a post-restart re-warmup would be deduped (CR2 #14189)", id1)
	}
	for _, id := range []string{id1, id2} {
		if !strings.HasPrefix(id, "concierge-warmup-ws-dup-") {
			t.Errorf("messageId %q lost the recognizable 'concierge-warmup-<ws>-' prefix", id)
		}
	}
}

func warmupMessageID(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal warmup body: %v", err)
	}
	return m.Params.Message.MessageID
}

// TestConciergeWarmup_TimeoutIsBounded is a cheap guard that the warmup POST
// timeout constant stays sane (bounded so the goroutine can't leak).
func TestConciergeWarmup_TimeoutIsBounded(t *testing.T) {
	if conciergeWarmupTimeout <= 0 || conciergeWarmupTimeout > 5*time.Minute {
		t.Errorf("conciergeWarmupTimeout=%s is out of the sane (0, 5m] range", conciergeWarmupTimeout)
	}
}
