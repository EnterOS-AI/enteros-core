//go:build integration
// +build integration

// mail_summary_integration_test.go — REAL Postgres proof of the idle-digest
// mail-summary aggregate (task #219 phase-2, D5 ruling, issue #4308).
//
// Pins the D5 semantics against live ledgers:
//   - received_unread counts ONLY a2a_receive rows past the acked floor
//     (unread, not lifetime total — CTO-confirmed semantics), excluding
//     delegate_result rows;
//   - replies_unread counts delegate_result rows past the same floor;
//   - a workspace with NO inbox_delivery_state row (push fleet) reports
//     mode=queued_backlog and counts the platform-queued backlog instead;
//   - sent_awaiting_reply counts the caller's non-terminal delegations, and
//     the >threshold subset lands in `overdue` (oldest first, with target +
//     age) — the "target agent may have an issue" warning feed.
//
// Same harness as activity_delegation_a2a_integration_test.go:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_MailSummary
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

func mailSummaryGET(t *testing.T, wsID, query string) map[string]interface{} {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewMailSummaryHandler()
	r.GET("/workspaces/:id/mail/summary", h.Summary)
	req := httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/mail/summary"+query, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("mail summary: HTTP %d: %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("mail summary: bad JSON: %v", err)
	}
	return out
}

// seedReceivedWithMessageID inserts an a2a_receive activity_logs row carrying a
// real per-message identity (message_id + source_id) and returns its seq.
func seedReceivedWithMessageID(t *testing.T, conn *sql.DB, wsID, peerID, method, messageID string) int64 {
	t.Helper()
	var seq int64
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, status, source_id, target_id, request_body, message_id)
		VALUES ($1, 'a2a_receive', $2, 'ok', $3, $1, $4::jsonb, $5)
		RETURNING seq
	`, wsID, method, peerID,
		`{"params":{"message":{"messageId":"`+messageID+`"}}}`, messageID).Scan(&seq); err != nil {
		t.Fatalf("seedReceivedWithMessageID: %v", err)
	}
	return seq
}

func seedDelegationAt(t *testing.T, callerID, calleeID, status string, createdAt time.Time) string {
	t.Helper()
	var id string
	if err := db.DB.QueryRowContext(context.Background(), `
		INSERT INTO delegations (delegation_id, caller_id, callee_id, task_preview, status, created_at, updated_at)
		VALUES (gen_random_uuid()::text, $1, $2, 'mail-summary test', $3, $4, $4)
		RETURNING delegation_id
	`, callerID, calleeID, status, createdAt).Scan(&id); err != nil {
		t.Fatalf("seedDelegationAt: %v", err)
	}
	return id
}

func TestIntegration_MailSummary_AckedFloorSemantics(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-floor")
	peer := seedWorkspace(t, conn, "test-2151-mailsum-peer")

	// 5 inbound messages + 2 delegate_result replies.
	var seqs []int64
	for i := 0; i < 5; i++ {
		id := seedActivityLog(t, conn, ws, "a2a_receive", "message/send", "ok", &peer, nil)
		var seq int64
		if err := conn.QueryRowContext(context.Background(),
			`SELECT seq FROM activity_logs WHERE id = $1`, id).Scan(&seq); err != nil {
			t.Fatalf("seq read: %v", err)
		}
		seqs = append(seqs, seq)
	}
	for i := 0; i < 2; i++ {
		seedActivityLog(t, conn, ws, "a2a_receive", "delegate_result", "ok", &ws, nil)
	}

	// Floor at the 3rd message: 2 messages + both replies remain unread.
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO inbox_delivery_state (workspace_id, last_acked_seq)
		VALUES ($1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET last_acked_seq = EXCLUDED.last_acked_seq
	`, ws, seqs[2]); err != nil {
		t.Fatalf("floor seed: %v", err)
	}

	out := mailSummaryGET(t, ws, "")
	if got := out["mode"]; got != "acked_seq" {
		t.Fatalf("mode = %v, want acked_seq", got)
	}
	if got := out["received_unread"].(float64); got != 2 {
		t.Fatalf("received_unread = %v, want 2 (UNREAD past the floor, not lifetime total)", got)
	}
	if got := out["replies_unread"].(float64); got != 2 {
		t.Fatalf("replies_unread = %v, want 2", got)
	}
}

// TestIntegration_MailSummary_ReceivedIdentitiesAckedMode is the core#5028
// regression: the response must carry a stable PER-MESSAGE identity for each
// unread inbound, not only the aggregate count. The digest provider hashes
// these ids; with only a count, a read + a fresh arrival between two ticks
// leaves the count (and therefore the hash) unchanged and the new inbound is
// never surfaced.
func TestIntegration_MailSummary_ReceivedIdentitiesAckedMode(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-recv-ids")
	peer := seedWorkspace(t, conn, "test-2151-mailsum-recv-ids-peer")

	// One acked (below the floor) + two unread inbound + one unread reply.
	acked := seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "msg-acked")
	seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "msg-unread-1")
	seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "msg-unread-2")
	seedReceivedWithMessageID(t, conn, ws, peer, "delegate_result", "msg-reply-1")

	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO inbox_delivery_state (workspace_id, last_acked_seq)
		VALUES ($1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET last_acked_seq = EXCLUDED.last_acked_seq
	`, ws, acked); err != nil {
		t.Fatalf("floor seed: %v", err)
	}

	out := mailSummaryGET(t, ws, "")
	if got := out["received_unread"].(float64); got != 2 {
		t.Fatalf("received_unread = %v, want 2", got)
	}
	raw, ok := out["received"]
	if !ok {
		t.Fatalf("response has NO `received` key — per-message identity is not projected (core#5028). keys=%v", out)
	}
	received, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("`received` = %T, want a list", raw)
	}
	// Unread inbound AND unread replies are both inbound mail the digest must
	// re-surface: 2 messages + 1 delegate_result = 3 identities past the floor.
	if len(received) != 3 {
		t.Fatalf("received = %d entries, want 3 (2 unread + 1 unread reply past the floor); got %v", len(received), received)
	}
	byMsgID := map[string]map[string]interface{}{}
	for _, e := range received {
		entry := e.(map[string]interface{})
		id, _ := entry["id"].(string)
		if id == "" {
			t.Fatalf("received entry has no stable `id`: %v", entry)
		}
		mid, _ := entry["message_id"].(string)
		byMsgID[mid] = entry
	}
	for _, want := range []string{"msg-unread-1", "msg-unread-2", "msg-reply-1"} {
		if _, ok := byMsgID[want]; !ok {
			t.Fatalf("message_id %q missing from received=%v", want, received)
		}
	}
	// The ACKED message must NOT be re-surfaced — the floor is the read marker.
	if _, ok := byMsgID["msg-acked"]; ok {
		t.Fatalf("acked message re-surfaced in received: %v", received)
	}
	// Sender + method ride along so a reconciler can correlate without a body.
	if got := byMsgID["msg-unread-1"]["sender_id"]; got != peer {
		t.Fatalf("sender_id = %v, want the peer workspace %s", got, peer)
	}
	if got := byMsgID["msg-reply-1"]["method"]; got != "delegate_result" {
		t.Fatalf("method = %v, want delegate_result", got)
	}
	// seq is the monotonic ordering key the reconciler pages on.
	if _, ok := byMsgID["msg-unread-1"]["seq"].(float64); !ok {
		t.Fatalf("received entry carries no seq: %v", byMsgID["msg-unread-1"])
	}
}

// TestIntegration_MailSummary_EqualCountsDifferentMessages is the
// compensating-churn proof, server side: between two ticks the agent reads one
// message and one new one arrives. received_unread is IDENTICAL across the two
// reads — only the identity list distinguishes them. A digest that hashes only
// the count is structurally blind here.
func TestIntegration_MailSummary_EqualCountsDifferentMessages(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-churn")
	peer := seedWorkspace(t, conn, "test-2151-mailsum-churn-peer")

	seqA := seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "churn-A")
	seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "churn-B")

	setFloor := func(seq int64) {
		t.Helper()
		if _, err := conn.ExecContext(context.Background(), `
			INSERT INTO inbox_delivery_state (workspace_id, last_acked_seq)
			VALUES ($1, $2)
			ON CONFLICT (workspace_id) DO UPDATE SET last_acked_seq = EXCLUDED.last_acked_seq
		`, ws, seq); err != nil {
			t.Fatalf("floor seed: %v", err)
		}
	}
	ids := func(out map[string]interface{}) []string {
		t.Helper()
		raw, ok := out["received"]
		if !ok {
			t.Fatalf("response has NO `received` key — core#5028 not fixed. keys=%v", out)
		}
		var got []string
		for _, e := range raw.([]interface{}) {
			entry := e.(map[string]interface{})
			mid, _ := entry["message_id"].(string)
			got = append(got, mid)
		}
		return got
	}

	// Tick 1: floor below both -> A and B unread.
	setFloor(seqA - 1)
	tick1 := mailSummaryGET(t, ws, "")

	// Between ticks: A is read (floor advances past it) and C arrives.
	setFloor(seqA)
	seedReceivedWithMessageID(t, conn, ws, peer, "message/send", "churn-C")
	tick2 := mailSummaryGET(t, ws, "")

	if tick1["received_unread"].(float64) != tick2["received_unread"].(float64) {
		t.Fatalf("precondition broken: counts differ (%v vs %v) — this test only proves anything when they MATCH",
			tick1["received_unread"], tick2["received_unread"])
	}
	got1, got2 := ids(tick1), ids(tick2)
	if len(got1) != 2 || len(got2) != 2 {
		t.Fatalf("want 2 identities per tick, got %v and %v", got1, got2)
	}
	if fmt.Sprint(got1) == fmt.Sprint(got2) {
		t.Fatalf("identity list is IDENTICAL across a compensating churn (%v) — the new inbound churn-C is invisible", got1)
	}
	joined := fmt.Sprint(got2)
	if !strings.Contains(joined, "churn-C") {
		t.Fatalf("the brand-new inbound churn-C is missing from tick 2: %v", got2)
	}
	if strings.Contains(joined, "churn-A") {
		t.Fatalf("the read message churn-A is still reported unread in tick 2: %v", got2)
	}
}

// TestIntegration_MailSummary_OverdueCountIsUncapped pins the second live bug:
// the runtime falls back to len(overdue) "only for a server that predates
// overdue_count" — but the server never emitted the field, so the capped
// fallback was permanent and 25 overdue rendered as 10.
func TestIntegration_MailSummary_OverdueCountIsUncapped(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-overdue-n")
	peer := seedWorkspace(t, conn, "test-2151-mailsum-overdue-n-peer")

	now := time.Now().UTC()
	const overdueN = 13 // > mailOverdueListCap (10)
	for i := 0; i < overdueN; i++ {
		seedDelegationAt(t, ws, peer, "dispatched", now.Add(-7*time.Hour))
	}
	seedDelegationAt(t, ws, peer, "dispatched", now.Add(-1*time.Minute)) // not overdue

	out := mailSummaryGET(t, ws, "")
	if got := out["sent_awaiting_reply"].(float64); got != overdueN+1 {
		t.Fatalf("sent_awaiting_reply = %v, want %d", got, overdueN+1)
	}
	if got := len(out["overdue"].([]interface{})); got != mailOverdueListCap {
		t.Fatalf("overdue sample = %d, want the cap %d", got, mailOverdueListCap)
	}
	rawN, ok := out["overdue_count"]
	if !ok {
		t.Fatalf("response has NO `overdue_count` — the runtime's len(overdue) fallback is permanent and under-reports %d as %d. keys=%v",
			overdueN, mailOverdueListCap, out)
	}
	if got := rawN.(float64); got != overdueN {
		t.Fatalf("overdue_count = %v, want the UNCAPPED %d", got, overdueN)
	}
}

func TestIntegration_MailSummary_SentAwaitingAndOverdue(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-sent")
	peer := seedWorkspace(t, conn, "test-2151-mailsum-sent-peer")

	now := time.Now().UTC()
	// 3 in-flight sends: one fresh, one 7h old (overdue at the 6h default),
	// one 30m old. Plus one completed (terminal — never counted).
	seedDelegationAt(t, ws, peer, "dispatched", now.Add(-5*time.Minute))
	overdueID := seedDelegationAt(t, ws, peer, "in_progress", now.Add(-7*time.Hour))
	seedDelegationAt(t, ws, peer, "queued", now.Add(-30*time.Minute))
	seedDelegationAt(t, ws, peer, "completed", now.Add(-8*time.Hour))

	out := mailSummaryGET(t, ws, "")
	if got := out["sent_awaiting_reply"].(float64); got != 3 {
		t.Fatalf("sent_awaiting_reply = %v, want 3 (terminal rows excluded)", got)
	}
	overdue := out["overdue"].([]interface{})
	if len(overdue) != 1 {
		t.Fatalf("overdue = %v, want exactly the 7h row", overdue)
	}
	entry := overdue[0].(map[string]interface{})
	if entry["delegation_id"] != overdueID || entry["target_workspace_id"] != peer {
		t.Fatalf("overdue entry mismatch: %v", entry)
	}
	if age := entry["age_seconds"].(float64); age < 6*3600 {
		t.Fatalf("overdue age_seconds = %v, want >= 6h", age)
	}

	// A tighter threshold pulls the 30m row in too (the 5m row stays out).
	out = mailSummaryGET(t, ws, "?overdue_after_seconds=600")
	if got := len(out["overdue"].([]interface{})); got != 2 {
		t.Fatalf("overdue@600s = %d entries, want 2 (7h + 30m rows)", got)
	}
}

func TestIntegration_MailSummary_QueuedBacklogMode(t *testing.T) {
	conn := integrationDB_ActivityDelegationA2A(t)
	ws := seedWorkspace(t, conn, "test-2151-mailsum-push")

	// NO inbox_delivery_state row (push fleet). Two queued rows + one
	// dispatched (consumed) + one queued delegate_result. Bodies carry a real
	// a2a messageId so the identity projection has something to read.
	for _, row := range []struct{ method, status, msgID string }{
		{"message/send", "queued", "q-msg-1"},
		{"message/send", "queued", "q-msg-2"},
		{"message/send", "dispatched", "q-msg-dispatched"},
		{"delegate_result", "queued", "q-reply-1"},
	} {
		if _, err := conn.ExecContext(context.Background(), `
			INSERT INTO a2a_queue (workspace_id, body, method, status)
			VALUES ($1, $2::jsonb, $3, $4)
		`, ws, `{"params":{"message":{"messageId":"`+row.msgID+`"}}}`, row.method, row.status); err != nil {
			t.Fatalf("a2a_queue seed: %v", err)
		}
	}

	out := mailSummaryGET(t, ws, "")
	if got := out["mode"]; got != "queued_backlog" {
		t.Fatalf("mode = %v, want queued_backlog", got)
	}
	if got := out["received_unread"].(float64); got != 2 {
		t.Fatalf("received_unread = %v, want 2 (queued only — dispatched is consumed)", got)
	}
	if got := out["replies_unread"].(float64); got != 1 {
		t.Fatalf("replies_unread = %v, want 1", got)
	}

	// core#5028: identity projection must work in push (queued_backlog) mode
	// too — the container fleet never acks, so this is the mode most tenants
	// actually run in.
	raw, ok := out["received"]
	if !ok {
		t.Fatalf("response has NO `received` key in queued_backlog mode (core#5028). keys=%v", out)
	}
	seen := map[string]bool{}
	for _, e := range raw.([]interface{}) {
		entry := e.(map[string]interface{})
		if id, _ := entry["id"].(string); id == "" {
			t.Fatalf("queued received entry has no stable `id`: %v", entry)
		}
		mid, _ := entry["message_id"].(string)
		seen[mid] = true
	}
	if len(seen) != 3 {
		t.Fatalf("received = %v, want the 3 QUEUED rows (dispatched is consumed)", seen)
	}
	for _, want := range []string{"q-msg-1", "q-msg-2", "q-reply-1"} {
		if !seen[want] {
			t.Fatalf("queued message_id %q missing from received=%v", want, seen)
		}
	}
	if seen["q-msg-dispatched"] {
		t.Fatalf("a DISPATCHED (consumed) queue row leaked into received: %v", seen)
	}
}
