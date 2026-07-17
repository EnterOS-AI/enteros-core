package handlers

// schedules_inheritance_restore_test.go — the RESTORE half of the P4b volume-side
// org-re-import schedule inheritance path (core#4435).
// ScheduleHandler.RestoreInheritedRuntimeSchedules replays a removed predecessor's
// captured runtime grid onto a freshly-online successor and clears the buffer
// one-shot.
//
// NEGATIVE CONTROL: every test here fails on pre-change main (verified by stubbing
// RestoreInheritedRuntimeSchedules to a no-op body and re-running) — a no-op never
// POSTs the carried schedules, never arms the daemon, and never clears the buffer,
// so the POST/arm assertions and the sqlmock clear-UPDATE expectation all fail.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// inheritanceRuntimeStub is a minimal successor /internal/schedules* runtime API
// that serves a configurable present grid, records every create POST (name +
// source), and records the daemon-arm reload poke.
type inheritanceRuntimeStub struct {
	mu       sync.Mutex
	grid     string
	names    []string
	sources  []string
	reloaded bool
	requests []string
	srv      *httptest.Server
}

func newInheritanceRuntimeStub(t *testing.T, grid string) *inheritanceRuntimeStub {
	t.Helper()
	s := &inheritanceRuntimeStub{grid: grid}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules":
			fmt.Fprint(w, s.grid)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/schedules":
			var e volumeEntry
			_ = json.NewDecoder(r.Body).Decode(&e)
			s.mu.Lock()
			s.names = append(s.names, e.Name)
			s.sources = append(s.sources, e.Source)
			s.mu.Unlock()
			e.Source = "runtime" // runtime create() re-stamps source
			w.WriteHeader(http.StatusCreated)
			b, _ := json.Marshal(e)
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/daemons/reload":
			s.mu.Lock()
			s.reloaded = true
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "{}")
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such route"}`)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *inheritanceRuntimeStub) createdNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func (s *inheritanceRuntimeStub) createdSources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sources...)
}

func (s *inheritanceRuntimeStub) armed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloaded
}

// carriedAB is the captured buffer JSON for two runtime schedules A, B — exactly
// the shape captureRuntimeSchedulesForCarryover writes.
const carriedAB = `[` +
	`{"name":"A","cron":"*/30 * * * *","timezone":"UTC","prompt":"pa","enabled":true,"source":"runtime"},` +
	`{"name":"B","cron":"0 9 * * *","timezone":"UTC","prompt":"pb","enabled":false,"source":"runtime"}` +
	`]`

// expectIdentity stubs the (a) role/name/parent_id load for the successor.
func expectIdentity(mock sqlmock.Sqlmock, newID, role, name string) {
	mock.ExpectQuery(`SELECT role, name, parent_id FROM workspaces WHERE id = \$1`).
		WithArgs(newID).
		WillReturnRows(sqlmock.NewRows([]string{"role", "name", "parent_id"}).AddRow(role, name, nil))
}

// TestRestore_PostsCarriedSchedules_ThenClears — carried [A,B] + a volume-native
// successor with an empty grid: both A and B are POSTed to the successor volume
// with source=runtime, the daemon is armed, and the buffer is cleared (one-shot).
func TestRestore_PostsCarriedSchedules_ThenClears(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	newID := "33e59f01-0001-4000-8000-000000000001"
	predID := "33e59f01-00de-4000-8000-0000000000de"
	markVolumeScheduler(t, newID)

	stub := newInheritanceRuntimeStub(t, `{"schedules":[]}`) // empty → both A,B posted

	expectIdentity(mock, newID, "eng-agent", "Engineer")
	mock.ExpectQuery(`SELECT id, carryover_runtime_schedules FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "carryover_runtime_schedules"}).
			AddRow(predID, []byte(carriedAB)))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectFanoutURL(mock, newID, stub.srv.URL) // resolveScheduleFanoutTarget url
	expectInboundSecret(mock, newID, "restore-secret-1")
	expectFanoutURL(mock, newID, stub.srv.URL) // armSchedulerPlugin url (secret cached)
	mock.ExpectExec(`UPDATE workspaces SET carryover_runtime_schedules = NULL`).
		WithArgs(predID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.RestoreInheritedRuntimeSchedules(context.Background(), newID)

	if got := stub.createdNames(); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("want POST A then B, got %v", got)
	}
	for _, src := range stub.createdSources() {
		if src != "runtime" {
			t.Errorf("carried entry must be POSTed with source=runtime, got %q", src)
		}
	}
	if !stub.armed() {
		t.Errorf("daemon must be armed (POST /internal/daemons/reload) so carried schedules fire promptly")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met (buffer must be cleared on success): %v", err)
	}
}

// TestRestore_SkipsAlreadyPresentName_NoDup — carried [A,B] but A is already on
// the successor grid: only B is POSTed (no duplicate A), yet the buffer still
// clears (a present-name is a success, not a failure). This is the template-name-
// collision behavior preserved from the DB world (an existing name wins).
func TestRestore_SkipsAlreadyPresentName_NoDup(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	newID := "33e59f01-0002-4000-8000-000000000002"
	predID := "33e59f01-00d2-4000-8000-0000000000d2"
	markVolumeScheduler(t, newID)

	// A already present (e.g. re-seeded by the template) → must NOT be re-POSTed.
	stub := newInheritanceRuntimeStub(t,
		`{"schedules":[{"name":"A","cron":"*/30 * * * *","timezone":"UTC","prompt":"pa","enabled":true,"source":"template"}]}`)

	expectIdentity(mock, newID, "eng-agent", "Engineer")
	mock.ExpectQuery(`SELECT id, carryover_runtime_schedules FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "carryover_runtime_schedules"}).
			AddRow(predID, []byte(carriedAB)))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectFanoutURL(mock, newID, stub.srv.URL)
	expectInboundSecret(mock, newID, "restore-secret-2")
	expectFanoutURL(mock, newID, stub.srv.URL)
	mock.ExpectExec(`UPDATE workspaces SET carryover_runtime_schedules = NULL`).
		WithArgs(predID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.RestoreInheritedRuntimeSchedules(context.Background(), newID)

	if got := stub.createdNames(); len(got) != 1 || got[0] != "B" {
		t.Errorf("only B must be POSTed (A already present, no dup), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met (present-name skip is still a success → clear): %v", err)
	}
}

// TestRestore_NotYetVolumeNative_DefersWithoutClearing — a successor whose
// scheduler capability is not yet advertised (scheduleBackendIsVolume==false):
// the plugin is declared (so the daemon installs), but the restore DEFERS —
// contacting no runtime and, crucially, NOT clearing the buffer — so the next
// transition-to-online retries and converges.
func TestRestore_NotYetVolumeNative_DefersWithoutClearing(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewScheduleHandler()

	newID := "33e59f01-0003-4000-8000-000000000003"
	predID := "33e59f01-00d3-4000-8000-0000000000d3"
	// Intentionally NOT markVolumeScheduler → scheduleBackendIsVolume == false.

	expectIdentity(mock, newID, "eng-agent", "Engineer")
	mock.ExpectQuery(`SELECT id, carryover_runtime_schedules FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "carryover_runtime_schedules"}).
			AddRow(predID, []byte(carriedAB)))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No resolve, no clear: any such query would fail the test — proving the
	// buffer is NOT cleared (retry path) and no runtime is contacted.

	h.RestoreInheritedRuntimeSchedules(context.Background(), newID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("must defer (declare only, no runtime, no clear) when not yet volume-native: %v", err)
	}
}

// TestRestore_NoPredecessor_CleanNoOp — no removed predecessor carries a buffer:
// the predecessor lookup returns ErrNoRows and the function returns BEFORE
// declaring the plugin, contacting a runtime, or writing anything.
func TestRestore_NoPredecessor_CleanNoOp(t *testing.T) {
	mock := setupTestDB(t)
	h := NewScheduleHandler()

	newID := "33e59f01-0004-4000-8000-000000000004"

	expectIdentity(mock, newID, "eng-agent", "Engineer")
	mock.ExpectQuery(`SELECT id, carryover_runtime_schedules FROM workspaces`).
		WillReturnError(sql.ErrNoRows)
	// No INSERT, no resolve, no clear — all downstream of the ErrNoRows return.

	h.RestoreInheritedRuntimeSchedules(context.Background(), newID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no-predecessor path must be a clean no-op: %v", err)
	}
}
