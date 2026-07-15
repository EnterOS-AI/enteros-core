package router

import (
	"crypto/sha256"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

func buildNativeChannelCutoverInventoryEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerNativeChannelCutoverInventoryRoute(r, db.DB)
	return r
}

func TestNativeChannelCutoverInventoryPathIsStable(t *testing.T) {
	const want = "/admin/cutovers/native-channels/inventory"
	if nativeChannelCutoverInventoryPath != want {
		t.Fatalf("cutover inventory path = %q, want %q", nativeChannelCutoverInventoryPath, want)
	}
}

func TestSetupRegistersNativeChannelCutoverInventoryRouteExactlyOnce(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}

	calls := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Setup" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if ok && ident.Name == "registerNativeChannelCutoverInventoryRoute" {
				calls++
			}
			return true
		})
	}
	if calls != 1 {
		t.Fatalf("Setup registration calls = %d, want exactly 1", calls)
	}
}

func TestNativeChannelCutoverInventoryRouteRequiresAdminAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret-not-presented-by-caller")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := buildNativeChannelCutoverInventoryEngine()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, nativeChannelCutoverInventoryPath, nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestNativeChannelCutoverInventoryRouteServesAuthorizedCountOnlyResponse(t *testing.T) {
	const adminToken = "test-native-channel-cutover-admin-token"
	t.Setenv("ADMIN_TOKEN", adminToken)
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	adminHash := sha256.Sum256([]byte(adminToken))
	mock.ExpectQuery("SELECT id, prefix, org_id, expires_at FROM org_api_tokens").
		WithArgs(adminHash[:]).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT to_regclass\('public\.workspace_channels'\) IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\).*FILTER`).
		WillReturnRows(sqlmock.NewRows([]string{"total_rows", "orphan_rows"}).AddRow(0, 0))
	mock.ExpectQuery(`SELECT\s+w\.id, COUNT\(wc\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "row_count"}).
			AddRow("ws-zero", 0))
	mock.ExpectCommit()

	r := buildNativeChannelCutoverInventoryEngine()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, nativeChannelCutoverInventoryPath, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	for _, required := range []string{`"table_state":"present"`, `"total_rows":0`, `"workspace_id":"ws-zero"`} {
		if !strings.Contains(w.Body.String(), required) {
			t.Fatalf("authorized response missing %s: %s", required, w.Body.String())
		}
	}
	for _, forbidden := range []string{"channel_config", "bot_token", "signing_secret"} {
		if strings.Contains(strings.ToLower(w.Body.String()), forbidden) {
			t.Fatalf("authorized response contains forbidden field %q: %s", forbidden, w.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
