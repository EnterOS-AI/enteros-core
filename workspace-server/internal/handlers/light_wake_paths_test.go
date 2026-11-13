package handlers

// light_wake_paths_test.go — the pin behind Enter OS Light's wake-path
// closures. Every reference to a function that can wake, restart or
// (re-)provision a workspace — a call, or a method value handed on to be
// called later (NewSecretsHandler(wh.RestartByID)) — is counted per (file,
// enclosing function, callee), in this package and in the packages that wire
// the handlers (router, cmd/server). Each site is listed below with how many
// references it has and the closure that keeps a parking or parked Light agent
// asleep there. A new site, or one more reference inside a reviewed function,
// turns this test red until someone decides how Light treats it.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
	// The plugins handler hands its restartFunc out (the drift applier calls it).
	"GetRestartFunc": true,
}

// lightWakeScanDirs are the packages scanned, relative to this one.
var lightWakeScanDirs = []struct{ dir, prefix string }{
	{".", "handlers/"},
	{filepath.Join("..", "router"), "router/"},
	{filepath.Join("..", "..", "cmd", "server"), "cmd/server/"},
}

type reviewedWakeSite struct {
	n       int    // references in that function
	closure string // how Light keeps a parking or parked agent asleep there
}

// reviewedLightWakeSites maps "pkg/file|enclosing func|callee" to its reviewed
// reference count and closure (the closures table of the Task 3 plan).
var reviewedLightWakeSites = map[string]reviewedWakeSite{
	// A2A auto-wake: the gate in proxyA2ARequest answers 409; WakeWorkspace's claim carries the predicate.
	"handlers/a2a_proxy.go|WorkspaceHandler.resolveAgentURL|WakeWorkspace": {1, "a2a gate + wake claim predicate"},
	// Dead-container probes → AutoRestartByID → runRestartCycle (predicate in its SELECT and its provisioning write).
	"handlers/a2a_proxy_helpers.go|WorkspaceHandler.declareContainerDead|AutoRestartByID":     {1, "runRestartCycle predicates"},
	"handlers/a2a_proxy_helpers.go|WorkspaceHandler.preflightContainerHealth|AutoRestartByID": {1, "runRestartCycle predicates"},
	"handlers/auto_restart_breaker.go|WorkspaceHandler.AutoRestartByID|runRestartCycle":       {1, "runRestartCycle predicates"},
	// New rows only.
	"handlers/org_import.go|OrgHandler.createWorkspaceTree|provisionWorkspaceAuto": {1, "creates a new workspace"},
	"handlers/workspace.go|WorkspaceHandler.Create|provisionWorkspaceAuto":         {1, "creates a new workspace"},
	// Platform agent only (Light refuses kind=platform).
	"handlers/platform_agent_ensure.go|WorkspaceHandler.triggerPlatformProvision|RestartByIDAfterMutation": {1, "platform agent, never Light"},
	// Mutation restarts → runRestartCycle: refused by its predicates and queued as a refresh.
	// Each restartFunc site is a nil check plus the call (2 references).
	"handlers/plugins_install.go|PluginsHandler.uninstallViaDocker|restartFunc":             {2, "runRestartCycle predicates + refresh"},
	"handlers/plugins_install.go|PluginsHandler.uninstallViaEIC|restartFunc":                {2, "runRestartCycle predicates + refresh"},
	"handlers/plugins_install_pipeline.go|PluginsHandler.deliverToContainer|restartFunc":    {2, "runRestartCycle predicates + refresh"},
	"handlers/plugins_install_pipeline.go|PluginsHandler.reMaterialize|restartFunc":         {2, "runRestartCycle predicates + refresh"},
	"handlers/plugins_install_pipeline.go|PluginsHandler.deliverViaDocker|restartFunc":      {2, "runRestartCycle predicates + refresh"},
	"handlers/secrets.go|SecretsHandler.Set|restartFunc":                                    {2, "runRestartCycle predicates + refresh"},
	"handlers/secrets.go|SecretsHandler.Delete|restartFunc":                                 {2, "runRestartCycle predicates + refresh"},
	"handlers/secrets.go|SecretsHandler.restartAllAffectedByGlobalKey|restartFunc":          {2, "runRestartCycle predicates + refresh"},
	"handlers/secrets.go|SecretsHandler.SetModel|restartFunc":                               {2, "runRestartCycle predicates + refresh"},
	"handlers/workspace_continuous.go|ContinuousHandler.Set|restartFunc":                    {2, "runRestartCycle predicates + refresh"},
	"handlers/workspace_admin_restart.go|WorkspaceHandler.AdminRestart|RestartByID":         {1, "runRestartCycle predicates + refresh"},
	"handlers/workspace_restart.go|WorkspaceHandler.maybeRestartAfterFileWrite|RestartByID": {1, "runRestartCycle predicates + refresh"},
	"handlers/workspace_restart.go|WorkspaceHandler.restartByID|runRestartCycle":            {1, "runRestartCycle predicates + refresh"},
	// The plugin drift applier restarts through the plugins handler's restartFunc
	// (wh.RestartByIDAfterMutation), handed out by GetRestartFunc.
	"handlers/plugins_install_pipeline.go|PluginsHandler.GetRestartFunc|restartFunc":               {1, "runRestartCycle predicates + refresh"},
	"handlers/admin_plugin_drift.go|AdminPluginDriftHandler.FragmentChanged|GetRestartFunc":        {1, "runRestartCycle predicates + refresh"},
	"handlers/admin_plugin_drift.go|AdminPluginDriftHandler.applyRestartAfterDrift|GetRestartFunc": {1, "runRestartCycle predicates + refresh"},
	// The dispatchers' own chain (reached only through the entries above).
	"handlers/workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAuto|provisionWorkspaceCP":                 {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAuto|provisionWorkspace":                   {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSync|provisionWorkspaceAutoSyncLocked": {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSyncLocked|provisionWorkspaceCP":       {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.provisionWorkspaceAutoSyncLocked|provisionWorkspace":         {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAuto|RestartWorkspaceAutoOpts":               {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAutoOpts|provisionWorkspaceCP":               {1, "dispatcher"},
	"handlers/workspace_dispatchers.go|WorkspaceHandler.RestartWorkspaceAutoOpts|provisionWorkspaceOpts":             {1, "dispatcher"},
	"handlers/workspace_provision.go|WorkspaceHandler.provisionWorkspace|provisionWorkspaceOpts":                     {1, "dispatcher"},
	"handlers/workspace_restart.go|WorkspaceHandler.runRestartCycle|provisionWorkspaceAutoSyncLocked":                {1, "runRestartCycle predicates (after its provisioning write)"},
	// Manual restart: guard 409 + markProvisioningForRestart claim predicate.
	"handlers/workspace_restart.go|WorkspaceHandler.Restart|RestartWorkspaceAutoOpts": {1, "restart guard + claim predicate"},
	// Wake: claim predicate.
	"handlers/workspace_restart.go|WorkspaceHandler.WakeWorkspace|provisionWorkspaceAuto": {1, "wake claim predicate"},
	// Resume: guard 409 + claim predicate.
	"handlers/workspace_restart.go|WorkspaceHandler.Resume|provisionWorkspaceAuto": {1, "resume guard + claim predicate"},
	// Provider switch: guard 409 + pre-claim `light_state IS NULL`.
	"handlers/workspace_switch_provider.go|WorkspaceHandler.SwitchProvider|provisionWorkspaceAuto": {1, "switch guard + pre-claim"},
	// Light enable/disable apply the MOLECULE_LIGHT env change while the agent
	// is open (enable) or no longer Light (disable): both are outside parking/parked.
	"handlers/light_handlers.go|WorkspaceHandler.applyLightEnvChange|WakeWorkspace":            {1, "Light enable/disable, light_state open or NULL"},
	"handlers/light_handlers.go|WorkspaceHandler.applyLightEnvChange|RestartByIDAfterMutation": {1, "Light enable/disable, light_state open or NULL"},
	// Light's own wake: open pauses the CronJobs, claims 'opening' (which the
	// wake predicate allows) and only then wakes.
	"handlers/light_park.go|WorkspaceHandler.openLight|WakeWorkspace": {1, "Light open, after the pause, light_state opening"},
	// The wiring packages hand method values on; each is reached only through
	// a closure above.
	"router/router.go|Setup|RestartByID":              {2, "secrets handlers (workspace, global) → runRestartCycle predicates + refresh"},
	"router/router.go|Setup|RestartByIDAfterMutation": {2, "24/7 switch, plugins → runRestartCycle predicates + refresh"},
	"cmd/server/main.go|main|RestartByID":             {1, "platform agent boot provision, never Light"},
	"cmd/server/main.go|main|AutoRestartByID":         {2, "offline hooks, stall watchdog → runRestartCycle predicates"},
}

// scanLightWakeReferences counts, per "pkg/file|enclosing func|callee", the
// selector references (calls and method values) and bare calls of the wake
// functions in the non-test Go files of dirs.
func scanLightWakeReferences(t *testing.T) map[string]int {
	t.Helper()
	found := map[string]int{}
	fset := token.NewFileSet()
	for _, d := range lightWakeScanDirs {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", d.dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(d.dir, name), nil, 0)
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
					if callee := lightWakeReference(n); callee != "" {
						found[d.prefix+name+"|"+enclosing+"|"+callee]++
					}
					return true
				})
			}
		}
	}
	return found
}

func TestEveryWakeAndProvisionCallSiteIsReviewedForLight(t *testing.T) {
	found := scanLightWakeReferences(t)
	var problems []string
	for site, n := range found {
		r, ok := reviewedLightWakeSites[site]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("new: %s (%d reference(s))", site, n))
		case r.n != n:
			problems = append(problems, fmt.Sprintf("changed: %s has %d reference(s), %d reviewed", site, n, r.n))
		}
	}
	for site := range reviewedLightWakeSites {
		if _, ok := found[site]; !ok {
			problems = append(problems, "gone: "+site)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("wake/provision references differ from the ones reviewed for Enter OS Light — decide how a parking or parked Light agent is kept asleep at each, then update reviewedLightWakeSites:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// The pin must see what the review found it missing: a second reference in an
// already reviewed function, and a method value that is not called on the spot.
func TestLightWakePinCountsReferencesAndMethodValues(t *testing.T) {
	src := `package p
func (h *WorkspaceHandler) Resume() {
	h.provisionWorkspaceAuto(1)
	h.provisionWorkspaceAuto(2)
	go NewSecretsHandler(h.RestartByID)
	restartFunc("x")
}`
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		if callee := lightWakeReference(n); callee != "" {
			got[callee]++
		}
		return true
	})
	want := map[string]int{"provisionWorkspaceAuto": 2, "RestartByID": 1, "restartFunc": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s counted %d times, want %d (got %v)", k, got[k], v, got)
		}
	}
}

// lightWakeReference names the wake function n refers to: a selector (a call
// h.WakeWorkspace(...) or a method value wh.RestartByID) or a bare call
// (restartFunc(id)). "" for anything else.
func lightWakeReference(n ast.Node) string {
	var callee string
	switch x := n.(type) {
	case *ast.SelectorExpr:
		callee = x.Sel.Name
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok {
			callee = id.Name
		}
	}
	if lightWakeCallees[callee] {
		return callee
	}
	return ""
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
