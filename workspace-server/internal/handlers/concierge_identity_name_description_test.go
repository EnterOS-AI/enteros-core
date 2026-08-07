package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// concierge_identity_name_description_test.go — the concierge's composed
// config.yaml must carry the ORG CONCIERGE identity, not the base runtime
// template's.
//
// THE BUG (observed live on prod, 2026-08-06): composeConciergeRuntimeConfig
// neutralized runtime_config.required_env and grafted prompt_files, but copied
// the base runtime template's `name` and `description` VERBATIM. For the default
// hermes runtime those are:
//
//	name: Hermes Agent
//	description: Nous Research hermes-agent — the self-improving AI agent ...
//
// The runtime renders that description into the system prompt as the agent's
// Role, directly alongside the grafted Org Concierge persona. The prompt then
// carries TWO COMPETING IDENTITIES and the model picks one non-deterministically:
// a prod concierge with a correct, fully-substituted persona file on disk still
// answered "I am Hermes Agent, operated by Nous Research — a self-improving AI
// agent", quoting this description near-verbatim, while a sibling concierge on
// the same build answered correctly. That coin-flip is why "the persona bytes
// are on disk" kept reading as fixed while users still saw the wrong identity.
//
// Delivering the persona file is necessary but NOT sufficient — the competing
// identity has to be removed at the same time.

// writeConciergeIdentityFixtures writes per-runtime base templates that carry a
// REAL vendor name+description, mirroring the shipped hermes template. The
// description text is the load-bearing part: it is what the runtime renders as
// the agent's Role.
func writeConciergeIdentityFixtures(t *testing.T, configsDir string) {
	t.Helper()
	fixtures := map[string]string{
		"hermes/config.yaml": "name: Hermes Agent\n" +
			"description: >-\n" +
			"    Nous Research hermes-agent — the self-improving AI agent with built-in\n" +
			"    terminal, file ops, web search, memory, skills, and cross-session recall.\n" +
			"runtime: hermes\n" +
			"prompt_files:\n" +
			"- system-prompt.md\n" +
			"runtime_config:\n" +
			"  model: minimax/MiniMax-M2.7\n" +
			"  required_env: [MINIMAX_API_KEY]\n",
		"openclaw/config.yaml": "name: OpenClaw Agent\n" +
			"description: OpenClaw — a general-purpose coding agent.\n" +
			"runtime: openclaw\n" +
			"runtime_config:\n" +
			"  model: minimax:MiniMax-M2.7\n",
		"claude-code-default/config.yaml": "name: Claude Code Agent\n" +
			"description: Anthropic Claude Code — a terminal coding assistant.\n" +
			"runtime: claude-code\n" +
			"runtime_config:\n" +
			"  model: moonshot/kimi-k2.6\n" +
			"  required_env: [ANTHROPIC_API_KEY]\n",
		"platform-agent/prompts/concierge.md": "# You are {{CONCIERGE_NAME}} — the Org Concierge\n",
	}
	for rel, content := range fixtures {
		p := filepath.Join(configsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// parseComposedIdentity extracts top-level name/description for assertion.
func parseComposedIdentity(t *testing.T, cfg []byte) (string, string) {
	t.Helper()
	var doc struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("parse composed config: %v", err)
	}
	return doc.Name, doc.Description
}

// vendorIdentityMarkers are strings that must NOT survive into a concierge's
// composed config. Each is quoted from the shipped hermes template and each has
// been observed in a wrong self-identification on prod.
var vendorIdentityMarkers = []string{
	"Nous Research",
	"self-improving",
	"hermes-agent",
	"Hermes Agent",
}

// TestComposeConciergeRuntimeConfig_OverridesName is the primary regression
// guard: the composed config's name is the ORG CONCIERGE's name.
func TestComposeConciergeRuntimeConfig_OverridesName(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	const conciergeName = "Acme Rockets Agent"
	for _, runtime := range []string{"hermes", "openclaw", "claude-code"} {
		t.Run(runtime, func(t *testing.T) {
			composed, err := h.composeConciergeRuntimeConfig(runtime, conciergeName)
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig(%q) error: %v", runtime, err)
			}
			gotName, _ := parseComposedIdentity(t, composed)
			if gotName != conciergeName {
				t.Errorf("composed name = %q, want %q\n%s", gotName, conciergeName, composed)
			}
		})
	}
}

// TestComposeConciergeRuntimeConfig_DropsVendorIdentity proves the competing
// identity is GONE from the whole document — not merely that name was replaced.
// The description is the field that actually reached the model as Role.
func TestComposeConciergeRuntimeConfig_DropsVendorIdentity(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes", "Acme Rockets Agent")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	for _, marker := range vendorIdentityMarkers {
		if strings.Contains(string(composed), marker) {
			t.Errorf("composed config still carries vendor identity %q — it will be rendered as the agent's Role and compete with the concierge persona\n%s",
				marker, composed)
		}
	}
	_, gotDesc := parseComposedIdentity(t, composed)
	if strings.TrimSpace(gotDesc) == "" {
		t.Error("composed description is empty; the concierge should describe its own role, not carry nothing")
	}
	if !strings.Contains(strings.ToLower(gotDesc), "concierge") {
		t.Errorf("composed description should describe the org-concierge role, got %q", gotDesc)
	}
}

// TestComposeConciergeRuntimeConfig_FixtureIsRepresentative is the ANTI-VACUOUS
// control. If the base fixture did not actually contain the vendor identity,
// TestComposeConciergeRuntimeConfig_DropsVendorIdentity would pass while
// measuring nothing. This asserts the fixture carries every marker the other
// test then requires to be absent, so that test can never pass vacuously.
func TestComposeConciergeRuntimeConfig_FixtureIsRepresentative(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	raw, err := os.ReadFile(filepath.Join(dir, "hermes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range vendorIdentityMarkers {
		if !strings.Contains(string(raw), marker) {
			t.Fatalf("base fixture lacks %q — the DropsVendorIdentity test would pass vacuously", marker)
		}
	}
}

// TestComposeConciergeRuntimeConfig_EmptyNameKeepsDocumentValid guards the
// degenerate input: an empty concierge name must not write an empty `name:` into
// the config (a nameless agent card). The vendor description is still dropped —
// an unknown name is not a reason to reinstate the wrong identity.
func TestComposeConciergeRuntimeConfig_EmptyNameKeepsDocumentValid(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes", "")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	gotName, _ := parseComposedIdentity(t, composed)
	if strings.TrimSpace(gotName) == "" {
		t.Errorf("empty concierge name produced an empty name: in the composed config\n%s", composed)
	}
	if strings.Contains(string(composed), "Nous Research") {
		t.Errorf("vendor identity survived on the empty-name path\n%s", composed)
	}
	var probe map[string]interface{}
	if err := yaml.Unmarshal(composed, &probe); err != nil {
		t.Fatalf("composed config does not re-parse: %v", err)
	}
}

// TestComposeConciergeRuntimeConfig_IdentityOverrideKeepsRuntimeContract proves
// the identity override did not regress the #2027 contract this function already
// carries: the composed config must still declare the ACTUAL runtime and still
// neutralize required_env.
func TestComposeConciergeRuntimeConfig_IdentityOverrideKeepsRuntimeContract(t *testing.T) {
	dir := t.TempDir()
	writeConciergeIdentityFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes", "Acme Rockets Agent")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	if got := parseTopLevelRuntime(composed); got != "hermes" {
		t.Errorf("composed runtime = %q, want hermes\n%s", got, composed)
	}
	if env := parseComposedRequiredEnv(t, composed); len(env) != 0 {
		t.Errorf("required_env = %v, want neutralized to []", env)
	}
	if pf := parseComposedPromptFiles(t, composed); len(pf) != 1 || pf[0] != conciergePersonaPromptPath {
		t.Errorf("prompt_files = %v, want [%s]", pf, conciergePersonaPromptPath)
	}
}
