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
// We use raw TCP listeners (net.Listener) instead of httptest.Server to avoid
// any HTTP-library-level goroutine complexity. The test opens a TCP port,
// serves one HTTP response, then closes the connection. The a2aClient transport
// is overridden with a DialContext that intercepts all dials and redirects to
// the test server's port. No DNS, no TCP handshake overhead, no HTTP library
// goroutines that could block on request-body reads.
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
	"runtime"
	"strconv"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// integrationDB is imported from delegation_ledger_integration_test.go.
// Each test gets a fresh table state.

const integrationTestDelegationID = "del-159-test-integration"
const integrationTestSourceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const integrationTestTargetID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
const integrationTestParentID = "cccccccc-cccc-cccc-cccc-cccccccccccc"

// rawHTTPServer starts a TCP listener, serves one HTTP response, and closes.
// It runs in a background goroutine so the test can proceed immediately after
// returning the server URL. The server URL (e.g. "http://127.0.0.1:<port>/")
// is suitable for caching in Redis and passing to executeDelegation.
//
// The server reads HTTP headers using a deadline, then immediately sends the
// response. This prevents the classic TCP deadlock: server blocked reading
// body while client blocked waiting for response.
func rawHTTPServer(t *testing.T, statusCode int, body string) (serverURL string, closeFn func()) {
	t.Helper()
	// Use ListenTCP with explicit IPv4 to avoid IPv6 mismatch on macOS
	// (Listen("tcp", "127.0.0.1:0") might bind ::1 on some systems).
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("rawHTTPServer listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	serverURL = "http://127.0.0.1:" + strconv.Itoa(port) + "/"

	connCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		connCh <- conn
	}()

	closeFn = func() {
		ln.Close()
	}

	// Handle in background so we don't block test execution.
	// Strategy: read available bytes with a deadline (enough for headers).
	// After deadline fires, send the response immediately.
	// The kernel discards any unread buffered body bytes when the
	// connection closes — harmless.
	go func() {
		conn := <-connCh
		if conn == nil {
			return
		}

		// Read what we can with a 2s deadline. Headers always arrive first.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		headerBuf := make([]byte, 4096)
		for {
			n, err := conn.Read(headerBuf)
			if n > 0 {
				_ = headerBuf[:n]
			}
			if err != nil {
				break
			}
		}

		// Send response and IMMEDIATELY close the connection.
		// If we keep it open, the client's request-body writer goroutine
		// might block on the socket (waiting for the server to drain the
		// body). Closing immediately unblocks it. The client already
		// received the response, so the write error is harmless.
		resp := buildHTTPResponse(statusCode, body)
		conn.Write(resp) //nolint:errcheck
		conn.Close()
	}()

	return serverURL, closeFn
}

// buildHTTPResponse constructs a minimal HTTP/1.1 response.
func buildHTTPResponse(statusCode int, body string) []byte {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown"
	}
	header := "HTTP/1.1 " + strconv.Itoa(statusCode) + " " + statusText + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n"
	return []byte(header + body)
}

// setupIntegrationFixtures inserts the rows executeDelegation requires:
//   - workspaces: source (org root) + target as its CHILD, so both live in the
//     SAME org. CanCommunicate=true (parent↔child) AND the #1953 sameOrg() guard
//     in proxyA2ARequest passes (both resolve to the same org root). A real
//     delegation happens INSIDE one org. (Previously both were parent_id=NULL —
//     two DISTINCT org roots — which only "communicated" via CanCommunicate's
//     root-sibling rule; #1953 added a sameOrg() guard that now denies routing
//     between two org roots as cross-tenant, so the success-path tests below
//     must use a same-org source/target pair.)
//   - activity_logs: the 'delegate' row that updateDelegationStatus UPDATE will find
//   - delegations: the ledger row that recordLedgerStatus will UPDATE
//
// Returns a cleanup function the test should defer.
func setupIntegrationFixtures(t *testing.T, conn *sql.DB) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	sourceID := integrationTestSourceID // org root (parent_id NULL); target hangs off it
	for _, ws := range []struct {
		id       string
		name     string
		parentID *string
	}{
		{integrationTestSourceID, "test-source", nil},
		{integrationTestTargetID, "test-target", &sourceID}, // child of source → same org
	} {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO workspaces (id, name, parent_id) VALUES ($1::uuid, $2, $3) ON CONFLICT (id) DO NOTHING`,
			ws.id, ws.name, ws.parentID,
		); err != nil {
			cancel()
			t.Fatalf("seed workspace %s: %v", ws.id, err)
		}
	}

	reqBody, _ := json.Marshal(map[string]any{
		"delegation_id": integrationTestDelegationID,
		"task":          "do work",
	})
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO activity_logs
			(workspace_id, activity_type, method, source_id, target_id, request_body, status)
		VALUES ($1, 'delegate', 'delegate', $1, $2, $3::jsonb, 'pending')
		ON CONFLICT DO NOTHING
	`, integrationTestSourceID, integrationTestTargetID, string(reqBody)); err != nil {
		cancel()
		t.Fatalf("seed activity_logs: %v", err)
	}

	if _, err := conn.ExecContext(ctx, `
		INSERT INTO delegations
			(delegation_id, caller_id, callee_id, task_preview, status)
		VALUES ($1, $2::uuid, $3::uuid, 'do work', 'queued')
		ON CONFLICT (delegation_id) DO NOTHING
	`, integrationTestDelegationID, integrationTestSourceID, integrationTestTargetID); err != nil {
		cancel()
		t.Fatalf("seed delegations: %v", err)
	}
	cancel()

	return func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		conn.ExecContext(ctx2,
			`DELETE FROM activity_logs WHERE workspace_id = $1 AND request_body->>'delegation_id' = $2`,
			integrationTestSourceID, integrationTestDelegationID)
		conn.ExecContext(ctx2,
			`DELETE FROM delegations WHERE delegation_id = $1`, integrationTestDelegationID)
		conn.ExecContext(ctx2,
			`DELETE FROM workspaces WHERE id IN ($1, $2)`, integrationTestSourceID, integrationTestTargetID)
	}
}

// readDelegationRow returns (status, result_preview, error_detail) for the test
// delegation, or fails the test if the row is not found.
func readDelegationRow(t *testing.T, conn *sql.DB) (status, preview, errorDetail string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var prev, errDet sql.NullString
	err := conn.QueryRowContext(ctx,
		`SELECT status, result_preview, error_detail FROM delegations WHERE delegation_id = $1`,
		integrationTestDelegationID,
	).Scan(&status, &prev, &errDet)
	if err != nil {
		t.Fatalf("readDelegationRow: %v", err)
	}
	return status, prev.String, errDet.String
}

// stack returns the current goroutine stack trace. Used by runWithTimeout to
// pinpoint the blocking call site when a test times out.
func stack() string {
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

// runWithTimeout calls fn in a goroutine and fails t if it doesn't return within
// timeout. ctx is passed to fn so it can propagate cancellation to
// executeDelegation's DB and network operations — without this, the goroutine
// leaks indefinitely when the test times out (context.Background() never cancels).
func runWithTimeout(t *testing.T, timeout time.Duration, fn func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan struct{})
	var panicErr interface{}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				panicErr = p
			}
			close(done)
		}()
		fn(ctx)
	}()

	select {
	case <-done:
		if panicErr != nil {
			t.Fatalf("executeDelegation panicked: %v\n%s", panicErr, stack())
		}
	case <-ctx.Done():
		cancel()
		t.Fatalf("executeDelegation timed out after %s\n%s", timeout, stack())
	}
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

	agentURL, closeServer := rawHTTPServer(t, 200, `{"result":{"parts":[{"text":"work completed successfully"}]}}`)
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})
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

	agentURL, closeServer := rawHTTPServer(t, 500, `{"error":"agent crashed"}`)
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})
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

	agentURL, closeServer := rawHTTPServer(t, 200, "")
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})
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

	agentURL, closeServer := rawHTTPServer(t, 200, `{"result":{"parts":[{"text":"all good"}]}}`)
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})
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
// (not panic) when the target is genuinely unreachable. proxyA2ARequest falls
// back to DB URL lookup when Redis is down.
//
// The target is put in a TERMINAL state (failed) so its missing URL is a real
// dead end → hard 503 → delegation fails. A URL-less target in a
// recoverable-settling state instead ENQUEUES; that path is covered by
// TestIntegration_ExecuteDelegation_SettlingTarget_Enqueues.
func TestIntegration_ExecuteDelegation_RedisDown_FallsBackToDB(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Terminal target: no URL AND not self-recovering → genuinely unreachable.
	if _, err := conn.Exec(`UPDATE workspaces SET status = 'failed' WHERE id = $1::uuid`, integrationTestTargetID); err != nil {
		t.Fatalf("failed to set target status=failed: %v", err)
	}

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})
	t.Logf("executeDelegation took %v", time.Since(start))

	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("status: want failed (no target URL), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on failure due to unreachable target")
	}
}

// A delegation to a URL-less target in a recoverable-settling state
// (provisioning / awaiting_agent) must be ENQUEUED for durable drain when the
// workspace comes online — NOT dropped as failed. Pins the
// classWorkspaceSettling enqueue path added to resolveAgentURL /
// proxyA2ARequest so a config-PUT restart flap (or any mid-restart window) no
// longer loses the turn.
func TestIntegration_ExecuteDelegation_SettlingTarget_Enqueues(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	// Recoverable-settling target: no URL, status=provisioning (as during a
	// mid-restart re-provision). resolveAgentURL falls back to the DB, sees no
	// URL + a settling status, and enqueues instead of hard-503-dropping.
	if _, err := conn.Exec(`UPDATE workspaces SET status = 'provisioning', url = NULL WHERE id = $1::uuid`, integrationTestTargetID); err != nil {
		t.Fatalf("failed to set target status=provisioning: %v", err)
	}

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
	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, "")
	})

	status, _, errDet := readDelegationRow(t, conn)
	if status != "dispatched" {
		t.Errorf("status: want dispatched (settling target enqueued for durable drain), got %q", status)
	}
	if errDet != "" {
		t.Errorf("error_detail should be empty when enqueued (not a failure): got %q", errDet)
	}
}

// TestIntegration_SameOrg_RealCTE_ResolvesAncestorChain is the regression gate
// for the org_scope.go recursive-CTE bug (#1953 follow-up). The sqlmock unit
// tests feed sameOrg() a pre-computed root_id row, so they CANNOT catch a wrong
// CTE — they assume it already returns the right value. Only a real Postgres
// run exercises orgRootSubtreeCTE itself.
//
// The bug: the CTE carried `id AS root_id` from the recursive SEED, so a
// non-root workspace resolved to ITSELF instead of its topmost ancestor. That
// made sameOrg() return false for two genuinely same-org workspaces and 403 a
// legitimate same-org a2a route (over-block). This test seeds a real
// root → child → grandchild chain plus a separate org root, and asserts:
//   - every node in the chain resolves to the SAME org root (root, child, grandchild)
//   - two workspaces in the same chain are sameOrg (incl. grandchild ↔ root)
//   - a workspace in a DIFFERENT chain is NOT sameOrg (cross-tenant stays closed)
func TestIntegration_SameOrg_RealCTE_ResolvesAncestorChain(t *testing.T) {
	conn := integrationDB(t)

	const (
		rootA       = "11111111-1111-1111-1111-111111111111"
		childA      = "22222222-2222-2222-2222-222222222222"
		grandchildA = "33333333-3333-3333-3333-333333333333"
		rootB       = "44444444-4444-4444-4444-444444444444"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		// Delete leaf-first to respect the parent_id self-FK.
		for _, id := range []string{grandchildA, childA, rootA, rootB} {
			conn.ExecContext(c2, `DELETE FROM workspaces WHERE id = $1`, id)
		}
	})

	// Insert parent-before-child to satisfy the self-referential FK.
	seed := []struct {
		id, name string
		parent   *string
	}{
		{rootA, "org-a-root", nil},
		{childA, "org-a-child", strPtr(rootA)},
		{grandchildA, "org-a-grandchild", strPtr(childA)},
		{rootB, "org-b-root", nil},
	}
	for _, s := range seed {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO workspaces (id, name, parent_id) VALUES ($1::uuid, $2, $3) ON CONFLICT (id) DO NOTHING`,
			s.id, s.name, s.parent); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}

	// Every node in chain A must resolve to rootA via the REAL CTE.
	for _, id := range []string{rootA, childA, grandchildA} {
		got, err := orgRootID(ctx, conn, id)
		if err != nil {
			t.Fatalf("orgRootID(%s): %v", id, err)
		}
		if got != rootA {
			t.Errorf("orgRootID(%s) = %q, want rootA %q (CTE must walk to topmost ancestor)", id, got, rootA)
		}
	}

	// Same-org positives — including the grandchild↔root pair that the buggy
	// CTE got wrong.
	for _, pair := range [][2]string{{childA, grandchildA}, {rootA, grandchildA}, {rootA, childA}} {
		ok, err := sameOrg(ctx, conn, pair[0], pair[1])
		if err != nil {
			t.Fatalf("sameOrg(%s,%s): %v", pair[0], pair[1], err)
		}
		if !ok {
			t.Errorf("sameOrg(%s,%s) = false, want true (same org chain)", pair[0], pair[1])
		}
	}

	// Cross-org negative — isolation must stay closed.
	for _, pair := range [][2]string{{rootA, rootB}, {grandchildA, rootB}, {childA, rootB}} {
		ok, err := sameOrg(ctx, conn, pair[0], pair[1])
		if err != nil {
			t.Fatalf("sameOrg(%s,%s): %v", pair[0], pair[1], err)
		}
		if ok {
			t.Errorf("sameOrg(%s,%s) = true, want false (different orgs — cross-tenant must stay denied)", pair[0], pair[1])
		}
	}
}

// seedConsumedGrant inserts an APPROVED + already-CONSUMED privileged-delegation
// grant owned by workspaceID and returns (grantID, cleanup). Mirrors what
// gatePrivilegedDelegation's atomic consume leaves behind just before the
// detached dispatch runs.
func seedConsumedGrant(t *testing.T, conn *sql.DB, workspaceID string) (string, func()) {
	t.Helper()
	const grantID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO approval_requests (id, workspace_id, action, status, consumed_at)
		VALUES ($1::uuid, $2::uuid, 'privileged_delegate', 'approved', now())
		ON CONFLICT (id) DO UPDATE SET status = 'approved', consumed_at = now()
	`, grantID, workspaceID); err != nil {
		t.Fatalf("seed consumed grant: %v", err)
	}
	return grantID, func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		conn.ExecContext(c, `DELETE FROM approval_requests WHERE id = $1`, grantID)
	}
}

// grantConsumedAt returns whether the seeded grant is still consumed.
func grantConsumedAt(t *testing.T, conn *sql.DB, grantID string) (consumed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var consumedAt sql.NullTime
	if err := conn.QueryRowContext(ctx,
		`SELECT consumed_at FROM approval_requests WHERE id = $1`, grantID).Scan(&consumedAt); err != nil {
		t.Fatalf("read grant consumed_at: %v", err)
	}
	return consumedAt.Valid
}

// FINDING[3] (end-to-end, fail direction): a privileged delegation whose A2A
// hand-off NEVER reaches the target (terminal-unreachable) must RESTORE the
// consumed single-use grant — so an operator's one approval is not permanently
// burned on a delegation that never dispatched. This is the hole the old design
// left open: consume-at-gate + detached fire-and-forget dispatch that fails
// after the 202.
func TestIntegration_ExecuteDelegation_DispatchFailure_RestoresGrant(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	grantID, grantCleanup := seedConsumedGrant(t, conn, integrationTestSourceID)
	defer grantCleanup()

	// Terminal target: no URL and not self-recovering → genuinely unreachable →
	// the dispatch fails (not enqueues).
	if _, err := conn.Exec(`UPDATE workspaces SET status = 'failed' WHERE id = $1::uuid`, integrationTestTargetID); err != nil {
		t.Fatalf("set target status=failed: %v", err)
	}

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

	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, grantID)
	})

	// The delegation itself failed (target unreachable) ...
	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("delegation status: want failed (unreachable target), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on an unreachable-target failure")
	}
	// ... but the grant was RESTORED so it is not burned on a hand-off that never happened.
	if grantConsumedAt(t, conn, grantID) {
		t.Error("dispatch failure MUST restore the grant (consumed_at → NULL); grant is still consumed")
	}
}

// FINDING[3] (end-to-end, negative control / legit direction): a privileged
// delegation that DOES dispatch successfully must KEEP the grant consumed — the
// single-use approval was legitimately spent on a hand-off that actually
// happened. Guards against an over-eager restore that would let one approval
// drive unlimited dispatches.
func TestIntegration_ExecuteDelegation_DispatchSuccess_KeepsGrantConsumed(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	grantID, grantCleanup := seedConsumedGrant(t, conn, integrationTestSourceID)
	defer grantCleanup()

	agentURL, closeServer := rawHTTPServer(t, 200, `{"result":{"parts":[{"text":"all good"}]}}`)
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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

	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, grantID)
	})

	status, _, _ := readDelegationRow(t, conn)
	if status != "completed" {
		t.Errorf("delegation status: want completed, got %q", status)
	}
	if !grantConsumedAt(t, conn, grantID) {
		t.Error("a successfully dispatched delegation MUST keep its single-use grant consumed; it was restored")
	}
}

// FINDING[5] (end-to-end): a privileged delegation whose target answered 2xx
// with an EMPTY body is marked FAILED and delivered NOTHING — so, exactly like
// the proxyErr non-delivery branch, it must RESTORE the consumed single-use
// grant. Before this fix the 2xx-empty-body failure branch did NOT restore, so a
// delegation the system itself marks failed permanently BURNED the operator's
// one approval. Negative control against DispatchSuccess_KeepsGrantConsumed (a
// DELIVERED 2xx keeps the grant consumed): the two directions diverge only on
// whether the hand-off actually delivered a body.
func TestIntegration_ExecuteDelegation_EmptyBodyFailure_RestoresGrant(t *testing.T) {
	allowLoopbackForTest(t)
	conn := integrationDB(t)
	cleanup := setupIntegrationFixtures(t, conn)
	defer cleanup()
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")

	grantID, grantCleanup := seedConsumedGrant(t, conn, integrationTestSourceID)
	defer grantCleanup()

	// 200 + empty body → isDeliveryConfirmedSuccess fails (needs len>0), proxyErr
	// is nil, so executeDelegation takes the 2xx-empty-body FAILURE branch.
	agentURL, closeServer := rawHTTPServer(t, 200, "")
	defer closeServer()

	mr := setupTestRedis(t)
	defer mr.Close()
	db.CacheURL(context.Background(), integrationTestTargetID, agentURL)

	prevClient := a2aClient
	defer func() { a2aClient = prevClient }()
	a2aClient = newA2AClientForHost(extractHostPort(agentURL))

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

	runWithTimeout(t, 30*time.Second, func(ctx context.Context) {
		dh.executeDelegation(ctx, integrationTestSourceID, integrationTestTargetID, integrationTestDelegationID, a2aBody, grantID)
	})

	// The delegation failed (empty body) ...
	status, _, errDet := readDelegationRow(t, conn)
	if status != "failed" {
		t.Errorf("delegation status: want failed (empty-body target), got %q", status)
	}
	if errDet == "" {
		t.Error("error_detail should be set on an empty-body failure")
	}
	// ... but the grant is RESTORED so a failed, non-delivering hand-off does not
	// permanently burn the operator's single approval (FINDING[5]).
	if grantConsumedAt(t, conn, grantID) {
		t.Error("2xx-empty-body FAILURE must restore the grant (consumed_at → NULL); grant is still consumed")
	}
}

// extractHostPort parses "http://127.0.0.1:PORT/" and returns "127.0.0.1:PORT".
func extractHostPort(rawURL string) string {
	// Simple parse: strip "http://" prefix and trailing slash.
	// The URL format is always "http://127.0.0.1:PORT/" in our usage.
	if len(rawURL) > 7 {
		return rawURL[7 : len(rawURL)-1]
	}
	return rawURL
}

// newA2AClientForHost creates an http.Client that redirects all connections
// to the given host:port. This lets us mock the agent endpoint without
// running a real HTTP server.
func newA2AClientForHost(targetHost string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("tcp", targetHost)
			},
			ResponseHeaderTimeout: 180 * time.Second,
		},
	}
}
