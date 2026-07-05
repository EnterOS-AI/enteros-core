package handlers

// member_platform_default_test.go — regression coverage for the platform-mode
// DEFAULT-TO-PROXY behavior in applyPlatformManagedLLMEnv (the member-LLM path).
//
// Operator topology under test:
//   PLATFORM (proxy wired)  → the metered CP proxy is the DEFAULT; BYOK optional.
//   SELF-HOST (no proxy)    → BYOK is the default; there is no hosted proxy.
//
// The bug this guards: an agent-created team member is provisioned with NO
// explicit model, so it inherits the template DEFAULT model. A bare/colon vendor
// model-id (e.g. "MiniMax-M2.7" for claude-code) resolves via DeriveProvider to
// the `minimax` VENDOR arm — NOT the closed `platform` arm — yet the member holds
// no vendor key. Before the fix that fell to BYOK and failed closed with
// MISSING_BYOK_CREDENTIAL (422). On the platform the member must instead DEFAULT
// to the metered proxy. This complements #1101 (which locks the proxy-env SHAPE
// once route_to_platform) by locking the DECISION that gets a member there.
//
// "MiniMax-M2.7" is an EXACT model id in the claude-code runtime's `minimax`
// native provider set (providers.yaml runtimes block) and is deliberately NOT the
// slash-namespaced `minimax/MiniMax-M2.7` platform form, so it resolves to the
// vendor arm — the precise shape of the member default that regressed.

import (
	"context"
	"testing"
)

const memberVendorRuntime = "claude-code"

// setPlatformProxyEnv wires the server-side CP proxy env that
// PlatformManagedProxyConfigured() and the platform branch read from os.Getenv.
// Presence of these ⇔ "platform mode".
func setPlatformProxyEnv(t *testing.T) (token string) {
	t.Helper()
	token = "tenant-admin-token"
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.example.test/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", token)
	return token
}

// clearPlatformProxyEnv makes PlatformManagedProxyConfigured() report false —
// i.e. a SELF-HOSTED stack with no hosted proxy. Empty values are treated as
// unset by firstNonEmptyEnv (TrimSpace != "").
func clearPlatformProxyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
}

// PLATFORM + vendor-derived model + NO explicit BYOK credential → default to the
// metered proxy: route_to_platform, MOLECULE_RESOLVED_PROVIDER=platform, and the
// full proxy-auth env injected (the fix — was MISSING_BYOK_CREDENTIAL before).
func TestApplyPlatformManagedLLMEnv_PlatformMemberNoBYOK_DefaultsToProxy(t *testing.T) {
	token := setPlatformProxyEnv(t)

	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-member-nobyok", memberVendorRuntime, "MiniMax-M2.7", nil)

	if !res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = false, want true (a platform member with no BYOK key must default to the metered proxy, not fail closed)")
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true (the injected proxy usage token is the platform credential)")
	}
	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "platform" {
		t.Fatalf("MOLECULE_RESOLVED_PROVIDER = %q, want \"platform\" (invariant: ==platform ⇔ RoutedToPlatform)", got)
	}
	// Full proxy env injected (mirrors #1101's shape lock).
	if got := envVars["MOLECULE_LLM_USAGE_TOKEN"]; got != token {
		t.Errorf("MOLECULE_LLM_USAGE_TOKEN = %q, want the proxy usage token %q", got, token)
	}
	if got := envVars["MOLECULE_LLM_BASE_URL"]; got == "" {
		t.Errorf("MOLECULE_LLM_BASE_URL must be injected on the platform path; got empty")
	}
	// claude-code is anthropic-native: the platform proxy auth uses the closed
	// `platform` arm's auth_token_env (ANTHROPIC_API_KEY), NOT the vendor arm's
	// ANTHROPIC_AUTH_TOKEN — because the flip adopts the platform provider entry.
	if got := envVars["ANTHROPIC_API_KEY"]; got != token {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the proxy token %q (canonical platform shape, not the minimax vendor token env)", got, token)
	}
	if got := envVars["ANTHROPIC_BASE_URL"]; got == "" {
		t.Errorf("ANTHROPIC_BASE_URL must point at the proxy anthropic surface; got empty")
	}
}

// SELF-HOST (no proxy wired) + same vendor-derived model + no key → BYOK, no
// proxy. The default-to-proxy flip MUST NOT fire off-platform. At the resolver
// level this surfaces as RoutedToPlatform=false + HasUsableLLMCred=false (which
// the caller turns into MISSING_BYOK_CREDENTIAL — the correct self-host outcome:
// the operator must supply a key, the platform must not silently bill them).
func TestApplyPlatformManagedLLMEnv_SelfHostMemberNoBYOK_StaysBYOK(t *testing.T) {
	clearPlatformProxyEnv(t)

	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-member-selfhost", memberVendorRuntime, "MiniMax-M2.7", nil)

	if res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = true, want false (self-host has no proxy — the derived vendor arm must stay BYOK)")
	}
	if res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = true, want false (no vendor key on self-host → fail closed, not billed)")
	}
	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "minimax" {
		t.Errorf("MOLECULE_RESOLVED_PROVIDER = %q, want \"minimax\" (the resolved BYOK vendor arm, no invented platform)", got)
	}
	if _, injected := envVars["MOLECULE_LLM_USAGE_TOKEN"]; injected {
		t.Errorf("self-host BYOK must NOT inject the metered proxy env; MOLECULE_LLM_USAGE_TOKEN present")
	}
}

// PLATFORM + vendor model + EXPLICIT BYOK credential (a real provider-matching
// key the tenant set) → BYOK is HONORED even in platform mode (proxy is the
// default, not a forced override). No proxy env, the key is preserved.
func TestApplyPlatformManagedLLMEnv_PlatformMemberExplicitBYOK_StaysBYOK(t *testing.T) {
	setPlatformProxyEnv(t) // platform mode IS wired…

	envVars := map[string]string{
		"MINIMAX_API_KEY": "user-minimax-key", // …but the tenant explicitly chose BYOK.
	}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-member-byok", memberVendorRuntime, "MiniMax-M2.7", nil)

	if res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = true, want false (an explicit provider-matching BYOK key must be honored, not overridden by the proxy default)")
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true (the tenant's MINIMAX_API_KEY is the usable BYOK credential)")
	}
	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "minimax" {
		t.Errorf("MOLECULE_RESOLVED_PROVIDER = %q, want \"minimax\" (resolved BYOK vendor arm)", got)
	}
	if got := envVars["MINIMAX_API_KEY"]; got != "user-minimax-key" {
		t.Errorf("explicit BYOK key must be preserved; MINIMAX_API_KEY = %q", got)
	}
	if _, injected := envVars["MOLECULE_LLM_USAGE_TOKEN"]; injected {
		t.Errorf("explicit BYOK must NOT inject the metered proxy env; MOLECULE_LLM_USAGE_TOKEN present")
	}
}
