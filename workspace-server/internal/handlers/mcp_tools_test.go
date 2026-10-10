package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
)

// ─────────────────────────────────────────────────────────────────────────────
// extractA2AText tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractA2AText_InvalidJSON(t *testing.T) {
	// When JSON unmarshal fails, fall back to raw body.
	body := []byte("not json at all")
	got := extractA2AText(body)
	if got != "not json at all" {
		t.Errorf("invalid JSON: got %q, want raw body", got)
	}
}

func TestExtractA2AText_A2AError(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    -32600,
			"message": "workspace not found",
		},
	})
	got := extractA2AText(body)
	want := "[error] workspace not found"
	if got != want {
		t.Errorf("A2A error: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_A2AErrorMissingMessage(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"code": -32600,
		},
	})
	got := extractA2AText(body)
	// No message key → falls through to result check, then fallback
	if got == "" {
		t.Errorf("A2A error without message: got empty string")
	}
}

func TestExtractA2AText_TaskStatusMessageText(t *testing.T) {
	// The a2a-sdk TASK response shape (result.status.message.parts) — the
	// hermes runtime answers message/send with this; 2026-07-19 regression:
	// the first-boot greeting's in-character reply fell through to the JSON
	// fallback and was discarded in favor of the canned text.
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"id":   "task-1",
			"kind": "task",
			"status": map[string]interface{}{
				"state": "completed",
				"message": map[string]interface{}{
					"role": "agent",
					"parts": []interface{}{
						map[string]interface{}{"kind": "text", "text": "Hey there! I'm your agent."},
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	if got != "Hey there! I'm your agent." {
		t.Errorf("task status message: got %q", got)
	}
}

// The full A2A shape matrix is owned + exhaustively tested by internal/a2aresp
// (see a2aresp.TestText_ShapeMatrix / TestFiles_ShapeMatrix). extractA2AText is
// now a thin wrapper over a2aresp.Text, so this pins only the wrapper's ADDED
// contract: the "[error]" prefix and the raw-JSON diagnostic fallback that
// check_task_status and greetingTextFromReply depend on.
func TestExtractA2AText_WrapperContract(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"delegates a bare-message greeting to a2aresp.Text",
			`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Hey there"}]}}`, "Hey there"},
		{"a2a error surfaces with [error] prefix",
			`{"error":{"message":"workspace not found"}}`, "[error] workspace not found"},
		{"no text part falls back to raw result JSON (not empty)",
			`{"result":{"kind":"task","status":{"state":"working"}}}`, `{"kind":"task","status":{"state":"working"}}`},
		{"invalid json returns raw body",
			`not json`, `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractA2AText([]byte(tc.body)); got != tc.want {
				t.Errorf("%s\n  got  %q\n  want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestGreetingTextFromReply_BareMessage locks the END-TO-END greeting gate for
// the exact live 2026-07-26 shape: the bare A2A Message must be accepted as the
// greeting (not rejected as a raw-JSON envelope and degraded to the fallback).
func TestGreetingTextFromReply_BareMessage(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"b0953329","result":{"kind":"message","messageId":"1e5add40","parts":[{"kind":"text","text":"Hey there — I'm your Org Concierge."}]}}`)
	if got := greetingTextFromReply(body); got != "Hey there — I'm your Org Concierge." {
		t.Errorf("greetingTextFromReply dropped a valid bare-message greeting → fallback would ship\n  got %q", got)
	}
}

func TestExtractA2AText_ArtifactsText(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{
							"text": "Hello from the artifact",
						},
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "Hello from the artifact"
	if got != want {
		t.Errorf("artifacts text: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_ArtifactsEmptyArray(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{},
		},
	})
	got := extractA2AText(body)
	// Empty artifacts → falls through to message check, then fallback
	if got == "" {
		t.Errorf("empty artifacts: got empty string")
	}
}

func TestExtractA2AText_MessageText(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"message": map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{
						"text": "Hello from message",
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "Hello from message"
	if got != want {
		t.Errorf("message text: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_MessageNoParts(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"message": map[string]interface{}{},
		},
	})
	got := extractA2AText(body)
	// No parts → falls through to fallback (JSON marshal of result)
	if got == "" {
		t.Errorf("message with no parts: got empty string")
	}
}

func TestExtractA2AText_EmptyTextInPart(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{
							"text": "",
						},
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	// Empty text → falls through to message check, then fallback
	if got == "" {
		t.Errorf("empty text in part: got empty string")
	}
}

func TestExtractA2AText_NoResult(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"id": 1,
	})
	got := extractA2AText(body)
	// No result key → falls through to fallback
	if got == "" {
		t.Errorf("no result: got empty string")
	}
}

func TestExtractA2AText_FallbackMarshalsResult(t *testing.T) {
	// result is not artifacts or message → fallback to JSON marshal.
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"status": "ok",
			"count":  42,
		},
	})
	got := extractA2AText(body)
	// Fallback: json.Marshal(result) → {"count":42,"status":"ok"}
	if got == "" {
		t.Errorf("fallback marshal: got empty string")
	}
	// Verify it's valid JSON (marshaled result)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Errorf("fallback should produce valid JSON: got %q, error: %v", got, err)
	}
}

func TestExtractA2AText_ArtifactsPrecedence(t *testing.T) {
	// extractA2AText extracts the reply via a2aresp.Text, which uses SHAPE
	// PRECEDENCE (the first shape that yields text wins — no cross-shape gluing),
	// so a {message, artifacts} response returns the artifacts text: artifacts
	// precede message in the precedence order (parts → artifacts →
	// status.message → message).
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{"text": "from artifacts"},
					},
				},
			},
			"message": map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{"text": "from message"},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "from artifacts"
	if got != want {
		t.Errorf("artifacts precedence: got %q, want %q", got, want)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// insertMCPDelegationRow tests
// ─────────────────────────────────────────────────────────────────────────────

func TestInsertMCPDelegationRow_Success(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs("ws-src", "ws-src", "ws-tgt", "Delegating to ws-tgt", sqlmock.AnyArg(), "pending").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = insertMCPDelegationRow(context.Background(), mockDB, "ws-src", "ws-tgt", "del-123", "summarise the report")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestInsertMCPDelegationRow_DBError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`INSERT INTO activity_logs`).
		WithArgs("ws-src", "ws-src", "ws-tgt", sqlmock.AnyArg(), sqlmock.AnyArg(), "pending").
		WillReturnError(context.DeadlineExceeded)

	err = insertMCPDelegationRow(context.Background(), mockDB, "ws-src", "ws-tgt", "del-456", "check the logs")
	if err == nil {
		t.Error("expected error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// updateMCPDelegationStatus tests
// ─────────────────────────────────────────────────────────────────────────────

func TestUpdateMCPDelegationStatus_Success(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("completed", "", "ws-src", "del-789").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Should not panic, should not error
	updateMCPDelegationStatus(context.Background(), mockDB, mcpSyncRoute, "ws-src", "del-789", "completed", "")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestUpdateMCPDelegationStatus_WithErrorDetail(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("failed", "timeout", "ws-src", "del-000").
		WillReturnResult(sqlmock.NewResult(0, 1))

	updateMCPDelegationStatus(context.Background(), mockDB, mcpSyncRoute, "ws-src", "del-000", "failed", "timeout")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestUpdateMCPDelegationStatus_DBError_LoggedNotReturned(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectExec(`UPDATE activity_logs`).
		WithArgs("failed", sqlmock.AnyArg(), "ws-src", "del-abc").
		WillReturnError(context.DeadlineExceeded)

	// Function returns no value — error is logged, not propagated.
	// Verify it does not panic.
	updateMCPDelegationStatus(context.Background(), mockDB, mcpSyncRoute, "ws-src", "del-abc", "failed", "connection refused")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
