package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newA2AAuthContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/target/a2a", bytes.NewBufferString(`{}`))
	return c, w
}

// proxyA2AAuthenticatedForTest bypasses only the HTTP credential boundary so
// behavior-focused tests can keep exercising dispatch, queueing, budgets, and
// response handling. Authentication behavior has dedicated tests below.
func proxyA2AAuthenticatedForTest(handler *WorkspaceHandler, c *gin.Context) {
	c.Set(a2aInboundAuthenticatedContextKey, true)
	handler.ProxyA2A(c)
}

func getA2AQueueStatusAuthenticatedForTest(handler *WorkspaceHandler, c *gin.Context) {
	c.Set(a2aInboundAuthenticatedContextKey, true)
	handler.GetA2AQueueStatus(c)
}

func TestAuthenticateA2AHTTPCaller_MissingCredentialsDenied(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	c, w := newA2AAuthContext()

	_, _, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")

	if err == nil {
		t.Fatal("missing credentials must be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticateA2AHTTPCaller_InvalidBearerCannotBecomeCanvas(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WillReturnError(sql.ErrNoRows)
	c, w := newA2AAuthContext()
	c.Request.Header.Set("Authorization", "Bearer revoked-token")

	_, _, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")

	if err == nil {
		t.Fatal("revoked bearer must be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateA2AHTTPCaller_DatastoreErrorsFailClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(errors.New("workspace token store unavailable"))
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WillReturnError(errors.New("org token store unavailable"))
	c, w := newA2AAuthContext()
	c.Request.Header.Set("Authorization", "Bearer indeterminate-token")

	_, _, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "workspace-a")

	if err == nil {
		t.Fatal("datastore errors must not authorize A2A")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateA2AHTTPCaller_ClaimMustMatchBearerOwner(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("token-a", "workspace-a"))
	c, w := newA2AAuthContext()
	c.Request.Header.Set("Authorization", "Bearer token-for-a")

	_, _, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "workspace-b")

	if err == nil {
		t.Fatal("workspace B claim with workspace A bearer must be rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateA2AHTTPCaller_MatchingWorkspaceBearer(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("token-a", "workspace-a"))
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("token-a", "workspace-a"))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("token-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	c, w := newA2AAuthContext()
	c.Request.Header.Set("Authorization", "Bearer token-for-a")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "workspace-a")

	if err != nil {
		t.Fatalf("matching bearer rejected: %v, response=%s", err, w.Body.String())
	}
	if callerID != "workspace-a" || isCanvasUser {
		t.Fatalf("got caller=%q canvas=%v, want workspace-a,false", callerID, isCanvasUser)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateA2AHTTPCaller_AdminIsPrivilegedCanvas(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "admin-secret")
	c, w := newA2AAuthContext()
	c.Request.Header.Set("Authorization", "Bearer admin-secret")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "identity-workspace")

	if err != nil {
		t.Fatalf("admin credential rejected: %v, response=%s", err, w.Body.String())
	}
	if callerID != "identity-workspace" || !isCanvasUser {
		t.Fatalf("got caller=%q canvas=%v, want identity-workspace,true", callerID, isCanvasUser)
	}
}

func TestAuthenticateA2AHTTPCaller_VerifiedInboundMarker(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	c, _ := newA2AAuthContext()
	c.Set(a2aInboundAuthenticatedContextKey, true)

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")

	if err != nil || callerID != "" || isCanvasUser {
		t.Fatalf("verified inbound: caller=%q canvas=%v err=%v", callerID, isCanvasUser, err)
	}
}

func TestProxyA2A_ForgedSelfClaimRejected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("attacker-token", "workspace-attacker"))
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "workspace-victim"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/workspace-victim/a2a", bytes.NewBufferString(`{"method":"message/send"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer attacker-token")
	c.Request.Header.Set("X-Workspace-ID", "workspace-victim")

	handler.ProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged self claim: want 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
