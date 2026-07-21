package handlers

// admin_plugin_drift_test.go — coverage for plugin drift queue admin endpoints.
// Tests: ListPending (empty, non-empty), Apply (not found, already applied,
// already dismissed, workspace_plugins missing, install failure).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestAdminPluginDrift_ListPending_Empty(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT id, workspace_id, plugin_name, tracked_ref,\s+current_sha, latest_sha, status, created_at\s+FROM plugin_update_queue\s+WHERE status = 'pending'\s+ORDER BY created_at DESC`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "plugin_name", "tracked_ref",
			"current_sha", "latest_sha", "status", "created_at",
		}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %d rows", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_ListPending_WithRows(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	now := time.Now()
	mock.ExpectQuery(`SELECT id, workspace_id, plugin_name, tracked_ref,\s+current_sha, latest_sha, status, created_at\s+FROM plugin_update_queue\s+WHERE status = 'pending'\s+ORDER BY created_at DESC`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "plugin_name", "tracked_ref",
			"current_sha", "latest_sha", "status", "created_at",
		}).AddRow(
			"queue-id-1", "ws-uuid-1", "my-plugin", "tag:v1.0.0",
			"abc123def456", "def456abc789", "pending", now,
		).AddRow(
			"queue-id-2", "ws-uuid-2", "other-plugin", "tag:latest",
			"111111aaaaaa", "222222bbbbbb", "pending", now,
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
	// Verify first row fields.
	if got := rows[0]["plugin_name"]; got != "my-plugin" {
		t.Errorf("plugin_name: expected my-plugin, got %v", got)
	}
	if got := rows[0]["status"]; got != "pending" {
		t.Errorf("status: expected pending, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_ListPending_DBError(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT id, workspace_id`).WillReturnError(
		json.Unmarshal([]byte("force error"), new(struct{})),
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/admin/plugin-updates-pending", nil)
	h.ListPending(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on DB error, got %d", w.Code)
	}
}

func TestAdminPluginDrift_Apply_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("nonexistent-queue-id").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		})) // empty = no rows

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/nonexistent-queue-id/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "nonexistent-queue-id"}}
	h.Apply(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAdminPluginDrift_Apply_AlreadyApplied(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "applied"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for already-applied, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if got := body["status"]; got != "already_applied" {
		t.Errorf("status: expected already_applied, got %v", got)
	}
}

func TestAdminPluginDrift_Apply_AlreadyDismissed(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "dismissed"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for dismissed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminPluginDrift_Apply_WorkspacePluginsMissing(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(nil)

	// Queue entry found, pending.
	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue\s+WHERE id = \$1`).
		WithArgs("queue-id-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "plugin_name", "tracked_ref", "status",
		}).AddRow("ws-uuid-1", "my-plugin", "tag:v1.0.0", "pending"))

	// workspace_plugins row not found (plugin uninstalled after drift detected).
	mock.ExpectQuery(`SELECT source_raw FROM workspace_plugins\s+WHERE workspace_id = \$1 AND plugin_name = \$2`).
		WithArgs("ws-uuid-1", "my-plugin").
		WillReturnRows(sqlmock.NewRows([]string{"source_raw"})) // empty

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/queue-id-1/apply", nil)
	c.Params = []gin.Param{{Key: "id", Value: "queue-id-1"}}
	h.Apply(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when workspace_plugins row missing, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// restartSpy is a concurrency-safe recorder for the pluginsHandler restartFunc.
type restartSpy struct {
	mu    sync.Mutex
	calls []string
}

func (s *restartSpy) fn(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, id)
}

func (s *restartSpy) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestAdminPluginDrift_Apply_ConciergeSelfRestartDeferred is the SELF-BRICK
// guard: an Apply whose target is the kind=platform concierge in its fragile
// lifecycle window (online) must NOT dispatch an immediate unconditional
// restart — the concierge's own auto-update cron could otherwise reboot the
// org-root box into a brick on a bad ref, mid-batch, with no health probe.
//
// Negative control: the pre-fix code restarted UNCONDITIONALLY in step 5. Point
// this test at that path (drop the platformConciergeReconcileShouldSkipRestart
// guard from applyRestartAfterDrift) and restarting==true / spy.calls==[wsID],
// so the assertions below fail — proving the guard is what suppresses the
// restart, not some incidental nil.
func TestAdminPluginDrift_Apply_ConciergeSelfRestartDeferred(t *testing.T) {
	mock := setupTestDB(t)

	spy := &restartSpy{}
	ph := NewPluginsHandler(t.TempDir(), nil, spy.fn)
	h := NewAdminPluginDriftHandler(ph)

	const wsID = "concierge-ws-uuid"
	// The guard queries workspaces.kind/status: platform + online => defer.
	mock.ExpectQuery(`SELECT kind, status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow("platform", "online"))

	restarting := h.applyRestartAfterDrift(context.Background(), wsID)

	// Drain any detached restart goroutine so a late call can't slip past the
	// assertion (there should be none).
	waitGlobalAsyncForTest()

	if restarting {
		t.Errorf("expected restarting=false for platform concierge (self-brick guard), got true")
	}
	if calls := spy.snapshot(); len(calls) != 0 {
		t.Errorf("expected NO restart dispatched for platform concierge, got %v", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestAdminPluginDrift_Apply_NonConciergeStillRestarts proves auto-apply is
// preserved: a normal (kind=workspace) target still gets its immediate re-pin +
// restart. Negative control for the guard direction — if the guard over-fired
// and suppressed non-concierge restarts, restarting would be false and
// spy.calls empty, failing here.
func TestAdminPluginDrift_Apply_NonConciergeStillRestarts(t *testing.T) {
	mock := setupTestDB(t)

	spy := &restartSpy{}
	ph := NewPluginsHandler(t.TempDir(), nil, spy.fn)
	h := NewAdminPluginDriftHandler(ph)

	const wsID = "tenant-ws-uuid"
	mock.ExpectQuery(`SELECT kind, status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow("workspace", "online"))

	restarting := h.applyRestartAfterDrift(context.Background(), wsID)

	// The restart is dispatched on a detached globalGoAsync goroutine; wait for it.
	waitGlobalAsyncForTest()

	if !restarting {
		t.Errorf("expected restarting=true for non-concierge workspace (auto-apply preserved), got false")
	}
	calls := spy.snapshot()
	if len(calls) != 1 || calls[0] != wsID {
		t.Errorf("expected exactly one restart of %q, got %v", wsID, calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
