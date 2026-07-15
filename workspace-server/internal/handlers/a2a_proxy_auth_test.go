package handlers

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

type a2aAuthFailingReader struct{}

func (a2aAuthFailingReader) Read([]byte) (int, error) {
	return 0, errors.New("stop after auth")
}

func a2aAuthTokenHash(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

func newA2AAuthTestContext(t *testing.T, authHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/ws-target/a2a", a2aAuthFailingReader{})
	if authHeader != "" {
		c.Request.Header.Set("Authorization", authHeader)
	}
	return c, w
}

func TestAuthenticatedProxyA2A_AnonymousCanvasCallerReturns401(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous Canvas A2A: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticatedProxyA2A_ForgedSameOriginHeadersDoNotAuthenticate(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "")
	c.Request.Host = "tenant.example"
	c.Request.Header.Set("Origin", "https://tenant.example")
	c.Request.Header.Set("Referer", "https://tenant.example/canvas")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged same-origin Canvas A2A: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticatedProxyA2A_SelfClaimWithoutBearerReturns401(t *testing.T) {
	setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "")
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated self-claim: expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticatedProxyA2A_WorkspaceHeaderMustMatchBearerOwner(t *testing.T) {
	mock := setupTestDB(t)
	plaintext := "valid-token-for-other-caller"

	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "workspace_id"}).
			AddRow("wstok-other", "ws-other"))

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer "+plaintext)
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("workspace header/token mismatch: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestAuthenticatedProxyA2A_BoundSelfWorkspaceBearerReachesProxy(t *testing.T) {
	mock := setupTestDB(t)
	plaintext := "valid-token-for-target-self-call"

	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "workspace_id"}).
			AddRow("wstok-self", "ws-target"))

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer "+plaintext)
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stop after auth") {
		t.Fatalf("bound self-call bearer should reach ProxyA2A body reader; got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestAuthenticatedProxyA2A_AuthDatastoreErrorsFailClosed(t *testing.T) {
	mock := setupTestDB(t)
	plaintext := "a2a-token-during-datastore-outage"

	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnError(sql.ErrConnDone)

	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer "+plaintext)
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("auth datastore outage must fail closed: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestAuthenticatedProxyA2A_AdminTokenReachesProxy(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "a2a-admin-secret")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer a2a-admin-secret")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stop after auth") {
		t.Fatalf("ADMIN_TOKEN should reach ProxyA2A body reader; got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticatedProxyA2A_VerifiedTenantSessionReachesProxy(t *testing.T) {
	setupTestDB(t)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cp/auth/tenant-member" || r.URL.Query().Get("slug") != "a2a-session-tenant" {
			t.Errorf("unexpected CP verification request: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"member":true,"user_id":"a2a-user"}`))
	}))
	t.Cleanup(cp.Close)
	t.Setenv("CP_UPSTREAM_URL", cp.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "a2a-session-tenant")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "")
	c.Request.Header.Set("Cookie", "session=a2a-valid")

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stop after auth") {
		t.Fatalf("verified tenant session should reach ProxyA2A body reader; got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthenticatedProxyA2A_OrgTokenReachesProxy(t *testing.T) {
	mock := setupTestDB(t)
	plaintext := "valid-a2a-org-token"
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("orgtok-a2a", "valid-a2", "org-a2a", nil))
	mock.ExpectExec(`UPDATE org_api_tokens SET last_used_at`).
		WithArgs("orgtok-a2a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer "+plaintext)

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stop after auth") {
		t.Fatalf("org token should reach ProxyA2A body reader; got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestAuthenticatedProxyA2A_WorkspaceBearerWithoutHeaderRemainsSupported(t *testing.T) {
	mock := setupTestDB(t)
	plaintext := "valid-a2a-workspace-token"

	// Current tenant-admin auth rejects the workspace token first.
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnError(sql.ErrNoRows)
	// The wrapper then resolves the live workspace credential and derives the
	// trusted X-Workspace-ID before entering ProxyA2A.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(a2aAuthTokenHash(plaintext)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "workspace_id"}).
			AddRow("wstok-a2a", "ws-caller"))
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	c, w := newA2AAuthTestContext(t, "Bearer "+plaintext)

	handler.AuthenticatedProxyA2A(c)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stop after auth") {
		t.Fatalf("workspace bearer should reach ProxyA2A body reader; got %d: %s", w.Code, w.Body.String())
	}
	if got := c.Request.Header.Get("X-Workspace-ID"); got != "ws-caller" {
		t.Fatalf("derived X-Workspace-ID = %q, want ws-caller", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
