package handlers

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueueRowAuthFields_NilSafeScan proves queueRowAuthFields returns empty
// strings (not a panic / garbage) when the a2a_queue row has NULL caller_id
// or workspace_id. Before the fix it dereferenced NullString.String directly,
// which is only the zero value when Valid is false but masked the NULL-vs-""
// distinction; the guard makes the intent explicit and safe.
func TestQueueRowAuthFields_NilSafeScan(t *testing.T) {
	mock := setupTestDB(t)
	queueID := "queue-123"

	mock.ExpectQuery(`SELECT caller_id, workspace_id FROM a2a_queue WHERE id = \$1`).
		WithArgs(queueID).
		WillReturnRows(sqlmock.NewRows([]string{"caller_id", "workspace_id"}).AddRow(nil, nil))

	caller, workspace, err := queueRowAuthFields(context.Background(), queueID)
	if err != nil {
		t.Fatalf("queueRowAuthFields returned error: %v", err)
	}
	if caller != "" {
		t.Errorf("callerID = %q, want empty string for NULL caller_id", caller)
	}
	if workspace != "" {
		t.Errorf("workspaceID = %q, want empty string for NULL workspace_id", workspace)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestQueueRowAuthFields_PopulatedRow confirms the non-NULL path still returns
// the scanned values unchanged.
func TestQueueRowAuthFields_PopulatedRow(t *testing.T) {
	mock := setupTestDB(t)
	queueID := "queue-456"

	mock.ExpectQuery(`SELECT caller_id, workspace_id FROM a2a_queue WHERE id = \$1`).
		WithArgs(queueID).
		WillReturnRows(sqlmock.NewRows([]string{"caller_id", "workspace_id"}).AddRow("caller-x", "ws-y"))

	caller, workspace, err := queueRowAuthFields(context.Background(), queueID)
	if err != nil {
		t.Fatalf("queueRowAuthFields returned error: %v", err)
	}
	if caller != "caller-x" || workspace != "ws-y" {
		t.Fatalf("got caller=%q workspace=%q, want caller-x / ws-y", caller, workspace)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestQueueStatusByID_NULLResponseBodyScan verifies that a queue row with
// a NULL response_body (the common case for queued items that haven't completed
// and have no legacy delegation stitch) scans cleanly into QueueStatus without
// panics or spurious values. core#2671 regression guard — the helper projects
// response_body through a COALESCE/subquery and must tolerate NULL results.
func TestQueueStatusByID_NULLResponseBodyScan(t *testing.T) {
	mock := setupTestDB(t)
	queueID := "queue-null-resp"

	mock.ExpectQuery(`SELECT\s+q\.id,\s+q\.workspace_id,\s+q\.status,\s+q\.priority,\s+q\.attempts,\s+q\.last_error,\s+q\.enqueued_at::text,\s+q\.dispatched_at::text,\s+q\.completed_at::text,\s+q\.expires_at::text,\s+COALESCE\(\s+q\.response_body::text,\s+\(\s+SELECT al\.response_body::text\s+FROM activity_logs al\s+WHERE al\.method = 'delegate_result'\s+AND al\.target_id = q\.workspace_id\s+AND al\.workspace_id = q\.caller_id\s+AND al\.response_body->>'delegation_id' = \(q\.body->'params'->'message'->'metadata'->>'delegation_id'\)\s+LIMIT 1\s+\)\s+\)\s+FROM a2a_queue q\s+WHERE q\.id = \$1`).
		WithArgs(queueID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "status", "priority", "attempts",
			"last_error", "enqueued_at", "dispatched_at", "completed_at", "expires_at", "response_body",
		}).AddRow(
			queueID, "ws-target", "queued", 50, 0,
			sql.NullString{Valid: false}, "2026-06-13T00:00:00Z",
			sql.NullString{Valid: false}, sql.NullString{Valid: false},
			sql.NullString{Valid: false}, nil,
		))

	qs, err := QueueStatusByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("QueueStatusByID returned error: %v", err)
	}
	if qs == nil {
		t.Fatal("QueueStatusByID returned nil")
	}
	if qs.ID != queueID {
		t.Errorf("id = %q, want %q", qs.ID, queueID)
	}
	if qs.Status != "queued" {
		t.Errorf("status = %q, want queued", qs.Status)
	}
	if qs.ResponseBody != nil {
		t.Errorf("ResponseBody = %v, want nil for NULL response_body", qs.ResponseBody)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestExtractExpiresInSeconds covers the JSON parser used at enqueue time
// to honor a caller-specified TTL. Zero return = "no TTL" — caller leaves
// expires_at NULL on the queue row.
func TestExtractExpiresInSeconds(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "absent",
			body: `{"params":{"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "positive",
			body: `{"params":{"expires_in_seconds":300,"message":{"messageId":"x"}}}`,
			want: 300,
		},
		{
			name: "zero",
			body: `{"params":{"expires_in_seconds":0,"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "negative coerced to zero",
			body: `{"params":{"expires_in_seconds":-30,"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "invalid JSON returns zero",
			body: `not json`,
			want: 0,
		},
		{
			name: "wrong type silently zero (json.Unmarshal returns err on type mismatch)",
			body: `{"params":{"expires_in_seconds":"not-a-number"}}`,
			want: 0,
		},
		{
			name: "params absent entirely",
			body: `{}`,
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractExpiresInSeconds([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}
