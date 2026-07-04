package handlers

// Unit (sqlmock) coverage for MUST-FIX 3 (durable inbox delivery ack) and
// MUST-FIX 4 (unconditional delegate_result on the FAILED branch).
//
// These are fast, DB-less statement/shape assertions. The behavioural proof
// (GREATEST actually keeps the higher value; the prune actually retains
// unacked rows; the failed row is actually observable) lives in the real
// Postgres integration tests (inbox_delivery_ack_integration_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ackTestEmitter is a no-op events.EventEmitter — using it for the delegation
// handler avoids the structure_events INSERT a real Broadcaster would fire,
// so the mock only has to model the two writes under test.
type ackTestEmitter struct{}

func (ackTestEmitter) RecordAndBroadcast(ctx context.Context, eventType, workspaceID string, payload interface{}) error {
	return nil
}
func (ackTestEmitter) BroadcastOnly(workspaceID, eventType string, payload interface{}) {}

// postAck builds a POST /workspaces/:id/activity/ack gin context with a JSON
// body. Shared with the integration test (which compiles this untagged file
// under -tags=integration too).
func postAck(wsID, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/activity/ack", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

// postDelegationUpdate builds a POST
// /workspaces/:id/delegations/:delegation_id/update gin context.
func postDelegationUpdate(wsID, delegationID, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "delegation_id", Value: delegationID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/delegations/"+delegationID+"/update", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

// ---------- MUST-FIX 3: Ack handler ----------

// TestActivityAck_MonotonicUpsertShape pins the monotonic max-advance UPSERT.
// The expected query REQUIRES the `ON CONFLICT (workspace_id) ... GREATEST(`
// shape — a regression to a plain `SET last_acked_seq = EXCLUDED.last_acked_seq`
// overwrite would not match, the QueryRow would error, and the handler would
// 500, failing this test loudly.
func TestActivityAck_MonotonicUpsertShape(t *testing.T) {
	mock := setupTestDB(t)
	h := NewActivityHandler(newTestBroadcaster())

	const wsID = "ws-ack-advance"
	mock.ExpectQuery(`(?s)INSERT INTO inbox_delivery_state.+ON CONFLICT \(workspace_id\).+GREATEST\(`).
		WithArgs(wsID, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"last_acked_seq"}).AddRow(int64(7)))

	c, w := postAck(wsID, `{"acked_seq": 7}`)
	h.Ack(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Ack returned %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := decodeAckSeq(t, w); got != 7 {
		t.Fatalf("last_acked_seq = %d, want 7", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestActivityAck_SurfacesStoredGreatest documents the idempotent/no-op arm:
// when the stored cursor is already higher than the incoming ack (a later ack
// won the race, or this is a duplicate/stale ack), GREATEST keeps the stored
// value and the handler RETURNs the authoritative higher number.
func TestActivityAck_SurfacesStoredGreatest(t *testing.T) {
	mock := setupTestDB(t)
	h := NewActivityHandler(newTestBroadcaster())

	const wsID = "ws-ack-noop"
	mock.ExpectQuery(`(?s)INSERT INTO inbox_delivery_state.+GREATEST\(`).
		WithArgs(wsID, int64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"last_acked_seq"}).AddRow(int64(10)))

	c, w := postAck(wsID, `{"acked_seq": 3}`)
	h.Ack(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Ack returned %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := decodeAckSeq(t, w); got != 10 {
		t.Fatalf("last_acked_seq = %d, want 10 (stored GREATEST, not the lower incoming 3)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestActivityAck_ZeroIsValid — acked_seq 0 is a valid (no-op) ack, not a
// missing-field error. The UPSERT still fires.
func TestActivityAck_ZeroIsValid(t *testing.T) {
	mock := setupTestDB(t)
	h := NewActivityHandler(newTestBroadcaster())

	const wsID = "ws-ack-zero"
	mock.ExpectQuery(`(?s)INSERT INTO inbox_delivery_state`).
		WithArgs(wsID, int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"last_acked_seq"}).AddRow(int64(0)))

	c, w := postAck(wsID, `{"acked_seq": 0}`)
	h.Ack(c)

	if w.Code != http.StatusOK {
		t.Fatalf("Ack(0) returned %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestActivityAck_RejectsBadBody — a missing/negative/malformed acked_seq is a
// 400 at the trust boundary and MUST NOT touch the DB (no mock expectations
// set → any query would be an unexpected-call failure).
func TestActivityAck_RejectsBadBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing", `{}`},
		{"negative", `{"acked_seq": -1}`},
		{"malformed", `not json`},
		{"wrong_type", `{"acked_seq": "five"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = setupTestDB(t) // any DB call would fail as unexpected
			h := NewActivityHandler(newTestBroadcaster())
			c, w := postAck("ws-ack-bad", tc.body)
			h.Ack(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("Ack(%s) returned %d, want 400; body=%s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// ---------- MUST-FIX 4: FAILED branch emits delegate_result ----------

// TestUpdateStatusFailed_EmitsDelegateResultRow proves the FAILED branch now
// writes a delegate_result activity row UNCONDITIONALLY (both ledger + inbox
// push flags default off), mirroring the COMPLETED branch. Two ordered Execs
// are expected: the status-flip UPDATE, then the new delegate_result INSERT
// with status 'failed' + the error carried in error_detail.
func TestUpdateStatusFailed_EmitsDelegateResultRow(t *testing.T) {
	mock := setupTestDB(t)
	h := NewDelegationHandler(&WorkspaceHandler{broadcaster: ackTestEmitter{}}, ackTestEmitter{})

	const wsID = "ws-deleg-failed"
	const delID = "del-failed-1"

	// 1) updateDelegationStatus flips the original 'delegate' row.
	mock.ExpectExec(`(?s)UPDATE activity_logs`).
		WithArgs("failed", "boom", wsID, delID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 2) MUST-FIX 4: the new unconditional delegate_result row.
	mock.ExpectExec(`(?s)INSERT INTO activity_logs.+delegate_result.+failed`).
		WithArgs(wsID, wsID, sqlmock.AnyArg(), sqlmock.AnyArg(), "boom").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, w := postDelegationUpdate(wsID, delID, `{"status":"failed","error":"boom"}`)
	h.UpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateStatus(failed) returned %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("FAILED branch did not emit the delegate_result INSERT: %v", err)
	}
}

func decodeAckSeq(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var resp struct {
		LastAckedSeq int64 `json:"last_acked_seq"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal ack response %q: %v", w.Body.String(), err)
	}
	return resp.LastAckedSeq
}
