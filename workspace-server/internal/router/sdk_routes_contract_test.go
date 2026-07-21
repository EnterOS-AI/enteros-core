package router

// SDK route derive-gate (RFC molecule-core#4428, Phase 1 — SHADOW / non-blocking).
//
// Asserts that every SDK-owned workspace-comms route declared in the vendored
// contract binding (molcontracts.SDKRoutes — the registry + A2A lane) is
// actually registered in router.go under the same HTTP method. This is the
// core-side derive-gate for issue #87: the SDK manifest is the SSOT for the
// contract lane, and this test proves core still serves every route the SSOT
// declares.
//
// Phase 1 runs this ONLY as a post-merge / scheduled shadow signal
// (.gitea/workflows/sdk-route-milestone-contract-drift.yml, push[main]/schedule,
// NOT pull_request) — per task #113, a pull_request-triggered status would post
// a commit status that core main's branch-protection required_contexts=['*']
// counts, jamming the merge queue even with continue-on-error. It is therefore
// NOT wired to pull_request and NOT in .gitea/required-contexts.txt.
//
// The router.go parser + gin :param/*wild matcher below are a Go port of the
// reference matcher in tests/e2e/lib/assert_e2e_tenant_contract.py:117-144.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"go.moleculesai.app/sdk/gen/go/molcontracts"
)

// A group's Go variable -> its path prefix, e.g. wsAuth := r.Group("/workspaces/:id", ...)
var groupRE = regexp.MustCompile(`(\w+)\s*:?=\s*\w+\.Group\(\s*"([^"]*)"`)

// A route registration, e.g. wsAuth.GET("/activity", ...)
var routeRE = regexp.MustCompile(`(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any)\(\s*"([^"]*)"`)

type registeredRoute struct {
	method string
	path   string // raw gin pattern (group prefix + path)
}

// registeredRoutes parses router.go the same way the Python reference matcher
// does: a flat map of group-variable -> prefix, then every route registration
// gets its owning group's prefix prepended. `Any` fans out to all methods.
func registeredRoutes(t *testing.T) []registeredRoute {
	t.Helper()
	src := readRouterSource(t)

	prefixes := map[string]string{}
	for _, m := range groupRE.FindAllStringSubmatch(src, -1) {
		prefixes[m[1]] = m[2]
	}

	anyMethods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	var out []registeredRoute
	for _, m := range routeRE.FindAllStringSubmatch(src, -1) {
		varName, method, path := m[1], m[2], m[3]
		full := prefixes[varName] + path
		methods := []string{method}
		if method == "Any" {
			methods = anyMethods
		}
		for _, mm := range methods {
			out = append(out, registeredRoute{method: mm, path: full})
		}
	}
	return out
}

func readRouterSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate router.go")
	}
	routerPath := filepath.Join(filepath.Dir(thisFile), "router.go")
	b, err := os.ReadFile(routerPath)
	if err != nil {
		t.Fatalf("read router.go (%s): %v", routerPath, err)
	}
	return string(b)
}

func segments(p string) []string {
	// Drop any query string, then split on "/" discarding empty segments.
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// matches reports whether an SDK-declared path (`call`) is served by a gin
// route pattern (`route`). Port of assert_e2e_tenant_contract.py:matches:
//
//	':param' in the route matches exactly ONE call segment
//	'*wild'  in the route swallows the REST of the path (gin requires it last)
//	'$VAR'   in the call matches any single route segment (shell-var permissive)
func matches(call, route string) bool {
	c, r := segments(call), segments(route)
	i := 0
	for _, rseg := range r {
		if strings.HasPrefix(rseg, "*") {
			return true // wildcard swallows the remainder
		}
		if i >= len(c) {
			return false
		}
		cseg := c[i]
		if strings.HasPrefix(rseg, ":") || strings.HasPrefix(cseg, "$") {
			i++
			continue
		}
		if rseg != cseg {
			return false
		}
		i++
	}
	return i == len(c)
}

// contractDriftGateEnv, when unset, makes the shadow derive-gates SKIP. This is
// the mechanism that keeps them NON-BLOCKING in Phase 1: ci.yml runs
// `go test ./...` on every pull_request inside the required `CI / all-required`
// job, so an UNGUARDED drift here would flip that required status red and block
// the PR — the exact "shadow gate must not block a PR" failure #113 warns about.
// The gate is executed for real ONLY by
// .gitea/workflows/sdk-route-milestone-contract-drift.yml (push[main]/dispatch),
// which sets this env var. Locally: MOLECULE_RUN_CONTRACT_DRIFT_GATES=1 go test ...
const contractDriftGateEnv = "MOLECULE_RUN_CONTRACT_DRIFT_GATES"

// capturedRoutesPendingSDKModel are workspace-server routes the e2e suite
// exercises that today are contract-checked ONLY because
// tests/e2e/lib/assert_e2e_tenant_contract.py validates the LIVE e2e call sites
// against router.go. They are NOT (yet) declared in the SDK route SSOT
// (workspace-comms/routes.manifest.json → molcontracts.SDKRoutes), whose bounded
// scope is registry+a2a (SDKRoutesOwnedScope). The workspace-request lane
// (wsAuth `/workspaces/:id/requests*` + the admin `/requests/*` mirror) sits
// outside that scope, so the SSOT is silent on it.
//
// This matters now because the post-push staging E2E lanes are being retired in
// favour of the per-PR ephemeral gate (task #86). Once those lanes go, the e2e
// call sites that keep the Python guard honest disappear, and this derive-gate
// becomes the ONLY structural proof that core still serves these routes.
// Retiring staging must NOT narrow the sole contract-check of them — so we
// CAPTURE them here first (capture-first per RFC molecule-core#4428, issue #87)
// and hold core to serving each one under the same method.
//
// FOLLOW-UP (RFC #4428 D1/D2, issue #87): absorb these into
// workspace-comms/routes.manifest.json in molecule-ai-sdk (a `requests` scope, or
// widening SDKRoutesOwnedScope) and regenerate molcontracts.SDKRoutes, then delete
// this slice so the SDK manifest is once again the single SSOT. The
// list_peers/A2A-peers route (GET /registry/:id/peers) is intentionally NOT here:
// it is already declared in SDKRoutes (id="peers").
var capturedRoutesPendingSDKModel = []molcontracts.Route{
	{ID: "workspace_request_create", Method: "POST", Path: "/workspaces/:id/requests"},
	{ID: "workspace_requests_pending", Method: "GET", Path: "/requests/pending"},
	{ID: "workspace_request_add_message", Method: "POST", Path: "/requests/:id/messages"},
}

// routeRegistered reports whether some registered gin route serves the given
// SDK-declared method+path (via the gin :param/*wild matcher).
func routeRegistered(routes []registeredRoute, method, path string) bool {
	for _, rr := range routes {
		if rr.method == method && matches(path, rr.path) {
			return true
		}
	}
	return false
}

// TestSDKRoutesContract is the shadow route derive-gate: every molcontracts.SDKRoutes
// entry — plus every capturedRoutesPendingSDKModel entry (capture-first, issue #87)
// — must be registered in router.go under the same method.
func TestSDKRoutesContract(t *testing.T) {
	if os.Getenv(contractDriftGateEnv) == "" {
		t.Skipf("shadow contract-drift gate (RFC #4428 Phase 1, issue #87) — set %s=1 to run. "+
			"Skipped by default so it never blocks a PR via ci.yml's `go test ./...`; it executes "+
			"post-merge in sdk-route-milestone-contract-drift.yml.", contractDriftGateEnv)
	}
	routes := registeredRoutes(t)
	if len(routes) == 0 {
		t.Fatal("parsed zero routes from router.go — parser or file layout changed")
	}
	if len(molcontracts.SDKRoutes) == 0 {
		t.Fatal("molcontracts.SDKRoutes is empty — vendored binding is wrong")
	}

	for _, sr := range molcontracts.SDKRoutes {
		if !routeRegistered(routes, sr.Method, sr.Path) {
			t.Errorf("SDK contract route %s %s (id=%q) is NOT registered in router.go under method %s — "+
				"either core dropped a contract route or the SDK manifest drifted (RFC #4428, issue #87)",
				sr.Method, sr.Path, sr.ID, sr.Method)
		}
	}

	// Capture-first routes: same check, but these are core-captured pending SDK
	// modelling (see capturedRoutesPendingSDKModel). Losing one means retiring
	// staging E2E silently dropped the only contract-check of a live route.
	for _, cr := range capturedRoutesPendingSDKModel {
		if !routeRegistered(routes, cr.Method, cr.Path) {
			t.Errorf("captured route %s %s (id=%q) is NOT registered in router.go under method %s — "+
				"a route the e2e suite exercises lost its registration, and it is NOT covered by "+
				"molcontracts.SDKRoutes (RFC #4428 capture-first, issue #87). Re-add it to router.go "+
				"or, if genuinely removed, drop it from capturedRoutesPendingSDKModel.",
				cr.Method, cr.Path, cr.ID, cr.Method)
		}
	}
}
