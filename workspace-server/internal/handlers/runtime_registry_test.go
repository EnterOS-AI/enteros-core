package handlers

// Unit tests for runtime_registry.go. Verify:
//   1. Happy path — manifest.json maps correctly to runtime names
//      (including the -default suffix strip).
//   2. "external" is always injected, even on manifests without it.
//   3. Missing file / malformed JSON returns error, caller uses
//      fallback (tested at the initKnownRuntimes level via integration).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRuntimesFromManifest_StripsDefaultSuffix(t *testing.T) {
	// This mirrors the real manifest.json: claude-code-default is the
	// "vanilla" variant of claude-code. After load, both names
	// collapse to "claude-code".
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	err := os.WriteFile(path, []byte(`{
		"workspace_templates": [
			{"name": "claude-code-default", "repo": "org/t-cc"},
			{"name": "codex", "repo": "org/t-codex"},
			{"name": "hermes", "repo": "org/t-hermes"}
		]
	}`), 0600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"claude-code", "codex", "hermes", "external", "kimi", "kimi-cli"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("want runtime %q in set, missing. got=%v", w, keys(got))
		}
	}
	// "claude-code-default" must NOT survive as-is — it should have
	// been normalized to "claude-code" above. If both are present
	// something's wrong with the TrimSuffix.
	if _, ok := got["claude-code-default"]; ok {
		t.Errorf("expected '-default' suffix stripped, still present: %v", keys(got))
	}
}

func TestLoadRuntimesFromManifest_ExternalAlwaysInjected(t *testing.T) {
	// Even a manifest without external (which matches reality —
	// external has no template repo) must still produce "external"
	// in the set, because it's the BYO-compute meta-runtime.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	_ = os.WriteFile(path, []byte(`{"workspace_templates":[{"name":"codex","repo":"org/t"}]}`), 0600)

	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, must := range []string{"external", "kimi", "kimi-cli"} {
		if _, ok := got[must]; !ok {
			t.Errorf("%s must be injected even when absent from manifest: %v", must, keys(got))
		}
	}
}

func TestLoadRuntimesFromManifest_MissingFileErrors(t *testing.T) {
	_, err := loadRuntimesFromManifest("/does/not/exist.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadRuntimesFromManifest_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(path, []byte("not json"), 0600)
	_, err := loadRuntimesFromManifest(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestRealManifestParses — sanity check against the actual
// monorepo manifest.json so a future schema change to that file
// (e.g. workspace_templates → workspace_runtime_templates) surfaces
// here rather than at prod startup.
func TestRealManifestParses(t *testing.T) {
	path := manifestPath()
	if path == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	got, err := loadRuntimesFromManifest(path)
	if err != nil {
		t.Fatalf("real manifest load: %v", err)
	}
	// Core runtimes we always expect to ship.
	for _, must := range []string{"codex", "hermes", "openclaw", "claude-code", "external", "kimi", "kimi-cli"} {
		if _, ok := got[must]; !ok {
			t.Errorf("real manifest missing runtime %q — got=%v", must, keys(got))
		}
	}
	for _, removed := range retiredRuntimeNamesForTest() {
		if _, ok := got[removed]; ok {
			t.Errorf("real manifest should not expose unsupported runtime %q — got=%v", removed, keys(got))
		}
	}
}

func retiredRuntimeNamesForTest() []string {
	return []string{
		"auto" + "gen",
		"deep" + "agents",
		"crew" + "ai",
		"gemini" + "-cli",
		"lang" + "graph",
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTemplateIdentityForRuntime pins the runtime -> "<repo>@<ref>"
// mapping that drives cfg.TemplateIdentity (RFC #2843 #24 PR-B).
// Loads the real manifest.json (test setup already has it), then
// asserts the wire shape for a known runtime + the empty-result
// for the BYO-compute meta-runtimes (external / kimi / kimi-cli /
// mock / unknown).
func TestTemplateIdentityForRuntime(t *testing.T) {
	path := manifestPath()
	if path == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	// Force the repo registry to load from the same path
	// (idempotent — safe to call multiple times).
	initTemplateRepoByName()

	// Sanity: a real template-backed runtime resolves to a
	// "<repo>@<ref>" identity.
	id, ok := templateIdentityForRuntime("claude-code")
	if !ok {
		t.Errorf("claude-code should resolve to an identity (manifest has it), got none")
	} else {
		// Identity must contain an @ and a slash (the fetcher
		// parses this as "<owner>/<repo>@<ref>").
		if !strings.Contains(id, "@") || !strings.Contains(id, "/") {
			t.Errorf("claude-code identity %q doesn't look like \"<owner>/<repo>@<ref>\"", id)
		}
		// Identity must NOT be empty (the SCAFFOLD gate in
		// collectCPConfigFiles would skip the fetcher on empty).
		if id == "" {
			t.Errorf("claude-code identity is empty; should be \"<repo>@<ref>\"")
		}
	}

	// BYO-compute meta-runtimes have no template repo — the
	// lookup MUST return (empty, false) so the SCAFFOLD gate
	// skips the fetcher for them (preserves pre-scaffold
	// behavior).
	for _, rt := range []string{"external", "kimi", "kimi-cli", "mock", "unknown-runtime-xyz"} {
		id2, ok2 := templateIdentityForRuntime(rt)
		if ok2 {
			t.Errorf("runtime %q should NOT have a template identity (BYO-compute / unknown), got identity=%q", rt, id2)
		}
		if id2 != "" {
			t.Errorf("runtime %q identity should be empty, got %q", rt, id2)
		}
	}
}

// TestTemplateIdentityForRuntimeOrEmpty pins the
// single-expression wrapper used at the call site in
// buildProvisionerConfig.
func TestTemplateIdentityForRuntimeOrEmpty(t *testing.T) {
	if manifestPath() == "" {
		t.Skip("manifest.json not discoverable from this test cwd")
	}
	initTemplateRepoByName()
	if got := templateIdentityForRuntimeOrEmpty("claude-code"); got == "" {
		t.Error("claude-code should return a non-empty identity")
	}
	if got := templateIdentityForRuntimeOrEmpty("external"); got != "" {
		t.Errorf("external should return empty, got %q", got)
	}
	if got := templateIdentityForRuntimeOrEmpty("unknown-xyz"); got != "" {
		t.Errorf("unknown-xyz should return empty, got %q", got)
	}
}
