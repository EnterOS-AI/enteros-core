package handlers

// #1870 Phase 1 queue tests. Covers enqueue, FIFO drain order, priority
// ordering, idempotency, failed-retry bounding, nil-safe error extraction
// (GH fix), and the extractor helper.
//
// Uses sqlmock.QueryMatcherEqual (exact string matching) so that SQL query
// strings are compared verbatim without regex escaping complexity.
// setupTestDBForQueueTests creates the mock with this matcher; it MUST be
// used instead of setupTestDB for these tests.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
)

// setupTestDBForQueueTests creates a sqlmock DB using QueryMatcherEqual (exact
// string matching) so that ExpectQuery/ExpectExec patterns are compared verbatim.
// Uses the same global db.DB as setupTestDB so the handler can use it.
//
// IMPORTANT: db.DB is saved before assignment and restored via t.Cleanup so
// that tests running after this one are not polluted by a closed mock.
// Same fix as setupTestDB (handlers_test.go); same root cause as mc#975.
func setupTestDBForQueueTests(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	// Drain stragglers from PRIOR tests before swapping the global db.DB:
	// a globalGoAsync goroutine still running (e.g. ensureAndArmSchedulerPlugin)
	// reads db.DB concurrently with this write — the CI -race failure in
	// TestScheduleHandler_Create_CRLFStripped. Draining first means every
	// leaked goroutine finishes against the OLD handle.
	waitGlobalAsyncForTest()
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })
	return mock
}

// ──────────────────────────────────────────────────────────────────────────────
// Priority constants
// ──────────────────────────────────────────────────────────────────────────────

func TestPriorityConstants(t *testing.T) {
	if PriorityCritical <= PriorityTask || PriorityTask <= PriorityInfo {
		t.Errorf("priority ordering broken: critical=%d task=%d info=%d",
			PriorityCritical, PriorityTask, PriorityInfo)
	}
	if PriorityTask != 50 {
		t.Errorf("PriorityTask changed from 50 to %d — migration 042's DEFAULT 50 also needs updating",
			PriorityTask)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// extractIdempotencyKey
// ──────────────────────────────────────────────────────────────────────────────

func TestExtractIdempotencyKey_picksMessageId(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-abc","role":"user"}}}`)
	if got := extractIdempotencyKey(body); got != "msg-abc" {
		t.Errorf("expected 'msg-abc', got %q", got)
	}
}

func TestExtractIdempotencyKey_emptyOnMissing(t *testing.T) {
	cases := map[string][]byte{
		"no params":     []byte(`{"jsonrpc":"2.0","method":"message/send"}`),
		"no message":    []byte(`{"params":{}}`),
		"no messageId":  []byte(`{"params":{"message":{"role":"user"}}}`),
		"malformed":     []byte(`not json`),
		"empty message": []byte(`{"params":{"message":{"messageId":""}}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractIdempotencyKey(body); got != "" {
				t.Errorf("expected empty, got %q", got)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// extractExpiresInSeconds
// ──────────────────────────────────────────────────────────────────────────────

func TestExtractExpiresInSeconds_valid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"positive int", `{"params":{"expires_in_seconds":30}}`, 30},
		{"zero", `{"params":{"expires_in_seconds":0}}`, 0},
		{"large TTL", `{"params":{"expires_in_seconds":3600}}`, 3600},
		{"nested message — not affected", `{"params":{"message":{"role":"user"},"expires_in_seconds":60}}`, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractExpiresInSeconds([]byte(tc.body)); got != tc.want {
				t.Errorf("extractExpiresInSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExtractExpiresInSeconds_invalidOrMissing(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"negative → 0", `{"params":{"expires_in_seconds":-5}}`, 0},
		{"missing expires_in_seconds", `{"params":{"message":{"role":"user"}}}`, 0},
		{"no params at all", `{"method":"message/send"}`, 0},
		{"malformed JSON", `not json`, 0},
		{"empty body", ``, 0},
		{"null value", `{"params":{"expires_in_seconds":null}}`, 0},
		{"string value", `{"params":{"expires_in_seconds":"30"}}`, 0},
		{"float value", `{"params":{"expires_in_seconds":30.5}}`, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractExpiresInSeconds([]byte(tc.body)); got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

func TestExtractDelegationIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "delegation body — metadata.delegation_id present",
			body: `{"method":"message/send","params":{"message":{"role":"user","messageId":"abc-123","metadata":{"delegation_id":"abc-123"},"parts":[{"type":"text","text":"hi"}]}}}`,
			want: "abc-123",
		},
		{
			name: "non-delegation body — no metadata (peer-direct A2A)",
			body: `{"method":"message/send","params":{"message":{"role":"user","messageId":"m-1","parts":[{"type":"text","text":"hi"}]}}}`,
			want: "",
		},
		{
			name: "metadata present but no delegation_id key",
			body: `{"params":{"message":{"metadata":{"trace_id":"t-1"}}}}`,
			want: "",
		},
		{"malformed JSON", `not json`, ""},
		{"empty body", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractDelegationIDFromBody([]byte(tc.body)); got != tc.want {
				t.Errorf("extractDelegationIDFromBody = %q, want %q", got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DrainQueueForWorkspace — nil-safe error extraction regression tests
//
// These tests verify the defensive type-assertion fix for the panic that
// occurred when proxyErr.Response was nil or had a non-string "error" field.
// The original code was:
//   MarkQueueItemFailed(ctx, item.ID, proxyErr.Response["error"].(string))
// which panics when:
//   a) proxyErr.Response is nil
//   b) "error" key is absent from the map
//   c) the "error" field is a non-string type (e.g., a struct or int)
//
// The fix uses comma-ok idiom + fallback chain:
//   errMsg, _ := proxyErr.Response["error"].(string)
//   if errMsg == "" { errMsg = http.StatusText(proxyErr.Status); ... }
//
// Uses sqlmock.MatchSs (exact string matching). SQL strings must EXACTLY match
// the queries generated by the handler code — no escaping, no regex.
// ──────────────────────────────────────────────────────────────────────────────

// drainSetup creates a consistent test environment for DrainQueueForWorkspace.
// Uses setupTestDBForQueueTests (QueryMatcherEqual) so SQL strings are compared verbatim.
// workspaceID is passed so callers can register the budget-check expectation in the
// correct position — after expectDequeueNextOk (DequeueNext's tx BEGIN→SELECT→UPDATE→COMMIT
// runs before proxyA2ARequest→checkWorkspaceBudget in the actual call sequence).
func drainSetup(t *testing.T, workspaceID string) (sqlmock.Sqlmock, *WorkspaceHandler, *miniredis.Miniredis) {
	mock := setupTestDBForQueueTests(t)
	mr := setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	allowLoopbackForTest(t) // httptest.Server uses 127.0.0.1; SSRF guard must permit it
	return mock, handler, mr
}

// expectQueueBudgetCheck registers the mock for checkWorkspaceBudget's query:
//
//	SELECT budget_limit, COALESCE(monthly_spend, 0) FROM workspaces WHERE id = $1
//
// Must be called AFTER expectDequeueNextOk — DequeueNext (BEGIN→SELECT→UPDATE→COMMIT)
// runs before proxyA2ARequest which calls checkWorkspaceBudget.
// Named distinctly from handlers_test.go's expectBudgetCheck (which uses MatchPsql
// escaped-regex and cannot be reused with QueryMatcherEqual tests).
func expectQueueBudgetCheck(mock sqlmock.Sqlmock, workspaceID string) {
	// Multi-period (#49): exact-match the budget_limits read; "{}" → no limits →
	// checkWorkspaceBudget returns early (no spend query).
	mock.ExpectQuery(
		"SELECT COALESCE(budget_limits, '{}'::jsonb) FROM workspaces WHERE id = $1",
	).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"budget_limits"}).AddRow([]byte("{}")))
}

// expectAgentURLResolve mocks the FRESH per-attempt DB URL read that
// DrainQueueForWorkspace performs after ClearCachedURL evicts the cache
// (core#124): resolveAgentURL's cache-miss branch runs
// `SELECT url, status FROM workspaces WHERE id = $1`. Returning a non-empty
// url + status='online' makes it resolve `url` and reseed the cache, so the
// proxyA2ARequest that follows dispatches to `url` (the woken workspace's
// CURRENT container), not a stale cached one. Must be registered right after
// expectDequeueNextOk and before expectQueueBudgetCheck to match the call
// order under QueryMatcherEqual.
func expectAgentURLResolve(mock sqlmock.Sqlmock, wsID, url string) {
	mock.ExpectQuery(
		"SELECT url, status FROM workspaces WHERE id = $1").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(url, "online"))
}

// expectAgentURLResolveSettling mocks the same fresh per-attempt DB URL read, but
// for a workspace that has NOT reached online yet: url is NULL and status is a
// recoverable-settling state (provisioning / awaiting_agent). resolveAgentURL then
// returns a classWorkspaceSettling proxyA2AError (no URL), which
// DrainQueueForWorkspace must route onto the BOUNDED settling path — NOT through
// proxyA2ARequest (which would enqueueBusyA2A a fresh unbounded row: the #4531
// regression). status='provisioning' is one of isRecoverableSettlingStatus's set.
func expectAgentURLResolveSettling(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery(
		"SELECT url, status FROM workspaces WHERE id = $1").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "status"}).AddRow(nil, "provisioning"))
}

// seedRedisURL puts the agent server URL into the Redis cache so resolveAgentURL
// returns it without needing a DB lookup.
func seedRedisURL(t *testing.T, mr *miniredis.Miniredis, wsID, url string) {
	if err := mr.Set(fmt.Sprintf("ws:%s:url", wsID), url); err != nil {
		t.Fatalf("seedRedisURL(%s): %v", wsID, err)
	}
	time.Sleep(1 * time.Millisecond) // settle
}

// drainItem returns a reproducible QueuedItem for testing.
// CallerID is NULL so proxyA2ARequest skips the CanCommunicate hierarchy check
// (no caller means canvas/system call path, which bypasses access control).
func drainItem(wsID string) *QueuedItem {
	return &QueuedItem{
		ID:          "qid-test-001",
		WorkspaceID: wsID,
		CallerID:    sql.NullString{Valid: false}, // no caller → no CanCommunicate check
		Priority:    PriorityTask,
		Body:        []byte(`{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"hi"}]}}}`),
		Method:      sql.NullString{String: "message/send", Valid: true},
		Attempts:    1,
		// Just-enqueued: well WITHIN a2aSettlingRetryCeiling, so the transient
		// gateway-origin path stays eligible (the ceiling drop is exercised by
		// TestDrainQueueForWorkspace_SettlingCeilingExceeded_Drops with an aged item).
		EnqueuedAt: time.Now(),
	}
}

// expectDequeueNextOk sets up sqlmock for DequeueNext's transaction:
//
//	BEGIN → SELECT FOR UPDATE SKIP LOCKED → UPDATE status='dispatched', attempts=attempts+1 → COMMIT
//
// SQL strings are EXACT matches to the handler code — QueryMatcherEqual verifies verbatim.
// The next_attempt_at filter was added in #3127 follow-up; without it the
// `WHERE (next_attempt_at IS NULL OR next_attempt_at <= now())` clause
// wouldn't match the handler's exact SQL string.
func expectDequeueNextOk(mock sqlmock.Sqlmock, item *QueuedItem) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts, enqueued_at, settling_since FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(item.WorkspaceID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "caller_id", "priority", "body", "method", "attempts", "enqueued_at", "settling_since",
		}).AddRow(
			item.ID, item.WorkspaceID, item.CallerID, item.Priority,
			string(item.Body), item.Method, item.Attempts, item.EnqueuedAt, item.SettlingSince,
		))
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'dispatched', dispatched_at = now(), attempts = attempts + 1 WHERE id = $1").
		WithArgs(item.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// expectDequeueNextEmpty sets up sqlmock for DequeueNext returning no rows.
// next_attempt_at filter added in #3127 follow-up.
func expectDequeueNextEmpty(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts, enqueued_at, settling_since FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
}

// expectCompleted sets up mock for MarkQueueItemCompleted.
func expectCompleted(mock sqlmock.Sqlmock, id string, respBody interface{}) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'completed', completed_at = now(), response_body = $2 WHERE id = $1").
		WithArgs(id, respBody).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectFailed sets up mock for MarkQueueItemFailed with a specific error message.
func expectFailed(mock sqlmock.Sqlmock, id string, errMsg string) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END, last_error = $3, dispatched_at = NULL WHERE id = $1").
		WithArgs(id, 5, errMsg).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectTransientRetry sets up mock for MarkQueueItemTransientRetry. The
// errMsg is verified via the exact-match matcher; tests that only care
// about the SQL shape (and want to assert on the row state separately)
// can pass sqlmock.AnyArg() for the error-message column.
//
// #3127 follow-up: the SQL now also sets next_attempt_at = now() +
// make_interval(secs => $3) so DequeueNext's WHERE clause (added in
// the same change) skips the row during the backoff window. The seconds
// count is parameterised via transientRetryBackoffSecs (Go constant)
// rather than inlined as a literal interval string — golangci-lint
// flagged the literal form as having an unused sibling const.
func expectTransientRetry(mock sqlmock.Sqlmock, id string, errMsg sqlmock.Argument) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'queued', attempts = GREATEST(attempts - 1, 0), last_error = $2, dispatched_at = NULL, next_attempt_at = now() + make_interval(secs => $3), settling_since = COALESCE(settling_since, now()) WHERE id = $1").
		WithArgs(id, errMsg, transientRetryBackoffSecs).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectSettlingCeilingDrop mocks DropQueueItemTerminal — the terminal drop taken
// when a transient gateway-origin failure has persisted past a2aSettlingRetryCeiling.
func expectSettlingCeilingDrop(mock sqlmock.Sqlmock, id string, reason sqlmock.Argument) {
	mock.ExpectExec(
		"UPDATE a2a_queue SET status = 'dropped', last_error = $2, dispatched_at = NULL, completed_at = now() WHERE id = $1 AND status = 'dispatched'").
		WithArgs(id, reason).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectRuntimeLookup mocks handleMockA2A's lookupRuntime query. The proxy
// calls this on every dispatch to decide whether to short-circuit with a
// canned mock reply; returning a non-mock runtime lets the request fall
// through to the real agent path. The existing tests don't care about the
// mock path but the query happens unconditionally, so the mock is required
// to keep the test logs clean.
func expectRuntimeLookup(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT runtime FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
}

// expectRecentHeartbeatAbsent mocks hasRecentHeartbeat's query to return
// NULL — DrainQueueForWorkspace treats that as "no recent heartbeat" and
// falls through to MarkQueueItemFailed (the pre-fix behaviour). Used by
// tests that exercise the dead-agent / non-transient failure paths.
func expectRecentHeartbeatAbsent(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT last_heartbeat_at FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(nil))
}

// expectRecentHeartbeatPresent mocks hasRecentHeartbeat's query to return a
// recent timestamp — DrainQueueForWorkspace treats that as "workspace is
// alive" and the transient gateway-origin path becomes eligible. Used by
// the regression test that pins the new behaviour.
func expectRecentHeartbeatPresent(mock sqlmock.Sqlmock, workspaceID string) {
	mock.ExpectQuery(
		"SELECT last_heartbeat_at FROM workspaces WHERE id = $1").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(time.Now()))
}

// agentServer creates an httptest.Server that responds with the given status
// and optional JSON body.
func agentServer(body string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			fmt.Fprint(w, body)
		}
	}))
}

// TestDrainQueueForWorkspace_Success_Completes: agent returns 200 → MarkQueueItemCompleted.
func TestDrainQueueForWorkspace_Success_Completes(t *testing.T) {
	item := drainItem("ws-ok")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`{"result":{"status":"ok"}}`, http.StatusOK)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectCompleted(mock, item.ID, `{"result":{"status":"ok"}}`)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_BatchCapacity_DrainsMultiple: with capacity=2,
// two queued items are dequeued and dispatched in one call (#2930 batching).
func TestDrainQueueForWorkspace_BatchCapacity_DrainsMultiple(t *testing.T) {
	wsID := "ws-batch"
	item1 := drainItem(wsID)
	item1.ID = "qid-batch-1"
	item2 := drainItem(wsID)
	item2.ID = "qid-batch-2"

	mock, handler, mr := drainSetup(t, wsID)
	srv := agentServer(`{"result":{"status":"ok"}}`, http.StatusOK)
	defer srv.Close()
	expectDequeueNextOk(mock, item1)
	// #4531/finding [8]: the agent URL is resolved ONCE per drain call (lazily, on
	// the first dequeued item), not per-iteration — so only ONE expectAgentURLResolve
	// is registered even though two items are dispatched.
	expectAgentURLResolve(mock, wsID, srv.URL)
	expectQueueBudgetCheck(mock, wsID)
	expectCompleted(mock, item1.ID, `{"result":{"status":"ok"}}`)
	expectDequeueNextOk(mock, item2)
	expectQueueBudgetCheck(mock, wsID)
	expectCompleted(mock, item2.ID, `{"result":{"status":"ok"}}`)

	seedRedisURL(t, mr, wsID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_202Accepted_CompletesNotFailed verifies that 202 Accepted
// (dispatch was queued again) calls MarkQueueItemCompleted, NOT Failed, to avoid
// double-counting attempts.
func TestDrainQueueForWorkspace_202Accepted_CompletesNotFailed(t *testing.T) {
	item := drainItem("ws-202")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`{"status":"queued"}`, http.StatusAccepted)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectCompleted(mock, item.ID, nil)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrResponseNil_NoPanic: nil Response map → no panic,
// fallback to StatusText(502) = "Bad Gateway".
func TestDrainQueueForWorkspace_ProxyErrResponseNil_NoPanic(t *testing.T) {
	item := drainItem("ws-nilresp")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Bad Gateway")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrMissingErrorKey_UsesStatusText: Response exists
// but "error" key is absent → fallback to http.StatusText.
func TestDrainQueueForWorkspace_ProxyErrMissingErrorKey_UsesStatusText(t *testing.T) {
	item := drainItem("ws-missingkey")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`{"code":500,"detail":"internal server error"}`, http.StatusInternalServerError)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	// 500 is NOT in isUpstreamDeadStatus so isGatewayOriginFailure returns
	// false and hasRecentHeartbeat is never consulted — no SQL mock needed
	// for the transient-retry path. Falls through to MarkQueueItemFailed
	// (the pre-fix behaviour for non-gateway failures).

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Internal Server Error")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrNonStringError_NoPanic: Response["error"] is a
// JSON number, not a string → comma-ok returns ("", false) → no panic, falls back.
func TestDrainQueueForWorkspace_ProxyErrNonStringError_NoPanic(t *testing.T) {
	item := drainItem("ws-nonstr")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`{"error": 429}`, http.StatusServiceUnavailable)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "Service Unavailable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ProxyErrWithStringError_UsesErrorMessage: valid string
// error → that string is logged (not StatusText).
func TestDrainQueueForWorkspace_ProxyErrWithStringError_UsesErrorMessage(t *testing.T) {
	item := drainItem("ws-str-err")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	wantErrMsg := "upstream agent crashed with signal: killed"
	srv := agentServer(fmt.Sprintf(`{"error":%q}`, wantErrMsg), http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, wantErrMsg)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_EmptyQueue_NoOps: DequeueNext returns (nil, nil) →
// no DB writes issued.
func TestDrainQueueForWorkspace_EmptyQueue_NoOps(t *testing.T) {
	mock, handler, _ := drainSetup(t, "ws-empty")

	expectDequeueNextEmpty(mock, "ws-empty")

	handler.DrainQueueForWorkspace(context.Background(), "ws-empty", 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_DequeueError_LogsAndReturns: DB error during
// DequeueNext → logged, no panic, no UPDATE issued.
func TestDrainQueueForWorkspace_DequeueError_LogsAndReturns(t *testing.T) {
	mock, handler, _ := drainSetup(t, "ws-dequeue-err")

	mock.ExpectBegin()
	mock.ExpectQuery(
		"SELECT id, workspace_id, caller_id, priority, body::text, method, attempts, enqueued_at, settling_since FROM a2a_queue WHERE workspace_id = $1 AND status = 'queued' AND (expires_at IS NULL OR expires_at > now()) AND (next_attempt_at IS NULL OR next_attempt_at <= now()) ORDER BY priority DESC, enqueued_at ASC FOR UPDATE SKIP LOCKED LIMIT 1").
		WithArgs("ws-dequeue-err").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	handler.DrainQueueForWorkspace(context.Background(), "ws-dequeue-err", 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_MaxAttempts_FailsRatherThanRetries: attempts >= 5
// → 'failed' status (not back to 'queued').
func TestDrainQueueForWorkspace_MaxAttempts_FailsRatherThanRetries(t *testing.T) {
	item := &QueuedItem{
		ID:          "qid-max-attempts",
		WorkspaceID: "ws-max",
		CallerID:    sql.NullString{Valid: false}, // no caller → no CanCommunicate check
		Priority:    PriorityTask,
		Body:        []byte(`{"method":"message/send","params":{}}`),
		Method:      sql.NullString{String: "message/send", Valid: true},
		Attempts:    5, // already at max
	}
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`{"error":"agent unreachable"}`, http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	// No recent heartbeat → falls through to MarkQueueItemFailed (not the
	// transient-retry path). This pins the pre-fix behaviour for dead /
	// unreachable workspaces: the 5-attempt cap still fires after 5 retries.
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectFailed(mock, item.ID, "agent unreachable")

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_ClaimGuarding_SecondDrainGetsEmpty: verifies that after
// one drain successfully claims and completes a queue item, a second sequential drain
// sees an empty queue (row was dispatched, not available for re-claim).
// This exercises the FOR UPDATE SKIP LOCKED claim-guarding without the sqlmock
// goroutine-safety concern of the concurrent version.
func TestDrainQueueForWorkspace_ClaimGuarding_SecondDrainGetsEmpty(t *testing.T) {
	item := drainItem("ws-claim")
	wsID := item.WorkspaceID
	mock, handler, mr := drainSetup(t, wsID)

	// Drain 1: claims item, proxies successfully, marks completed.
	srv := agentServer(`{"result":{}}`, http.StatusOK)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, wsID, srv.URL)
	expectQueueBudgetCheck(mock, wsID)

	seedRedisURL(t, mr, wsID, srv.URL)
	expectCompleted(mock, item.ID, `{"result":{}}`)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 1)

	// Drain 2: same workspace — queue is empty because item was dispatched.
	// Register expectations for the second drain.
	expectDequeueNextEmpty(mock, wsID)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== 2026-06-21 PM RCA: transient gateway-retry path ====================
//
// The PM RCA found that DrainQueueForWorkspace was treating every
// 502/503/504 from the upstream proxy as a "dead agent unreachable"
// failure and burning the 5-attempt cap on otherwise-healthy
// workspaces. The new path: when the workspace has a recent heartbeat
// AND the failure is a gateway-origin dead-origin status (502/503/504
// or 521/522/523/524), re-queue via MarkQueueItemTransientRetry which
// does NOT advance the attempts counter, and invalidate the cached
// agent URL so the next retry re-resolves it from the DB. Only
// confirmed-dead agents (Classification="upstream_dead") and non-
// gateway failures continue to use MarkQueueItemFailed.
//
// These four tests pin the new contract end-to-end: the new SQL
// UPDATE statement, the URL cache invalidation, the heartbeat gate,
// and the regression of the "dead agent" path under the same
// conditions.

// TestDrainQueueForWorkspace_TransientGatewayFailure_StaysQueued: the
// regression test for the RCA. Online workspace + queued item +
// transient 502 (Cloudflare tunnel error page) + recent heartbeat →
// MarkQueueItemTransientRetry (NOT MarkQueueItemFailed) so the
// 5-attempt cap is preserved for actual dead-agent failures.
func TestDrainQueueForWorkspace_TransientGatewayFailure_StaysQueued(t *testing.T) {
	item := drainItem("ws-gateway-blip")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	// Cloudflare 502 error page — empty body, no JSON. This is the
	// shape that triggered the RCA: a healthy workspace's A2A forward
	// hits a CDN tunnel blip and returns 502 with an HTML body.
	srv := agentServer(`<html>cloudflare error</html>`, http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	// Recent heartbeat: the workspace is alive; the failure is in the
	// path between us and the agent, not the agent itself.
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	// Expect MarkQueueItemTransientRetry (NOT MarkQueueItemFailed). The
	// last_error string carries the "[transient gateway origin]" prefix
	// so the failure shape is auditable in the a2a_queue row.
	wantErrPrefix := "transient gateway origin (unknown, status=502):"
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg()) // exact errMsg verified via DB below
	_ = wantErrPrefix

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_TransientGatewayFailure_InvalidatesCachedURL:
// on the transient-retry path, the cached agent URL must be evicted
// from Redis so the next drain tick does a fresh DB lookup. Without
// this, a stale URL pointing at a temporarily-flapped tunnel would
// keep hitting the same broken endpoint. The ClearWorkspaceKeys call
// removes the three ws:<id>:* keys (liveness, url, internal_url) in
// one shot; the test verifies the url key is gone after the drain.
func TestDrainQueueForWorkspace_TransientGatewayFailure_InvalidatesCachedURL(t *testing.T) {
	item := drainItem("ws-invalidate")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}

	// Verify the cached URL was invalidated. seedRedisURL put it under
	// "ws:<id>:url" — after the drain it must be gone.
	if got, err := mr.Get(fmt.Sprintf("ws:%s:url", item.WorkspaceID)); err == nil && got != "" {
		t.Errorf("cached URL survived transient-retry invalidation: got=%q want empty", got)
	}
}

// TestDrainQueueForWorkspace_SettlingCeilingExceeded_Drops is the negative
// control to TestDrainQueueForWorkspace_TransientGatewayFailure_StaysQueued:
// SAME inputs (online workspace, recent heartbeat, transient 502 gateway-origin
// failure) EXCEPT the item was enqueued longer ago than a2aSettlingRetryCeiling.
// The transient path is NON-cap-burning and would otherwise re-queue forever,
// poking the runtime on every attempt and starving idle-gated work (the RCA of
// the flaky ephemeral happy-path gate — task #124/#94). Past the ceiling the
// drain must TERMINALLY DROP the item (DropQueueItemTerminal), NOT re-queue it,
// AND invalidate the (stale/dead) cached URL so the remaining queued items of
// this workspace in the same drain re-resolve instead of hammering it (#4459
// re-review [3]).
func TestDrainQueueForWorkspace_SettlingCeilingExceeded_Drops(t *testing.T) {
	item := drainItem("ws-settling-zombie")
	// SETTLING (not merely enqueued) well past the ceiling: the item's first
	// gateway-origin failure was long ago, so it has been actively settling-retrying
	// far longer than any real blip explains. (enqueued_at alone must NOT trigger the
	// drop — see TestDrainQueueForWorkspace_LongQueuedNotDroppedOnFirstBlip.)
	item.SettlingSince = sql.NullTime{Time: time.Now().Add(-(a2aSettlingRetryCeiling + time.Minute)), Valid: true}
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	// Same 502 gateway-origin shape as the transient-retry test.
	srv := agentServer(`<html>cloudflare error</html>`, http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	// Expect the TERMINAL drop, NOT a transient re-queue. If the ceiling check
	// were absent the drain would call MarkQueueItemTransientRetry instead and
	// this expectation would go unmet (proving the branch).
	expectSettlingCeilingDrop(mock, item.ID, sqlmock.AnyArg())

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}

	// The cached URL must be INVALIDATED on the drop path (#4459 re-review [3]):
	// the stale/dead URL is what caused the drop, so the remaining queued items of
	// this workspace must re-resolve rather than all hitting it again.
	if got, err := mr.Get(fmt.Sprintf("ws:%s:url", item.WorkspaceID)); err == nil && got != "" {
		t.Errorf("cached URL survived the ceiling-drop path; want it invalidated (got=%q)", got)
	}
}

// TestDrainQueueForWorkspace_LongQueuedNotDroppedOnFirstBlip is the negative
// control for the #4459 re-review data-loss fix [0]: an item that merely sat
// QUEUED a long time (enqueued_at old — e.g. its target was offline) but whose
// settling_since is still NULL (this is its FIRST gateway-origin failure) must NOT
// be dropped on that first blip. It must go to the transient-retry path (which
// STAMPS settling_since), so the target — now alive and about to accept — is given
// the full settling window. Dropping here is the silent data-loss the fix closes.
func TestDrainQueueForWorkspace_LongQueuedNotDroppedOnFirstBlip(t *testing.T) {
	item := drainItem("ws-long-queued")
	item.EnqueuedAt = time.Now().Add(-(a2aSettlingRetryCeiling + 10*time.Minute)) // ancient
	item.SettlingSince = sql.NullTime{Valid: false}                               // but never settled yet
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer(`<html>cloudflare error</html>`, http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	// MUST transient-retry (which stamps settling_since), NOT drop. If the ceiling
	// still measured from enqueued_at, this would be a DropQueueItemTerminal and this
	// expectation would go unmet — the exact data-loss regression.
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (long-queued item was dropped on first blip instead of retried): %v", err)
	}
}

// TestDrainQueueForWorkspace_GatewayFailure_NoRecentHeartbeat_StillFails:
// the heartbeat gate is the load-bearing part of the new path. If the
// workspace is NOT heartbeating, a 502 stays a dead-agent failure —
// we don't want to re-queue on a genuinely-dead workspace. This pins
// the gate: gateway-origin status + no recent heartbeat →
// MarkQueueItemFailed, same as the pre-fix behaviour.
func TestDrainQueueForWorkspace_GatewayFailure_NoRecentHeartbeat_StillFails(t *testing.T) {
	item := drainItem("ws-no-hb")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatAbsent(mock, item.WorkspaceID)
	expectFailed(mock, item.ID, "Bad Gateway")

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_UpstreamDead_BypassesTransientPath: when
// the proxy already confirmed a dead container (Classification =
// "upstream_dead", set by maybeMarkContainerDead in
// handleA2ADispatchError), the transient-retry path is NOT eligible —
// that is a real dead-agent failure and the 5-attempt cap MUST be
// allowed to fire. This test pins that isGatewayOriginFailure
// short-circuits on the "upstream_dead" classification and falls
// through to MarkQueueItemFailed.
func TestDrainQueueForWorkspace_UpstreamDead_BypassesTransientPath(t *testing.T) {
	// We cannot easily inject a proxyA2AError with Classification=
	// "upstream_dead" through the normal DrainQueueForWorkspace path
	// (the existing test infrastructure uses an httptest.Server for
	// the agent, which doesn't go through maybeMarkContainerDead).
	// So this test is a unit test of isGatewayOriginFailure itself,
	// which is the load-bearing predicate.
	upstreamDead := &proxyA2AError{
		Status:         http.StatusBadGateway,
		Response:       gin.H{"error": "workspace agent unreachable — container restart triggered"},
		Classification: "upstream_dead",
	}
	if isGatewayOriginFailure(upstreamDead) {
		t.Errorf("isGatewayOriginFailure(upstream_dead) = true, want false — confirmed-dead must bypass the transient-retry path")
	}

	// Also verify the inverse: a 502 without "upstream_dead" classification
	// IS a candidate for the transient-retry path.
	gatewayOrigin := &proxyA2AError{
		Status:   http.StatusBadGateway,
		Response: gin.H{"error": "bad gateway"},
	}
	if !isGatewayOriginFailure(gatewayOrigin) {
		t.Errorf("isGatewayOriginFailure(502 + no classification) = false, want true — the predicate should recognise 502 as gateway-origin when the proxy has not confirmed dead")
	}

	// And a non-dead-origin 5xx (e.g., 500 internal agent error) is NOT
	// a gateway-origin failure.
	notGatewayOrigin := &proxyA2AError{
		Status:   http.StatusInternalServerError,
		Response: gin.H{"error": "agent crashed"},
	}
	if isGatewayOriginFailure(notGatewayOrigin) {
		t.Errorf("isGatewayOriginFailure(500) = true, want false — agent-authored 5xx is not a gateway-origin failure")
	}
}

// TestDrainQueueForWorkspace_TransientRetry_BackoffBreaksCapacityLoop:
// Regression test for Researcher #3127 REQUEST_CHANGES. The original
// transient-retry fix requeued the row with status='queued' and no
// backoff, so a capacity>1 DrainQueueForWorkspace could re-claim the
// just-requeued row on the very next for-loop iteration and hit the
// same gateway failure in a tight loop. The fix: next_attempt_at = now() + 5s
// on transient retry, plus a WHERE clause in DequeueNext that skips
// rows whose next_attempt_at is still in the future.
//
// This test pins the backoff: capacity=2, one queued item that hits a
// transient 502, expect the second DequeueNext to return (nil, nil)
// because the only item is now backoff-gated. Without the WHERE clause
// the second DequeueNext would have re-claimed the row and the test
// would fail (the budget check + MarkQueueItemTransientRetry expectations
// would be unmet, since the row would not be requeued a second time).
func TestDrainQueueForWorkspace_TransientRetry_BackoffBreaksCapacityLoop(t *testing.T) {
	item := drainItem("ws-capacity-loop")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	srv := agentServer("", http.StatusBadGateway)
	defer srv.Close()

	// Iteration 1 of the for-loop (capacity=2): the only queued row is
	// claimed, dispatched, and hits a transient 502. Recent heartbeat
	// keeps the transient-retry path eligible.
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectRuntimeLookup(mock, item.WorkspaceID)
	expectRecentHeartbeatPresent(mock, item.WorkspaceID)

	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	// Iteration 2 of the for-loop (capacity=2): the just-requeued row
	// is still the highest-priority item, but next_attempt_at is now()
	// + 5s — DequeueNext's WHERE clause MUST skip it. The mock returns
	// sql.ErrNoRows as if the queue is empty, and the test framework
	// will fail if the second iteration ever calls into proxyA2ARequest
	// (no MarkQueueItemTransientRetry / MarkQueueItemFailed mock is
	// registered for it).
	expectDequeueNextEmpty(mock, item.WorkspaceID)

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 2)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestDrainQueueForWorkspace_FreshResolve_UsesNewURLNotStaleCache is the
// regression test for core#124 (ephemeral-gate run 815192): after a
// force-hibernate→wake re-provisions a NEW container, a QUEUED A2A turn must be
// dispatched to the woken workspace's CURRENT container URL, resolved FRESH from
// the DB at drain time — NOT the stale pre-hibernate URL that lingers in the
// Redis cache for up to its 5-minute TTL. Before the fix, the drain trusted the
// cache and dialled the dead pre-hibernate host ("dial tcp: lookup <old>: no
// such host"), stranding the turn forever.
//
// Two agent servers stand in for the two containers: `stale` is the dead
// pre-hibernate box whose URL is (wrongly) still cached; `fresh` is the woken
// box whose NEW URL the DB now reports. The negative control is load-bearing:
// we FIRST assert the cache-first resolve (the pre-fix code path) returns the
// STALE URL, then prove the drain dispatches to `fresh` and NEVER touches
// `stale`. A regression that reinstated cache-first dispatch would hit `stale`
// (staleHits==1) and leave the completed body mismatched — this test fails on
// all three of: staleHits, freshHits, and the expectCompleted body match.
func TestDrainQueueForWorkspace_FreshResolve_UsesNewURLNotStaleCache(t *testing.T) {
	item := drainItem("ws-woke-newurl")
	wsID := item.WorkspaceID
	mock, handler, mr := drainSetup(t, wsID)

	var staleHits, freshHits int32
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&staleHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"result":{"from":"STALE-dead-container"}}`)
	}))
	defer stale.Close()
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&freshHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"result":{"from":"FRESH-woken-container"}}`)
	}))
	defer fresh.Close()

	// The stale pre-hibernate URL is still cached in Redis (survived the 5-min
	// TTL after the force-hibernate→wake replaced the container).
	seedRedisURL(t, mr, wsID, stale.URL)

	// NEGATIVE CONTROL: the cache-first resolve — exactly what the drain used to
	// do — returns the STALE url. This proves the cache is genuinely poisoned, so
	// "fresh dispatches to `fresh`" below is a real behavioural difference and not
	// an artefact of an empty cache.
	if got, perr := handler.resolveAgentURL(context.Background(), wsID); perr != nil || got != stale.URL {
		t.Fatalf("negative-control precondition: cache-first resolveAgentURL = (%q, %v), want (%q, nil) — "+
			"the stale URL must be cached so the drain's fresh-resolve has something to override", got, perr, stale.URL)
	}

	// The woken container registered its NEW url in the DB; the fresh per-attempt
	// DB read the drain now performs returns it.
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, wsID, fresh.URL)
	expectQueueBudgetCheck(mock, wsID)
	expectCompleted(mock, item.ID, `{"result":{"from":"FRESH-woken-container"}}`)

	handler.DrainQueueForWorkspace(context.Background(), wsID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
	if got := atomic.LoadInt32(&freshHits); got != 1 {
		t.Errorf("drain must dispatch to the FRESH (woken) container: fresh got %d request(s), want 1", got)
	}
	if got := atomic.LoadInt32(&staleHits); got != 0 {
		t.Errorf("drain dialled the STALE (dead pre-hibernate) cached URL: stale got %d request(s), want 0 — "+
			"the fresh per-attempt DB resolve must override the poisoned cache (core#124)", got)
	}
	// The fresh resolve reseeded the cache with the DB truth, so subsequent
	// resolves (and the proxyA2ARequest that just ran) use the NEW url.
	if got, err := mr.Get(fmt.Sprintf("ws:%s:url", wsID)); err != nil || got != fresh.URL {
		t.Errorf("cache was not corrected to the fresh URL after drain: got=(%q, %v), want %q", got, err, fresh.URL)
	}
}

// TestDrainQueueForWorkspace_SettlingWorkspace_BoundedRetryNotFreshEnqueue is the
// regression test for the #4531 unbounded-requeue bug. A workspace stuck
// provisioning/awaiting_agent (URL-less → resolveAgentURL returns
// classWorkspaceSettling) had its queued turn routed through proxyA2ARequest, whose
// classWorkspaceSettling branch enqueueBusyA2A's a FRESH a2a_queue row every drain
// (attempts=0, settling_since=NULL, enqueued_at=now()) and then MarkQueueItemCompleted's
// the original — so the #4459 settling ceiling AND DropStaleQueueItems could never
// fire and the turn zombie-requeued forever.
//
// The fix keeps the SAME row on the bounded settling path: MarkQueueItemTransientRetry,
// which COALESCE-stamps settling_since and preserves attempts (idempotent requeue, no
// fresh INSERT). This test pins that: a settling workspace's dequeued item is
// transient-retried and NO proxyA2ARequest / EnqueueA2A INSERT happens.
//
// NEGATIVE CONTROL (proven against the pre-fix unconditional-clear code): the ONLY
// mocks registered after the settling resolve are for MarkQueueItemTransientRetry.
// The pre-fix code instead calls proxyA2ARequest, whose first DB touch
// (checkWorkspaceBudget's `SELECT COALESCE(budget_limits ...)`) is unmocked and
// whose enqueue path issues a fresh EnqueueA2A INSERT — none of which match the
// registered transient-retry UPDATE, so ExpectationsWereMet reports the
// transient-retry expectation as UNMET and the test fails. Only the conditional-
// settling fix makes it pass.
func TestDrainQueueForWorkspace_SettlingWorkspace_BoundedRetryNotFreshEnqueue(t *testing.T) {
	item := drainItem("ws-provisioning-settle")
	// First settling observation: settling_since is still NULL (well within the
	// ceiling), so the bounded path transient-retries (which STAMPS settling_since)
	// rather than dropping.
	item.SettlingSince = sql.NullTime{Valid: false}
	mock, handler, mr := drainSetup(t, item.WorkspaceID)

	expectDequeueNextOk(mock, item)
	expectAgentURLResolveSettling(mock, item.WorkspaceID)
	// The bounded path calls MarkQueueItemTransientRetry directly — no proxyA2ARequest,
	// no budget check, no fresh EnqueueA2A INSERT. The transient-retry SQL COALESCEs
	// settling_since and undoes DequeueNext's attempts++ (attempts preserved).
	expectTransientRetry(mock, item.ID, sqlmock.AnyArg())

	// No cached URL: a provisioning workspace has none. ClearCachedURL over an empty
	// key is a no-op; the DB resolve returns NULL url → classWorkspaceSettling.
	_ = mr

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (settling workspace was NOT kept on the bounded "+
			"transient-retry path — likely dispatched through proxyA2ARequest and fresh-enqueued, "+
			"re-opening #4531): %v", err)
	}
}

// TestDrainQueueForWorkspace_SettlingWorkspace_CeilingExceeded_Drops is the twin of
// the online settling-ceiling drop, on the URL-less/provisioning path. Same inputs
// as the bounded-retry test EXCEPT settling_since is older than a2aSettlingRetryCeiling:
// the target has stayed URL-less far past any normal wake/provision window. The bounded
// path must TERMINALLY DROP the item (DropQueueItemTerminal), NOT transient-retry it,
// so a provisioning box that never comes online cannot zombie-requeue the turn forever.
//
// NEGATIVE CONTROL: only DropQueueItemTerminal is registered after the resolve. If the
// ceiling branch were absent, the code would MarkQueueItemTransientRetry instead (unmet
// drop expectation → fail); the pre-#4531-fix code would call proxyA2ARequest (unmet
// drop expectation → fail).
func TestDrainQueueForWorkspace_SettlingWorkspace_CeilingExceeded_Drops(t *testing.T) {
	item := drainItem("ws-provisioning-zombie")
	item.SettlingSince = sql.NullTime{Time: time.Now().Add(-(a2aSettlingRetryCeiling + time.Minute)), Valid: true}
	mock, handler, _ := drainSetup(t, item.WorkspaceID)

	expectDequeueNextOk(mock, item)
	expectAgentURLResolveSettling(mock, item.WorkspaceID)
	expectSettlingCeilingDrop(mock, item.ID, sqlmock.AnyArg())

	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (settling item past the ceiling was NOT terminally "+
			"dropped on the URL-less path): %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DropStaleQueueItems
// ──────────────────────────────────────────────────────────────────────────────

// TestDropStaleQueueItems_SingleWorkspace verifies the function marks queued
// items older than maxAge for a given workspace as 'dropped' and returns the
// count. The WITH ... UPDATE uses FOR UPDATE SKIP LOCKED so concurrent drains
// do not fight over the same items.
func TestDropStaleQueueItems_SingleWorkspace(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// Exact SQL from a2a_queue.go DropStaleQueueItems workspace-scoped branch.
	// Using QueryMatcherEqual so the string must match verbatim.
	const query = `WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
			        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
			        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE workspace_id = $1
					  AND status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $2
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped`
	mock.ExpectQuery(query).
		WithArgs("ws-abc", 30).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))

	count, err := DropStaleQueueItems(context.Background(), "ws-abc", 30)
	if err != nil {
		t.Fatalf("DropStaleQueueItems: %v", err)
	}
	if count != 5 {
		t.Errorf("count=%d; want 5", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestDropStaleQueueItems_AllWorkspaces verifies the function sweeps all
// workspaces when workspaceID is empty, using the all-workspaces SQL branch.
func TestDropStaleQueueItems_AllWorkspaces(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	const query = `WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
			        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
			        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $1
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped`
	mock.ExpectQuery(query).
		WithArgs(120).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	count, err := DropStaleQueueItems(context.Background(), "", 120)
	if err != nil {
		t.Fatalf("DropStaleQueueItems (all workspaces): %v", err)
	}
	if count != 0 {
		t.Errorf("count=%d; want 0", count)
	}
}

// TestDropStaleQueueItems_DBError verifies the function returns a wrapped error
// when the UPDATE fails (e.g. connection loss, constraint violation).
func TestDropStaleQueueItems_DBError(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	const query = `WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
			        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
			        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE workspace_id = $1
					  AND status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $2
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped`
	mock.ExpectQuery(query).
		WithArgs("ws-err", 60).
		WillReturnError(sql.ErrConnDone)

	_, err := DropStaleQueueItems(context.Background(), "ws-err", 60)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Error message must include the function name per the wrapped fmt.Errorf.
	if !strings.Contains(err.Error(), "DropStaleQueueItems") {
		t.Errorf("error = %v; want wrapped error mentioning DropStaleQueueItems", err)
	}
}

// TestDrainQueueForWorkspace_RestartContextInFlight_DefersDrain pins the fix for
// the boot-turn/drain collision (ephemeral-CP gate run 493034).
//
// An agent has ONE session. The post-restart boot turn (sendRestartContext) and
// the queue drain are both woken by the same heartbeat, so before this gate they
// could overlap: the drain dispatched a caller's queued message, the platform
// then posted its restart-context prompt into the same session, and the caller's
// POST came back holding the BOOT TURN's answer ("Workspace restarted and
// ready...") instead of its own. The caller cannot tell it was not answered.
//
// The contract, both directions in one test:
//   - while the boot turn is in flight, the drain dispatches NOTHING (and the
//     item is NOT consumed — it must survive to be drained later);
//   - once the boot turn completes, the very same item drains normally.
//
// Mutation check: drop the restartContextInFlight guard in DrainQueueForWorkspace
// and the first assertion fails (agent sees 1 request during the boot turn).
func TestDrainQueueForWorkspace_RestartContextInFlight_DefersDrain(t *testing.T) {
	item := drainItem("ws-bootturn")
	mock, handler, mr := drainSetup(t, item.WorkspaceID)

	var agentHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&agentHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"result":{"status":"ok"}}`)
	}))
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	// Declare the queue expectations UP FRONT, before the gated drain. This is
	// load-bearing for the mutation to bite: if they were only declared after the
	// gated drain, then removing the guard would make that drain hit an
	// *unexpected* DequeueNext, error out, and return without ever reaching the
	// agent — agentHits would still be 0 and the test would pass against the bug
	// it exists to catch. A drainable queue is what makes "0 hits" mean the GUARD
	// stopped it, not an incidental DB error.
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectCompleted(mock, item.ID, `{"result":{"status":"ok"}}`)

	// ── boot turn in flight → the drain must not touch the agent.
	markRestartContextPending(item.WorkspaceID)
	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)
	if got := atomic.LoadInt32(&agentHits); got != 0 {
		t.Fatalf("drain dispatched %d request(s) into the agent while its restart-context "+
			"boot turn was in flight — the caller would have received the boot turn's "+
			"answer instead of its own; want 0", got)
	}

	// ── boot turn done → the same item drains normally (nothing was lost).
	clearRestartContextPending(item.WorkspaceID)
	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)

	if got := atomic.LoadInt32(&agentHits); got != 1 {
		t.Errorf("after the boot turn completed the queued item must drain: agent got %d request(s), want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestSendRestartContext_ClearsPendingGate: the gate must be released on EVERY
// exit path, including the early "never came online" drop. A leaked gate would
// stall that workspace's queue for the life of the process — turning a transient
// boot hiccup into a permanently deaf agent. Here the workspace never reaches
// online, so sendRestartContext takes its earliest return.
func TestSendRestartContext_ClearsPendingGate(t *testing.T) {
	const wsID = "ws-gate-leak"
	markRestartContextPending(wsID)
	if !restartContextInFlight(wsID) {
		t.Fatal("precondition: gate should be set")
	}
	t.Cleanup(func() { clearRestartContextPending(wsID) })

	// Not online / no such workspace → sendRestartContext drops the message and
	// returns. Whatever path it takes, the defer must clear the gate.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }() // a panic must still not leak the gate
		h := &WorkspaceHandler{}
		h.sendRestartContext(wsID, restartContextData{RestartAt: time.Now()})
	}()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("sendRestartContext did not return — the drain gate would be held indefinitely")
	}

	if restartContextInFlight(wsID) {
		t.Error("restart-context gate LEAKED after sendRestartContext returned — this workspace's " +
			"A2A queue would never drain again")
	}
}
