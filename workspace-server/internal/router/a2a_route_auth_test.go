package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestSetupRoutesPublicA2AThroughAuthenticatedEntrypoint pins the security
// boundary between trusted in-process ProxyA2A calls and the public HTTP route.
// A future router refactor must not wire the public path back to ProxyA2A
// directly, which would restore anonymous callerID="" access.
func TestSetupRoutesPublicA2AThroughAuthenticatedEntrypoint(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}

	authenticatedRoutes := 0
	directRoutes := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || method.Sel.Name != "POST" {
			return true
		}
		path, ok := call.Args[0].(*ast.BasicLit)
		if !ok || path.Kind != token.STRING {
			return true
		}
		pathValue, err := strconv.Unquote(path.Value)
		if err != nil || pathValue != "/workspaces/:id/a2a" {
			return true
		}
		handler, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch handler.Sel.Name {
		case "AuthenticatedProxyA2A":
			authenticatedRoutes++
		case "ProxyA2A":
			directRoutes++
		}
		return true
	})

	if authenticatedRoutes != 1 || directRoutes != 0 {
		t.Fatalf("public A2A route wiring: authenticated=%d direct=%d, want 1 and 0", authenticatedRoutes, directRoutes)
	}
}
