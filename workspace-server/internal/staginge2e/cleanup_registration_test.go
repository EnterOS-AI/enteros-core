package staginge2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminCreateOrgRegistersCleanupBeforeProvisionWait(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "workspace_lifecycle_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse staging harness: %v", err)
	}

	var createFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "adminCreateOrg" {
			createFn = fn
			break
		}
	}
	if createFn == nil {
		t.Fatal("adminCreateOrg declaration not found")
	}

	var createAcceptedPos, cleanupPos, idParsePos, waitPos token.Pos
	ast.Inspect(createFn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if isExactCleanupRegistration(n) {
				cleanupPos = n.Pos()
			}
		case *ast.ForStmt:
			if waitPos == token.NoPos {
				waitPos = n.Pos()
			}
		case *ast.IfStmt:
			if isAcceptedCreateStatusGuard(n) {
				createAcceptedPos = n.End()
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if ok && ident.Name == "id" {
					idParsePos = n.Pos()
				}
			}
		}
		return true
	})

	if cleanupPos == token.NoPos {
		t.Fatal("adminCreateOrg must register exact-tenant cleanup after create succeeds")
	}
	if waitPos == token.NoPos {
		t.Fatal("adminCreateOrg provisioning wait loop not found")
	}
	if createAcceptedPos == token.NoPos || cleanupPos <= createAcceptedPos {
		t.Fatalf("cleanup registration at %s must follow the create-response status check ending at %s",
			fset.Position(cleanupPos), fset.Position(createAcceptedPos))
	}
	if idParsePos == token.NoPos || cleanupPos >= idParsePos {
		t.Fatalf("cleanup registration at %s must precede success-body parsing at %s",
			fset.Position(cleanupPos), fset.Position(idParsePos))
	}
	if cleanupPos >= waitPos {
		t.Fatalf("cleanup registration at %s must precede provisioning wait at %s",
			fset.Position(cleanupPos), fset.Position(waitPos))
	}
}

func isExactCleanupRegistration(call *ast.CallExpr) bool {
	if len(call.Args) != 3 {
		return false
	}
	fn, fnOK := call.Fun.(*ast.Ident)
	tArg, tOK := call.Args[0].(*ast.Ident)
	cfgArg, cfgOK := call.Args[1].(*ast.Ident)
	slugArg, slugOK := call.Args[2].(*ast.Ident)
	return fnOK && fn.Name == "registerTenantCleanup" &&
		tOK && tArg.Name == "t" && cfgOK && cfgArg.Name == "cfg" &&
		slugOK && slugArg.Name == "slug"
}

func isAcceptedCreateStatusGuard(stmt *ast.IfStmt) bool {
	var hasStatus, hasCreated, hasOK bool
	ast.Inspect(stmt.Cond, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			hasStatus = hasStatus || n.Name == "status"
		case *ast.SelectorExpr:
			pkg, ok := n.X.(*ast.Ident)
			if ok && pkg.Name == "http" {
				hasCreated = hasCreated || n.Sel.Name == "StatusCreated"
				hasOK = hasOK || n.Sel.Name == "StatusOK"
			}
		}
		return true
	})
	return hasStatus && hasCreated && hasOK
}

func TestAdminCreateOrgOwnsTenantCleanup(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read staginge2e directory: %v", err)
	}

	var deleteCalls []token.Position
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == "cleanup_registration_test.go" {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, identOK := call.Fun.(*ast.Ident)
			if identOK && ident.Name == "adminDeleteTenant" {
				deleteCalls = append(deleteCalls, fset.Position(call.Pos()))
			}
			return true
		})
	}

	if len(deleteCalls) != 1 {
		t.Fatalf("adminCreateOrg must be the sole owner of tenant cleanup; found %d adminDeleteTenant calls at %v",
			len(deleteCalls), deleteCalls)
	}
}
