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
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// dispatched (consumed) + one queued delegate_result.
	for _, row := range []struct{ method, status string }{
		{"message/send", "queued"},
		{"message/send", "queued"},
		{"message/send", "dispatched"},
		{"delegate_result", "queued"},
	} {
		if _, err := conn.ExecContext(context.Background(), `
			INSERT INTO a2a_queue (workspace_id, body, method, status)
			VALUES ($1, '{"test":true}'::jsonb, $2, $3)
		`, ws, row.method, row.status); err != nil {
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
}
