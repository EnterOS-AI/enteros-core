package handlers

// plugins_install_selfscope_test.go — pins the SELF-SCOPED contract of the
// plugin-install endpoint POST /workspaces/:id/plugins.
//
// Operator principle: installing a plugin ON YOURSELF is a self-scoped
// ability every workspace has by default (like installing an app on your own
// phone). Installing ON ANOTHER workspace is other-scoped and stays a
// platform/orchestrator privilege.
//
// The endpoint is mounted under the WorkspaceAuth group (router.go:
// `wsAuth := r.Group("/workspaces/:id", middleware.WorkspaceAuth(db.DB))`,
// then `wsAuth.POST("/plugins", plgh.Install)`). WorkspaceAuth enforces that
// a *per-workspace* bearer token authenticates ONLY its own :id. So:
//
//   - A plain workspace (holding only its own per-workspace token) CAN reach
//     the Install handler for its OWN :id → self-install is allowed for any
//     workspace, no platform entitlement required. WHICH plugin it may install
//     is then governed by checkOrgPluginAllowlist inside the handler (see
//     org_plugin_allowlist_test.go); this test pins the AUTH boundary.
//
//   - The SAME per-workspace token CANNOT reach the handler for ANOTHER :id →
//     WorkspaceAuth 401s before the handler runs. Installing onto another
//     workspace therefore requires a broader credential (org-scoped API token,
//     ADMIN_TOKEN, or a verified CP session) — i.e. an orchestrator/platform
//     privilege — exactly the intended split.
//
// The endpoint needs NO change to support self-install: WorkspaceAuth already
// admits a workspace's own token for its own :id. This test locks that in so a
// future middleware refactor can't silently make self-install a privileged op
// or, worse, let a per-workspace token install onto a sibling.

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Regexes mirror the ones in internal/middleware/wsauth_middleware_test.go so
// this test speaks the same query-shape contract WorkspaceAuth relies on.
const (
	selfscopeOrgTokenSelectQuery = "SELECT id, prefix, org_id, expires_at FROM org_api_tokens"
	selfscopeWSTokenSelectQuery  = "SELECT t\\.id, t\\.workspace_id.*FROM workspace_auth_tokens t.*JOIN workspaces"
	selfscopeWSTokenUpdateQuery  = "UPDATE workspace_auth_tokens SET last_used_at"
)

func TestPluginsInstall_SelfInstall_Allowed_ForPlainWorkspaceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("ADMIN_TOKEN", "") // disable the admin-token fast path
	t.Setenv("CANVAS_PROXY_URL", "")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	const wsSelf = "ws-self"
	token := "plain-workspace-own-token-abc123"
	tokenHash := sha256.Sum256([]byte(token))

	// 1) orgtoken.Validate probes org_api_tokens first — a plain per-workspace
	//    token is NOT an org token, so this returns no row (ErrInvalidToken),
	//    and WorkspaceAuth falls through to the per-workspace check.
	mock.ExpectQuery(selfscopeOrgTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}))

	// 2) ValidateToken SELECT — token resolves to its OWN workspace (wsSelf),
	//    which matches the request :id, so auth passes.
	mock.ExpectQuery(selfscopeWSTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-self", wsSelf))
	// 3) Best-effort last_used_at bump.
	mock.ExpectExec(selfscopeWSTokenUpdateQuery).
		WithArgs("tok-self").
		WillReturnResult(sqlmock.NewResult(0, 1))

	reached := false
	r := gin.New()
	wsAuth := r.Group("/workspaces/:id", middleware.WorkspaceAuth(mockDB))
	wsAuth.POST("/plugins", func(c *gin.Context) {
		reached = true
		c.JSON(http.StatusOK, gin.H{"status": "installed"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/workspaces/"+wsSelf+"/plugins", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if !reached {
		t.Fatalf("self-install: WorkspaceAuth blocked a workspace installing on ITSELF "+
			"(status %d, body %s) — self-install must be allowed for any workspace's own token",
			w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Errorf("self-install: expected 200 from handler, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestPluginsInstall_InstallOnAnother_Rejected_ForPlainWorkspaceToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("CANVAS_PROXY_URL", "")

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer mockDB.Close()

	const (
		wsSelf  = "ws-self"
		wsOther = "ws-other"
	)
	token := "plain-workspace-own-token-abc123"
	tokenHash := sha256.Sum256([]byte(token))

	// orgtoken probe: not an org token → no row.
	mock.ExpectQuery(selfscopeOrgTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}))

	// ValidateToken SELECT resolves the token to wsSelf, but the request is for
	// wsOther — wsauth.ValidateToken catches the workspace-binding mismatch and
	// returns an error, so WorkspaceAuth 401s. No last_used_at UPDATE runs
	// (ValidateToken returns before the best-effort bump on mismatch).
	mock.ExpectQuery(selfscopeWSTokenSelectQuery).
		WithArgs(tokenHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-self", wsSelf)) // != wsOther → mismatch

	reached := false
	r := gin.New()
	wsAuth := r.Group("/workspaces/:id", middleware.WorkspaceAuth(mockDB))
	wsAuth.POST("/plugins", func(c *gin.Context) {
		reached = true
		c.JSON(http.StatusOK, gin.H{"status": "installed"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/workspaces/"+wsOther+"/plugins", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if reached {
		t.Fatalf("install-on-another: a plain per-workspace token reached the Install "+
			"handler for a DIFFERENT workspace (%s) — installing onto another workspace "+
			"must require a platform/orchestrator credential, not a self token", wsOther)
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("install-on-another: expected 401 from WorkspaceAuth, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
