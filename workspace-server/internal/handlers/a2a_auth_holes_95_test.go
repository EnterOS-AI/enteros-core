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

// TestAuthenticateA2AHTTPCaller_OrgToken_ConciergeCPOrgUUID_CanvasClass is the
// #95 hole-2/hole-4 positive + attribution control, AND the regression guard for
// the catastrophic namespace bug. The concierge managed org token is anchored to
// the raw CP org UUID (MOLECULE_ORG_ID); the target's org root is the
// platform-agent WORKSPACE id, DeterministicPlatformAgentID(cpOrgUUID) — a
// DISTINCT value. The prior pass compared them directly and 403'd every anchored
// org token; here the token authenticates as a canvas-class principal and:
//   - it must NOT carry the caller-supplied X-Workspace-ID (an unverified claim);
//   - it is attributed as a CANVAS-CLASS caller ("") — an org key is a human/
//     org-admin principal, not a workspace (finding[6]); returning the org root
//     workspace id would collide with the target on a self-dispatch.
//
// Realistic DISTINCT UUIDs — the token org_id and the org root are NOT the same
// literal — so this fails against a naive `root == orgID` bind and passes only
// with the like-for-like mapping (the det() cheap arm here).
func TestAuthenticateA2AHTTPCaller_OrgToken_ConciergeCPOrgUUID_CanvasClass(t *testing.T) {
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
	if callerID != "" {
		t.Fatalf("org token must be attributed canvas-class (\"\"), never a workspace id or the forged claim; got %q", callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_WorkspaceRootAnchor_Allowed covers the
// other legitimate namespace: an org token anchored directly to the org-root
// WORKSPACE id (the FK form). The direct-equality cheap arm allows it, attributed
// canvas-class ("").
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
	if callerID != "" {
		t.Fatalf("attribution: got %q, want canvas-class \"\"", callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_NonDerivedRandomRoot_Allowed is the
// finding[2] positive control: an org whose root workspace has a PLAIN RANDOM id
// (NOT DeterministicPlatformAgentID) with a token anchored to the CP org UUID.
// Neither cheap arm of orgAnchorMatchesRoot matches (anchor != root, and
// det(anchor) != the random root), so the bind must RESOLVE the token anchor to
// its own org root and compare in the same namespace — here both resolve to the
// same random root → allowed. Distinct realistic UUIDs throughout.
func TestAuthenticateA2AHTTPCaller_OrgToken_NonDerivedRandomRoot_Allowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	cpOrgUUID := "3a9d1e2f-4b6c-4d8e-9f01-2c3b4a5d6e7f"
	randomOrgRoot := "b17c9e04-5f2a-4c1d-8e3b-6a7d9c0f1234" // NOT det(cpOrgUUID)
	if DeterministicPlatformAgentID(cpOrgUUID) == randomOrgRoot || cpOrgUUID == randomOrgRoot {
		t.Fatal("test precondition broken: root must be a non-derived random id distinct from the anchor")
	}

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, cpOrgUUID)
	// Target's org root is the RANDOM id (cheap arms miss).
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(randomOrgRoot))
	// Resolve-arm: the token anchor's OWN org root is that same random root.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(cpOrgUUID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(randomOrgRoot))

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer org-token-value")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")
	if err != nil {
		t.Fatalf("non-derived-root in-org org token rejected (finding[2]): %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("in-org org token must authenticate as a canvas-class principal")
	}
	if callerID != "" {
		t.Fatalf("attribution: got %q, want canvas-class \"\"", callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_SelfDispatchToOrgRoot_NotSelf is the
// finding[6] control: an org token dispatching to the ORG-ROOT workspace itself
// (target == org root). The old code returned targetRoot as callerID, so
// callerID == workspaceID — mis-classified as a workspace self-request. The
// caller must instead be attributed canvas-class (""), NEVER equal to the target
// workspace id, so a real org-admin→concierge dispatch is not treated as self.
func TestAuthenticateA2AHTTPCaller_OrgToken_SelfDispatchToOrgRoot_NotSelf(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	orgRoot := "d4f10a2b-3c5e-4a6d-8b9f-1e2d3c4b5a60"

	expectNotAWorkspaceToken(mock)
	expectValidAnchoredOrgToken(mock, orgRoot)
	// The TARGET is the org root itself; its org root resolves to itself.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(orgRoot).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(orgRoot))

	c, w := a2aAuthContextWithTarget(orgRoot) // dispatch TO the org root workspace
	c.Request.Header.Set("Authorization", "Bearer org-token-value")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "")
	if err != nil {
		t.Fatalf("org-admin dispatch to org root rejected: %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("org token must authenticate as a canvas-class principal")
	}
	if callerID == orgRoot {
		t.Fatal("finding[6]: caller must NOT equal the target org-root workspace (self-request mis-classification)")
	}
	if callerID != "" {
		t.Fatalf("attribution: got %q, want canvas-class \"\"", callerID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestAuthenticateA2AHTTPCaller_OrgToken_Unanchored_AcceptedAsCanvasClass is the
// finding[3] control: an UNANCHORED org token (org_id NULL) is accepted on every
// OTHER /workspaces/:id/* route (WorkspaceAuth applies no org bind to a NULL-org
// token), so failing closed on /a2a alone would break legacy automation without
// closing anything. It is accepted here as a canvas-class caller ("") — aligned
// with the middleware. RESIDUAL LEGACY GAP (#95): no org scope is enforced for
// an unanchored token; anchoring (PATCH /org/tokens/:id) is the migration. No
// org-root lookup fires (accepted before the bind).
func TestAuthenticateA2AHTTPCaller_OrgToken_Unanchored_AcceptedAsCanvasClass(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	t.Setenv("ADMIN_TOKEN", "")

	expectNotAWorkspaceToken(mock)
	expectValidUnanchoredOrgToken(mock)
	// NOTE: no org_chain lookup is expected — the unanchored token is accepted
	// before the bind. ExpectationsWereMet fails if an unexpected query fires.

	c, w := a2aAuthContextWithTarget("ws-target")
	c.Request.Header.Set("Authorization", "Bearer legacy-unanchored-token")
	// A forged in-target claim must NOT be returned as the caller either.
	c.Request.Header.Set("X-Workspace-ID", "ws-target")

	callerID, isCanvasUser, err := authenticateA2AHTTPCaller(c.Request.Context(), c, "ws-target")

	if err != nil {
		t.Fatalf("unanchored org token must be accepted (finding[3]): %v, response=%s", err, w.Body.String())
	}
	if !isCanvasUser {
		t.Fatal("unanchored org token must authenticate as a canvas-class principal (aligned with middleware)")
	}
	if callerID != "" {
		t.Fatalf("unanchored org token must be canvas-class (\"\"), never the forged claim; got %q", callerID)
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
	// Target ws-victim belongs to a DIFFERENT org root — cheap arms miss.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(victimOrgRootWorkspaceID))
	// Resolve-arm (finding[2]): the attacker anchor's OWN org root is the
	// attacker's org (its derived platform-agent id) — CONFIRMED different from
	// the victim's root → 403 via a real cross-org mismatch, not a lookup error.
	mock.ExpectQuery(`WITH RECURSIVE org_chain`).
		WithArgs(attackerCPOrgUUID).
		WillReturnRows(sqlmock.NewRows([]string{"root_id"}).AddRow(DeterministicPlatformAgentID(attackerCPOrgUUID)))

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
