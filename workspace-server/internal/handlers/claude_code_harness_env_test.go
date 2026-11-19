package handlers

// Tests for claude_code_harness_env.go: the Claude Code subagent, small/fast and
// alias model tiers of a platform-managed claude-code workspace follow that
// workspace's own model.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

var allClaudeCodeHarnessEnvKeys = append(claudeCodeHarnessModelEnvKeys[:], claudeCodeToolSearchEnvKey)

func TestClaudeCodeHarnessModelEnv_NonAnthropicModelOnEveryTier(t *testing.T) {
	for _, model := range []string{
		"google/gemini-4-hydrogen",
		"google/gemini-3.1-flash-lite",
		"minimax/MiniMax-M2.7",
		"platform/auto",
		"MiniMax-M2.7", // the bare vendor form a member without a key is proxied on
	} {
		env := claudeCodeHarnessModelEnv(model)
		for _, k := range claudeCodeHarnessModelEnvKeys {
			if env[k] != model {
				t.Errorf("model %q: %s = %q, want the workspace model verbatim", model, k, env[k])
			}
		}
		if env[claudeCodeToolSearchEnvKey] != "false" {
			t.Errorf("model %q: %s = %q, want \"false\"", model, claudeCodeToolSearchEnvKey, env[claudeCodeToolSearchEnvKey])
		}
		if len(env) != len(allClaudeCodeHarnessEnvKeys) {
			t.Errorf("model %q: %d variables, want exactly %d: %v", model, len(env), len(allClaudeCodeHarnessEnvKeys), env)
		}
	}
}

// A model the proxy sends to Anthropic keeps Claude Code's built-in claude-*
// tier defaults: they already go to the same vendor.
func TestClaudeCodeHarnessModelEnv_AnthropicModelsKeepBuiltInTiers(t *testing.T) {
	for _, model := range []string{
		"anthropic/claude-opus-4-7",
		"anthropic:claude-sonnet-4-6",
		"claude-opus-4-8",
		"Claude-Haiku-4-5",
		"sonnet",
		"opus",
		"haiku",
		"",
		"   ",
	} {
		if env := claudeCodeHarnessModelEnv(model); env != nil {
			t.Errorf("model %q: got %v, want nil (no tier override)", model, env)
		}
	}
}

func TestClaudeCodeModelServedByAnthropic(t *testing.T) {
	cases := map[string]bool{
		"anthropic/claude-fable-5-1":   true,
		"anthropic:claude-opus-4-7":    true,
		"claude:claude-opus-4-7":       true,
		"claude-sonnet-4-5":            true,
		"opus":                         true,
		"google/gemini-4-hydrogen":     false,
		"google/gemini-3.1-flash-lite": false,
		"minimax/MiniMax-M3":           false,
		"platform/auto":                false,
		"auto":                         false,
		"kimi-for-coding":              false,
		"MiniMax-M2.7-highspeed":       false,
		"vendor/claude-lookalike":      false, // the namespace decides, not the suffix
	}
	for model, want := range cases {
		if got := claudeCodeModelServedByAnthropic(model); got != want {
			t.Errorf("claudeCodeModelServedByAnthropic(%q) = %v, want %v", model, got, want)
		}
	}
}

// A tier variable the workspace or org already set as a secret is a deliberate
// choice and is kept; the others are still derived.
func TestApplyClaudeCodeHarnessModelEnv_ExplicitSecretWins(t *testing.T) {
	envVars := map[string]string{"CLAUDE_CODE_SUBAGENT_MODEL": "google/gemini-3.1-flash-lite"}
	applyClaudeCodeHarnessModelEnv(envVars, "ws-1", "claude-code", "google/gemini-4-hydrogen")
	if got := envVars["CLAUDE_CODE_SUBAGENT_MODEL"]; got != "google/gemini-3.1-flash-lite" {
		t.Errorf("CLAUDE_CODE_SUBAGENT_MODEL = %q, want the explicit secret kept", got)
	}
	if got := envVars["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; got != "google/gemini-4-hydrogen" {
		t.Errorf("ANTHROPIC_DEFAULT_HAIKU_MODEL = %q, want the workspace model", got)
	}
}

func TestApplyClaudeCodeHarnessModelEnv_OtherRuntimesUntouched(t *testing.T) {
	for _, runtime := range []string{"hermes", "codex", "openclaw", ""} {
		envVars := map[string]string{}
		applyClaudeCodeHarnessModelEnv(envVars, "ws-1", runtime, "google/gemini-4-hydrogen")
		if len(envVars) != 0 {
			t.Errorf("runtime %q: got %v, want no harness variables", runtime, envVars)
		}
	}
}

// End to end through prepareProvisionContext: a platform-managed claude-code
// workspace on google/gemini-3.1-flash-lite gets every tier on its OWN model —
// not the tenant-level values the control plane computed for the tenant default
// (which the tenant server holds in its own process env).
func TestPrepareProvisionContext_ClaudeCodeTiersFollowWorkspaceModel(t *testing.T) {
	setPlatformProxyEnv(t)
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "google/gemini-4-hydrogen")
	for _, k := range claudeCodeHarnessModelEnvKeys {
		t.Setenv(k, "gemini-4-hydrogen") // the CP's tenant-level emission
	}

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs("ws-e2e-flash-lite").
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	payload := models.CreateWorkspacePayload{
		Name:    "e2e",
		Runtime: "claude-code",
		Tier:    4,
		Model:   "google/gemini-3.1-flash-lite",
	}
	prepared, abort := handler.prepareProvisionContext(context.Background(), "ws-e2e-flash-lite", "/nonexistent", nil, payload, false)
	if abort != nil {
		t.Fatalf("unexpected abort: %s %v", abort.Msg, abort.Extra)
	}
	for _, k := range claudeCodeHarnessModelEnvKeys {
		if got := prepared.EnvVars[k]; got != "google/gemini-3.1-flash-lite" {
			t.Errorf("%s = %q, want the workspace's own model google/gemini-3.1-flash-lite", k, got)
		}
	}
	if got := prepared.EnvVars[claudeCodeToolSearchEnvKey]; got != "false" {
		t.Errorf("%s = %q, want \"false\"", claudeCodeToolSearchEnvKey, got)
	}
}

// A BYOK claude-code workspace talks to its vendor directly: no tier override.
func TestPrepareProvisionContext_ByokClaudeCodeKeepsBuiltInTiers(t *testing.T) {
	setPlatformProxyEnv(t)

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs("ws-byok-minimax").
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("MINIMAX_API_KEY", []byte("user-minimax-key"), 0))

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	payload := models.CreateWorkspacePayload{
		Name:    "byok",
		Runtime: "claude-code",
		Tier:    4,
		Model:   "MiniMax-M2.7",
	}
	prepared, abort := handler.prepareProvisionContext(context.Background(), "ws-byok-minimax", "/nonexistent", nil, payload, false)
	if abort != nil {
		t.Fatalf("unexpected abort: %s %v", abort.Msg, abort.Extra)
	}
	for _, k := range allClaudeCodeHarnessEnvKeys {
		if v, set := prepared.EnvVars[k]; set {
			t.Errorf("%s = %q on a BYOK workspace, want it unset", k, v)
		}
	}
}
