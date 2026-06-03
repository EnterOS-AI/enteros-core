//go:build integration
// +build integration

// admin_test_token_integration_test.go — REAL Postgres integration tests
// for GET /admin/workspaces/:id/test-token (handlers/admin_test_token.go).
//
// Mirrors the pending_uploads_integration_test.go /
// delegation_ledger_integration_test.go pattern (handlers-postgres-integration.yml).
// Unit tests in admin_test_token_test.go pin the route shape + TestTokensEnabled
// gating; these tests pin the OBSERVABLE behavior against real DB rows:
//   - 404 in production-disabled mode (MOLECULE_ENV=production)
//   - exact ADMIN_TOKEN match when set
//   - 404 for unknown workspace
//   - minted auth_token validates against the real workspace_auth_tokens table
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	psql ... < workspace-server/migrations/001_workspaces.sql
//	psql ... < workspace-server/migrations/020_workspace_auth_tokens.up.sql
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_AdminTestToken -v

package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	mdb "github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// integrationDB_AdminTestToken opens the integration PG connection, wipes
// the workspaces + workspace_auth_tokens tables for our test rows, and
// hot-swaps the package-level mdb.DB so the handler sees the same conn.
// NOT SAFE FOR t.Parallel() — each test must own the global.
func integrationDB_AdminTestToken(t *testing.T) *sql.DB {
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
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspace_auth_tokens WHERE workspace_id LIKE 'integ-adm-%'`); err != nil {
		t.Fatalf("cleanup tokens: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE id LIKE 'integ-adm-%'`); err != nil {
		t.Fatalf("cleanup workspaces: %v", err)
	}
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspace_auth_tokens WHERE workspace_id LIKE 'integ-adm-%'`)
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE id LIKE 'integ-adm-%'`)
		mdb.DB = prev
		conn.Close()
	})
	return conn
}

// seedWorkspace_AdminTestToken inserts a minimal workspaces row so the
// test-token handler can find it.
func seedWorkspace_AdminTestToken(t *testing.T, conn *sql.DB, id string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'running')`,
		id, "integ-adm-"+id); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// TestIntegration_AdminTestToken_AuthGateAndMint pins the production
// gating, the ADMIN_TOKEN bearer match, the 404-on-unknown path, and
// that the minted auth_token lands in workspace_auth_tokens with a
// matching sha256(token_hash) row.
func TestIntegration_AdminTestToken_AuthGateAndMint(t *testing.T) {
	conn := integrationDB_AdminTestToken(t)
	handler := NewAdminTestTokenHandler()

	wsOK := "integ-adm-ws-ok"
	wsGhost := "integ-adm-ws-ghost"
	seedWorkspace_AdminTestToken(t, conn, wsOK)

	// --- Case 1: production-disabled (MOLECULE_ENV=production) → 404 ---
	// The handler returns 404 (not 403) so attackers can't probe for the
	// route's existence. t.Setenv restores on test exit.
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("MOLECULE_ENABLE_TEST_TOKENS", "")
	t.Setenv("ADMIN_TOKEN", "")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsOK}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+wsOK+"/test-token", nil)
	handler.GetTestToken(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("prod-disabled: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// Re-enable for the rest of the cases.
	t.Setenv("MOLECULE_ENV", "dev")
	t.Setenv("MOLECULE_ENABLE_TEST_TOKENS", "1")

	// --- Case 2: enabled, no ADMIN_TOKEN set, valid workspace → 200 with auth_token ---
	t.Setenv("ADMIN_TOKEN", "")
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsOK}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+wsOK+"/test-token", nil)
	handler.GetTestToken(c)
	if w.Code != http.StatusOK {
		t.Fatalf("mint no-admin: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp1 struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("mint no-admin: parse: %v", err)
	}
	if resp1.AuthToken == "" {
		t.Errorf("mint no-admin: auth_token empty in response")
	}
	if resp1.WorkspaceID != wsOK {
		t.Errorf("mint no-admin: workspace_id want %q, got %q", wsOK, resp1.WorkspaceID)
	}

	// --- Case 3: enabled, ADMIN_TOKEN set, wrong bearer → 401 ---
	t.Setenv("ADMIN_TOKEN", "real-admin-secret-xyz")
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsOK}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+wsOK+"/test-token", nil)
	c.Request.Header.Set("Authorization", "Bearer wrong-secret")
	handler.GetTestToken(c)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("admin wrong: status want 401, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 4: enabled, ADMIN_TOKEN set, correct bearer → 200 ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsOK}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+wsOK+"/test-token", nil)
	c.Request.Header.Set("Authorization", "Bearer real-admin-secret-xyz")
	handler.GetTestToken(c)
	if w.Code != http.StatusOK {
		t.Fatalf("admin correct: status want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp2 struct {
		AuthToken   string `json:"auth_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("admin correct: parse: %v", err)
	}
	if resp2.AuthToken == "" {
		t.Errorf("admin correct: auth_token empty in response")
	}

	// --- Case 5: enabled, unknown workspace → 404 ---
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsGhost}}
	c.Request = httptest.NewRequest("GET", "/admin/workspaces/"+wsGhost+"/test-token", nil)
	handler.GetTestToken(c)
	if w.Code != http.StatusNotFound {
		t.Errorf("ghost ws: status want 404, got %d: %s", w.Code, w.Body.String())
	}

	// --- Case 6: mint validates against real DB ---
	// Take the auth_token from Case 4 and verify there's a workspace_auth_tokens
	// row whose token_hash = sha256(auth_token) for wsOK.
	want := sha256.Sum256([]byte(resp2.AuthToken))
	var rowCount int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND token_hash = $2`,
		wsOK, want[:],
	).Scan(&rowCount); err != nil {
		t.Fatalf("verify hash: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("verify hash: want exactly 1 row matching sha256(token) for wsOK, got %d", rowCount)
	}
}
