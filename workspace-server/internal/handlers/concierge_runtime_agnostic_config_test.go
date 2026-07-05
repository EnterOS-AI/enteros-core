package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gopkg.in/yaml.v3"
)

// concierge_runtime_agnostic_config_test.go — proves the general fix for the
// #2027 runtime-seed-mismatch abort that fired for every non-claude-code
// concierge.
//
// The concierge (kind=platform) runs on its org's SWITCHABLE default runtime
// (MOLECULE_DEFAULT_RUNTIME; default openclaw). Pre-fix, the single
// platform-agent template config.yaml pinned `runtime: claude-code`, so a
// concierge on ANY other runtime received a config whose top-level runtime
// contradicted the requested one — runtimeSeedMismatchAbort refused the launch
// and the concierge (including the default openclaw one) got no persona.
//
// The fix composes the concierge's /configs/config.yaml from its ACTUAL
// runtime's native base template + grafts the runtime-agnostic persona per that
// runtime's convention. These tests assert, for a concierge on runtime in
// {openclaw(default), claude-code, codex, google-adk}:
//   - the seeded config.yaml declares the concierge's ACTUAL runtime (so
//     runtimeSeedMismatchAbort does NOT fire),
//   - runtime_config.required_env is neutralized to [] (platform-managed),
//   - the persona is delivered (system-prompt.md for claude-code;
//     prompts/concierge.md for every other runtime).

// writeConciergeBaseFixtures writes minimal, representative per-runtime base
// template config.yaml files + the platform-agent persona into configsDir, the
// same on-disk layout the tenant's workspace-configs-templates cache carries.
func writeConciergeBaseFixtures(t *testing.T, configsDir string) {
	t.Helper()
	fixtures := map[string]string{
		// claude-code base carries a top-level runtime_config.required_env with a
		// real key — the compose MUST neutralize it (the concierge is
		// platform-managed) or the missingRequiredEnv preflight would abort.
		"claude-code-default/config.yaml": "name: Claude Code Agent\n" +
			"runtime: claude-code\n" +
			"runtime_config:\n" +
			"  model: moonshot/kimi-k2.6\n" +
			"  required_env: [ANTHROPIC_API_KEY]\n",
		// openclaw base uses a multi-file prompt_files list — the compose MUST
		// replace it with the single concierge persona file.
		"openclaw/config.yaml": "name: OpenClaw Agent\n" +
			"runtime: openclaw\n" +
			"model: minimax:MiniMax-M2.7\n" +
			"prompt_files:\n" +
			"- SOUL.md\n" +
			"- BOOTSTRAP.md\n" +
			"- AGENTS.md\n" +
			"runtime_config:\n" +
			"  model: minimax:MiniMax-M2.7\n",
		// codex base carries a load-bearing top-level required_env (the real prod
		// config does) — the compose MUST neutralize it.
		"codex/config.yaml": "name: Codex Agent\n" +
			"runtime: codex\n" +
			"runtime_config:\n" +
			"  model: gpt-5.5\n" +
			"  required_env: [OPENAI_API_KEY, CODEX_AUTH_JSON]\n",
		"google-adk/config.yaml": "name: Google ADK Agent\n" +
			"runtime: google-adk\n" +
			"prompt_files:\n" +
			"  - system-prompt.md\n" +
			"runtime_config:\n" +
			"  model: platform:gemini-2.5-pro\n" +
			"  required_env: []\n",
		"platform-agent/prompts/concierge.md": "# You are {{CONCIERGE_NAME}} — the Org Concierge\n\nYou orchestrate the org.\n",
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

// parseComposedRequiredEnv extracts runtime_config.required_env from a composed
// config for assertion.
func parseComposedRequiredEnv(t *testing.T, cfg []byte) []string {
	t.Helper()
	var doc struct {
		RuntimeConfig struct {
			RequiredEnv []string `yaml:"required_env"`
		} `yaml:"runtime_config"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("parse composed config: %v", err)
	}
	return doc.RuntimeConfig.RequiredEnv
}

// parseComposedPromptFiles extracts top-level prompt_files from a composed config.
func parseComposedPromptFiles(t *testing.T, cfg []byte) []string {
	t.Helper()
	var doc struct {
		PromptFiles []string `yaml:"prompt_files"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("parse composed config: %v", err)
	}
	return doc.PromptFiles
}

func TestComposeConciergeRuntimeConfig_RuntimeAgnostic(t *testing.T) {
	dir := t.TempDir()
	writeConciergeBaseFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	cases := []struct {
		runtime         string
		wantPersonaFile string // "" means system-prompt.md convention (claude-code)
	}{
		{"openclaw", conciergePersonaPromptPath},
		{"claude-code", ""},
		{"codex", conciergePersonaPromptPath},
		{"google-adk", conciergePersonaPromptPath},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			composed, err := h.composeConciergeRuntimeConfig(tc.runtime)
			if err != nil {
				t.Fatalf("composeConciergeRuntimeConfig(%q) error: %v", tc.runtime, err)
			}

			// 1. The seeded config.yaml declares the concierge's ACTUAL runtime.
			if got := parseTopLevelRuntime(composed); got != tc.runtime {
				t.Errorf("composed runtime = %q, want %q\n%s", got, tc.runtime, composed)
			}

			// 2. The #2027 guard does NOT fire (this is the core bug being fixed).
			cf := map[string][]byte{"config.yaml": composed}
			if abort := runtimeSeedMismatchAbort(tc.runtime, "", cf); abort != nil {
				t.Errorf("runtimeSeedMismatchAbort fired for %q concierge: %s", tc.runtime, abort.Msg)
			}

			// 3. required_env is neutralized (concierge is platform-managed).
			if re := parseComposedRequiredEnv(t, composed); len(re) != 0 {
				t.Errorf("%q: runtime_config.required_env = %v, want [] (platform-managed concierge)", tc.runtime, re)
			}

			// 4. Persona grafted per the runtime's convention.
			pf := parseComposedPromptFiles(t, composed)
			if tc.runtime == "claude-code" {
				// claude-code reads system-prompt.md; its base has no prompt_files
				// and the compose leaves it untouched.
				if len(pf) != 0 {
					t.Errorf("claude-code: prompt_files = %v, want none (reads system-prompt.md)", pf)
				}
			} else {
				if len(pf) != 1 || pf[0] != conciergePersonaPromptPath {
					t.Errorf("%q: prompt_files = %v, want [%q]", tc.runtime, pf, conciergePersonaPromptPath)
				}
			}
		})
	}
}

func TestComposeConciergeRuntimeConfig_MissingBaseFallsBackToError(t *testing.T) {
	// No fixtures → the base config is unavailable → compose returns an error so
	// the caller falls back to the delivered config unchanged (never panics).
	h := &WorkspaceHandler{configsDir: t.TempDir()}
	if _, err := h.composeConciergeRuntimeConfig("openclaw"); err == nil {
		t.Fatal("expected error when the runtime base config is missing, got nil")
	}
}

// TestApplyConciergeProvisionConfig_ComposesRuntimeNativeConfigForEveryRuntime
// drives the full provision hook (the seam prepareProvisionContext calls) for a
// concierge on each of the four runtimes and asserts the delivered /configs
// carry the runtime-native config + the grafted persona, with NO seed mismatch.
func TestApplyConciergeProvisionConfig_ComposesRuntimeNativeConfigForEveryRuntime(t *testing.T) {
	dir := t.TempDir()
	writeConciergeBaseFixtures(t, dir)
	h := &WorkspaceHandler{configsDir: dir}

	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`

	cases := []struct {
		runtime string
		model   string // an existing platform-managed model for the runtime
	}{
		{"openclaw", "minimax/MiniMax-M2.7"},
		{"claude-code", "moonshot/kimi-k2.6"},
		{"codex", "minimax/MiniMax-M2.7"},
		{"google-adk", "platform:gemini-2.5-pro"},
	}

	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			// Stored model == resolved SSOT → the reconcile is a no-op (no re-persist).
			setConciergeModelResolver(t, tc.model, nil)
			mock := setupTestDB(t)
			mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
				WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", tc.runtime))
			mock.ExpectQuery(modelSelQuery).WithArgs("ws-concierge").
				WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
					AddRow([]byte(tc.model), 0))
			// LLM_PROVIDER already pinned → ensureConciergeProvider early-returns.
			mock.ExpectQuery(providerSelQuery).WithArgs("ws-concierge").
				WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
					AddRow([]byte("platform"), 0))
			// seedTemplatePlugins → recordDeclaredPlugin (privileged kind precheck + INSERT).
			mock.ExpectQuery(recordKindQuery).WithArgs("ws-concierge").
				WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
			mock.ExpectExec(declaredInsert).
				WithArgs("ws-concierge", sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))

			env := map[string]string{}
			// configFiles nil on the auto-restart path — the hook composes it.
			out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", nil, env, "Test Org Agent")

			// The delivered config.yaml declares the concierge's ACTUAL runtime.
			if got := parseTopLevelRuntime(out["config.yaml"]); got != tc.runtime {
				t.Errorf("delivered config runtime = %q, want %q", got, tc.runtime)
			}
			// The #2027 guard does NOT fire.
			if abort := runtimeSeedMismatchAbort(tc.runtime, "", out); abort != nil {
				t.Errorf("runtimeSeedMismatchAbort fired for %q concierge: %s", tc.runtime, abort.Msg)
			}
			// The persona is present, name-substituted, per the runtime's convention.
			var personaKey string
			if tc.runtime == "claude-code" {
				personaKey = "system-prompt.md"
			} else {
				personaKey = conciergePersonaPromptPath
			}
			persona := string(out[personaKey])
			if persona == "" {
				var have []string
				for k := range out {
					have = append(have, k)
				}
				t.Fatalf("%q: no persona delivered at %q; files=%v", tc.runtime, personaKey, have)
			}
			if !strings.Contains(persona, "Test Org Agent") {
				t.Errorf("%q: persona missing substituted name:\n%s", tc.runtime, persona)
			}
			if strings.Contains(persona, conciergeNamePlaceholder) {
				t.Errorf("%q: {{CONCIERGE_NAME}} placeholder survived:\n%s", tc.runtime, persona)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("%q: unmet sqlmock expectations: %v", tc.runtime, err)
			}
		})
	}
}
