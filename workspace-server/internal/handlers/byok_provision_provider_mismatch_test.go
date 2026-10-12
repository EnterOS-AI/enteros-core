package handlers

// byok_provision_provider_mismatch_test.go — RC 12082 regression guard.
//
// Pins the contract for hasAnyPlatformManagedLLMKey: the presence check
// for the BYOK provision-time fail-closed branch MUST be provider-AWARE.
// A stray key (e.g. OPENAI_API_KEY in a claude-code+anthropic workspace)
// MUST NOT satisfy presence even though the key IS in the global bypass
// set — the resolved provider (anthropic-api) would never authenticate
// with it, and the workspace would 201+credential-less+die-at-provision.
//
// Pre-fix bug: the global bypass set (platformManagedDirectLLMBypassKeys)
// was the SOLE presence check — it accepted ANY key in the set as
// presence, regardless of whether the derived provider would accept it.
// The fix intersects the bypass set with the resolved provider's auth_env.
//
// The create-time gate (byok_credential_gate.go) has the parallel fix in
// anyBYOKCredentialKeyMatchesProvider. These tests pin the provision-time
// side of the same contract.

import (
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
)

// TestHasAnyPlatformManagedLLMKey_ProviderMismatch_FailsClosed pins the
// RC 12082 fix: a claude-code+anthropic workspace carrying ONLY
// OPENAI_API_KEY (in the global bypass set but NOT in anthropic-api's
// auth_env) MUST NOT satisfy presence — HasUsableLLMCred must be false
// so the provision-time fail-closed branch fires MISSING_BYOK_CREDENTIAL
// rather than starting the workspace credential-less.
func TestHasAnyPlatformManagedLLMKey_ProviderMismatch_FailsClosed(t *testing.T) {
	// anthropic-api's auth_env (per providers.yaml): ANTHROPIC_API_KEY,
	// ANTHROPIC_AUTH_TOKEN. Notably NO OPENAI_API_KEY.
	provider := providers.Provider{
		Name:    "anthropic-api",
		AuthEnv: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	}

	envVars := map[string]string{
		// OPENAI_API_KEY is in the global bypass set BUT NOT in anthropic-api's
		// auth_env. The runtime would never accept it — presence must fail.
		"OPENAI_API_KEY": "sk-stray-1234",
		// Non-bypass keys (operator secrets that anthropic-api happens to
		// accept but that aren't LLM keys) MUST also not satisfy presence
		// (catches the over-broad bypass-set filter).
		"FOO_NON_LLM": "x",
	}

	if hasAnyPlatformManagedLLMKey(provider, envVars) {
		t.Errorf("hasAnyPlatformManagedLLMKey returned true for a claude-code+anthropic workspace with only OPENAI_API_KEY (RC 12082 bypass); want false so the provision-time fail-closed branch fires MISSING_BYOK_CREDENTIAL")
	}
}

// TestHasAnyPlatformManagedLLMKey_ProviderMatch_Passes pins the
// positive case: a workspace with a credential that DOES match the
// resolved provider's auth_env MUST satisfy presence.
func TestHasAnyPlatformManagedLLMKey_ProviderMatch_Passes(t *testing.T) {
	provider := providers.Provider{
		Name:    "anthropic-api",
		AuthEnv: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	}

	envVars := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-1234",
	}

	if !hasAnyPlatformManagedLLMKey(provider, envVars) {
		t.Errorf("hasAnyPlatformManagedLLMKey returned false for a workspace with ANTHROPIC_API_KEY matching anthropic-api's auth_env; want true")
	}
}

// TestHasAnyPlatformManagedLLMKey_OpenAI_OK pins that the fix is
// symmetric — an openai-resolved workspace with only OPENAI_API_KEY
// (in the global bypass set AND in openai's auth_env) DOES satisfy
// presence.
func TestHasAnyPlatformManagedLLMKey_OpenAI_OK(t *testing.T) {
	provider := providers.Provider{
		Name:    "openai",
		AuthEnv: []string{"OPENAI_API_KEY"},
	}

	envVars := map[string]string{
		"OPENAI_API_KEY": "sk-openai-1234",
	}

	if !hasAnyPlatformManagedLLMKey(provider, envVars) {
		t.Errorf("hasAnyPlatformManagedLLMKey returned false for an openai workspace with OPENAI_API_KEY matching openai's auth_env; want true")
	}
}

// TestHasAnyPlatformManagedLLMKey_EmptyEnvVars pins that no envVars at
// all fails (the pre-fix behavior; preserved).
func TestHasAnyPlatformManagedLLMKey_EmptyEnvVars(t *testing.T) {
	provider := providers.Provider{
		Name:    "anthropic-api",
		AuthEnv: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
	}

	if hasAnyPlatformManagedLLMKey(provider, map[string]string{}) {
		t.Errorf("hasAnyPlatformManagedLLMKey returned true for empty envVars; want false")
	}
}

// TestHasAnyPlatformManagedLLMKey_EmptyAuthEnv pins that a provider
// with NO auth_env (e.g. a misconfigured registry) fails presence even
// for a key in the global bypass set — the provider.AuthEnv intersection
// is empty, so no key matches.
func TestHasAnyPlatformManagedLLMKey_EmptyAuthEnv(t *testing.T) {
	provider := providers.Provider{
		Name:    "unknown-provider",
		AuthEnv: []string{}, // empty — fall-through safe-default
	}

	envVars := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-1234",
	}

	if hasAnyPlatformManagedLLMKey(provider, envVars) {
		t.Errorf("hasAnyPlatformManagedLLMKey returned true for a provider with empty auth_env; want false (no key can match a provider with no accepted keys)")
	}
}
