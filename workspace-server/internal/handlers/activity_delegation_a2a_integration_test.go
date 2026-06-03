//go:build integration
// +build integration

// activity_delegation_a2a_integration_test.go — REAL Postgres integration tests
// for Activity, Delegation, and A2A Queue handlers (#2151 CHUNK 1).
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on every
// PR that touches workspace-server/internal/handlers/**.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// integrationDB_ActivityDelegationA2A opens a connection from
// $INTEGRATION_DB_URL (skipping the test if unset), wipes the tables
// used by these tests, hot-swaps the package-level db.DB, and registers
// a Cleanup that restores the previous db.DB + closes the connection.
//
// NOT SAFE FOR `t.Parallel()` — each test gets the tables to itself.
func integrationDB_ActivityDelegationA2A(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if _, err := conn.ExecContext(ctx2, `
		DELETE FROM a2a_queue WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM delegations WHERE caller_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-2151-%');
		DELETE FROM workspaces WHERE name LIKE 'test-2151-%';
	`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWorkspace inserts a workspace with the given name and returns its id.
func seedWorkspace(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (id, name, status)
		VALUES (gen_random_uuid(), $1, 'online')
		RETURNING id
	`, name).Scan(&id); err != nil {
		t.Fatalf("seedWorkspace %q: %v", name, err)
	}
	return id
}

// seedActivityLog inserts an activity_logs row and returns its id.
func seedActivityLog(t *testing.T, conn *sql.DB, workspaceID, activityType, method, status string, sourceID, targetID *string) string {
	t.Helper()
	var id string
	reqBody := map[string]interface{}{"test": true}
	reqJSON, _ := json.Marshal(reqBody)
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, status, source_id, target_id, request_body)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		RETURNING id
	`, workspaceID, activityType, method, status, sourceID, targetID, string(reqJSON)).Scan(&id); err != nil {
		t.Fatalf("seedActivityLog: %v", err)
	}
	return id
}

// seedA2AQueueItem inserts an a2a_queue row and returns its id.
func seedA2AQueueItem(t *testing.T, conn *sql.DB, workspaceID, callerID string, priority int, body []byte, status string) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, status)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		RETURNING id
	`, workspaceID, callerID, priority, string(body), status).Scan(&id); err != nil {
		t.Fatalf("seedA2AQueueItem: %v", err)
	}
	return id
}

// noOpEmitter is a test-only stub that satisfies events.EventEmitter.
type noOpEmitter struct{}

func (noOpEmitter) RecordAndBroadcast(ctx context.Context, eventType string, workspaceID string, payload interface{}) error { return nil }
func (noOpEmitter) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {}

// newTestGinContext creates a gin.Context with an httptest recorder.
func newTestGinContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

// ---------- Activity handler integration tests ----------

func TestIntegration_ActivityList_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-activity-list")
	seedActivityLog(t, conn, wsID, "agent_log", "test_method", "ok", nil, nil)

	h := NewActivityHandler(nil)
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("List returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(resp))
	}
}

// TODO(#2151): Activity List filter matrix (type, source, since_secs, since_id, peer_id, include=peer_info, before_ts)
// TODO(#2151): Activity Report + source_id spoof guard
// TODO(#2151): SessionSearch basic + empty query
// TODO(#2151): Notify with attachments validation

// ---------- Delegation handler integration tests ----------

func TestIntegration_DelegationList_Basic(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-delegation-list")

	// Insert a delegation row via the legacy activity_logs path
	taskJSON, _ := json.Marshal(map[string]interface{}{"task": "hello", "delegation_id": "del-test-1"})
	respJSON, _ := json.Marshal(map[string]interface{}{"delegation_id": "del-test-1"})
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, source_id, target_id, summary, request_body, response_body, status)
		VALUES ($1, 'delegation', 'delegate', $1, $1, 'Delegating to test', $2::jsonb, $3::jsonb, 'pending')
	`, wsID, string(taskJSON), string(respJSON)); err != nil {
		t.Fatalf("seed delegation: %v", err)
	}

	wh := &WorkspaceHandler{broadcaster: noOpEmitter{}}
	dh := NewDelegationHandler(wh, noOpEmitter{})
	c, w := newTestGinContext()
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	dh.ListDelegations(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ListDelegations returned %d, want 200", w.Code)
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected 1 delegation, got %d", len(resp))
	}
}

// TODO(#2151): Delegate endpoint (idempotency, self-delegation guard, success path)
// TODO(#2151): Record endpoint
// TODO(#2151): UpdateStatus endpoint

// ---------- A2A Queue integration tests ----------

func TestIntegration_A2AQueue_EnqueueAndDepth(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-queue")
	callerID := seedWorkspace(t, conn, "test-2151-a2a-caller")

	body := []byte(`{"method":"message/send","params":{"message":{"text":"hi"}}}`)
	id, depth, err := EnqueueA2A(context.Background(), wsID, callerID, PriorityTask, body, "message/send", "", nil)
	if err != nil {
		t.Fatalf("EnqueueA2A: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty queue id")
	}
	if depth != 1 {
		t.Fatalf("expected depth 1, got %d", depth)
	}

	// Verify row exists
	var status string
	if err := conn.QueryRowContext(context.Background(), `SELECT status FROM a2a_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("select queue row: %v", err)
	}
	if status != "queued" {
		t.Fatalf("expected status queued, got %s", status)
	}

	// Verify depth
	d := QueueDepth(context.Background(), wsID)
	if d != 1 {
		t.Fatalf("expected QueueDepth 1, got %d", d)
	}
}

func TestIntegration_A2AQueue_DequeueNext(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	wsID := seedWorkspace(t, conn, "test-2151-a2a-dequeue")
	body := []byte(`{"test":true}`)
	seedA2AQueueItem(t, conn, wsID, "", PriorityTask, body, "queued")

	item, err := DequeueNext(context.Background(), wsID)
	if err != nil {
		t.Fatalf("DequeueNext: %v", err)
	}
	if item == nil {
		t.Fatal("expected item, got nil")
	}

	// Verify status flipped to dispatched
	var status string
	if err := conn.QueryRowContext(context.Background(), `SELECT status FROM a2a_queue WHERE id = $1`, item.ID).Scan(&status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "dispatched" {
		t.Fatalf("expected dispatched, got %s", status)
	}
}

// TODO(#2151): A2A Queue Status endpoint (auth rules, 404 vs 403, response body inclusion)
// TODO(#2151): A2A Queue idempotency conflict
// TODO(#2151): A2A Queue MarkQueueItemCompleted / Failed
// TODO(#2151): A2A Queue DropStaleQueueItems
