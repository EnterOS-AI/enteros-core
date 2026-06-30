package handlers

// resolved_provider_env_test.go — SSOT-emitter coverage for the single published
// provider signal MOLECULE_RESOLVED_PROVIDER (internal#718). applyPlatformManagedLLMEnv
// is the ONE place the provider is derived; it must publish the resolved registry
// arm name for EVERY workspace — both the platform/proxy branch AND the BYOK
// branch — so downstream layers (CP local_docker_workspace, template adapters)
// READ it and never re-derive. This replaces the deleted llm_billing_mode field
// and the LLM_PROVIDER=platform force-pin.
//
// Invariant under test: MOLECULE_RESOLVED_PROVIDER == "platform" ⇔ RoutedToPlatform.

import (
	"context"
	"testing"
)

// A platform-derived model publishes MOLECULE_RESOLVED_PROVIDER=platform and routes
// to the metered proxy.
func TestApplyPlatformManagedLLMEnv_PublishesResolvedProvider_Platform(t *testing.T) {
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	envVars := map[string]string{}
	// moonshot/kimi-k2.6 is the prod default model and derives to the closed
	// `platform` registry arm for claude-code (providers SSOT).
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-resolved-platform", "claude-code", "moonshot/kimi-k2.6", nil)

	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "platform" {
		t.Fatalf("MOLECULE_RESOLVED_PROVIDER = %q, want \"platform\"", got)
	}
	if !res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = false, want true (a platform-derived model must route to the metered proxy)")
	}
}

// A BYOK model (codex gpt-5.4 with CODEX_AUTH_JSON present) publishes the resolved
// BYOK arm name and does NOT route to the proxy. CODEX_AUTH_JSON in availableAuthEnv
// must steer the gpt-* family to the OAuth arm `openai-subscription` (NOT openai-api).
func TestApplyPlatformManagedLLMEnv_PublishesResolvedProvider_BYOKSubscription(t *testing.T) {
	envVars := map[string]string{
		"CODEX_AUTH_JSON": "{\"tokens\":{\"access_token\":\"user-codex-oauth\"}}",
		"MODEL":           "gpt-5.4",
	}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-resolved-byok", "codex", "gpt-5.4", nil)

	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "openai-subscription" {
		t.Fatalf("MOLECULE_RESOLVED_PROVIDER = %q, want \"openai-subscription\" (gpt-5.4 + CODEX_AUTH_JSON → OAuth arm, not openai-api)", got)
	}
	if res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = true, want false (a resolved BYOK provider must not route to the metered proxy)")
	}
}
