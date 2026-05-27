package handlers

import (
	"context"
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
