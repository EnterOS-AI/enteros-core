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
