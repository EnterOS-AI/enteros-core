package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The mini-company org template is pinned in manifest.json (org_templates ->
// mini-company) and is the showcase tree the plugin-configuration programme
// demonstrates against. Its shape is a CONTRACT: 7 agents arranged in 4 levels.
//
// WHY THIS FILE EXISTS. The programme's checklist recorded this shape as
// "proven" via:
//
//	MINI_COMPANY_DIR=<checkout> go test ./internal/handlers/ -run TestMiniCompany -v
//
// That test was never committed — `TestMiniCompany` existed in no repo. Running
// the documented command prints:
//
//	testing: warning: no tests to run
//	PASS
//
// i.e. it reads GREEN while executing nothing. That is the same vacuous-pass
// class as the e2e_busy_inject seam (task #112, "a test no workflow names is
// never run") and the path-gated template-delivery lane that reported a no-op
// pass for months. A pin bump to a regressed tree would therefore have landed
// unnoticed.
//
// The assertions below drive the REAL loader path — resolveYAMLIncludes (the
// same expander ListTemplates/org-import use) followed by a yaml.Unmarshal into
// OrgTemplate — rather than re-implementing a walker in the test. A test that
// re-implements the parser proves the test's parser works, not the product's.
//
// NON-VACUITY: this test must never silently skip in CI. If the template tree
// cannot be located while CI is set, it FAILS. A skip in CI would recreate
// exactly the false green it was written to prevent. See the
// e2e_busy_inject "--- PASS:" grep guard in .gitea/workflows/ci.yml for the
// companion half of this contract.

// miniCompanyTemplateDir locates the pinned mini-company checkout.
//
// Order: explicit MINI_COMPANY_DIR (local iteration), then the manifest clone
// destination populated by `make bundle-deps` (.tenant-bundle-deps/org-templates),
// which is the same tree Dockerfile COPYs into the tenant image.
func miniCompanyTemplateDir(t *testing.T) string {
	t.Helper()

	var tried []string
	if d := strings.TrimSpace(os.Getenv("MINI_COMPANY_DIR")); d != "" {
		if fileExists(filepath.Join(d, "org.yaml")) {
			return d
		}
		tried = append(tried, d+" (from MINI_COMPANY_DIR)")
	}
	// workspace-server/internal/handlers -> repo root
	bundled := filepath.Join("..", "..", "..", ".tenant-bundle-deps", "org-templates", "mini-company")
	if fileExists(filepath.Join(bundled, "org.yaml")) {
		return bundled
	}
	tried = append(tried, bundled+" (make bundle-deps)")

	msg := fmt.Sprintf("mini-company org template not found; looked in: %s", strings.Join(tried, ", "))
	if inCI() {
		// Fail, never skip. A skipped test in CI is indistinguishable from a
		// passing one in the summary — which is the precise failure this
		// guard exists to close.
		t.Fatalf("%s\nCI must run this guard: populate the tree (`make bundle-deps`) or set MINI_COMPANY_DIR.", msg)
	}
	t.Skipf("%s\nRun `make bundle-deps` (or set MINI_COMPANY_DIR) to exercise this guard locally.", msg)
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func inCI() bool {
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITEA_ACTIONS"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" && v != "false" && v != "0" {
			return true
		}
	}
	return false
}

// loadMiniCompany expands !include and unmarshals through the product's own
// code path.
func loadMiniCompany(t *testing.T) OrgTemplate {
	t.Helper()
	dir := miniCompanyTemplateDir(t)

	raw, err := os.ReadFile(filepath.Join(dir, "org.yaml"))
	if err != nil {
		t.Fatalf("read org.yaml: %v", err)
	}
	expanded, err := resolveYAMLIncludes(raw, dir)
	if err != nil {
		// A broken include is the most likely real regression here: the tree is
		// split one-file-per-agent, so a renamed or deleted file breaks
		// expansion rather than changing a count.
		t.Fatalf("resolveYAMLIncludes failed — the per-agent !include layout is broken: %v", err)
	}
	var org OrgTemplate
	if err := yaml.Unmarshal(expanded, &org); err != nil {
		t.Fatalf("unmarshal expanded org.yaml: %v", err)
	}
	return org
}

// TestMiniCompanyOrgTemplate_SevenAgents pins the agent count through the real
// recursive counter used at import time.
func TestMiniCompanyOrgTemplate_SevenAgents(t *testing.T) {
	org := loadMiniCompany(t)

	const wantAgents = 7
	if got := countWorkspaces(org.Workspaces); got != wantAgents {
		t.Fatalf("mini-company declares %d agents, want %d — the manifest.json pin points at a tree whose shape changed; names seen: %v",
			got, wantAgents, collectNames(org.Workspaces))
	}
}

// TestMiniCompanyOrgTemplate_FourLevelShape pins the actual hierarchy, not just
// its size. A count alone would pass if an agent were re-parented — which is a
// behavioural change (delegation flows down the tree), not a cosmetic one.
func TestMiniCompanyOrgTemplate_FourLevelShape(t *testing.T) {
	org := loadMiniCompany(t)

	// level 0 is the org itself; agents occupy levels 1..3.
	want := map[string]int{
		"Company Coordinator":  1,
		"Marketing Manager":    2,
		"Accounting":           2,
		"Legal":                2,
		"Content Writer":       3,
		"SEO Specialist":       3,
		"Social Media Manager": 3,
	}

	got := map[string]int{}
	var walk func(ws []OrgWorkspace, depth int)
	walk = func(ws []OrgWorkspace, depth int) {
		for _, w := range ws {
			got[w.Name] = depth
			walk(w.Children, depth+1)
		}
	}
	walk(org.Workspaces, 1)

	for name, wantDepth := range want {
		gotDepth, ok := got[name]
		if !ok {
			t.Errorf("agent %q missing from the pinned tree (found: %v)", name, collectNames(org.Workspaces))
			continue
		}
		if gotDepth != wantDepth {
			t.Errorf("agent %q sits at level %d, want %d — re-parenting changes delegation flow", name, gotDepth, wantDepth)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected agent %q in the pinned tree — update this guard deliberately if the tree grew", name)
		}
	}

	maxDepth := 0
	for _, d := range got {
		if d > maxDepth {
			maxDepth = d
		}
	}
	if maxDepth != 3 {
		t.Errorf("deepest agent level is %d, want 3 (a 4-level tree counting the org root)", maxDepth)
	}
}

func collectNames(ws []OrgWorkspace) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Name)
		out = append(out, collectNames(w.Children)...)
	}
	return out
}
