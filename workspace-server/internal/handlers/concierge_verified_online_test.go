package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestHeartbeat_PlatformWarming_NotYetOnline is the core#3082 keystone: a
// kind=platform concierge whose heartbeat does NOT report provision_workspace in
// loaded_mcp_tools is HELD in 'provisioning' (warming) — it is NEVER flipped to
// online. The only writes are the heartbeat columns + loaded_mcp_tools persist +
// the warming-window stamp. CRUCIALLY no "status=online" UPDATE is issued; if the
// code wrongly flipped, the stamp expectation would go unmatched and the test
// would fail. This proves "online is NOT set until a heartbeat proves the tool".
func TestHeartbeat_PlatformWarming_NotYetOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-warm-hold").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-warm-hold", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-warm-hold").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-warm-hold").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-warm-hold").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Warming branch stamps the window; NO status='online' flip.
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = COALESCE").
		WithArgs("ws-warm-hold").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// mcp_server_present=true but provision_workspace NOT in loaded_mcp_tools.
	body := `{"workspace_id":"ws-warm-hold","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other__tool"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (warming row must NOT flip online): %v", err)
	}
}

// TestHeartbeat_PlatformWarming_FlipsOnlineWhenToolReported is the other half of
// the keystone: once a heartbeat reports loaded_mcp_tools CONTAINING
// provision_workspace, the warming row is VERIFIED-ready and flips
// provisioning→online (clearing the warming stamp) and broadcasts WORKSPACE_ONLINE.
func TestHeartbeat_PlatformWarming_FlipsOnlineWhenToolReported(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-warm-verified").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-warm-verified", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-warm-verified").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-warm-verified").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-warm-verified").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// VERIFIED flip: provisioning→online, clearing mcp_unloaded_since.
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL").
		WithArgs(models.StatusOnline, "ws-warm-verified", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// WORKSPACE_ONLINE broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-warm-verified","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (verified flip): %v", err)
	}
}

// TestHeartbeat_VerifiedFlip_FiresFirstBootGreeting proves the verified
// provisioning→online promotion invokes the first-boot greeting hook with the
// heartbeat's tool count — the seam first_boot_greeting.go hangs off. Without
// this wire, a fresh onboarding lands in a silent empty chat again.
func TestHeartbeat_VerifiedFlip_FiresFirstBootGreeting(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	type call struct {
		workspaceID string
		toolCount   int
	}
	greeted := make(chan call, 1)
	handler.SetFirstBootGreeter(func(workspaceID string, toolCount int) {
		greeted <- call{workspaceID, toolCount}
	})

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-warm-greet").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-warm-greet", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-warm-greet").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-warm-greet").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-warm-greet").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL").
		WithArgs(models.StatusOnline, "ws-warm-greet", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-warm-greet","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case got := <-greeted:
		if got.workspaceID != "ws-warm-greet" {
			t.Errorf("greeter workspace = %q, want ws-warm-greet", got.workspaceID)
		}
		if got.toolCount != 2 {
			t.Errorf("greeter toolCount = %d, want 2 (len(loaded_mcp_tools))", got.toolCount)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("first-boot greeter was not fired on the verified online flip")
	}
}

// TestHoldOnlineBroadcastForWarmingPlatform pins Register's broadcast gate:
// a platform row still HELD in 'provisioning' (warming) must not be announced
// WORKSPACE_ONLINE — that flips the canvas to an interactive chat whose sends
// bounce off the warming 503 (2026-07-18 fresh-onboarding repro). Everything
// else keeps the legacy announce.
func TestHoldOnlineBroadcastForWarmingPlatform(t *testing.T) {
	cases := []struct {
		kind, status string
		want         bool
	}{
		{"platform", "provisioning", true},
		{"platform", "online", false},
		{"workspace", "provisioning", false},
		{"workspace", "online", false},
	}
	for _, tc := range cases {
		if got := holdOnlineBroadcastForWarmingPlatform(tc.kind, tc.status); got != tc.want {
			t.Errorf("holdOnlineBroadcastForWarmingPlatform(%q,%q) = %v, want %v", tc.kind, tc.status, got, tc.want)
		}
	}
}

// TestHeartbeat_PlatformWarming_HealthyLongWarmingHolds proves the DYNAMIC,
// signal-driven warm-up terminal (core#3082 warm-up determinism): a HEALTHY
// concierge that has been warming a long time (mcp_unloaded_since well in the
// past) but has not yet reported provision_workspace is NOT force-failed on a
// clock — it keeps HOLDING 'provisioning', waiting on the real loaded_mcp_tools
// signal. This is the deletion of the old 180s managementMCPUnloadedGrace wall-
// clock FAIL (an arbitrary cutoff that killed healthy concierges whose management
// MCP was merely slow to connect — the flaky e2e-smoke STEP-4 failure). The only
// terminals now are HEALTH (fail-fast on conciergeUnhealthy, tested below) and
// LIVENESS (the provision-timeout sweep, once the box stops heartbeating). Here
// mcp_unloaded_since is already set, so NO stamp write and NO status flip occur —
// if the code wrongly failed OR flipped online, an expectation would go unmet.
func TestHeartbeat_PlatformWarming_HealthyLongWarmingHolds(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-warm-hold-long").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-warm-hold-long", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-warm-hold-long").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Warming since well past the OLD 180s grace — must STILL hold (no clock fail).
	sustained := time.Now().Add(-10 * time.Minute)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-warm-hold-long").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, sustained))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-warm-hold-long").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// NO further writes: mcp_unloaded_since already set (no re-stamp), healthy (no
	// fail), tool absent (no online flip). Any extra UPDATE would be unmatched.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-warm-hold-long","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (healthy long-warming must HOLD, not fail): %v", err)
	}
}

// TestHeartbeat_PlatformWarming_UnhealthyLongWarmingStillHolds proves the clock
// fail is gone even for an UNHEALTHY warming concierge: one that self-reports
// error_rate ≥ 0.5 AND has been warming well past the old 180s grace is HELD (not
// promoted, not failed). Holding — rather than force-failing — lets a TRANSIENT
// unhealth clear on a later heartbeat and then promote, instead of killing a
// recoverable concierge on a clock. (A genuinely dead box is caught by the
// liveness sweep once it stops heartbeating.) mcp_unloaded_since is already set,
// so no re-stamp and no other write occurs.
func TestHeartbeat_PlatformWarming_UnhealthyLongWarmingStillHolds(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-warm-unhealthy").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-warm-unhealthy", 0.9, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-warm-unhealthy").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Unhealthy AND warming since well past the OLD 180s grace — must STILL hold.
	sustained := time.Now().Add(-10 * time.Minute)
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-warm-unhealthy").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, sustained))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-warm-unhealthy").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// NO fail, NO re-stamp, NO online flip. Any extra write would be unmatched.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// error_rate 0.9 ≥ 0.5 → conciergeUnhealthy, but the row is HELD not failed.
	body := `{"workspace_id":"ws-warm-unhealthy","error_rate":0.9,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (unhealthy long-warming must HOLD, not fail): %v", err)
	}
}

// TestHeartbeat_PlatformProvisioning_LegacyRuntimeFlipsOnline pins the
// backward-compat fast path: a pre-#147 runtime (no mcp_server_present, can never
// report loaded_mcp_tools) MUST NOT be stranded in 'provisioning' by the strict
// verified gate. It flips provisioning→online on a live heartbeat (the #147
// nil=>allow rollout-order contract) so the gate deploying ahead of the runtime
// release cannot strand the fleet.
func TestHeartbeat_PlatformProvisioning_LegacyRuntimeFlipsOnline(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-legacy", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// loaded_mcp_tools omitted (nil) → no loaded_mcp_tools persist UPDATE.
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-legacy").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Legacy fast online flip (no mcp_unloaded_since clause — distinguishes it from
	// the verified flip). core#3082 fix: the status params carry an explicit
	// ::workspace_status cast so the enum write succeeds on real Postgres.
	mock.ExpectExec("UPDATE workspaces SET status = \\$1::workspace_status, updated_at = now").
		WithArgs(models.StatusOnline, "ws-legacy", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// No mcp_server_present, no loaded_mcp_tools — a pre-#147 runtime.
	body := `{"workspace_id":"ws-legacy","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (legacy fast online): %v", err)
	}
}

// expectWarmingHeldHeartbeat sets up the sqlmock expectations for a kind=platform
// concierge in 'provisioning' whose heartbeat is NOT promoted to online — it is
// HELD in the warming state, stamping the first-seen mcp_unloaded_since window.
// CRUCIALLY there is NO "status=online" UPDATE: if the gate wrongly promoted, the
// stamp expectation would go unmet (or an unexpected online UPDATE would fire) and
// the test fails. Used by the CR2 negative-case (health-gate) tests below: a
// concierge that reports provision_workspace loaded BUT is wedged / high-error /
// recently-register-failed must take this held path, not the verified flip.
func expectWarmingHeldHeartbeat(mock sqlmock.Sqlmock, wsID string, lastRegisterFailure interface{}, hasLoadedTools bool) {
	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "provisioning"))
	mock.ExpectExec("UPDATE workspaces SET").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if hasLoadedTools {
		mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
			WithArgs(sqlmock.AnyArg(), wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(evalStatusRows("provisioning", "platform", lastRegisterFailure, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Warming branch stamps the window. NO status='online' flip. If a status=online
	// UPDATE were issued instead, this stamp would go unmatched → test fails.
	mock.ExpectExec("UPDATE workspaces SET mcp_unloaded_since = COALESCE").
		WithArgs(wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestHeartbeat_PlatformVerified_WedgedNotPromoted is a CR2 #14642 negative case
// (GATE BEFORE WRITE): a warming concierge that reports provision_workspace IN
// loaded_mcp_tools — i.e. the verified-ready signal IS present — but ALSO
// self-reports runtime_state="wedged" must NOT be promoted to online. The health
// gate runs BEFORE the promotion write, so the row is held in 'provisioning'
// (warming) instead of flipping online. Proves the verified-ready flip cannot mask
// a wedged runtime (the original false-online failure mode, on a different axis).
func TestHeartbeat_PlatformVerified_WedgedNotPromoted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	expectWarmingHeldHeartbeat(mock, "ws-wedged-noflip", nil, true)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// provision_workspace IS loaded, but the runtime self-reports wedged.
	body := `{"workspace_id":"ws-wedged-noflip","error_rate":0.0,"sample_error":"SDK init timeout — restart workspace","active_tasks":0,"uptime_seconds":60,"runtime_state":"wedged","mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (wedged must NOT be promoted to online): %v", err)
	}
}

// TestHeartbeat_PlatformVerified_HighErrorRateNotPromoted is a CR2 negative case:
// a warming concierge reporting provision_workspace loaded but with error_rate
// >= 0.5 must NOT be promoted. error_rate>=0.5 is the same threshold that demotes
// an already-online agent; here it BLOCKS the promotion (gate before write).
func TestHeartbeat_PlatformVerified_HighErrorRateNotPromoted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	expectWarmingHeldHeartbeat(mock, "ws-err-noflip", nil, true)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-err-noflip","error_rate":0.6,"sample_error":"upstream 500s","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (high error_rate must NOT be promoted): %v", err)
	}
}

// TestHeartbeat_PlatformVerified_RecentRegisterFailureNotPromoted is a CR2
// negative case: a warming concierge reporting provision_workspace loaded but with
// a register failure inside the 5-minute window (last_register_failure_at recent)
// must NOT be promoted — a stale auth token starves canvas delivery, so the same
// signal that demotes an online agent (#2530) blocks promotion here.
func TestHeartbeat_PlatformVerified_RecentRegisterFailureNotPromoted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	recentFailure := time.Now().Add(-1 * time.Minute) // inside the 5-minute window
	expectWarmingHeldHeartbeat(mock, "ws-regfail-noflip", recentFailure, true)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"ws-regfail-noflip","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (recent register failure must NOT be promoted): %v", err)
	}
}

// TestHeartbeat_PlatformFailed_NonCallableToolNotPromoted is the non-callable-tool
// negative case for a NON-provisioning recoverable state: a concierge currently
// 'failed' whose loaded_mcp_tools does NOT contain provision_workspace must NOT be
// resurrected to online. It hits the default HOLD branch and returns BEFORE the
// generic recovery branches — no status=online UPDATE is issued. (The provisioning
// equivalent is TestHeartbeat_PlatformWarming_NotYetOnline; the degraded
// equivalent is TestHeartbeatHandler_DegradedNotRecoveredWhileMCPUnloaded.)
func TestHeartbeat_PlatformFailed_NonCallableToolNotPromoted(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-failed-hold").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "failed"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-failed-hold", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-failed-hold").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-failed-hold").
		WillReturnRows(evalStatusRows("failed", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-failed-hold").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// default HOLD branch: a healthy 'failed' row with the tool NOT loaded logs and
	// RETURNS. NO status=online recovery UPDATE, NO stamp, NO broadcast — any of
	// those would be an unexpected call and fail the test.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Healthy (error_rate 0, no wedge) but provision_workspace NOT in loaded_mcp_tools.
	body := `{"workspace_id":"ws-failed-hold","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","mcp__other__tool"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (failed+non-callable must NOT be promoted): %v", err)
	}
}

// TestHeartbeat_PlatformOnline_WedgedDemotes is the demotion-symmetry case
// (Researcher #14643): "online" is NOT sticky. A concierge that is already online
// — and still reports provision_workspace loaded — but newly self-reports
// runtime_state="wedged" MUST demote online→degraded. The verified-ready gate
// returns early ONLY on the non-online paths; an already-online platform row falls
// through the post-online block (tool present ⇒ no degrade there) to the generic
// wedged gate, which flips it to degraded and broadcasts WORKSPACE_DEGRADED.
func TestHeartbeat_PlatformOnline_WedgedDemotes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-online-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status"}).AddRow("", 0, "online"))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-online-wedged", 0.0, "SDK init timeout — restart workspace", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-online-wedged").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-online-wedged").
		WillReturnRows(evalStatusRows("online", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-online-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// post-online grace block: tool IS loaded, so platformAgentManagementMCPLoaded
	// reads the declared plugins and finds it present → no degrade there.
	mock.ExpectQuery("SELECT plugin_name, source_raw FROM workspace_declared_plugins").
		WithArgs("ws-online-wedged").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow(conciergePlatformMCPName, "gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main"))
	// Generic wedged gate: online→degraded.
	mock.ExpectExec("UPDATE workspaces SET status = \\$1::workspace_status, updated_at = now\\(\\) WHERE id = \\$2 AND status = 'online'").
		WithArgs(models.StatusDegraded, "ws-online-wedged").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// WORKSPACE_DEGRADED broadcast.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Already online, tool still loaded, but now wedged.
	body := `{"workspace_id":"ws-online-wedged","error_rate":0.0,"sample_error":"SDK init timeout — restart workspace","active_tasks":0,"uptime_seconds":60,"runtime_state":"wedged","mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (online wedged must demote to degraded): %v", err)
	}
}
