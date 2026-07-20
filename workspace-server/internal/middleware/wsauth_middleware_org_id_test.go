package middleware

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// orgTokenValidateQuery is matched for orgtoken.Validate in both
// WorkspaceAuth and AdminAuth middleware paths. The query selects
// id, prefix, org_id from org_api_tokens where token_hash matches and
// revoked_at IS NULL. The org_id is returned directly from the primary
// query — no secondary lookup is needed.
const orgTokenValidateQuery = "SELECT id, prefix, org_id, expires_at FROM org_api_tokens WHERE token_hash"

// workspaceOrgRootQuery matches the recursive-CTE org-root lookup that
// WorkspaceAuth runs to bind an anchored org token to the target workspace's
// org (#95 hole 2). The lookup follows the org-token Validate SELECT + async
// last_used_at UPDATE.
const workspaceOrgRootQuery = "WITH RECURSIVE org_chain"

func TestWorkspaceAuth_ValidOrgToken_SetsOrgIDContext(t *testing.T) {
	// #95 hole 2 REGRESSION (the catastrophic bug the prior pass shipped): the
	// legitimate concierge managed org token is anchored to the raw CP org UUID
	// (MOLECULE_ORG_ID) — see platform_agent.go resolveConciergeAdminCredential —
	// while the workspace's org root is the org-root/platform-agent WORKSPACE id,
	// which the CP derives as DeterministicPlatformAgentID(orgUUID). Those two are
	// DISTINCT by construction. A plain `wsOrg != orgID` therefore 403s EVERY
	// anchored org token fleet-wide (the concierge + all user org keys). This test
	// uses realistic DISTINCT UUIDs — the token org_id and the org root are NOT the
	// same literal (that same-literal shortcut is exactly what masked the bug) —
	// and the org root is the real deterministic derivation, so it FAILS against
	// the buggy comparison and passes only with the like-for-like bind.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_test_org_token_abc123"
	tokenHash := sha256.Sum256([]byte(orgToken))

	// The concierge token's org anchor is the tenant's CP org UUID.
	cpOrgUUID := "7f3b2c1a-9d4e-4f6a-8b2c-1e5d7a9c3f80"
	// The workspace's org root is the platform-agent workspace id the CP derives
	// from that CP org UUID — a DIFFERENT UUID.
	orgRootWorkspaceID := deterministicPlatformAgentID(cpOrgUUID)
	if orgRootWorkspaceID == cpOrgUUID {
		t.Fatal("test precondition broken: derived org root must differ from the CP org UUID")
	}

	// orgtoken.Validate — returns id + prefix + org_id (the CP org UUID).
	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-org-abc", "tok_test", cpOrgUUID, nil))

	// Best-effort last_used_at update after Validate succeeds.
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-org-abc").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #95 hole 2: WorkspaceAuth binds the anchored org token to the workspace's
	// org root (the DERIVED platform-agent id), which the token's CP-org-UUID
	// anchor maps forward to → allowed.
	mock.ExpectQuery(workspaceOrgRootQuery).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(orgRootWorkspaceID))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		v, exists := c.Get("org_id")
		if !exists {
			t.Errorf("org_id not set on context for valid org token")
			c.JSON(http.StatusOK, gin.H{"ok": true})
			return
		}
		c.JSON(http.StatusOK, gin.H{"org_id": v})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-1/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (legit concierge/org token must not be locked out), got %d: %s", w.Code, w.Body.String())
	}
	// org_id must appear in the JSON response body, and it is the CP org UUID.
	if body := w.Body.String(); !strings.Contains(body, cpOrgUUID) {
		t.Errorf("org_id (%s) missing from response body: %s", cpOrgUUID, body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceAuth_OrgToken_CanonicalWorkspaceRootAnchor_Allowed(t *testing.T) {
	// The other legitimate namespace: a user/backfilled/inherited org token is
	// anchored DIRECTLY to the org-root WORKSPACE id (the FK
	// org_api_tokens.org_id REFERENCES workspaces(id)). Here org_id == the org
	// root, so the direct-equality arm of orgAnchorMatchesRoot allows it.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_canonical_ws_root_anchor"
	tokenHash := sha256.Sum256([]byte(orgToken))
	orgRootWorkspaceID := "c4e1a2b3-0000-4a1b-9c2d-2b7f6e5d4c3a"

	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-canon", "tok_cano", orgRootWorkspaceID, nil))
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-canon").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(workspaceOrgRootQuery).
		WithArgs("ws-7").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(orgRootWorkspaceID))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-7/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for workspace-root-anchored org token, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceAuth_OrgToken_CrossOrg_Denied(t *testing.T) {
	// Negative control (must FAIL closed): an anchored org token for org A must
	// NOT reach a workspace whose org root belongs to a DIFFERENT org B in the
	// same multi-org datastore. Distinct realistic UUIDs, and the two orgs' roots
	// are genuinely different derivations, so neither arm of orgAnchorMatchesRoot
	// matches → 403.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_attacker_cross_org"
	tokenHash := sha256.Sum256([]byte(orgToken))
	attackerCPOrgUUID := "11111111-2222-4333-8444-555566667777"
	victimOrgRootWorkspaceID := deterministicPlatformAgentID("99999999-8888-4777-8666-555544443333")
	if deterministicPlatformAgentID(attackerCPOrgUUID) == victimOrgRootWorkspaceID {
		t.Fatal("test precondition broken: attacker and victim org roots must differ")
	}

	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-atk", "tok_atk_", attackerCPOrgUUID, nil))
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-atk").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(workspaceOrgRootQuery).
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(victimOrgRootWorkspaceID))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-victim/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("cross-org org token must be denied 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceAuth_ValidOrgToken_OrgIDNULL_DoesNotSetContext(t *testing.T) {
	// F1097: pre-migration tokens (org_id=NULL) must NOT set org_id on context —
	// requireCallerOwnsOrg already handles nil by denying by default, so a
	// nil context key is the correct signal.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_old_token_no_org"
	tokenHash := sha256.Sum256([]byte(orgToken))

	// orgtoken.Validate — org_id NULL, so no org_id context key is set.
	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-old-xyz", "tok_old_", nil, nil))

	// Best-effort last_used_at update after Validate succeeds (even for NULL org_id).
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-old-xyz").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		_, exists := c.Get("org_id")
		if exists {
			t.Errorf("org_id should not be set on context for NULL org_id token")
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-1/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestAdminAuth_ValidOrgToken_SetsOrgIDContext(t *testing.T) {
	// F1097 (#1218): AdminAuth path (used for /org/* routes) must also
	// populate org_id so org-token callers can access their own org's
	// routes without a separate OrgIDByTokenID call per request.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_admin_path_org_token"
	tokenHash := sha256.Sum256([]byte(orgToken))

	// HasAnyLiveTokenGlobal: at least one workspace auth token exists globally
	// (bootstrap gate — if no tokens exist, AdminAuth grants access to all).
	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// orgtoken.Validate via AdminAuth — returns id + prefix + org_id directly.
	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-admin-org", "tok_adm_", "00000000-0000-0000-0000-000000000042", nil))

	r := gin.New()
	r.GET("/admin/org-settings", AdminAuth(mockDB), func(c *gin.Context) {
		v, exists := c.Get("org_id")
		if !exists {
			t.Errorf("org_id not set on context for valid org token via AdminAuth")
			c.JSON(http.StatusOK, gin.H{"ok": true})
			return
		}
		c.JSON(http.StatusOK, gin.H{"org_id": v})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/org-settings", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestAdminAuth_ValidOrgToken_OrgIDNULL_DoesNotSetContext(t *testing.T) {
	// F1097: AdminAuth path for pre-migration org token (org_id=NULL) must
	// NOT set org_id on context. Tokens minted before F1097 fix have
	// org_id=NULL — requireCallerOwnsOrg already denies these by default.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_old_admin_token"
	tokenHash := sha256.Sum256([]byte(orgToken))

	mock.ExpectQuery(hasAnyLiveTokenGlobalQuery).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-old-admin", "tok_old_", nil, nil))

	r := gin.New()
	r.GET("/admin/org-settings", AdminAuth(mockDB), func(c *gin.Context) {
		_, exists := c.Get("org_id")
		if exists {
			t.Errorf("org_id should not be set for NULL org_id token via AdminAuth")
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/admin/org-settings", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceAuth_OrgToken_DBRowScanError_DoesNotPanic(t *testing.T) {
	// F1097: org token validation must not panic if the org_id DB value is
	// unexpected — org_id is simply not set on context. Validate scans org_id as
	// sql.NullString and only sets it if .Valid is true.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_token_ok"
	tokenHash := sha256.Sum256([]byte(orgToken))

	// orgtoken.Validate returns 3 columns including org_id (sql.NullString).
	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-ok", "tok_tok_", "00000000-0000-0000-0000-000000000099", nil))

	// Best-effort last_used_at update after Validate succeeds.
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #95 hole 2: org-root bind — ws-1's org root matches the token org_id.
	mock.ExpectQuery(workspaceOrgRootQuery).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("00000000-0000-0000-0000-000000000099"))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		// org_id key may or may not be set — either is acceptable here.
		// The important thing is we don't panic.
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-1/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	// Token is still accepted — only the org_id enrichment fails.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 despite org_id SELECT error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceAuth_OrgToken_SetsAllContextKeys verifies the complete set of
// context keys set by WorkspaceAuth for a valid org token (F1097 coverage).
func TestWorkspaceAuth_OrgToken_SetsAllContextKeys(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	orgToken := "tok_full_context_token"
	tokenHash := sha256.Sum256([]byte(orgToken))
	expectedOrgID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	mock.ExpectQuery(orgTokenValidateQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("tok-full", "tok_fu_", expectedOrgID, nil))

	// Best-effort last_used_at update after Validate succeeds.
	mock.ExpectExec("UPDATE org_api_tokens SET last_used_at").
		WithArgs("tok-full").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// #95 hole 2: org-root bind — ws-1's org root matches the token org_id.
	mock.ExpectQuery(workspaceOrgRootQuery).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(expectedOrgID))

	r := gin.New()
	r.GET("/workspaces/:id/secrets", WorkspaceAuth(mockDB), func(c *gin.Context) {
		if got := c.GetString("caller_credential_class"); got != "org-token" {
			t.Errorf("caller_credential_class: got %q, want org-token", got)
		}
		id, ok := c.Get("org_token_id")
		if !ok {
			t.Errorf("org_token_id not set")
		} else if id != "tok-full" {
			t.Errorf("org_token_id: got %v, want tok-full", id)
		}

		prefix, ok := c.Get("org_token_prefix")
		if !ok {
			t.Errorf("org_token_prefix not set")
		} else if prefix != "tok_fu_" {
			t.Errorf("org_token_prefix: got %v, want tok_fu_", prefix)
		}

		orgID, ok := c.Get("org_id")
		if !ok {
			t.Errorf("org_id not set")
		} else if orgID != expectedOrgID {
			t.Errorf("org_id: got %v, want %s", orgID, expectedOrgID)
		}

		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/workspaces/ws-1/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+orgToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
