package handlers

// activity_run_id_test.go — Enter OS Light, contract C3b: activity rows a
// Light run's requests write carry its run id; every other write is unchanged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
)

const lightTestRunID = "light-be7540cfd160-1a2b3c4d-29384756"

func runCtx() context.Context {
	return middleware.WithLightRun(context.Background(), lightTestRunID, lightTestWS)
}

func TestLogActivityTagsTheRunsOwnRows(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`INSERT INTO activity_logs \(workspace_id, activity_type, source_id, target_id, method, summary, request_body, response_body, tool_trace, duration_ms, status, error_detail, message_id, run_id\)\s+VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, \$7::jsonb, \$8::jsonb, \$9::jsonb, \$10, \$11, \$12, \$13, \$14\)[\s\S]*run_id        = COALESCE\(activity_logs\.run_id, EXCLUDED\.run_id\)`).
		WithArgs(lightTestWS, "agent_log", lightTestWS, nil, nil, "tool use", nil, nil, nil, nil, "ok", nil, nil, lightTestRunID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := logActivityExec(runCtx(), db.DB, nil, ActivityParams{
		WorkspaceID: lightTestWS, ActivityType: "agent_log", SourceID: strPtr(lightTestWS), Summary: strPtr("tool use"), Status: "ok",
	}); err != nil {
		t.Fatalf("logActivityExec: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLogActivityTagsThePeerRowOfARunsA2A(t *testing.T) {
	mock := setupTestDB(t)
	peer := "00000000-0000-0000-0000-0000000000aa"
	mock.ExpectExec(`message_id, run_id\)`).
		WithArgs(peer, "a2a_receive", lightTestWS, peer, nil, nil, nil, nil, nil, nil, "ok", nil, nil, lightTestRunID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := logActivityExec(runCtx(), db.DB, nil, ActivityParams{
		WorkspaceID: peer, ActivityType: "a2a_receive", SourceID: strPtr(lightTestWS), TargetID: strPtr(peer), Status: "ok",
	}); err != nil {
		t.Fatalf("logActivityExec: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestLogActivityDoesNotTagAnotherWorkspacesRow(t *testing.T) {
	mock := setupTestDB(t)
	other := "00000000-0000-0000-0000-0000000000bb"
	mock.ExpectExec(regexp.QuoteMeta(activityInsertSQL)).
		WithArgs(other, "agent_log", other, nil, nil, nil, nil, nil, nil, nil, "ok", nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := logActivityExec(runCtx(), db.DB, nil, ActivityParams{
		WorkspaceID: other, ActivityType: "agent_log", SourceID: strPtr(other), Status: "ok",
	}); err != nil {
		t.Fatalf("logActivityExec: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestLogActivityWithoutARunIsByteIdentical pins the statement every non-run
// write issues to the one in use before Light (13 arguments, no run_id), so the
// existing sqlmock expectations across the package keep their meaning.
func TestLogActivityWithoutARunIsByteIdentical(t *testing.T) {
	const before = `
		WITH ins AS (
			INSERT INTO activity_logs (workspace_id, activity_type, source_id, target_id, method, summary, request_body, response_body, tool_trace, duration_ms, status, error_detail, message_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb, $10, $11, $12, $13)
			ON CONFLICT (workspace_id, message_id) WHERE message_id IS NOT NULL
			DO UPDATE SET
				response_body = COALESCE(EXCLUDED.response_body, activity_logs.response_body),
				status        = CASE WHEN activity_logs.response_body IS NOT NULL AND EXCLUDED.status = 'error'
				                     THEN activity_logs.status ELSE EXCLUDED.status END,
				duration_ms   = CASE WHEN activity_logs.response_body IS NOT NULL AND EXCLUDED.status = 'error'
				                     THEN activity_logs.duration_ms ELSE COALESCE(EXCLUDED.duration_ms, activity_logs.duration_ms) END,
				error_detail  = CASE WHEN activity_logs.response_body IS NOT NULL AND EXCLUDED.status = 'error'
				                     THEN activity_logs.error_detail ELSE EXCLUDED.error_detail END,
				tool_trace    = CASE WHEN EXCLUDED.tool_trace IS NOT NULL THEN EXCLUDED.tool_trace ELSE activity_logs.tool_trace END
		)
		UPDATE workspaces SET last_activity_at = now() WHERE id = $1
	`
	if activityInsertSQL != before {
		t.Fatalf("the non-run activity statement changed:\n%s", activityInsertSQL)
	}
	mock := setupTestDB(t)
	mock.ExpectExec(regexp.QuoteMeta(before)).
		WithArgs(lightTestWS, "agent_log", nil, nil, nil, nil, nil, nil, nil, nil, "ok", nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := logActivityExec(context.Background(), db.DB, nil, ActivityParams{WorkspaceID: lightTestWS, ActivityType: "agent_log", Status: "ok"}); err != nil {
		t.Fatalf("logActivityExec: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestAgentMessageWriterTagsTheRun(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT name, talk_to_user_enabled FROM workspaces`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("Light agent", true))
	mock.ExpectExec(`INSERT INTO activity_logs \(workspace_id, activity_type, method, summary, response_body, status, run_id\)\s+VALUES \(\$1, 'a2a_receive', 'notify', \$2, \$3::jsonb, 'ok', \$4\)`).
		WithArgs(lightTestWS, "Agent message: report ready", `{"result":"report ready"}`, lightTestRunID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := NewAgentMessageWriter(db.DB, newTestBroadcaster()).Send(runCtx(), lightTestWS, "report ready", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestActivityListFiltersByRunID(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := NewActivityHandler(newTestBroadcaster(), nil)

	mock.ExpectQuery(`FROM activity_logs WHERE workspace_id = \$1 AND run_id = \$2 ORDER BY created_at DESC, seq DESC LIMIT \$3`).
		WithArgs(lightTestWS, lightTestRunID, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+lightTestWS+"/activity?run_id="+lightTestRunID, nil)
	h.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: lightTestWS}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+lightTestWS+"/activity?run_id=has%20space", nil)
	h.List(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unsafe run_id = %d, want 400", w.Code)
	}
}
