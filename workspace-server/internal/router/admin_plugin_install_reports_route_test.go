package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/handlers"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// admin_plugin_install_reports_route_test.go — pin the #4981 §1 wiring.
//
// The hazard is specific and it is why this route is NOT in the wsAuth group its
// POST/GET siblings live in. Those are mounted under WorkspaceAuth, which binds
// the bearer to :id so a workspace can only read its OWN report. This route
// returns rows for workspaces the caller never names — plugin sources including
// private gitea URLs, and the identity of every box whose plugins are down. A
// router refactor that moved it under wsAuth, or dropped the middleware
// altogether, would hand the whole fleet's failure map to any workspace token on
// the Docker network, and every existing test would still pass.
//
// So: unauthenticated is refused, a WORKSPACE token is refused, and — the control
// — the admin credential is not.

func buildAdminPluginInstallReportsEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	apirH := handlers.NewAdminPluginInstallReportsHandler(db.DB)
	r.GET("/admin/plugin-install-reports", middleware.AdminAuth(db.DB), apirH.List)
	return r
}

// Same two switches as the delegations gate test: ADMIN_TOKEN set so AdminAuth
// enforces Tier 2b, and HasAnyLiveTokenGlobal returning ≥1 so the fresh-install
// probe does not short-circuit.
func TestAdminPluginInstallReportsRoute_RequiresAdminAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret-not-presented-by-caller")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := buildAdminPluginInstallReportsEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/plugin-install-reports", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unmet: %v", err)
	}
}

// THE fleet-exposure assertion. A workspace-scoped bearer is a legitimate
// credential for /workspaces/:id/plugin-install-report and must NOT reach the
// fleet read. AdminAuth Tier 2b rejects any bearer that is not the ADMIN_TOKEN
// once one is configured — this pins that the fleet route is behind that gate and
// not behind WorkspaceAuth.
func TestAdminPluginInstallReportsRoute_WorkspaceTokenIsRefused(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// AdminAuth Tier 2a validates the bearer as an org token first; a workspace
	// token is not one, so the lookup finds nothing and it falls through to 2b.
	mock.ExpectQuery("(?i)org_api_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_prefix", "org_id"}))

	r := buildAdminPluginInstallReportsEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/plugin-install-reports", nil)
	req.Header.Set("Authorization", "Bearer a-perfectly-valid-workspace-token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("a workspace-scoped token must not reach the fleet read, got %d: %s",
			w.Code, w.Body.String())
	}
}

// Negative control for both tests above: with the ADMIN credential presented, the
// gate must NOT refuse. Without this, a route that 401s unconditionally — or one
// wired to a handler that never runs — would satisfy the two assertions above
// while serving nobody.
func TestAdminPluginInstallReportsRoute_AdminTokenReachesTheHandler(t *testing.T) {
	const adminSecret = "test-admin-secret"
	t.Setenv("ADMIN_TOKEN", adminSecret)
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("(?i)org_api_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_prefix", "org_id"}))
	mock.ExpectQuery("FROM workspace_plugin_install_reports").
		WillReturnRows(sqlmock.NewRows([]string{
			"workspace_id", "name", "declared", "plugins_dir",
			"installed", "skipped", "failed", "swapped", "reported_at",
		}))

	r := buildAdminPluginInstallReportsEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/plugin-install-reports", nil)
	req.Header.Set("Authorization", "Bearer "+adminSecret)
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Fatalf("control failed: the admin credential must reach the handler, got 401: %s",
			w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for an admin-authed fleet read, got %d: %s", w.Code, w.Body.String())
	}
}
