package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// requestColumnNames mirrors the requestColumns SELECT projection order so the
// sqlmock rows line up with scanRequest.
var requestColumnNames = []string{
	"id", "kind", "requester_type", "requester_id", "org_id",
	"recipient_type", "recipient_id", "title", "detail", "status",
	"responder_type", "responder_id", "priority", "created_at", "updated_at", "responded_at",
}

// oneRequestRow builds a single-row result set for a Get/List SELECT.
func oneRequestRow(id, kind, requesterID, recipientType, recipientID, status string) *sqlmock.Rows {
	return sqlmock.NewRows(requestColumnNames).AddRow(
		id, kind, "agent", requesterID, nil,
		recipientType, recipientID, "Some title", nil, status,
		nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
	)
}

// ---------- Create ----------

func TestRequests_Create_Task(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Create handler first resolves the org root (org_scope CTE).
	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))
	// INSERT request → id
	mock.ExpectQuery("INSERT INTO requests").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-1"))
	// REQUEST_CREATED broadcast (recipient is a user here → anchors on requester agent)
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"kind":"task","recipient_type":"user","recipient_id":"","title":"Review the launch draft","detail":"posts/launch.md"}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["request_id"] != "req-1" {
		t.Errorf("expected request_id req-1, got %v", resp["request_id"])
	}
	if resp["status"] != "pending" {
		t.Errorf("expected status pending, got %v", resp["status"])
	}
}

func TestRequests_Create_Approval_AgentRecipient(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))
	mock.ExpectQuery("INSERT INTO requests").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-2"))
	// Recipient is an agent → REQUEST_CREATED anchors on the recipient workspace.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	body := `{"kind":"approval","recipient_type":"agent","recipient_id":"ws-2","title":"Approve deploy"}`
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Create_MissingTitle(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"kind":"task","recipient_type":"user"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", w.Code)
	}
}

func TestRequests_Create_InvalidKind(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// org root resolves, then Create rejects the kind before any INSERT.
	mock.ExpectQuery("WITH RECURSIVE org_chain").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"kind":"banana","recipient_type":"user","title":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid kind, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Inbox vs Outgoing listing ----------

func TestRequests_ListInbox(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-2").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListInbox(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0]["id"] != "req-1" {
		t.Errorf("expected one inbox request req-1, got %v", resp)
	}
}

func TestRequests_ListInbox_WithStatusFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-2", "pending").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	c.Request = httptest.NewRequest("GET", "/?status=pending", nil)

	handler.ListInbox(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_ListOutgoing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests").
		WithArgs("agent", "ws-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "done"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListOutgoing(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 || resp[0]["status"] != "done" {
		t.Errorf("expected one outgoing request status done, got %v", resp)
	}
}

// ---------- Get (with thread) ----------

func TestRequests_Get_WithThread(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "info_requested"))
	mock.ExpectQuery("FROM request_messages WHERE request_id").
		WithArgs("req-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "author_type", "author_id", "body", "created_at"}).
			AddRow("msg-1", "req-1", "user", "u-1", "what file?", "2026-06-10T01:00:00Z"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["request"] == nil || resp["messages"] == nil {
		t.Errorf("expected request + messages keys, got %v", resp)
	}
}

func TestRequests_Get_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows(requestColumnNames))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "nope"}}
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.Get(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- Respond (valid + invalid action-for-kind) ----------

func TestRequests_Respond_ApprovalApproved(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Respond does Get first to validate action↔kind.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "user", "", "pending"))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// REQUEST_RESPONDED broadcast (anchored on requester agent ws-1).
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"approved","responder_id":"u-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "approved" {
		t.Errorf("expected status approved, got %v", resp["status"])
	}
}

func TestRequests_Respond_TaskDone(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "user", "", "pending"))
	mock.ExpectExec("UPDATE requests").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Respond_InvalidActionForKind(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// kind=approval cannot be "done" — Get succeeds, actionToStatus rejects.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "approval", "ws-1", "user", "", "pending"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for done-on-approval, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Respond_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("nope").
		WillReturnRows(sqlmock.NewRows(requestColumnNames))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "nope"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"done"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Respond(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- AddMessage → info_requested when recipient asks ----------

func TestRequests_AddMessage_RecipientFlipsInfoRequested(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Author is the recipient agent ws-2 → flips to info_requested.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "pending"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-1"))
	// recipient-authored → status flip UPDATE
	mock.ExpectExec("UPDATE requests SET status = 'info_requested'").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// REQUEST_MESSAGE broadcast (anchored on requester ws-1)
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"which file?","author_type":"agent","author_id":"ws-2"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["message_id"] != "msg-1" {
		t.Errorf("expected message_id msg-1, got %v", resp["message_id"])
	}
}

func TestRequests_AddMessage_RequesterDoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// Author is the requester ws-1 (not recipient) → NO info_requested flip.
	mock.ExpectQuery("FROM requests WHERE id").
		WithArgs("req-1").
		WillReturnRows(oneRequestRow("req-1", "task", "ws-1", "agent", "ws-2", "info_requested"))
	mock.ExpectQuery("INSERT INTO request_messages").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("msg-2"))
	// No status-flip UPDATE expected here.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"body":"posts/launch.md","author_type":"agent","author_id":"ws-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.AddMessage(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- Cancel ----------

func TestRequests_Cancel_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE requests SET status = 'cancelled'").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "req-1"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Cancel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_Cancel_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectExec("UPDATE requests SET status = 'cancelled'").
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "requestId", Value: "gone"}}
	c.Request = httptest.NewRequest("POST", "/", nil)

	handler.Cancel(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ---------- ListPending (org view) + kind filter ----------

// pendingColumnNames mirrors ListPendingForOrg's projection (requestColumns + w.name).
var pendingColumnNames = append(append([]string{}, requestColumnNames...), "workspace_name")

func onePendingRow(id, kind string) *sqlmock.Rows {
	return sqlmock.NewRows(pendingColumnNames).AddRow(
		id, kind, "agent", "ws-1", "org-root-1",
		"user", "", "Some title", nil, "pending",
		nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
		"Marketing Agent",
	)
}

func TestRequests_ListPending_NoFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	mock.ExpectQuery("FROM requests r").
		WillReturnRows(onePendingRow("req-1", "task").AddRow(
			"req-2", "approval", "agent", "ws-2", "org-root-1",
			"user", "", "Approve deploy", nil, "pending",
			nil, nil, nil, "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z", nil,
			"Ops Agent",
		))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)

	handler.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 2 {
		t.Errorf("expected 2 pending, got %d (%v)", len(resp), resp)
	}
	if resp[0]["workspace_name"] != "Marketing Agent" {
		t.Errorf("expected decorated workspace_name, got %v", resp[0]["workspace_name"])
	}
}

func TestRequests_ListPending_KindFilter(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	// ?kind=approval must add an arg.
	mock.ExpectQuery("FROM requests r").
		WithArgs("approval").
		WillReturnRows(onePendingRow("req-2", "approval"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/?kind=approval", nil)

	handler.ListPending(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequests_ListPending_InvalidKind(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewRequestsHandler(newTestBroadcaster())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/?kind=banana", nil)

	handler.ListPending(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid kind, got %d", w.Code)
	}
}
