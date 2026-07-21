package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestProxyA2ABootTurnInFlightReturnsPollableQueueEnvelope(t *testing.T) {
	const (
		workspaceID = "11111111-1111-1111-1111-111111111111"
		queueID     = "22222222-2222-2222-2222-222222222222"
	)

	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expectBudgetCheck(mock, workspaceID)
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT delivery_mode FROM workspaces WHERE id = \$1`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"delivery_mode"}).AddRow("push"))
	mock.ExpectQuery(`INSERT INTO a2a_queue`).
		WithArgs(
			workspaceID,
			workspaceID,
			PriorityTask,
			sqlmock.AnyArg(),
			"message/send",
			nil,
			nil,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(queueID))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM a2a_queue`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	markRestartContextPending(workspaceID)
	t.Cleanup(func() { clearRestartContextPending(workspaceID) })

	status, body, proxyErr := handler.proxyA2ARequest(
		context.Background(),
		workspaceID,
		[]byte(`{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"ping"}]}}}`),
		workspaceID,
		false,
		false,
	)
	if proxyErr != nil {
		t.Fatalf("boot-turn queue returned proxy error: %v", proxyErr)
	}
	if status != http.StatusOK {
		t.Fatalf("boot-turn queue status = %d, want 200 for canvas compatibility", status)
	}

	var response struct {
		Status     string `json:"status"`
		Method     string `json:"method"`
		Queued     bool   `json:"queued"`
		QueueID    string `json:"queue_id"`
		QueueDepth int    `json:"queue_depth"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("boot-turn queue response is not JSON: %v; body=%s", err, body)
	}
	if response.Status != "queued" || response.Method != "message/send" || !response.Queued {
		t.Fatalf("boot-turn queue response lost queued compatibility fields: %+v", response)
	}
	if response.QueueID != queueID || response.QueueDepth != 1 {
		t.Fatalf("boot-turn queue response is not pollable: %+v", response)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("boot-turn queue contract did not execute as expected: %v", err)
	}
}
