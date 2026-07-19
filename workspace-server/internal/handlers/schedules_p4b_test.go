package handlers

// schedules_p4b_test.go — the P4b readiness audit + fleet migrate-all tooling
// (scheduler-as-trigger-plugin RFC, issue #4411). These make the irreversible
// DROP-TABLE precondition measurable (readiness) and executable (migrate-all,
// dry-run by default) without ever deleting a workspace_schedules row.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// createCapableStub is a runtime /internal/schedules stub that also answers the
// create POST (201) — the shared volumeRuntimeStub only handles GET + /run. It
// records the create bodies so a test can assert exactly which entries migrated.
type createCapableStub struct {
	mu      sync.Mutex
	grid    string
	creates []string // "name" of each POST /internal/schedules body
	srv     *httptest.Server
}

func newCreateCapableStub(t *testing.T, grid string) *createCapableStub {
	t.Helper()
	s := &createCapableStub{grid: grid}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules":
			fmt.Fprint(w, s.grid)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/schedules":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			if n, ok := body["name"].(string); ok {
				s.creates = append(s.creates, n)
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"name":"ok"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such route"}`)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *createCapableStub) created() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.creates...)
}

func adminCtx(t *testing.T, method, target string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, nil)
	return w, c
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v — body=%s", err, w.Body.String())
	}
	return m
}

// ==================== P4b readiness ====================

// expectReadinessCounts stubs the two fleet count queries: the per-workspace
// grouping and the orphan count.
func expectReadinessCounts(mock sqlmock.Sqlmock, group *sqlmock.Rows, orphans int) {
	mock.ExpectQuery("SELECT s.workspace_id, s.source, COUNT").WillReturnRows(group)
	mock.ExpectQuery("LEFT JOIN workspaces w").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(orphans))
}

func TestP4bReadiness_Droppable_WhenRuntimeRowsAllOnVolume(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0001-4000-8000-000000000001"
	markVolumeScheduler(t, wsID)

	stub := newCreateCapableStub(t, `{"schedules":[
		{"name":"a","cron":"*/5 * * * *","timezone":"UTC","prompt":"p","enabled":true},
		{"name":"b","cron":"*/5 * * * *","timezone":"UTC","prompt":"p","enabled":true}
	]}`)

	expectReadinessCounts(mock,
		sqlmock.NewRows([]string{"workspace_id", "source", "count"}).AddRow(wsID, "runtime", 2), 0)
	mock.ExpectQuery("SELECT name, source FROM workspace_schedules").WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "source"}).
			AddRow("a", "runtime").AddRow("b", "runtime"))
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "sec-1")

	w, ctx := adminCtx(t, "GET", "/admin/schedules/p4b-readiness")
	handler.P4bReadiness(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decodeJSON(t, w)
	if m["droppable"] != true {
		t.Errorf("expected droppable=true, got %v (body=%s)", m["droppable"], w.Body.String())
	}
	if m["workspaces_needing_migration"].(float64) != 0 || m["not_volume_native"].(float64) != 0 {
		t.Errorf("expected zero blockers, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestP4bReadiness_BlocksOnUnmigratedRuntimeRows(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0002-4000-8000-000000000002"
	markVolumeScheduler(t, wsID)

	// Volume grid is EMPTY → the two runtime rows are unmigrated.
	stub := newCreateCapableStub(t, `{"schedules":[]}`)

	expectReadinessCounts(mock,
		sqlmock.NewRows([]string{"workspace_id", "source", "count"}).AddRow(wsID, "runtime", 2), 0)
	mock.ExpectQuery("SELECT name, source FROM workspace_schedules").WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "source"}).
			AddRow("a", "runtime").AddRow("b", "runtime"))
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "sec-2")

	w, ctx := adminCtx(t, "GET", "/admin/schedules/p4b-readiness")
	handler.P4bReadiness(ctx)

	m := decodeJSON(t, w)
	if m["droppable"] != false {
		t.Errorf("expected droppable=false with unmigrated rows, got %s", w.Body.String())
	}
	if m["workspaces_needing_migration"].(float64) != 1 {
		t.Errorf("expected 1 workspace needing migration, got %s", w.Body.String())
	}
	wss := m["workspaces"].([]interface{})
	row := wss[0].(map[string]interface{})
	if row["blocking"] != true || row["runtime_rows_unsynced"].(float64) != 2 {
		t.Errorf("expected blocking with 2 unsynced, got %v", row)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestP4bReadiness_BlocksOnNotVolumeNative(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0003-4000-8000-000000000003"
	// NOT marked volume-native → no runtime fan-out happens (DB path still owns it).

	expectReadinessCounts(mock,
		sqlmock.NewRows([]string{"workspace_id", "source", "count"}).AddRow(wsID, "template", 1), 0)

	w, ctx := adminCtx(t, "GET", "/admin/schedules/p4b-readiness")
	handler.P4bReadiness(ctx)

	m := decodeJSON(t, w)
	if m["droppable"] != false || m["not_volume_native"].(float64) != 1 {
		t.Errorf("expected droppable=false, not_volume_native=1, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestP4bReadiness_BlocksOnUnseededTemplateRows(t *testing.T) {
	// The DROP removes template rows too — a template row absent from the volume
	// is a data-loss blocker that migrate-all can NOT fix (needs a reprovision).
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0006-4000-8000-000000000006"
	markVolumeScheduler(t, wsID)

	stub := newCreateCapableStub(t, `{"schedules":[]}`) // grid empty → template row missing

	expectReadinessCounts(mock,
		sqlmock.NewRows([]string{"workspace_id", "source", "count"}).AddRow(wsID, "template", 1), 0)
	mock.ExpectQuery("SELECT name, source FROM workspace_schedules").WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "source"}).AddRow("nightly", "template"))
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "sec-6")

	w, ctx := adminCtx(t, "GET", "/admin/schedules/p4b-readiness")
	handler.P4bReadiness(ctx)

	m := decodeJSON(t, w)
	if m["droppable"] != false || m["workspaces_needing_reseed"].(float64) != 1 {
		t.Errorf("expected droppable=false, needing_reseed=1, got %s", w.Body.String())
	}
	row := m["workspaces"].([]interface{})[0].(map[string]interface{})
	if row["template_rows_unsynced"].(float64) != 1 {
		t.Errorf("expected template_rows_unsynced=1, got %v", row)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ==================== migrate-all-to-volume ====================

func TestMigrateAllToVolume_DryRun_CountsWithoutPosting(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0004-4000-8000-000000000004"
	markVolumeScheduler(t, wsID) // puts wsID into volumeSchedulerWorkspaceIDs()

	stub := newCreateCapableStub(t, `{"schedules":[]}`) // empty grid → both would migrate

	// Order in migrateWorkspaceRuntimeToVolume: resolve URL+secret, GET grid, SELECT rows.
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "sec-4")
	mock.ExpectQuery("SELECT name, cron_expr, timezone, prompt, enabled").WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "cron_expr", "timezone", "prompt", "enabled"}).
			AddRow("a", "*/5 * * * *", "UTC", "p", true).
			AddRow("b", "*/5 * * * *", "UTC", "p", true))

	w, ctx := adminCtx(t, "POST", "/admin/schedules/migrate-all-to-volume") // no ?apply → dry-run
	handler.MigrateAllToVolume(ctx)

	m := decodeJSON(t, w)
	if m["apply"] != false {
		t.Errorf("expected apply=false, got %v", m["apply"])
	}
	if m["total_migrated"].(float64) != 2 {
		t.Errorf("dry-run should report 2 WOULD-migrate, got %s", w.Body.String())
	}
	if got := stub.created(); len(got) != 0 {
		t.Errorf("dry-run must NOT POST any create, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestMigrateAllToVolume_Apply_PostsMissingRows(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	handler := NewScheduleHandler()
	wsID := "22e59f01-0005-4000-8000-000000000005"
	markVolumeScheduler(t, wsID)

	// Grid already has "a"; "b" is missing → only "b" is POSTed (idempotency).
	stub := newCreateCapableStub(t, `{"schedules":[
		{"name":"a","cron":"*/5 * * * *","timezone":"UTC","prompt":"p","enabled":true}
	]}`)

	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "sec-5")
	mock.ExpectQuery("SELECT name, cron_expr, timezone, prompt, enabled").WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "cron_expr", "timezone", "prompt", "enabled"}).
			AddRow("a", "*/5 * * * *", "UTC", "p", true).
			AddRow("b", "*/5 * * * *", "UTC", "p", true))

	w, ctx := adminCtx(t, "POST", "/admin/schedules/migrate-all-to-volume?apply=true")
	handler.MigrateAllToVolume(ctx)

	m := decodeJSON(t, w)
	if m["apply"] != true {
		t.Errorf("expected apply=true, got %v", m["apply"])
	}
	if m["total_migrated"].(float64) != 1 || m["total_skipped"].(float64) != 1 {
		t.Errorf("expected 1 migrated + 1 skipped (idempotent), got %s", w.Body.String())
	}
	if got := stub.created(); len(got) != 1 || got[0] != "b" {
		t.Errorf("apply must POST exactly the missing entry 'b', got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}
