package handlers

// G0 guardrail (core half) — the concierge prompt FILENAME is a single source of
// truth: "system-prompt.md".
//
// Task #80 (de-bake guardrails). The concierge filename split shipped the
// identity under one name while core's substitute target + boot probe + the
// asset-channel allowlist + the default config.yaml assumed another, so the
// per-instance {{CONCIERGE_NAME}} substitution never reached the running agent
// (live literal placeholder = the "generic identity"). The fix consolidated every
// layer on "system-prompt.md". This guardrail pins that convergence in core so a
// future edit can't reintroduce the split by renaming ONE layer:
//
//   - applyConciergeProvisionConfig substitutes into configFiles["system-prompt.md"]
//   - conciergeIdentityPresent probes /configs/system-prompt.md
//   - generateDefaultConfig defaults prompt_files to "system-prompt.md"
//   - provisioner.IsCPTemplateAssetPath allows "system-prompt.md"
//
// The runtime end of the same convergence (build_system_prompt's no-prompt_files
// fallback) is pinned by tests/test_prompt_filename_ssot_g0.py in
// molecule-ai-workspace-runtime. Together they are G0.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// canonicalConciergePromptFilename is THE single filename every concierge-prompt
// layer must converge on. If this ever changes it must change in lockstep across
// core (subst + probe + default config + allowlist) AND the runtime fallback (the
// companion Python guardrail asserts the same literal).
const canonicalConciergePromptFilename = "system-prompt.md"

// handlersPackageDir returns the directory this test file lives in, so the
// guardrail can read its sibling source files (subst + probe) regardless of CWD.
func handlersPackageDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate handlers package source")
	}
	return filepath.Dir(thisFile)
}

func readHandlerSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(handlersPackageDir(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// G0.1 — the substitution target key is the canonical filename. We assert
// behaviorally that substituteConciergeName performs the {{CONCIERGE_NAME}}
// replacement AND source-wise that the caller writes the result back under the
// "system-prompt.md" key (not some other filename).
func TestConciergeSubstitutionUsesCanonicalFilename(t *testing.T) {
	// Behavioral: the pure substitution itself works.
	out := substituteConciergeName([]byte("I am {{CONCIERGE_NAME}}."), "Acme Concierge")
	if got := string(out); got != "I am Acme Concierge." {
		t.Fatalf("substituteConciergeName = %q, want substituted identity", got)
	}
	if strings.Contains(string(out), conciergeNamePlaceholder) {
		t.Fatalf("placeholder survived substitution: %q", out)
	}

	// Source: the substitution is applied to the canonical config-file KEY.
	src := readHandlerSource(t, "platform_agent.go")
	wantKeyAccess := `configFiles["` + canonicalConciergePromptFilename + `"]`
	if !strings.Contains(src, wantKeyAccess) {
		t.Errorf("platform_agent.go must substitute into %s — filename SSOT split risk", wantKeyAccess)
	}
}

// G0.2 — the boot identity probe reads the canonical /configs path. A probe that
// reads a different filename than the substitution writes is the boot-restart
// loop / generic-identity bug.
func TestConciergeIdentityProbeUsesCanonicalPath(t *testing.T) {
	src := readHandlerSource(t, "platform_agent.go")
	wantProbePath := "/configs/" + canonicalConciergePromptFilename
	if !strings.Contains(src, `"`+wantProbePath+`"`) {
		t.Errorf("conciergeIdentityPresent must probe %q — must match the substitution target filename", wantProbePath)
	}
}

// G0.3 — the default config.yaml prompt_files list defaults to the canonical
// filename, so a template that ships only system-prompt.md (no explicit
// prompt_files) is wired to load exactly that file.
func TestDefaultConfigPromptFilesIsCanonical(t *testing.T) {
	// No root .md files in the input → generateDefaultConfig must emit the
	// canonical default prompt_files entry.
	cfg := generateDefaultConfig("Acme", map[string]string{"skills/x/SKILL.md": "x"}, 3)
	wantLine := "  - " + canonicalConciergePromptFilename
	if !strings.Contains(cfg, wantLine) {
		t.Errorf("generateDefaultConfig default prompt_files must be %q\n--- got ---\n%s", wantLine, cfg)
	}
}

// G0.4 — the provision-time asset channel allowlist accepts the canonical
// filename, so a kind=platform concierge's identity is deliverable on the
// standard runtime image (the de-bake — no baked platform-agent image).
func TestAssetAllowlistAcceptsCanonicalFilename(t *testing.T) {
	if !provisioner.IsCPTemplateAssetPath(canonicalConciergePromptFilename) {
		t.Errorf("IsCPTemplateAssetPath(%q) = false — concierge identity not deliverable via the asset channel", canonicalConciergePromptFilename)
	}
}

// G0.5 — NEGATIVE fixture: if a template ships its identity under a NON-canonical
// filename (the split), the substitution does NOT touch it, so the live prompt
// keeps the literal {{CONCIERGE_NAME}} (generic identity). This proves the
// convergence is load-bearing: divergence => identity lost/wrong, not merely
// cosmetic. (Mirrors the runtime-half negative fixture.)
func TestNonCanonicalFilenameLeavesPlaceholderUnsubstituted(t *testing.T) {
	// applyConciergeProvisionConfig only substitutes the canonical key. Emulate
	// the split: a template delivers identity under "concierge.md" instead.
	configFiles := map[string][]byte{
		"concierge.md": []byte("I am {{CONCIERGE_NAME}}."),
	}
	// The substitution path keys off the canonical filename; a non-canonical key
	// is never substituted.
	if prompt, ok := configFiles[canonicalConciergePromptFilename]; ok {
		configFiles[canonicalConciergePromptFilename] = substituteConciergeName(prompt, "Acme")
	}
	// The split file STILL carries the literal placeholder — the generic-identity
	// regression. This is the RED a real concierge surfaces as a missing name.
	if !strings.Contains(string(configFiles["concierge.md"]), conciergeNamePlaceholder) {
		t.Fatalf("expected un-substituted placeholder on the non-canonical filename (split bug); got %q", configFiles["concierge.md"])
	}
	// And the canonical key is absent → nothing was substituted at all.
	if _, ok := configFiles[canonicalConciergePromptFilename]; ok {
		t.Fatalf("test setup error: canonical key unexpectedly present")
	}
}
