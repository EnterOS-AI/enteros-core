package handlers

// light_wake_paths_test.go — the pin behind Enter OS Light's wake-path
// closures. Every call site that can wake, restart or (re-)provision a
// workspace is listed here with the closure that keeps a parking or parked
// Light agent asleep there. A new call site turns this test red until someone
// decides how Light treats it and adds it below.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// lightWakeCallees are the functions that wake, restart or provision a box.
var lightWakeCallees = map[string]bool{
	"provisionWorkspaceAuto":           true,
	"provisionWorkspaceAutoSync":       true,
	"provisionWorkspaceAutoSyncLocked": true,
	"provisionWorkspaceCP":             true,
	"provisionWorkspace":               true,
	"provisionWorkspaceOpts":           true,
	"RestartWorkspaceAuto":             true,
	"RestartWorkspaceAutoOpts":         true,
	"WakeWorkspace":                    true,
	"runRestartCycle":                  true,
	"RestartByID":                      true,
	"RestartByIDAfterMutation":         true,
	"AutoRestartByID":                  true,
	"restartFunc":                      true,
}

// reviewedLightWakeSites maps "file|enclosing func|callee" to how Light closes
// that path (the closures table of the Task 3 plan).
var reviewedLightWakeSites = map[string]string{
	// A2A auto-wake: the gate in proxyA2ARequest answers 409; WakeWorkspace's claim carries the predicate.
	"a2a_proxy.go|WorkspaceHandler.resolveAgentURL|WakeWorkspace": "a2a gate + wake claim predicate",
	// Dead-container probes → AutoRestartByID → runRestartCycle (predicate in its SELECT).
	"a2a_proxy_helpers.go|WorkspaceHandler.declareContainerDead|AutoRestartByID":     "runRestartCycle predicate",
	"a2a_proxy_helpers.go|WorkspaceHandler.preflightContainerHealth|AutoRestartByID": "runRestartCycle predicate",
	"auto_restart_breaker.go|WorkspaceHandler.AutoRestartByID|runRestartCycle":       "runRestartCycle predicate",
	// New rows only.
	"org_import.go|OrgHandler.createWorkspaceTree|provisionWorkspaceAuto": "creates a new workspace",
	"workspace.go|WorkspaceHandler.Create|provisionWorkspaceAuto":         "creates a new workspace",
	// Platform agent only (Light refuses kind=platform).
	"platform_agent_ensure.go|WorkspaceHandler.triggerPlatformProvision|RestartByIDAfterMutation": "platform agent, never Light",
	// Mutation restarts → runRestartCycle: refused by its predicate and queued as a refresh.
	"plugins_install.go|PluginsHandler.uninstallViaDocker|restartFunc":             "runRestartCycle predicate + refresh",
	"plugins_install.go|PluginsHandler.uninstallViaEIC|restartFunc":                "runRestartCycle predicate + refresh",
	"plugins_install_pipeline.go|PluginsHandler.deliverToContainer|restartFunc":    "runRestartCycle predicate + refresh",
	"plugins_install_pipeline.go|PluginsHandler.reMaterialize|restartFunc":         "runRestartCycle predicate + refresh",
	"plugins_install_pipeline.go|PluginsHandler.deliverViaDocker|restartFunc":      "runRestartCycle predicate + refresh",
	"secrets.go|SecretsHandler.Set|restartFunc":                                    "runRestartCycle predicate + refresh",
	"secrets.go|SecretsHandler.Delete|restartFunc":                                 "runRestartCycle predicate + refresh",
	"secrets.go|SecretsHandler.restartAllAffectedByGlobalKey|restartFunc":          "runRestartCycle predicate + refresh",
	"secrets.go|SecretsHandler.SetModel|restartFunc":                               "runRestartCycle predicate + refresh",
	"workspace_continuous.go|ContinuousHandler.Set|restartFunc":                    "runRestartCycle predicate + refresh",
	"workspace_admin_restart.go|WorkspaceHandler.AdminRestart|RestartByID":         "runRestartCycle predicate + refresh",
	"workspace_restart.go|WorkspaceHandler.maybeRestartAfterFileWrite|RestartByID": "runRestartCycle predicate + refresh",
	"workspace_restart.go|WorkspaceHandler.restartByID|runRestartCycle":            "runRestartCycle predicate + refresh",
	// The dispatchers' own chain (reached only through the entries above).
	"workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAuto|provisionWorkspaceCP":                 "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAuto|provisionWorkspace":                   "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSync|provisionWorkspaceAutoSyncLocked": "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSyncLocked|provisionWorkspaceCP":       "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSyncLocked|provisionWorkspace":         "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAuto|RestartWorkspaceAutoOpts":               "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAutoOpts|provisionWorkspaceCP":               "dispatcher",
	"workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAutoOpts|provisionWorkspaceOpts":             "dispatcher",
	"workspace_provision.go|WorkspaceHandler.provisionWorkspace|provisionWorkspaceOpts":                     "dispatcher",
	"workspace_restart.go|WorkspaceHandler.runRestartCycle|provisionWorkspaceAutoSyncLocked":                "runRestartCycle predicate",
	// Manual restart: guard 409 + markProvisioningForRestart claim predicate.
	"workspace_restart.go|WorkspaceHandler.Restart|RestartWorkspaceAutoOpts": "restart guard + claim predicate",
	// Wake: claim predicate.
	"workspace_restart.go|WorkspaceHandler.WakeWorkspace|provisionWorkspaceAuto": "wake claim predicate",
	// Resume: guard 409 + claim predicate.
	"workspace_restart.go|WorkspaceHandler.Resume|provisionWorkspaceAuto": "resume guard + claim predicate",
	// Provider switch: guard 409 + pre-claim `light_state IS NULL`.
	"workspace_switch_provider.go|WorkspaceHandler.SwitchProvider|provisionWorkspaceAuto": "switch guard + pre-claim",
	// Light enable/disable apply the MOLECULE_LIGHT env change while the agent
	// is open (enable) or no longer Light (disable): both are outside parking/parked.
	"light_handlers.go|WorkspaceHandler.applyLightEnvChange|WakeWorkspace":            "Light enable/disable, light_state open or NULL",
	"light_handlers.go|WorkspaceHandler.applyLightEnvChange|RestartByIDAfterMutation": "Light enable/disable, light_state open or NULL",
}

func TestEveryWakeAndProvisionCallSiteIsReviewedForLight(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			enclosing := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				enclosing = receiverTypeName(fn.Recv.List[0].Type) + "." + enclosing
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				}
				if lightWakeCallees[callee] {
					found[name+"|"+enclosing+"|"+callee] = true
				}
				return true
			})
		}
	}

	var unreviewed, gone []string
	for site := range found {
		if _, ok := reviewedLightWakeSites[site]; !ok {
			unreviewed = append(unreviewed, site)
		}
	}
	for site := range reviewedLightWakeSites {
		if !found[site] {
			gone = append(gone, site)
		}
	}
	sort.Strings(unreviewed)
	sort.Strings(gone)
	if len(unreviewed) > 0 {
		t.Errorf("new wake/provision call site(s) not reviewed for Enter OS Light — decide how a parking or parked Light agent is kept asleep there, then list them in reviewedLightWakeSites:\n  %s", strings.Join(unreviewed, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("reviewed call site(s) no longer exist — remove them from reviewedLightWakeSites:\n  %s", strings.Join(gone, "\n  "))
	}
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	}
	return "?"
}
