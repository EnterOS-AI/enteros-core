//go:build integration
// +build integration

// delegation_executor_integration_test.go — REAL Postgres integration tests for
// executeDelegation HTTP proxy edge cases that sqlmock cannot cover.
//
// The sqlmock tests in delegation_test.go pin which SQL statements fire but
// cannot detect bugs that depend on the row state AFTER the SQL runs. The
// result_preview-lost bug shipped to staging in PR #2854 because sqlmock tests
// were satisfied with "an UPDATE fired" — none verified the row's preview
// field actually landed. These integration tests close that gap.
//
// How HTTP is mocked
// -----------------
// a2aClient is a package-level var so tests can reassign it. Each test
// creates an httptest.Server (same-process, same-host) and redirects
// a2aClient's Transport to point at it. Same-process HTTP has no DNS, no
// TCP handshake overhead, and no network partition risk. The httptest.Server
// is started BEFORE a2aClient is updated so every request hits a live server.
//
// Run with:
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   sleep 4
//   psql ... < workspace-server/migrations/049_delegations.up.sql
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run Integration_ExecuteDelegation
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/handlers/**.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/alicebob/miniredis/v2"
)

// integrationDB is imported from delegation_ledger_integration_test.go.
// Each test gets a fresh table state.

const testDelegationID = "del-159-test-integration"
const testSourceID    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const testTargetID   = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// setupIntegrationFixtures inserts the rows executeDelegation requires:
//   - workspaces: source and target (siblings, parent_id=NULL so CanCommunicate=true)
//   - activity_logs: the 'delegate' row that updateDelegationStatus UPDATE will find
//   - delegations: the ledger row that recordLedgerStatus will UPDATE
//
// Returns a cleanup function the test should defer.
func setupIntegrationFixtures(t *testing.T, conn *sql.DB) func() {
	t.Helper()
	for _, ws := range []struct {
		id       string
		name     string
		parentID *string
	}{
		{testSourceID, "test-source", nil},
		{testTargetID, "test-target", nil},
	} {
		if _, err := conn.ExecContext(context.Background(),
			`INSERT INTO workspaces (id, name, parent_id) VALUES ($1::uuid, $2, $3) ON CONFLICT (id) DO NOTHING`,
			ws.id, ws.name, ws.parentID,
		); err != nil {
			t.Fatalf("seed workspace %s: %v", ws.id, err)
		}
	}

	reqBody, _ := json.Marshal(map[string]any{
		"delegation_id": testDelegationID,
		"task":         "do work",
	})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs
			(workspace_id, activity_type, method, source_id, target_id, request_body, status)
		VALUES ($1, 'delegate', 'delegate', $1, $2, $3::jsonb, 'pending')
		ON CONFLICT DO NOTHING
	`, testSourceID, testTargetID, string(reqBody)); err != nil {
		t.Fatalf("seed activity_logs: %v", err)
	}

	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO delegations
			(delegation_id, caller_id, callee_id, task_preview, status)
		VALUES ($1, $2::uuid, $3::uuid, 'do work', 'queued')
		ON CONFLICT (delegation_id) DO NOTHING
	`, testDelegationID, testSourceID, testTargetID); err != nil {
		t.Fatalf("seed delegations: %v", err)
	}

	return func() {
		conn.ExecContext(context.Background(),
			`DELETE FROM activity_logs WHERE workspace_id = $1 AND request_body->>'delegation_id' = $2`,
			testSourceID, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM delegations WHERE delegation_id = $1`, testDelegationID)
		conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE id IN ($1, $2)`, testSourceID, testTargetID)
	}
}

// readDelegationRow returns (status, result_preview, error_detail) for the test
// delegation, or fails the test if the row is not found.
func readDelegationRow(t *testing.T, conn *sql.DB) (status, preview, errorDetail string) {
	t.Helper()
	var prev, errDet sql.NullString
	err := conn.QueryRowContext(context.Background(),
		`SELECT status, result_preview, error_detail FROM delegations WHERE delegation_id = $1`,
		testDelegationID,
	).Scan(&status, &prev, &errDet)
	if err != nil {
		t.Fatalf("readDelegationRow: %v", err)
	}
	return status, prev.String, errDet.String
}

// TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess
// is the integration regression gate for issue #159.
//
// Scenario: proxyA2ARequest returns a 200 status code with a non-empty body.
// isDeliveryConfirmedSuccess guard (status>=200 && <300 && len(body)>0 && err!=nil)
// routes to handleSuccess. The integration test verifies the DB row lands at
// 'completed' with the response body as result_preview.
func TestIntegration_ExecuteDelegation_DeliveryConfirmedProxyError_TreatsAsSuccess(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Create test server with 200 response.
	// NOTE: Do NOT read r.Body here. httptest.Server reads the full request
	// body into an in-memory buffer before calling the handler — r.Body is
	// already populated. Reading it here would not block in theory, but
	// omitting the drain avoids any subtle goroutine lifetime issues.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":{"parts":[{"text":"work completed successfully"}]}}`))
	}))
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	// Override a2aClient so requests go to the test server (same-process).
	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	u, _ := url.Parse(ts.URL)
	a2aClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", u.Host)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		},
	}

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	start := time.Now()
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	t.Logf("executeDelegation took %v", time.Since(start))

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
		t.Errorf("result_preview should be non-empty, got %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed verifies that
// a 500 response routes to failure, not success. isDeliveryConfirmedSuccess
// requires status>=200 && <300, so 500 always fails the guard.
func TestIntegration_ExecuteDelegation_ProxyErrorNon2xx_RemainsFailed(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"agent crashed"}`))
	}))
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	u, _ := url.Parse(ts.URL)
	a2aClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", u.Host)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		},
	}

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	start := time.Now()
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	t.Logf("executeDelegation took %v", time.Since(start))

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be non-empty on failure")
	}
}

// TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed verifies that
// a 200 response with an empty body routes to failure. isDeliveryConfirmedSuccess
// requires len(body) > 0, so an empty body fails the guard.
func TestIntegration_ExecuteDelegation_ProxyErrorEmptyBody_RemainsFailed(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No body — the handler sends a 200 with Content-Length: 0.
	}))
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	u, _ := url.Parse(ts.URL)
	a2aClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", u.Host)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		},
	}

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	start := time.Now()
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	t.Logf("executeDelegation took %v", time.Since(start))

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed, got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be non-empty on failure")
	}
}

// TestIntegration_ExecuteDelegation_CleanProxyResponse_Unchanged is the baseline:
// a clean 200 response with a valid body and no error routes to success.
func TestIntegration_ExecuteDelegation_CleanProxyResponse_Unchanged(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":{"parts":[{"text":"all good"}]}}`))
	}))
	defer ts.Close()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), testTargetID, ts.URL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	u, _ := url.Parse(ts.URL)
	a2aClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", u.Host)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		},
	}

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	start := time.Now()
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	t.Logf("executeDelegation took %v", time.Since(start))

	status, preview, errDet := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("status: want completed, got %q", status)
	}
	if preview == "" {
		t.Errorf("result_preview should be non-empty, got %q", preview)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty on success: got %q", errDet)
	}
}

// Test that a delegation where Redis cannot be reached still routes to failure
// (not panic). proxyA2ARequest falls back to DB URL lookup when Redis is down.
func TestIntegration_ExecuteDelegation_RedisDown_FallsBackToDB(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Set up miniredis so db.RDB is non-nil, but do NOT cache any URL.
	// resolveAgentURL skips Redis and falls back to DB, which also has no URL.
	mr := setupTestRedis(t)
	defer mr.Close()

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	a2aBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": "1", "method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":  "user",
				"parts": []map[string]string{{"type": "text", "text": "do work"}},
			},
		},
	})
	start := time.Now()
	dh.executeDelegation(testSourceID, testTargetID, testDelegationID, a2aBody)
	t.Logf("executeDelegation took %v", time.Since(start))

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed (no target URL), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on failure due to unreachable target")
	}
}
