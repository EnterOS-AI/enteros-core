package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
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

	if !errors.Is(err, errCallerIdentityMismatch) {
		t.Fatalf("workspace B claim with workspace A bearer: got error %v, want source-binding mismatch", err)
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "caller ID does not match workspace auth token") {
		t.Fatalf("want source-binding response, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_SameOriginNoBearerRejected is the #95 hole-1
// negative control. The subprocess sets CANVAS_PROXY_URL so IsSameOriginCanvas
// returns true for the crafted Referer/Host — i.e. the exact forgeable signal
// the old branch trusted. With NO real credential the request must now FAIL
// CLOSED: a matching Referer/Origin/Host is trivially forged by any container
// on the Docker network and can no longer grant the canvas bypass.
func TestAuthenticateA2AHTTPCaller_SameOriginNoBearerRejected(t *testing.T) {
	const subprocess = "MOLECULE_TEST_SAME_ORIGIN_NOBEARER_A2A"
	if os.Getenv(subprocess) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAuthenticateA2AHTTPCaller_SameOriginNoBearerRejected$", "-test.v")
		cmd.Env = append(os.Environ(),
			subprocess+"=1",
			"CANVAS_PROXY_URL=http://local-canvas.test",
			"CP_UPSTREAM_URL=",
			"MOLECULE_ORG_SLUG=",
			"ADMIN_TOKEN=", // no admin token configured
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("same-origin no-bearer subprocess failed: %v\n%s", err, out)
		}
		return
	}

	setupTestDB(t)
	setupTestRedis(t)
	c, w := newA2AAuthContext()
	c.Request.Host = "local-canvas.test"
	c.Request.Header.Set("Referer", "http://local-canvas.test/")
	// NO Authorization header — a forged same-origin request with no credential.

	_, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "local-canvas-user")

	if err == nil {
		t.Fatal("forged same-origin request with no credential must be rejected (#95 hole 1)")
	}
	if isCanvasUser {
		t.Fatal("a no-credential same-origin request must NOT become a canvas user")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAuthenticateA2AHTTPCaller_SelfHostAdminBearerCanvas proves the LEGITIMATE
// self-hosted canvas path is preserved. On self-host the canvas bundle attaches
// `Authorization: Bearer <NEXT_PUBLIC_ADMIN_TOKEN>` (canvas/src/lib/api.ts), a
// real non-forgeable credential — that must still authenticate as a canvas user
// even from the same-origin combined image.
func TestAuthenticateA2AHTTPCaller_SelfHostAdminBearerCanvas(t *testing.T) {
	const subprocess = "MOLECULE_TEST_SELFHOST_ADMIN_A2A"
	if os.Getenv(subprocess) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAuthenticateA2AHTTPCaller_SelfHostAdminBearerCanvas$", "-test.v")
		cmd.Env = append(os.Environ(),
			subprocess+"=1",
			"CANVAS_PROXY_URL=http://local-canvas.test",
			"CP_UPSTREAM_URL=",
			"MOLECULE_ORG_SLUG=",
			"ADMIN_TOKEN=self-host-admin-secret",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("self-host admin-bearer subprocess failed: %v\n%s", err, out)
		}
		return
	}

	setupTestDB(t)
	setupTestRedis(t)
	c, w := newA2AAuthContext()
	c.Request.Host = "local-canvas.test"
	c.Request.Header.Set("Referer", "http://local-canvas.test/")
	c.Request.Header.Set("Authorization", "Bearer self-host-admin-secret")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "local-canvas-user")

	if err != nil {
		t.Fatalf("self-host canvas with ADMIN_TOKEN bearer rejected: %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatalf("self-host canvas ADMIN_TOKEN must authenticate as canvas user, got caller=%q canvas=%v", callerID, isCanvasUser)
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
	if !strings.Contains(w.Body.String(), "caller ID does not match workspace auth token") {
		t.Fatalf("forged self claim: want source-binding response, got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
