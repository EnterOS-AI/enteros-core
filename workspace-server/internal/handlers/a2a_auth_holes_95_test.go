package handlers

// a2a_auth_holes_95_test.go — regression tests for the #95 A2A caller-auth
// holes. Each hole is negative-controlled in BOTH directions: a forged /
// unauthorized request is now REJECTED, and a legitimate request still
// SUCCEEDS.

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func a2aAuthContextWithTarget(target string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: target}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+target+"/a2a", bytes.NewBufferString(`{}`))
	return c, w
}

// expectNotAWorkspaceToken mocks the WorkspaceFromToken SELECT returning
// ErrNoRows so authentication falls through to the org-token branch.
func expectNotAWorkspaceToken(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces`).
		WillReturnError(sql.ErrNoRows)
}

// expectValidAnchoredOrgToken mocks a successful orgtoken.Validate returning an
// anchored org_id, plus the best-effort last_used_at bump.
func expectValidAnchoredOrgToken(mock sqlmock.Sqlmock, orgID string) {
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-1", "pre_", orgID, nil))
	mock.ExpectExec(`UPDATE org_api_tokens SET last_used_at`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestAuthenticateA2AHTTPCaller_OrgToken_SameOrg_DropsForgedCaller is the #95
// hole-2 positive + attribution-spoofing control. A managed-org token scoped to
// the target workspace's org authenticates as a canvas-class principal, but:
//   - it must NOT carry the caller-supplied X-Workspace-ID (an unverified
//     claim); the returned callerID is the anonymous-canvas identity ("").
func TestAuthenticateA2AHTTPCaller_OrgToken_SameOrg_DropsForgedCaller(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, "org-root-1")
	// Target ws-target resolves to the SAME org root as the token → allowed.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-1"))

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer org-token-value")
	// Attacker forges an arbitrary source workspace via X-Workspace-ID.
	c.Request.Header.Set("X-Workspace-ID", "some-victim-workspace")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "some-victim-workspace")

	if err != nil {
		t.Fatalf("in-org org token rejected: %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("managed-org token must authenticate as a canvas-class principal")
	}
	if callerID != "" {
		t.Fatalf("org token must NOT return the unverified claimed caller; got %q, want \"\"", callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_CrossOrg_Rejected is the #95 hole-2
// negative control: an org token whose org does NOT own the target workspace is
// rejected 403 — it cannot dispatch into a sibling org sharing the datastore.
func TestAuthenticateA2AHTTPCaller_OrgToken_CrossOrg_Rejected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, "org-root-ATTACKER")
	// Target ws-victim belongs to a DIFFERENT org root.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow("org-root-VICTIM"))

	c, w := a2aAuthContextWithTarget("ws-victim")
	c.Request.Header.Set("Authorization", "Bearer cross-org-token")

	_, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")

	if !errors.Is(err, errCallerOrgMismatch) {
		t.Fatalf("cross-org org token: got err %v, want errCallerOrgMismatch", err)
	}
	if isCanvasUser {
		t.Fatal("cross-org org token must NOT become a canvas user")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_CrossOrg_LookupFail_FailsClosed proves
// the org-root bind fails CLOSED on a datastore error rather than leaking access
// (matches sameOrg's tenant-isolation posture).
func TestAuthenticateA2AHTTPCaller_OrgToken_LookupFail_FailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, "org-root-1")
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-target").
		WillReturnError(errors.New("org-root store unavailable"))

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer org-token-value")

	_, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")

	if err == nil || isCanvasUser {
		t.Fatalf("org-root lookup failure must fail closed; got canvas=%v err=%v", isCanvasUser, err)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
}

// --- #95 hole 3: /delegate source must be the authenticated principal ---

func newDelegationHandlerForTest(t *testing.T) *DelegationHandler {
	t.Helper()
	b := newTestBroadcaster()
	wh := NewWorkspaceHandler(b, nil, "http://localhost:8080", t.TempDir())
	return NewDelegationHandler(wh, b)
}

// TestDelegate_WorkspaceToken_CannotForgeForeignSource is the #95 hole-3
// negative control. WorkspaceAuth bound the caller's per-workspace token to
// ws-attacker (authenticated_workspace_id); a POST to /workspaces/ws-victim/
// delegate must be rejected — a workspace cannot forge a peer as the delegation
// source. The guard fires before any DB side effect.
func TestDelegate_WorkspaceToken_CannotForgeForeignSource(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	dh := newDelegationHandlerForTest(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-victim"}}
	c.Set("authenticated_workspace_id", "ws-attacker") // token is bound to ws-attacker
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-victim/delegate",
		bytes.NewBufferString(`{"target_id":"11111111-1111-1111-1111-111111111111","task":"x"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("forged delegation source: want 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "source workspace does not match authenticated identity") {
		t.Fatalf("want source-binding rejection, got %s", w.Body.String())
	}
}

// TestDelegate_WorkspaceToken_MatchingSource_PassesGuard proves the legitimate
// path is preserved: when the authenticated workspace matches the URL :id the
// source-binding guard does NOT fire (the request proceeds past it and fails
// only on the invalid body — a 400, never the 403 source-mismatch).
func TestDelegate_WorkspaceToken_MatchingSource_PassesGuard(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	dh := newDelegationHandlerForTest(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-a"}}
	c.Set("authenticated_workspace_id", "ws-a") // token bound to ws-a == :id
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-a/delegate",
		bytes.NewBufferString(`{bad-json`))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code == http.StatusForbidden {
		t.Fatalf("matching source must NOT be blocked by the source-binding guard; got 403: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (invalid body, guard passed), got %d: %s", w.Code, w.Body.String())
	}
}

// TestDelegate_HigherPrivilegePrincipal_ActsForAnySource proves an org-token /
// admin / CP-session caller (no authenticated_workspace_id key) may legitimately
// delegate on behalf of any :id in scope — the guard is skipped for them.
func TestDelegate_HigherPrivilegePrincipal_ActsForAnySource(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	dh := newDelegationHandlerForTest(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-b"}}
	// No authenticated_workspace_id — org/admin/cp-session principal.
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-b/delegate",
		bytes.NewBufferString(`{bad-json`))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code == http.StatusForbidden {
		t.Fatalf("higher-privilege principal must not be source-bound; got 403: %s", w.Body.String())
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (invalid body, guard skipped), got %d: %s", w.Code, w.Body.String())
	}
}
