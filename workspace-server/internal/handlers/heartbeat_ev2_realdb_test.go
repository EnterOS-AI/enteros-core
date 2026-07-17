//go:build heartbeat_ev2_realdb

// Real-Postgres proof for EV2 mcp_tools_ready — NOT sqlmock.
//
// The sqlmock suite (heartbeat_mcp_tools_ready_test.go) pins the exact SQL +
// args of the online-flip. This test proves the SAME behavior end-to-end against
// a REAL Postgres running the REAL migrations: it seeds a provisioning platform
// concierge, POSTs a heartbeat carrying mcp_tools_ready=true through the real
// handler, and asserts the persisted row actually transitions to 'online' — real
// wire types, real UPDATE ... WHERE status= semantics, real jsonb, no mock.
//
// Build-tag gated (like cmd/memory-plugin-postgres/boot_e2e_test.go) so the
// default `go test ./...` never needs a database. Run:
//
//	HEARTBEAT_EV2_DB=postgres://u:p@localhost:5544/test?sslmode=disable \
//	MOLECULE_MIGRATIONS_DIR=$(pwd)/migrations \
//	  go test -tags heartbeat_ev2_realdb -run TestHeartbeat_EV2_RealDB ./internal/handlers/
package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

func requireEV2DB(t *testing.T) string {
	dsn := os.Getenv("HEARTBEAT_EV2_DB")
	if dsn == "" {
		t.Skip("HEARTBEAT_EV2_DB not set — skipping real-Postgres EV2 online-flip test")
	}
	return dsn
}

func migrationsDirOrSkip(t *testing.T) string {
	dir := os.Getenv("MOLECULE_MIGRATIONS_DIR")
	if dir == "" {
		// Conventional location relative to internal/handlers.
		dir = "../../migrations"
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("migrations dir %q not found (%v) — set MOLECULE_MIGRATIONS_DIR", dir, err)
	}
	return dir
}

// seedProvisioningPlatformConcierge inserts a kind=platform workspace in
// 'provisioning' plus its MODEL secret so the fail-closed gate (has_model &&
// mcp_server_present) passes. Returns the workspace id.
func seedProvisioningPlatformConcierge(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	// Idempotent across reruns against a reused DB.
	db.DB.ExecContext(ctx, `DELETE FROM workspace_secrets WHERE workspace_id = $1`, id)
	db.DB.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, id)
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, kind, status) VALUES ($1, $2, 'platform', 'provisioning')`,
		id, "ev2-"+id); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO workspace_secrets (workspace_id, key, encrypted_value) VALUES ($1, 'MODEL', $2)`,
		id, []byte("dummy-encrypted-model")); err != nil {
		t.Fatalf("seed MODEL secret: %v", err)
	}
}

func currentStatus(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT status FROM workspaces WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

func postHeartbeat(t *testing.T, handler *RegistryHandler, body string) int {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.Heartbeat(c)
	return w.Code
}

// initEV2DB opens the pool + runs migrations for a real-DB EV2 test, skipping
// when HEARTBEAT_EV2_DB is unset. Shared by every real-DB EV2 case.
func initEV2DB(t *testing.T) {
	t.Helper()
	dsn := requireEV2DB(t)
	migDir := migrationsDirOrSkip(t)
	if err := db.InitPostgres(dsn); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	if err := db.RunMigrations(migDir); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
}

// setPlatformRow forces the concierge row into a given status and backdates its
// mcp_unloaded_since clock, so a single synchronous heartbeat can exercise a
// past-grace transition without sleeping real wall-clock. secondsAgo<=0 leaves
// mcp_unloaded_since NULL.
func setPlatformRow(t *testing.T, id, status string, unloadedSecondsAgo int) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1::workspace_status, updated_at = now() WHERE id = $2`,
		status, id); err != nil {
		t.Fatalf("force status %q: %v", status, err)
	}
	if unloadedSecondsAgo > 0 {
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET mcp_unloaded_since = now() - make_interval(secs => $1) WHERE id = $2`,
			unloadedSecondsAgo, id); err != nil {
			t.Fatalf("backdate mcp_unloaded_since: %v", err)
		}
	}
}

// TestHeartbeat_EV2_RealDB_MCPToolsReadyFlipsOnline is the real-beat proof.
func TestHeartbeat_EV2_RealDB_MCPToolsReadyFlipsOnline(t *testing.T) {
	dsn := requireEV2DB(t)
	migDir := migrationsDirOrSkip(t)

	if err := db.InitPostgres(dsn); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	if err := db.RunMigrations(migDir); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	// --- POSITIVE: mcp_tools_ready=true flips provisioning -> online ---
	const readyID = "11111111-1111-4111-8111-111111111111"
	seedProvisioningPlatformConcierge(t, readyID)
	t.Cleanup(func() {
		db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, readyID)
		db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, readyID)
	})

	if got := currentStatus(t, readyID); got != "provisioning" {
		t.Fatalf("precondition: expected provisioning, got %q", got)
	}

	body := `{"workspace_id":"` + readyID + `","error_rate":0.0,"uptime_seconds":60,"mcp_server_present":true,"mcp_tools_ready":true,"first_ready_at":"2026-07-17T17:00:00Z"}`
	if code := postHeartbeat(t, handler, body); code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200", code)
	}
	if got := currentStatus(t, readyID); got != "online" {
		t.Fatalf("EV2 online-flip FAILED: mcp_tools_ready=true beat left row %q, want online", got)
	}

	// --- NEGATIVE CONTROL: absent mcp_tools_ready HOLDS provisioning ---
	// Only one kind=platform root workspace may exist (uniq_workspaces_one_platform_root),
	// so retire the positive concierge before seeding the negative one.
	db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, readyID)
	db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, readyID)

	const absentID = "22222222-2222-4222-8222-222222222222"
	seedProvisioningPlatformConcierge(t, absentID)
	t.Cleanup(func() {
		db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, absentID)
		db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, absentID)
	})

	absentBody := `{"workspace_id":"` + absentID + `","error_rate":0.0,"uptime_seconds":60,"mcp_server_present":true}`
	if code := postHeartbeat(t, handler, absentBody); code != http.StatusOK {
		t.Fatalf("absent heartbeat status = %d, want 200", code)
	}
	if got := currentStatus(t, absentID); got != "provisioning" {
		t.Fatalf("negative control FAILED: absent mcp_tools_ready must HOLD provisioning, got %q", got)
	}
}

// TestHeartbeat_EV2_RealDB_OnlineStaysOnlineDespiteEmptyLoadedTools proves the
// fix for defect #2 (#4449 stuck-degrade / online<->degraded flap).
//
// An ONLINE codex/hermes concierge that AFFIRMS mcp_tools_ready=true (the reliable,
// provision-ready-by-contract signal) but whose SEPARATE, under-emitting per-turn
// loaded_mcp_tools producer (runtime#181) returns an empty list must STAY ONLINE —
// even after the 180s managementMCPUnloadedGrace has fully elapsed. Before the fix,
// the post-online #3082 gate ignored mcp_tools_ready and degraded on the empty
// loaded_mcp_tools past grace (then readyForOnline re-promoted it → flap).
//
// REPRODUCE-then-PASS: run this test on main (with the runtime source available)
// and the row transitions to 'degraded' (assertion fails); on the fix branch it
// stays 'online'.
func TestHeartbeat_EV2_RealDB_OnlineStaysOnlineDespiteEmptyLoadedTools(t *testing.T) {
	initEV2DB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	const id = "33333333-3333-4333-8333-333333333333"
	seedProvisioningPlatformConcierge(t, id)
	t.Cleanup(func() {
		db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, id)
		db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, id)
	})

	// Already ONLINE, and it has been reporting an empty loaded_mcp_tools for 400s —
	// WELL past the 180s managementMCPUnloadedGrace. Under the OLD code this beat
	// degrades; under the fix, mcp_tools_ready=true keeps it online.
	setPlatformRow(t, id, "online", 400)

	// mcp_server_present=true, mcp_tools_ready=true, loaded_mcp_tools OMITTED (empty).
	body := `{"workspace_id":"` + id + `","error_rate":0.0,"uptime_seconds":600,"mcp_server_present":true,"mcp_tools_ready":true}`
	if code := postHeartbeat(t, handler, body); code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200", code)
	}
	if got := currentStatus(t, id); got != "online" {
		t.Fatalf("defect #2 (stuck-degrade/flap): online + mcp_tools_ready=true + empty loaded_mcp_tools past 180s must STAY online, got %q", got)
	}
	// The stale unloaded stamp must be cleared so a future genuine loss starts fresh.
	var unloaded interface{}
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT mcp_unloaded_since FROM workspaces WHERE id = $1`, id).Scan(&unloaded); err != nil {
		t.Fatalf("read mcp_unloaded_since: %v", err)
	}
	if unloaded != nil {
		t.Fatalf("defect #2: mcp_unloaded_since must be cleared when mcp_tools_ready affirmed, got %v", unloaded)
	}
}

// TestHeartbeat_EV2_RealDB_StuckWarmingSurfacesFault proves the fix for defect #3
// (#4449 stuck-warming-forever).
//
// A provisioning concierge whose readiness probe never publishes mcp_tools_ready
// (probe disabled/persistently failing) AND whose loaded_mcp_tools under-emits will
// never satisfy readyForOnline. Before the fix it held 'provisioning' FOREVER — no
// online, no degrade, no operator signal. The restored bounded warm-fail
// (conciergeWarmupFailGrace) must surface it as 'degraded' once the warming window
// is exceeded — WITHOUT any synthetic warmup turn.
//
// REPRODUCE-then-PASS: on main this beat leaves the row 'provisioning' (assertion
// fails); on the fix branch it transitions to 'degraded'.
func TestHeartbeat_EV2_RealDB_StuckWarmingSurfacesFault(t *testing.T) {
	initEV2DB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	const id = "44444444-4444-4444-8444-444444444444"

	// --- WITHIN WINDOW (negative control): still warming, must HOLD provisioning ---
	seedProvisioningPlatformConcierge(t, id)
	setPlatformRow(t, id, "provisioning", 100) // 100s < 300s conciergeWarmupFailGrace
	warmBody := `{"workspace_id":"` + id + `","error_rate":0.0,"uptime_seconds":100,"mcp_server_present":true}`
	if code := postHeartbeat(t, handler, warmBody); code != http.StatusOK {
		t.Fatalf("within-window heartbeat status = %d, want 200", code)
	}
	if got := currentStatus(t, id); got != "provisioning" {
		t.Fatalf("negative control: within the warmup window a never-ready box must HOLD provisioning, got %q", got)
	}
	db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, id)
	db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, id)

	// --- PAST WINDOW: never reached ready → must surface a fault (degraded) ---
	seedProvisioningPlatformConcierge(t, id)
	t.Cleanup(func() {
		db.DB.Exec(`DELETE FROM workspace_secrets WHERE workspace_id = $1`, id)
		db.DB.Exec(`DELETE FROM workspaces WHERE id = $1`, id)
	})
	setPlatformRow(t, id, "provisioning", 400) // 400s > 300s conciergeWarmupFailGrace

	// mcp_server_present=true, NO mcp_tools_ready, NO loaded_mcp_tools — never ready.
	stuckBody := `{"workspace_id":"` + id + `","error_rate":0.0,"uptime_seconds":400,"mcp_server_present":true}`
	if code := postHeartbeat(t, handler, stuckBody); code != http.StatusOK {
		t.Fatalf("past-window heartbeat status = %d, want 200", code)
	}
	if got := currentStatus(t, id); got != "degraded" {
		t.Fatalf("defect #3 (stuck-warming): a never-ready box past conciergeWarmupFailGrace must surface a fault (degraded), got %q", got)
	}
	// Operator-visible signal must be persisted.
	var sampleErr string
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT COALESCE(last_sample_error,'') FROM workspaces WHERE id = $1`, id).Scan(&sampleErr); err != nil {
		t.Fatalf("read last_sample_error: %v", err)
	}
	if sampleErr == "" {
		t.Fatalf("defect #3: warm-fail must persist an operator-visible last_sample_error, got empty")
	}
}
