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

// expectValidUnanchoredOrgToken mocks a successful orgtoken.Validate for a
// legacy/bootstrap token with org_id NULL (no org anchor), plus the last_used_at
// bump.
func expectValidUnanchoredOrgToken(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-legacy", "pre_", nil, nil))
	mock.ExpectExec(`UPDATE org_api_tokens SET last_used_at`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestAuthenticateA2AHTTPCaller_OrgToken_ConciergeCPOrgUUID_AttributesOrgRoot is
// the #95 hole-2/hole-4 positive + attribution control, AND the regression guard
// for the catastrophic namespace bug. The concierge managed org token is anchored
// to the raw CP org UUID (MOLECULE_ORG_ID); the target's org root is the
// platform-agent WORKSPACE id, DeterministicPlatformAgentID(cpOrgUUID) — a
// DISTINCT value. The prior pass compared them directly and 403'd every anchored
// org token; here the token authenticates as a canvas-class principal and:
//   - it must NOT carry the caller-supplied X-Workspace-ID (an unverified claim);
//   - it must be attributed to a REAL, verified caller (the token's org root
//     workspace), NOT the anonymous-canvas identity ("").
//
// Realistic DISTINCT UUIDs — the token org_id and the org root are NOT the same
// literal — so this fails against a naive `root == orgID` bind and passes only
// with the like-for-like mapping.
func TestAuthenticateA2AHTTPCaller_OrgToken_ConciergeCPOrgUUID_AttributesOrgRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	cpOrgUUID := "7f3b2c1a-9d4e-4f6a-8b2c-1e5d7a9c3f80"
	orgRootWorkspaceID := DeterministicPlatformAgentID(cpOrgUUID)
	if orgRootWorkspaceID == cpOrgUUID {
		t.Fatal("test precondition broken: derived org root must differ from the CP org UUID")
	}

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, cpOrgUUID)
	// Target ws-target's org root is the DERIVED platform-agent workspace id.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(orgRootWorkspaceID))

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer org-token-value")
	// Attacker forges an arbitrary source workspace via X-Workspace-ID.
	c.Request.Header.Set("X-Workspace-ID", "some-victim-workspace")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "some-victim-workspace")

	if err != nil {
		t.Fatalf("in-org concierge org token rejected (catastrophic-bug regression): %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("managed-org token must authenticate as a canvas-class principal")
	}
	if callerID == "some-victim-workspace" {
		t.Fatal("org token must NOT return the unverified claimed caller (forgery)")
	}
	if callerID != orgRootWorkspaceID {
		t.Fatalf("org token must be attributed to the verified org root %q, got %q", orgRootWorkspaceID, callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_WorkspaceRootAnchor_Allowed covers the
// other legitimate namespace: an org token anchored directly to the org-root
// WORKSPACE id (the FK form). The direct-equality arm allows it, attributed to
// that same org root.
func TestAuthenticateA2AHTTPCaller_OrgToken_WorkspaceRootAnchor_Allowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	orgRootWorkspaceID := "c4e1a2b3-0000-4a1b-9c2d-2b7f6e5d4c3a"

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, orgRootWorkspaceID)
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(orgRootWorkspaceID))

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer org-token-value")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")
	if err != nil {
		t.Fatalf("workspace-root-anchored org token rejected: %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("managed-org token must authenticate as a canvas-class principal")
	}
	if callerID != orgRootWorkspaceID {
		t.Fatalf("attribution: got %q, want org root %q", callerID, orgRootWorkspaceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_Unanchored_FailsClosed is the #95 hole-1
// negative control: an UNANCHORED org token (org_id NULL) has no verifiable org
// scope and must NOT be trusted as a cross-workspace canvas-class caller on the
// A2A dispatch path (isCanvasUser=true would skip CanCommunicate/sameOrg/
// can_delegate). It fails CLOSED 403 — never reaching the org-root lookup.
func TestAuthenticateA2AHTTPCaller_OrgToken_Unanchored_FailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	expectNotAWorkspaceToken(mock)
	expectValidUnanchoredOrgToken(mock)
	// NOTE: no org_chain lookup is expected — the unanchored token is rejected
	// before the bind. ExpectationsWereMet fails if an unexpected query fires.

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer legacy-unanchored-token")
	// Even a forged in-target claim must not rescue an unanchored token.
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "ws-target")

	if !errors.Is(err, errCallerOrgMismatch) {
		t.Fatalf("unanchored org token: got err %v, want errCallerOrgMismatch", err)
	}
	if isCanvasUser {
		t.Fatal("unanchored org token must NOT become a canvas user (hole 1)")
	}
	if callerID != "" {
		t.Fatalf("unanchored org token must not authenticate any caller; got %q", callerID)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_CrossOrg_Rejected is the #95 hole-2
// negative control: an org token whose org does NOT own the target workspace is
// rejected 403 — it cannot dispatch into a sibling org sharing the datastore.
// Distinct realistic UUIDs across two genuinely different org roots so neither
// arm of the like-for-like bind matches.
func TestAuthenticateA2AHTTPCaller_OrgToken_CrossOrg_Rejected(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	attackerCPOrgUUID := "11111111-2222-4333-8444-555566667777"
	victimOrgRootWorkspaceID := DeterministicPlatformAgentID("99999999-8888-4777-8666-555544443333")
	if DeterministicPlatformAgentID(attackerCPOrgUUID) == victimOrgRootWorkspaceID {
		t.Fatal("test precondition broken: attacker and victim org roots must differ")
	}

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, attackerCPOrgUUID)
	// Target ws-victim belongs to a DIFFERENT org root.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(victimOrgRootWorkspaceID))

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
