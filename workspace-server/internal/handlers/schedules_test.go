package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ==================== Health — issue #249 ====================
//
// GET /workspaces/:id/schedules/health is accessible to CanCommunicate peers
// with the peer's own source-bound workspace bearer. The handler mirrors the
// A2A proxy's auth pattern: X-Workspace-ID + caller token + CanCommunicate gate.
// The auth gates run BEFORE the volume forward, so they are exercised here
// without a runtime backend; the volume-served body/redaction is covered by
// schedules_volume_repoint_test.go.

const healthWorkspaceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const healthCallerID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

func expectScheduleHealthWorkspaceAuth(mock sqlmock.Sqlmock, workspaceID string) {
	// authenticateA2AHTTPCaller first resolves the bearer owner, then validates
	// the same token and refreshes last_used_at.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("health-token", workspaceID))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("health-token", workspaceID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("health-token").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestScheduleHealth_MissingCallerID_Rejected verifies that requests without
// X-Workspace-ID are rejected with 401 — anonymous peer reads are not allowed.
func TestScheduleHealth_MissingCallerID_Rejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)

	handler.Health(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing caller, got %d: %s", w.Code, w.Body.String())
	}
}

// TestScheduleHealth_AccessDenied_NonPeer verifies that a workspace which fails
// CanCommunicate (different org branch) receives 403 — not 401 or 500 — before
// any volume forward is attempted.
func TestScheduleHealth_AccessDenied_NonPeer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	// 1. Source workspace bearer authentication.
	expectScheduleHealthWorkspaceAuth(mock, healthCallerID)

	// 2. CanCommunicate: different parents → denied.
	mockCanCommunicate(mock, healthCallerID, healthWorkspaceID, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", healthCallerID)
	req.Header.Set("Authorization", "Bearer health-token")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-peer, got %d: %s", w.Code, w.Body.String())
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestScheduleHealth_SystemCallerHeaderRejected verifies that trusted internal
// caller prefixes cannot be forged through the public HTTP endpoint.
func TestScheduleHealth_SystemCallerHeaderRejected(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	handler := NewScheduleHandler()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: healthWorkspaceID}}
	req := httptest.NewRequest("GET", "/workspaces/"+healthWorkspaceID+"/schedules/health", nil)
	req.Header.Set("X-Workspace-ID", "system:monitor")
	c.Request = req

	handler.Health(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for forged system caller, got %d: %s", w.Code, w.Body.String())
	}
}
