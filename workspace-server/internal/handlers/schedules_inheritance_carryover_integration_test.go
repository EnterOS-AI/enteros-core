//go:build integration
// +build integration

// schedules_inheritance_carryover_integration_test.go — REAL Postgres integration
// tests for the P4b volume-side schedule-inheritance buffer (core#4435 / PR #4453).
//
// WHY THIS EXISTS (review F1): the capture/clear/restore JSONB writes reuse the
// registry.go loaded_mcp_tools `$1::jsonb` idiom, but the unit tests exercise them
// against sqlmock (which does not type-check SQL, evaluate the ::jsonb cast, or run
// the `IS NOT DISTINCT FROM` / `IS NOT NULL` predecessor-match predicate). These
// tests pin the OBSERVABLE real-DB row state end-to-end:
//   - CAPTURE writes carryover_runtime_schedules as JSONB holding EXACTLY the
//     source='runtime' grid entries (template excluded) — the ::jsonb write really
//     round-trips on Postgres.
//   - RESTORE's removed-predecessor SELECT actually matches on real Postgres (role
//     + null-safe parent + `carryover IS NOT NULL`), reposts the carried entries,
//     and clearCarryover NULLs the column (one-shot).
//
// The workspace RUNTIME (its /internal/schedules API) is an external service, so it
// is stubbed at the HTTP layer here — legitimate: F1's concern is the CORE DB
// writes, not the runtime. A full LIVE volume-native delete→re-import soak against a
// real trigger-plugin runtime is still required before P4b retires the DB-world
// path; see the PR #4453 soak plan and the dark-code caution in workspace_crud.go.
//
// Run with (mirrors admin_schedules_health_integration_test.go):
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	# apply every migrations/*.sql (non-down) in lexicographic order, then:
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_CarryoverSchedules -v

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"

	_ "github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// integrationDB_Carryover opens INTEGRATION_DB_URL, wipes any leftover fixture
// rows (marker name prefix), hot-swaps db.DB to the real connection for the test
// duration, and restores + re-wipes on cleanup.
func integrationDB_Carryover(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	wipeCarryover(t, conn)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		wipeCarryover(t, conn)
		db.DB = prev
		conn.Close()
	})
	return conn
}

// wipeCarryover clears fixture rows. declared_plugins first (FK on workspace_id),
// then the workspaces themselves.
func wipeCarryover(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspace_declared_plugins WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'integ-carry-%')`); err != nil {
		t.Fatalf("cleanup declared_plugins: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE name LIKE 'integ-carry-%'`); err != nil {
		t.Fatalf("cleanup workspaces: %v", err)
	}
}

// seedCarryoverWorkspace inserts a workspace row directly. parent_id is left NULL
// (both predecessor and successor share role + a NULL parent so the restore
// predecessor-match's `parent_id IS NOT DISTINCT FROM $2` binds them).
func seedCarryoverWorkspace(t *testing.T, conn *sql.DB, id, name, status, role, url, secret string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, role, url, platform_inbound_secret)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, name, status, role, url, secret); err != nil {
		t.Fatalf("seed workspace %s: %v", name, err)
	}
}

// readCarryover returns the raw carryover_runtime_schedules JSONB (nil when NULL).
func readCarryover(t *testing.T, conn *sql.DB, id string) []byte {
	t.Helper()
	var raw []byte
	if err := conn.QueryRowContext(context.Background(),
		`SELECT carryover_runtime_schedules FROM workspaces WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("read carryover for %s: %v", id, err)
	}
	return raw
}

// carryoverStub is a minimal volume-runtime /internal/schedules API: it serves a
// configured grid on GET, accepts creates on POST (recording the names), and 200s
// the daemon-reload (arm) poke.
type carryoverStub struct {
	mu      sync.Mutex
	grid    string
	created []string
	srv     *httptest.Server
}

func newCarryoverStub(t *testing.T, grid string) *carryoverStub {
	t.Helper()
	s := &carryoverStub{grid: grid}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/schedules":
			fmt.Fprint(w, s.grid)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/schedules":
			body, _ := io.ReadAll(r.Body)
			var e struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &e)
			s.created = append(s.created, e.Name)
			w.WriteHeader(http.StatusCreated)
			w.Write(body) // echo the definition back (createVolume-style)
		case r.Method == http.MethodPost && r.URL.Path == "/internal/daemons/reload":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"status":"reloaded"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such route"}`)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *carryoverStub) createdNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.created...)
	sort.Strings(out)
	return out
}

// TestIntegration_CarryoverSchedules_CaptureWritesRealJSONB proves capture writes
// EXACTLY the source='runtime' grid entries into carryover_runtime_schedules as
// real Postgres JSONB (template entry excluded), via the ::jsonb cast.
func TestIntegration_CarryoverSchedules_CaptureWritesRealJSONB(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB_Carryover(t)

	predID := integUUID("integ-carry-cap-pred")
	markVolumeScheduler(t, predID)

	stub := newCarryoverStub(t, `{"schedules":[
		{"name":"digest A","cron":"*/30 * * * *","timezone":"UTC","prompt":"pa","enabled":true,"source":"runtime"},
		{"name":"digest B","cron":"0 9 * * *","timezone":"UTC","prompt":"pb","enabled":false,"source":"runtime"},
		{"name":"seeded T","cron":"0 0 * * *","timezone":"UTC","prompt":"pt","enabled":true,"source":"template"}
	]}`)

	// status='removed' mirrors the real post-CascadeDelete state (the row is
	// marked removed before capture runs, container still up).
	seedCarryoverWorkspace(t, conn, predID, "integ-carry-cap-pred", "removed",
		"integ-carry-cap-role", stub.srv.URL, "cap-secret")

	captureRuntimeSchedulesForCarryover(context.Background(), []string{predID})

	raw := readCarryover(t, conn, predID)
	if raw == nil {
		t.Fatal("carryover_runtime_schedules is NULL after capture — the ::jsonb write did not land")
	}
	var got []volumeEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("carryover JSONB did not decode: %v (raw=%s)", err, raw)
	}
	if len(got) != 2 {
		t.Fatalf("want exactly 2 runtime entries buffered (template excluded), got %d: %+v", len(got), got)
	}
	if got[0].Name != "digest A" || got[1].Name != "digest B" {
		t.Errorf("want [digest A, digest B] in grid order, got [%q, %q]", got[0].Name, got[1].Name)
	}
	for _, e := range got {
		if e.Source != "runtime" {
			t.Errorf("only source='runtime' entries may be captured; got %q for %q", e.Source, e.Name)
		}
	}
}

// TestIntegration_CarryoverSchedules_RestoreReadsPostsAndClears exercises the full
// delete→re-import chain against real Postgres: capture writes the buffer on the
// removed predecessor, then restore matches the predecessor via the real SQL
// predicate, reposts the carried entries to the successor, and NULLs the buffer.
func TestIntegration_CarryoverSchedules_RestoreReadsPostsAndClears(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB_Carryover(t)

	const role = "integ-carry-rt-role"
	predID := integUUID("integ-carry-rt-pred")
	succID := integUUID("integ-carry-rt-succ")
	markVolumeScheduler(t, predID)
	markVolumeScheduler(t, succID)

	// Predecessor: removed, its grid has two runtime entries to carry.
	predStub := newCarryoverStub(t, `{"schedules":[
		{"name":"digest A","cron":"*/30 * * * *","timezone":"UTC","prompt":"pa","enabled":true,"source":"runtime"},
		{"name":"digest B","cron":"0 9 * * *","timezone":"UTC","prompt":"pb","enabled":false,"source":"runtime"}
	]}`)
	seedCarryoverWorkspace(t, conn, predID, "integ-carry-rt-pred", "removed",
		role, predStub.srv.URL, "pred-secret")

	// Successor: online, SAME role + NULL parent, EMPTY grid → both carried
	// entries are absent and must be reposted.
	succStub := newCarryoverStub(t, `{"schedules":[]}`)
	seedCarryoverWorkspace(t, conn, succID, "integ-carry-rt-succ", "online",
		role, succStub.srv.URL, "succ-secret")

	// (1) Capture writes the real-DB buffer on the removed predecessor.
	captureRuntimeSchedulesForCarryover(context.Background(), []string{predID})
	if readCarryover(t, conn, predID) == nil {
		t.Fatal("precondition: capture did not populate the predecessor buffer")
	}

	// (2) Restore matches the predecessor via the real SELECT predicate, reposts
	// the carried entries onto the successor, and clears the buffer.
	h := NewScheduleHandler()
	h.RestoreInheritedRuntimeSchedules(context.Background(), succID)

	// The successor runtime received a create for each carried entry.
	if names := succStub.createdNames(); len(names) != 2 || names[0] != "digest A" || names[1] != "digest B" {
		t.Errorf("successor should have received creates for [digest A, digest B], got %v", names)
	}

	// The one-shot buffer is cleared (NULL) on the predecessor after a clean
	// restore (failed==0) — proving clearCarryover's real-DB UPDATE landed.
	if raw := readCarryover(t, conn, predID); raw != nil {
		t.Errorf("predecessor carryover buffer must be NULL after a clean restore, got: %s", raw)
	}
}
