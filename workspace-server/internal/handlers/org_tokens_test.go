package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// setupOrgTokenTest wires the package-global db.DB to a sqlmock for
// the duration of a test, returning the handler + mock + cleanup.
// Gin runs in release mode to suppress debug noise.
func setupOrgTokenTest(t *testing.T) (*OrgTokenHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	mock, mockDB, cleanup := mockSQLDB(t)
	prev := db.DB
	db.DB = mockDB
	return NewOrgTokenHandler(), mock, func() {
		db.DB = prev
		cleanup()
	}
}

// mockSQLDB returns an sqlmock + *sql.DB pair. Caller restores
// package state via the cleanup func.
func mockSQLDB(t *testing.T) (sqlmock.Sqlmock, *sql.DB, func()) {
	t.Helper()
	d, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return m, d, func() { _ = d.Close() }
}

// buildCtx returns a gin.Context + recorder wired for the given
// method+path+body. Test code can set headers / context values
// before calling the handler.
func buildCtx(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	c.Request = r.WithContext(context.Background())
	return c, w
}

// ---- List -----------------------------------------------------------------

func TestOrgTokenHandler_List_HappyPath(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, prefix.*FROM org_api_tokens`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "name", "org_id", "created_by", "created_at", "last_used_at"}).
			AddRow("tok-1", "abcd1234", "zapier", "", "session", now, nil))

	c, w := buildCtx("GET", "/org/tokens", "")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Count  int `json:"count"`
		Tokens []struct {
			ID     string `json:"id"`
			Prefix string `json:"prefix"`
			Name   string `json:"name"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Count != 1 || len(body.Tokens) != 1 {
		t.Fatalf("expected 1 token, got %+v", body)
	}
	if body.Tokens[0].Prefix != "abcd1234" {
		t.Errorf("prefix not propagated: %q", body.Tokens[0].Prefix)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestOrgTokenHandler_List_DBError500(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT id, prefix.*FROM org_api_tokens`).
		WillReturnError(sql.ErrConnDone)

	c, w := buildCtx("GET", "/org/tokens", "")
	h.List(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("db error → 500 expected; got %d", w.Code)
	}
}

// ---- Create ---------------------------------------------------------------

func TestOrgTokenHandler_Create_ActorFromAdminToken(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// No Cookie header, no org_token_prefix → actor should be
	// "admin-token" (bootstrap path).
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "my-ci", actorAdminToken, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-1"))

	c, w := buildCtx("POST", "/org/tokens", `{"name":"my-ci"}`)
	h.Create(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ID      string `json:"id"`
		Prefix  string `json:"prefix"`
		Name    string `json:"name"`
		Token   string `json:"auth_token"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Token == "" {
		t.Errorf("plaintext auth_token missing from response")
	}
	if body.Prefix != body.Token[:8] {
		t.Errorf("prefix %q should equal first 8 chars of token %q", body.Prefix, body.Token[:8])
	}
	if body.Name != "my-ci" {
		t.Errorf("name round-trip mismatch: %q", body.Name)
	}
	if body.Warning == "" {
		t.Errorf("warning text missing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestOrgTokenHandler_Create_ActorFromOrgTokenPrefix(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// When an existing org token authenticated the mint, audit
	// records "org-token:<prefix>" using the 8-char plaintext
	// prefix from the presenter's token.
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), nil, actorOrgTokenPrefix+"parent12", nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-new"))

	c, w := buildCtx("POST", "/org/tokens", `{}`)
	c.Set("org_token_prefix", "parent12")
	h.Create(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// (core#2574) Regression test: an admin-token-bearing caller (the
// concierge agent holds $ADMIN_TOKEN) MUST be gated by the Phase-4
// approval gate when minting an org token. The live incident (2026-06-11)
// had the gate INERT on the admin-token path — two live full-tenant-admin
// org API tokens were minted with zero pending approvals. The fix wires
// gateDestructive into OrgTokenHandler.Create and the gate's
// callerIsAdminToken detection overrides the rollout flag (admin-token
// is ALWAYS gated when the action is gated, regardless of the flag).
func TestOrgTokenHandler_Create_AdminToken_GatedByApproval(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// requireApproval sequence for an admin-token caller (gated action,
	// no pre-existing approval):
	//   1. UPDATE approval_requests SET consumed_at = now() … RETURNING id
	//      → no row (sql.ErrNoRows)
	//   2. WITH existing AS … INSERT INTO approval_requests … RETURNING id
	//   3. SELECT parent_id FROM workspaces WHERE id → NULL
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("appr-core2574-org-mint"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	// NOTE: deliberately NO `INSERT INTO org_api_tokens` mock setup. If
	// the gate is bypassed (the bug), the handler will reach the
	// orgtoken.Issue call and try to run that INSERT against the mock,
	// which has no expectation — sqlmock will return an error and the test
	// will fail. The gate firing = no INSERT = test passes.

	c, w := buildCtx("POST", "/org/tokens", `{"name":"concierge-rogue-mint"}`)
	// core#2574: the auth middleware sets caller_is_admin_token when the
	// request authenticates via Tier 2b ADMIN_TOKEN (or Tier 3 workspace-
	// token fallback). Simulate that here.
	c.Set("caller_is_admin_token", true)
	c.Set("caller_credential_class", "admin-token")

	// The rollout flag is OFF (default) — this is the regression
	// assertion: even without MOLECULE_PLATFORM_APPROVAL_GATE, the
	// admin-token path must gate.
	os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	defer os.Unsetenv("MOLECULE_PLATFORM_APPROVAL_GATE")

	h.Create(c)

	// Gate fires → 202 Accepted with a pending approval_id.
	if w.Code != http.StatusAccepted {
		t.Fatalf("admin-token + gated action MUST return 202 (Phase-4 approval gate), got %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		Action     string `json:"action"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Status != "pending_approval" {
		t.Errorf("status = %q, want \"pending_approval\"", body.Status)
	}
	if body.ApprovalID != "appr-core2574-org-mint" {
		t.Errorf("approval_id = %q, want \"appr-core2574-org-mint\"", body.ApprovalID)
	}
	if body.Action != "org_token_mint" {
		t.Errorf("action = %q, want \"org_token_mint\"", body.Action)
	}
	if body.Reason == "" {
		t.Errorf("reason text missing — operators need a human-readable explanation for the pending approval")
	}
}

func TestOrgTokenHandler_Create_ActorFromSession(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	// Cookie present but no org_token_prefix — that's the canvas
	// session path. Today recorded as bare "session". When the
	// follow-up captures WorkOS user_id, this test changes to
	// "session:user_01XXX".
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "from-browser", actorSession, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-browser"))

	c, w := buildCtx("POST", "/org/tokens", `{"name":"from-browser"}`)
	c.Request.Header.Set("Cookie", "mcp_session=abc")
	h.Create(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgTokenHandler_Create_NameTooLong400(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	longName := string(make([]byte, 101))
	for i := range longName {
		_ = i
	}
	// Build a 101-char name (any ASCII works; we're hitting the
	// length bound).
	buf := make([]byte, 101)
	for i := range buf {
		buf[i] = 'a'
	}
	c, w := buildCtx("POST", "/org/tokens", `{"name":"`+string(buf)+`"}`)
	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("oversize name: want 400, got %d", w.Code)
	}
}

func TestOrgTokenHandler_Create_EmptyBodyOK(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	// Empty POST must still mint a token — name is optional.
	mock.ExpectQuery(`INSERT INTO org_api_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), nil, actorAdminToken, nil).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-min"))

	c, w := buildCtx("POST", "/org/tokens", "")
	h.Create(c)

	if w.Code != http.StatusOK {
		t.Errorf("empty body should mint OK; got %d: %s", w.Code, w.Body.String())
	}
}

// ---- Revoke ---------------------------------------------------------------

func TestOrgTokenHandler_Revoke_HappyPath200(t *testing.T) {
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()

	mock.ExpectExec(`UPDATE org_api_tokens`).
		WithArgs("tok-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, w := buildCtx("DELETE", "/org/tokens/tok-1", "")
	c.Params = gin.Params{{Key: "id", Value: "tok-1"}}
	h.Revoke(c)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgTokenHandler_Revoke_Missing404(t *testing.T) {
	// Idempotency: revoking a non-existent or already-revoked id
	// returns 404 — callers can tell "worked" from "already done".
	h, mock, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	mock.ExpectExec(`UPDATE org_api_tokens`).
		WithArgs("ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	c, w := buildCtx("DELETE", "/org/tokens/ghost", "")
	c.Params = gin.Params{{Key: "id", Value: "ghost"}}
	h.Revoke(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestOrgTokenHandler_Revoke_MissingID400(t *testing.T) {
	h, _, cleanup := setupOrgTokenTest(t)
	defer cleanup()
	c, w := buildCtx("DELETE", "/org/tokens/", "")
	// No id param set.
	h.Revoke(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}
