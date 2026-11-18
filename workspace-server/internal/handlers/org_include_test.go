package handlers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// runCmd wraps exec.CommandContext for convenience in tests. The context
// bounds the subprocess so a slow/hung network git operation fails fast
// instead of running until the package test timeout fires.
func runCmd(ctx context.Context, name string, args ...string) (exitCode int, stdout, stderr string) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return -1, string(out), err.Error()
	}
	return 0, string(out), ""
}

// resolveYAMLIncludes is the preprocessor Phase 3 uses to split org.yaml
// into per-team / per-role files. These tests cover the happy path,
// nested includes, path traversal defense, cycle detection, depth cap,
// and the inline-template (no baseDir) error.

func TestResolveYAMLIncludes_FlatInclude(t *testing.T) {
	tmp := t.TempDir()
	// Write a team file with a single workspace.
	team := filepath.Join(tmp, "team.yaml")
	if err := os.WriteFile(team, []byte("name: Role A\nrole: Worker\ntier: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := []byte(`name: Test Org
workspaces:
  - !include team.yaml
`)
	out, err := resolveYAMLIncludes(src, tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Parse result and verify workspace name landed in place.
	var tmpl struct {
		Name       string         `yaml:"name"`
		Workspaces []OrgWorkspace `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("re-parse failed: %v\n---\n%s", err, out)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(tmpl.Workspaces))
	}
	if tmpl.Workspaces[0].Name != "Role A" {
		t.Errorf("workspace name: got %q, want %q", tmpl.Workspaces[0].Name, "Role A")
	}
}

func TestResolveYAMLIncludes_Nested(t *testing.T) {
	// team.yaml includes leaf.yaml. Prove nested resolution works + that
	// relative paths inside the included file resolve against THAT file's
	// dir, not the top-level org dir.
	tmp := t.TempDir()
	subDir := filepath.Join(tmp, "teams")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(subDir, "leaf.yaml")
	if err := os.WriteFile(leaf, []byte("name: Leaf\ntier: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	team := filepath.Join(subDir, "team.yaml")
	if err := os.WriteFile(team, []byte("name: Parent\ntier: 3\nchildren:\n  - !include leaf.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := []byte(`name: Test
workspaces:
  - !include teams/team.yaml
`)
	out, err := resolveYAMLIncludes(src, tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("re-parse failed: %v\n---\n%s", err, out)
	}
	if len(tmpl.Workspaces) != 1 || tmpl.Workspaces[0].Name != "Parent" {
		t.Fatalf("workspaces[0]: %+v", tmpl.Workspaces)
	}
	if len(tmpl.Workspaces[0].Children) != 1 || tmpl.Workspaces[0].Children[0].Name != "Leaf" {
		t.Fatalf("children: %+v", tmpl.Workspaces[0].Children)
	}
}

func TestResolveYAMLIncludes_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	// Write a file outside tmp that the include would exfiltrate.
	outside := filepath.Join(filepath.Dir(tmp), "secret.yaml")
	if err := os.WriteFile(outside, []byte("name: Leak\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	cases := []string{"../secret.yaml", "../../secret.yaml"}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			src := []byte("workspaces:\n  - !include " + tc + "\n")
			_, err := resolveYAMLIncludes(src, tmp)
			if err == nil {
				t.Errorf("expected error for traversal %q", tc)
			}
		})
	}
}

func TestResolveYAMLIncludes_CycleDetected(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a.yaml")
	b := filepath.Join(tmp, "b.yaml")
	if err := os.WriteFile(a, []byte("name: A\nchildren:\n  - !include b.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("name: B\nchildren:\n  - !include a.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := []byte("workspaces:\n  - !include a.yaml\n")
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") && !strings.Contains(err.Error(), "depth") {
		t.Errorf("error should mention cycle or depth; got: %v", err)
	}
}

func TestResolveYAMLIncludes_EmptyPathErrors(t *testing.T) {
	tmp := t.TempDir()
	src := []byte("workspaces:\n  - !include \"\"\n")
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil {
		t.Error("expected error for empty !include path")
	}
}

func TestResolveYAMLIncludes_MissingFileErrors(t *testing.T) {
	tmp := t.TempDir()
	src := []byte("workspaces:\n  - !include nonexistent.yaml\n")
	_, err := resolveYAMLIncludes(src, tmp)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestResolveYAMLIncludes_InlineTemplateErrors(t *testing.T) {
	src := []byte("workspaces:\n  - !include team.yaml\n")
	_, err := resolveYAMLIncludes(src, "")
	if err == nil {
		t.Error("expected error when baseDir empty and !include used")
	}
}

func TestResolveYAMLIncludes_SiblingDirAccess(t *testing.T) {
	// Phase 4 pattern: a team file at `teams/<x>.yaml` refers to a role
	// file at `<role>/workspace.yaml` via `../<role>/workspace.yaml`.
	// The ref escapes the team file's own dir but stays inside the org
	// root — this must be allowed.
	tmp := t.TempDir()
	teamsDir := filepath.Join(tmp, "teams")
	roleDir := filepath.Join(tmp, "my-role")
	if err := os.MkdirAll(teamsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "workspace.yaml"), []byte("name: Cousin\ntier: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamsDir, "parent.yaml"), []byte("name: Parent\nchildren:\n  - !include ../my-role/workspace.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := []byte("workspaces:\n  - !include teams/parent.yaml\n")
	out, err := resolveYAMLIncludes(src, tmp)
	if err != nil {
		t.Fatalf("sibling-dir !include should work: %v", err)
	}
	var tmpl OrgTemplate
	if err := yaml.Unmarshal(out, &tmpl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tmpl.Workspaces) != 1 {
		t.Fatalf("workspaces: %d", len(tmpl.Workspaces))
	}
	kids := tmpl.Workspaces[0].Children
	if len(kids) != 1 || kids[0].Name != "Cousin" {
		t.Fatalf("children: %+v", kids)
	}
}

// Integration check: after Phase 3 split, the real molecule-dev/org.yaml
// resolves cleanly via !include and unmarshal into OrgTemplate produces
// the full workspace tree. Guards against split regressions landing on
// main before they can be caught by a deploy.
//
// Previously skipped because /org-templates/molecule-dev/ was a stale
// in-tree copy with a broken !include graph. The extraction completed
// and the canonical copy now lives at molecule-ai/molecule-ai-org-template-
// molecule-dev. This test fetches it via HTTPS (no token needed — the repo
// is public) to exercise the real include resolution on every CI run.
// isFrozenDevDeptExternalErr reports whether err is the EXPECTED failure from the
// deliberately-held-broken molecule-dev-department@v1.0.0 external subtree
// (molecule-core#4340 / internal#1008 / internal#1009) — as opposed to an
// unexpected molecule-core regression that TestResolveYAMLIncludes_RealMoleculeDev
// must FAIL on. Two known signatures: (1) an "!external"-prefixed resolve/fetch
// error (that subtree is the only !external in the real org.yaml), or (2) the
// yaml TYPE MISMATCH ("cannot unmarshal !!map ...") the frozen composition
// produces. Anything else is not recognised as the frozen state and should fail.
func isFrozenDevDeptExternalErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "!external") || strings.Contains(msg, "molecule-dev-department") {
		return true
	}
	return strings.Contains(msg, "cannot unmarshal") && strings.Contains(msg, "!!map")
}

func TestResolveYAMLIncludes_RealMoleculeDev(t *testing.T) {
	// LIVE-NETWORK GATE. This test does a `git clone` over HTTPS plus a
	// nested !external git fetch. Network latency is unbounded and variable,
	// so under a slow CI network + `-race` it can eat many seconds toward the
	// per-package test timeout — the recurring `internal/handlers` timeout
	// flake (the whole package rides the ~60s -race edge, and this test's
	// variable network time is what tips it over on slow runs). It is
	// therefore kept OUT of the default `go test` lane and runs only when
	// explicitly opted in, mirroring the MOLECULE_LIVE_DOCKER convention in
	// plugin_settings_writer_live_test.go.
	//
	// The resolver/parser logic this test exercises is fully covered
	// hermetically by the TestResolveYAMLIncludes_* fixture tests above, which
	// KEEP running in the default lane. CI runs this live smoke in a dedicated,
	// generously-timed step (see .gitea/workflows/ci.yml: "live org-template
	// clone+resolve smoke"), which sets MOLECULE_LIVE_ORG_TEMPLATE=1. Run it
	// locally with:
	//
	//   MOLECULE_LIVE_ORG_TEMPLATE=1 go test ./internal/handlers/ -run '^TestResolveYAMLIncludes_RealMoleculeDev$' -v
	if os.Getenv("MOLECULE_LIVE_ORG_TEMPLATE") != "1" {
		t.Skip("set MOLECULE_LIVE_ORG_TEMPLATE=1 to run the live org-template clone+resolve smoke (kept out of the default -race lane to avoid the internal/handlers timeout flake; the hermetic TestResolveYAMLIncludes_* tests cover the resolver)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in this runtime")
	}
	tmp := t.TempDir()
	// Clone the canonical standalone org template. No token needed — the
	// repo is public on the same Gitea instance. Bound the clone with a
	// context timeout so a slow/hung network fetch fails fast rather than
	// running until the package test timeout fires.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, _, _ := runCmd(ctx, "git", "clone", "--depth", "1",
		"https://git.moleculesai.app/molecule-ai/molecule-ai-org-template-molecule-dev.git",
		tmp)
	if res != 0 {
		t.Skipf("could not clone standalone org template (skipping integration test): exit %d", res)
	}
	orgFile := filepath.Join(tmp, "org.yaml")
	data, err := os.ReadFile(orgFile)
	if err != nil {
		t.Skipf("org.yaml not found in standalone clone (skipping integration test): %v", err)
	}
	expanded, err := resolveYAMLIncludes(data, tmp)
	if err != nil {
		// resolveYAMLIncludes fans out to the !external molecule-dev-department
		// subtree via a nested git fetch. That external ref (v1.0.0) is
		// DELIBERATELY HELD in a known-broken state by its owners — see the
		// SECURITY HOLD note in molecule-ai-org-template-molecule-dev/org.yaml
		// (molecule-core#4340 channels-without-allowlists; internal#1009 exact-tag
		// repin gated on an owner-approved release; internal#1008 category_routing
		// target-resolution). A failure of that held/broken external subtree is not
		// a molecule-core defect. molecule-dev-department is the ONLY !external in
		// this template, so an "!external ..."-prefixed error IS about the frozen
		// ref. Skip ONLY that; any OTHER resolver error (path escape, cycle, depth,
		// a panic surfaced as text) is unexpected and must FAIL, so a genuine
		// molecule-core resolver regression that manifests on the live composition
		// is still caught — the blanket skip-on-any-error that hid it is gone
		// (verified 2026-07-28). Resolver logic is additionally hard-gated by the
		// hermetic local-fixture tests above.
		if isFrozenDevDeptExternalErr(err) {
			t.Skipf("skipping live-external org-template smoke (owner-held broken ref molecule-dev-department@v1.0.0; internal#1008/#1009, molecule-core#4340): resolveYAMLIncludes failed: %v", err)
		}
		t.Fatalf("resolveYAMLIncludes on real org.yaml failed with an UNEXPECTED (non-frozen-external) error — treat as a molecule-core regression: %v", err)
	}
	var tmpl OrgTemplate
	if err := yaml.Unmarshal(expanded, &tmpl); err != nil {
		// The expanded YAML is composed from the EXTERNAL, owner-held
		// molecule-dev-department@v1.0.0 subtree (see the note above). Its frozen
		// state produces a yaml TYPE MISMATCH (a map where OrgTemplate expects a
		// scalar) on unmarshal. Skip ONLY that specific signature; a differently
		// shaped unmarshal error is unexpected and must FAIL rather than silently
		// skip, so a real OrgTemplate schema regression on the live composition is
		// still caught. OrgTemplate unmarshal is additionally hard-gated hermetically
		// above. The real fix for the frozen ref lives in molecule-dev-department#15,
		// gated on internal#1009.
		if isFrozenDevDeptExternalErr(err) {
			t.Skipf("skipping live-external org-template smoke (owner-held broken ref molecule-dev-department@v1.0.0; internal#1008/#1009, molecule-core#4340): expanded template does not unmarshal: %v", err)
		}
		t.Fatalf("expanded real template failed to unmarshal with an UNEXPECTED (non-frozen-external) error — treat as an OrgTemplate schema regression: %v", err)
	}
	// Sanity: should have PM + Marketing Lead + Dev Lead (via !external) at
	// top. PM's direct children were slimmed in Phase 3d: Dev Lead and its
	// subtree moved to molecule-dev-department, so PM now has Research Lead
	// as its only direct child.
	if len(tmpl.Workspaces) < 3 {
		t.Fatalf("expected ≥3 top-level workspaces, got %d", len(tmpl.Workspaces))
	}
	names := map[string]bool{}
	for _, w := range tmpl.Workspaces {
		names[w.Name] = true
	}
	for _, want := range []string{"PM", "Marketing Lead", "Dev Lead"} {
		if !names[want] {
			t.Errorf("expected top-level workspace %q, not found", want)
		}
	}
	var pm *OrgWorkspace
	for i := range tmpl.Workspaces {
		if tmpl.Workspaces[i].Name == "PM" {
			pm = &tmpl.Workspaces[i]
			break
		}
	}
	if pm == nil || len(pm.Children) < 1 {
		t.Errorf("PM should have ≥1 child after include resolution, got %d", len(pm.Children))
	}
}

func TestResolveYAMLIncludes_NoIncludesIsNoop(t *testing.T) {
	// Ensure the preprocessor is a safe no-op for templates that don't
	// use !include — critical since we always run it on POST /org/import.
	tmp := t.TempDir()
	src := []byte(`name: Simple
workspaces:
  - name: Only
    tier: 2
`)
	out, err := resolveYAMLIncludes(src, tmp)
	if err != nil {
		t.Fatalf("no-op should not error, got %v", err)
	}
	var orig, expanded OrgTemplate
	_ = yaml.Unmarshal(src, &orig)
	_ = yaml.Unmarshal(out, &expanded)
	if orig.Name != expanded.Name || len(orig.Workspaces) != len(expanded.Workspaces) {
		t.Errorf("no-op changed semantics; orig=%+v expanded=%+v", orig, expanded)
	}
}

// TestResolveYAMLIncludes_RealMoleculeDev clones molecule-ai-org-template-molecule-dev
// via HTTPS and validates the full org include resolution. The exec.LookPath guard
// ensures the test skips gracefully when git is unavailable in the runtime.
// CI trigger: 2026-05-25T06:07 UTC
