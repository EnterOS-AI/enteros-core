//go:build integration
// +build integration

// registry_dormant_status_integration_test.go — REAL Postgres regression proof
// for core#2332: a deliberately-dormant workspace (status 'paused' or
// 'hibernated') MUST survive a lingering /registry/register from its own
// just-stopped runtime container without being resurrected to 'online'.
//
// WHY THIS EXISTS — the flaky workspace-lifecycle e2e-smoke HARD GATE.
// Pause/Hibernate genuinely STOP the workspace container, but the stop is not
// instantaneous, and a just-preceding Restart re-provisions a fresh container
// that boots + registers a few seconds later. That doomed/late container fires
// one more POST /registry/register AFTER the row was parked. The Register
// upsert's non-platform arm forces status→'online' (it only guarded the
// 'removed' state), so the register clobbered a 'paused' row back to 'online'
// with url repopulated. The e2e then saw the classic pair:
//
//	resume    → HTTP 404 {"error":"workspace not found or not paused"}
//	hibernate → HTTP 404 {"error":"...not in a hibernatable state..."}
//
// because by resume/hibernate time the row was 'online', not 'paused'.
//
// The unit tests (sqlmock) cannot catch this — they don't run the real upsert
// SQL, so the CASE/WHERE clause is never actually evaluated against a row. This
// integration test executes the true handler against a real Postgres and pins
// the OBSERVABLE row state: dormant stays dormant; provisioning still promotes.
//
// Run with (same harness as the other *_integration_test.go in this package):
//
//	INTEGRATION_DB_URL="postgres://postgres:test@HOST:5432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ \
//	  -run Integration_Register_Dormant -v

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

func integrationDB_DormantRegister(t *testing.T) *sql.DB {
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
	clean := func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'integ-dormant-%'`)
	}
	clean()
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		clean()
		db.DB = prev
		conn.Close()
	})
	return conn
}

func seedDormantWorkspace(t *testing.T, conn *sql.DB, id, status string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, kind) VALUES ($1, $2, $3::workspace_status, 'workspace')`,
		id, "integ-dormant-"+id, status); err != nil {
		t.Fatalf("seed workspace (status=%s): %v", status, err)
	}
}

// callRegister drives the real Register handler with a minimal valid payload.
// The workspace is seeded WITHOUT any live auth token, so requireWorkspaceToken
// takes the bootstrap-allowed path (no bearer required) — auth is orthogonal to
// the status-clobber this test pins.
func callRegister(t *testing.T, h *RegistryHandler, id, url string) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// agent_card carries NO url (so the write-time isSafeURL(agent_card.url)
	// gate is skipped — same shape the passing register unit tests use); the
	// top-level url is a loopback port, which the register accepts. What we are
	// exercising is the UPSERT's status guard, not URL validation.
	body := fmt.Sprintf(
		`{"id":%q,"url":%q,"agent_card":{"name":"integ-dormant","version":"1.0"}}`,
		id, url)
	c.Request = httptest.NewRequest("POST", "/registry/register", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Register(c)
	return w.Code
}

// statusOf is defined in registry_auth_integration_test.go (same package).

// TestIntegration_Register_Dormant_StatusInviolable proves the fix: a
// /registry/register landing on a paused or hibernated non-platform workspace
// is a no-op on its status (it is NOT resurrected to 'online'). Fails BEFORE
// the fix (row flips to 'online'); passes AFTER.
func TestIntegration_Register_Dormant_StatusInviolable(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	for _, dormant := range []string{"paused", "hibernated"} {
		dormant := dormant
		t.Run(dormant, func(t *testing.T) {
			id := uuid.NewString()
			seedDormantWorkspace(t, conn, id, dormant)

			code := callRegister(t, h, id, "http://localhost:9100")

			// The load-bearing assertion is the ROW status, not the HTTP code
			// (a bootstrap register legitimately returns 200 while still being a
			// status no-op under the fixed guard).
			if got := statusOf(t, conn, id); got != dormant {
				t.Fatalf("register RESURRECTED a %s workspace to %q (want %q) — a deliberately-dormant row must be inviolable to a lingering container re-register [register http=%d]",
					dormant, got, dormant, code)
			}
		})
	}
}

// TestIntegration_Register_PromotesProvisioning is the positive control: the
// dormant guard must NOT break the legitimate provisioning→online promotion
// that fresh create + Resume/Wake relaunch depend on (their re-provisioned
// container registers while the row is 'provisioning').
func TestIntegration_Register_PromotesProvisioning(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedDormantWorkspace(t, conn, id, "provisioning")

	callRegister(t, h, id, "http://localhost:9100")

	if got := statusOf(t, conn, id); got != "online" {
		t.Fatalf("register did not promote provisioning→online (got %q) — the dormant guard over-restricted the WHERE clause and broke normal relaunch", got)
	}
}
