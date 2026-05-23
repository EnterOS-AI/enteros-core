package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/middleware"
	"github.com/gin-gonic/gin"
)

func buildWorkspaceDisplayEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	wh := handlers.NewWorkspaceHandler(nil, nil, "http://localhost:8080", t.TempDir())
	r.GET("/workspaces/:id/display", middleware.AdminAuth(db.DB), wh.Display)
	return r
}

func TestWorkspaceDisplayRoute_RequiresAdminAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-secret-not-presented-by-caller")
	mock := setupRouterTestDB(t)
	mock.ExpectQuery("SELECT COUNT.*FROM workspace_auth_tokens").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	r := buildWorkspaceDisplayEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workspaces/ws-display/display", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated request, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unmet: %v", err)
	}
}
