package handlers

// workspace_admin_restart_test.go — tests for the AdminRestart handler
// (the partner of the user-facing POST /workspaces/:id/restart). The CP
// migrator calls this to re-inject the tenant's LLM creds via the
// loadWorkspaceSecrets path on a freshly-migrated box (today's
// 2026-06-15 fleet-credential incident root-cause durable fix — see
// PRs #824 (CP) and this one (tenant partner)). Mirrors the
// SetComputeInstance test pattern (workspace_set_compute_instance_test.go).

import (
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// AdminRestart re-injects LLM creds via the loadWorkspaceSecrets path
// (the durable fix for today's 2026-06-15 fleet-credential incident —
// see controlplane PR #824 for the migrator-side). The handler fires
// wh.RestartByID ASYNC (per the existing /restart endpoint's pattern)
// and returns 202 Accepted immediately.
func TestAdminRestart_HappyPath(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	// Pre-flight: confirm the workspace exists. The handler does
	// a SELECT 1 FROM workspaces WHERE id = $1 before firing the
	// async restart, so we expect that query.
	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-migrated").
		WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-migrated"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-migrated/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	// The actual restart is async; we don't assert on the goroutine
	// (it would no-op on the test bootstrap since h has no provisioner
	// wired; the goAsync panic-recovery swallows any panic cleanly).
}

// A workspace id that matches no row is a 404 — the migrator can tell
// a stale id from a real restart. Distinct from SetComputeInstance's
// NoRowIs404 (which fires on the UPDATE rowcount), here the
// pre-flight SELECT does the work.
func TestAdminRestart_NoRowIs404(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-gone").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-gone"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-gone/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A DB failure on the pre-flight surfaces as 500 so the migrator
// can fail loudly rather than silently restart into a missing
// workspace. (RestartByID would fail too, but with a less-precise
// error from the deeper code path; surfacing the pre-flight 500
// gives ops a clean diagnostic.)
func TestAdminRestart_DBErrorIs500(t *testing.T) {
	h, mock := setupBootstrapHandler(t)

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs("ws-1").
		WillReturnError(errors.New("connection reset"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-1/restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// An empty id is a 400 before any DB work — the migrator never
// issues an empty id (it always has a real wsID from the cutover
// record), so this is a defense-in-depth check, not a hot path.
func TestAdminRestart_EmptyIDIs400(t *testing.T) {
	h, _ := setupBootstrapHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ""}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces//restart", nil)

	h.AdminRestart(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

// TestAdminRestart_RestartByIDUsesTrackedAsyncDispatch pins the actual
// non-blocking invariant structurally: RestartByID must be inside the function
// literal passed to h.goAsync. A wall-clock assertion cannot prove this when
// the test handler has no provisioner (a synchronous RestartByID returns
// immediately), and a tight timeout makes the test itself scheduler-flaky.
func TestAdminRestart_RestartByIDUsesTrackedAsyncDispatch(t *testing.T) {
	t.Parallel()

	tracked, untracked, err := adminRestartDispatchShape("workspace_admin_restart.go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tracked != 1 || untracked != 0 {
		t.Fatalf(
			"AdminRestart RestartByID dispatch shape = tracked:%d untracked:%d; want tracked:1 untracked:0",
			tracked,
			untracked,
		)
	}
}

// TestAdminRestartAsyncShapeRejectsDirectDispatch is the fail-direction proof
// for the structural gate. It prevents a broken detector from silently passing
// the exact regression this test is meant to catch.
func TestAdminRestartAsyncShapeRejectsDirectDispatch(t *testing.T) {
	t.Parallel()

	const direct = `package handlers
func (h *WorkspaceHandler) AdminRestart() {
	h.RestartByID("ws-1")
}`
	tracked, untracked, err := adminRestartDispatchShape("direct_dispatch.go", direct)
	if err != nil {
		t.Fatal(err)
	}
	if tracked != 0 || untracked != 1 {
		t.Fatalf(
			"direct RestartByID mutation shape = tracked:%d untracked:%d; want tracked:0 untracked:1",
			tracked,
			untracked,
		)
	}
}

// adminRestartDispatchShape returns RestartByID calls inside a function literal
// passed to goAsync separately from every other RestartByID call. Passing nil
// src makes go/parser read filename from disk.
func adminRestartDispatchShape(filename string, src any) (tracked, untracked int, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %s: %w", filename, err)
	}

	var adminRestart *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Body != nil && fn.Name.Name == "AdminRestart" {
			adminRestart = fn
			break
		}
	}
	if adminRestart == nil {
		return 0, 0, fmt.Errorf("AdminRestart method not found in %s", filename)
	}
	receiver := adminRestart.Recv.List[0].Names[0].Name

	trackedLiterals := make(map[*ast.FuncLit]struct{})
	ast.Inspect(adminRestart.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ownerOK := selector.X.(*ast.Ident)
		if !ownerOK || owner.Name != receiver || selector.Sel.Name != "goAsync" || len(call.Args) != 1 {
			return true
		}
		literal, ok := call.Args[0].(*ast.FuncLit)
		if ok {
			trackedLiterals[literal] = struct{}{}
		}
		return true
	})

	var ancestors []ast.Node
	ast.Inspect(adminRestart.Body, func(node ast.Node) bool {
		if node == nil {
			ancestors = ancestors[:len(ancestors)-1]
			return true
		}

		if call, ok := node.(*ast.CallExpr); ok {
			selector, selectorOK := call.Fun.(*ast.SelectorExpr)
			if selectorOK {
				owner, ownerOK := selector.X.(*ast.Ident)
				if ownerOK && owner.Name == receiver && selector.Sel.Name == "RestartByID" {
					insideTrackedLiteral := false
					for _, ancestor := range ancestors {
						literal, ok := ancestor.(*ast.FuncLit)
						if !ok {
							continue
						}
						if _, ok := trackedLiterals[literal]; ok {
							insideTrackedLiteral = true
							break
						}
					}
					if insideTrackedLiteral {
						tracked++
					} else {
						untracked++
					}
				}
			}
		}

		ancestors = append(ancestors, node)
		return true
	})

	return tracked, untracked, nil
}
