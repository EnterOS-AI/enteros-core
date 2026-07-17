package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
)

// a2aWarmingTestHandler builds a WorkspaceHandler + sqlmock for the gate tests.
func a2aWarmingTestHandler(t *testing.T) (*WorkspaceHandler, sqlmock.Sqlmock) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	return NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()), mock
}

// TestConciergeWarmingGate_PlatformProvisioning_Returns503 proves the gate
// defers a real caller to a warming (kind=platform, status=provisioning)
// concierge with 503 + Retry-After and a {warming:true} body (canvas auto-retry).
func TestConciergeWarmingGate_PlatformProvisioning_Returns503(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-warming").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow(models.KindPlatform, string(models.StatusProvisioning)))

	proxyErr := h.conciergeWarmingGate(context.Background(), "ws-warming")
	if proxyErr == nil {
		t.Fatalf("expected a 503 deferral for a warming platform concierge, got nil")
	}
	if proxyErr.Status != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", proxyErr.Status)
	}
	if proxyErr.Headers["Retry-After"] == "" {
		t.Errorf("expected a Retry-After header, got none")
	}
	if proxyErr.Response["warming"] != true {
		t.Errorf("expected warming=true in body, got %v", proxyErr.Response)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmingGate_PlatformOnline_NoDefer: an already-online concierge is
// verified-ready, so the gate must NOT defer.
func TestConciergeWarmingGate_PlatformOnline_NoDefer(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-online").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow(models.KindPlatform, string(models.StatusOnline)))

	if proxyErr := h.conciergeWarmingGate(context.Background(), "ws-online"); proxyErr != nil {
		t.Fatalf("expected no deferral for an online concierge, got status %d", proxyErr.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmingGate_NonPlatformProvisioning_NoDefer: a regular workspace
// still booting is out of scope — the gate only governs platform concierges.
func TestConciergeWarmingGate_NonPlatformProvisioning_NoDefer(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-regular").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow(models.KindWorkspace, string(models.StatusProvisioning)))

	if proxyErr := h.conciergeWarmingGate(context.Background(), "ws-regular"); proxyErr != nil {
		t.Fatalf("expected no deferral for a non-platform workspace, got status %d", proxyErr.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestConciergeWarmingGate_NoRow_FailsOpen: a missing row must fail OPEN (nil) so
// resolveAgentURL emits the canonical 404 rather than the gate masking it.
func TestConciergeWarmingGate_NoRow_FailsOpen(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-missing").
		WillReturnError(sql.ErrNoRows)

	if proxyErr := h.conciergeWarmingGate(context.Background(), "ws-missing"); proxyErr != nil {
		t.Fatalf("expected fail-open (nil) on no row, got status %d", proxyErr.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_CanvasUser_WarmingConcierge_Deferred is the requirement: a user's
// (canvas, callerID="") first message to a still-warming platform concierge is
// NOT silently poll-dropped/hung — it is deferred with 503 + Retry-After. The
// gate runs AFTER the budget check and BEFORE persist (no half-persisted bubble).
func TestProxyA2A_CanvasUser_WarmingConcierge_Deferred(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)

	expectBudgetCheck(mock, "ws-warming-target")
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-warming-target").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow(models.KindPlatform, string(models.StatusProvisioning)))

	body := []byte(`{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"m1","parts":[{"kind":"text","text":"hi"}]}}}`)
	status, resp, proxyErr := h.proxyA2ARequest(context.Background(), "ws-warming-target", body, "", false, false)

	if proxyErr == nil {
		t.Fatalf("expected a 503 warming deferral, got status=%d resp=%s", status, resp)
	}
	if proxyErr.Status != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", proxyErr.Status)
	}
	if proxyErr.Headers["Retry-After"] == "" {
		t.Errorf("expected Retry-After header on the warming deferral")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestProxyA2A_SystemWarmupCaller_NotDeferred proves the call-site exemption: a
// system-prefixed self-turn (callerID "system:…") MUST reach the runtime DURING
// warming, so it is NOT deferred by the warming gate — and, being a system caller,
// it is NOT persisted as a user bubble. (EV2 retired the fireConciergeWarmup
// readiness turn that used to be the canonical such caller; the generic
// system-caller exemption remains, keyed on isSystemCaller, not on the warmup.)
// Here the target is poll-mode: the call reaches the poll-queue (200 queued),
// proving neither the warming gate (SELECT kind,status) nor the ingest persist
// (SELECT name / INSERT activity_logs) fired — the only reads are budget +
// delivery-mode.
func TestProxyA2A_SystemWarmupCaller_NotDeferred(t *testing.T) {
	h, mock := a2aWarmingTestHandler(t)

	expectBudgetCheck(mock, "ws-warming-target")
	// No SELECT kind,status (gate skipped for system caller) and no persist
	// SELECT name / INSERT activity_logs (skipped for system caller) — the next
	// read is delivery-mode.
	mock.ExpectQuery("SELECT delivery_mode FROM workspaces WHERE id =").
		WithArgs("ws-warming-target").
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow(string(models.DeliveryModePoll)))

	body := []byte(`{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"warmup-1","parts":[{"kind":"text","text":"Platform readiness check"}]}}}`)
	status, resp, proxyErr := h.proxyA2ARequest(context.Background(), "ws-warming-target", body, "system:concierge-warmup", false, false)

	if proxyErr != nil {
		t.Fatalf("warmup system caller must NOT be deferred, got status %d", proxyErr.Status)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200 queued for the poll-mode warmup, got %d", status)
	}
	if !bytes.Contains(resp, []byte("queued")) {
		t.Errorf("expected a poll-mode queued response, got %s", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
