package handlers

// scheduler_declare_before_provision_gate_test.go — C2 ordering gate for the
// scheduler-as-trigger-plugin RFC §8A P3 seam.
//
// The property: on every path that creates a workspace WITH template/org
// schedules, ensureSchedulerPluginDeclared (→ workspace_declared_plugins
// upsert) and — on the org-import path — renderTemplateSchedulesYAML must
// execute BEFORE the provision dispatch, because the dispatched goroutine
// assembles MOLECULE_DECLARED_PLUGINS from workspace_declared_plugins
// (buildProvisionerConfig → desiredPluginSources) and captures configFiles
// for delivery. Declared/rendered AFTER dispatch = a race the goroutine
// usually loses only by luck (pre-fix shape: workspace.go seeded+declared
// after provisionWorkspaceAuto; org_import.go never declared at all).
//
// A runtime test cannot pin this deterministically (the race almost always
// resolves the lucky way under sqlmock), so — same shape as the
// mintWorkspaceSecrets AST gate in workspace_provision_shared_test.go and
// the Class 1 gate — this walks the source and asserts CALL ORDER within the
// two functions. Reachable fail arm: moving either call below the dispatch
// (or deleting it) fails this test immediately.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// callPositionsInFunc parses file and returns the byte offsets of every call
// to each named function/method inside the named FuncDecl (matching both
// bare-identifier calls and selector calls like h.workspace.goAsync).
func callPositionsInFunc(t *testing.T, file, funcName string, callNames []string) map[string][]token.Pos {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var target *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == funcName {
			target = fd
			break
		}
	}
	if target == nil {
		t.Fatalf("function %s not found in %s", funcName, file)
	}
	want := map[string]bool{}
	for _, n := range callNames {
		want[n] = true
	}
	out := map[string][]token.Pos{}
	ast.Inspect(target, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		default:
			return true
		}
		if want[name] {
			out[name] = append(out[name], call.Pos())
		}
		return true
	})
	return out
}

// assertCallBefore fails unless every call to before precedes every call to
// after inside file/funcName — and unless BOTH calls exist at all (a deleted
// call is the other reachable fail arm).
func assertCallBefore(t *testing.T, file, funcName, before, after string) {
	t.Helper()
	pos := callPositionsInFunc(t, file, funcName, []string{before, after})
	if len(pos[before]) == 0 {
		t.Fatalf("%s: %s no longer calls %s — the P3 seeding seam (or its C2 pre-provision declare) was removed", file, funcName, before)
	}
	if len(pos[after]) == 0 {
		t.Fatalf("%s: %s no longer calls %s — dispatch shape changed; re-verify the declare-before-provision ordering and update this gate", file, funcName, after)
	}
	for _, b := range pos[before] {
		for _, a := range pos[after] {
			if b >= a {
				t.Errorf("%s: in %s, %s (offset %d) must be called BEFORE %s (offset %d) — the provision goroutine reads workspace_declared_plugins/configFiles at dispatch", file, funcName, before, b, after, a)
			}
		}
	}
}

// TestSchedulerDeclare_BeforeProvisionDispatch_OrgImport pins the org-import
// path: render + declare precede the goAsync provision dispatch inside
// createWorkspaceTree.
func TestSchedulerDeclare_BeforeProvisionDispatch_OrgImport(t *testing.T) {
	assertCallBefore(t, "org_import.go", "createWorkspaceTree", "renderTemplateSchedulesYAML", "goAsync")
	assertCallBefore(t, "org_import.go", "createWorkspaceTree", "ensureSchedulerPluginDeclared", "goAsync")
}

// TestSchedulerDeclare_BeforeProvisionDispatch_Create pins the workspace
// Create path: the pre-provision declare (and the schedule parse feeding it)
// precede the provisionWorkspaceAuto dispatch. The renderTemplateSchedulesYAML
// arm is the P4b direct-create leg (issue #4411): the schedules must be
// rendered into configFiles BEFORE the dispatch captures configFiles for the
// provision goroutine — the same happens-before the org-import leg carries.
// Pre-change (render only wired into org_import.go), Create does NOT call
// renderTemplateSchedulesYAML, so this arm's "no longer calls" fail path fires.
func TestSchedulerDeclare_BeforeProvisionDispatch_Create(t *testing.T) {
	assertCallBefore(t, "workspace.go", "Create", "parseTemplateSchedules", "provisionWorkspaceAuto")
	assertCallBefore(t, "workspace.go", "Create", "ensureSchedulerPluginDeclared", "provisionWorkspaceAuto")
	assertCallBefore(t, "workspace.go", "Create", "renderTemplateSchedulesYAML", "provisionWorkspaceAuto")
}
