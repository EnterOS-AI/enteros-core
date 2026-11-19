package handlers

// GOOGLE_API_KEY is the second Gemini key name: the registry's google and
// byok-gemini arms accept GEMINI_API_KEY and GOOGLE_API_KEY alike. Both must be
// on the direct-LLM bypass list, or a platform workspace keeps a stray
// GOOGLE_API_KEY, the Secrets API stores one on a platform workspace, and a BYOK
// Gemini workspace holding only GOOGLE_API_KEY is refused at provision
// (MISSING_BYOK_CREDENTIAL on self-host; silently proxied on the platform).

import (
	"context"
	"testing"
)

func TestBypassList_CoversEveryGeminiKeyName(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Fatalf("provider registry: %v", err)
	}
	for _, name := range []string{"google", "byok-gemini"} {
		found := false
		for _, p := range m.Providers {
			if p.Name != name {
				continue
			}
			found = true
			for _, key := range p.AuthEnv {
				if !isPlatformManagedDirectLLMBypassKey(key) {
					t.Errorf("provider %s accepts %s, which is missing from platformManagedDirectLLMBypassKeys", name, key)
				}
			}
		}
		if !found {
			t.Errorf("provider %s not in the registry", name)
		}
	}
	if !isPlatformManagedDirectLLMBypassKey("google_api_key") {
		t.Error("the bypass check is case-insensitive; google_api_key must match")
	}
}

// Self-host (no proxy): a BYOK Gemini hermes workspace whose only key is
// GOOGLE_API_KEY has a usable credential (was MISSING_BYOK_CREDENTIAL).
func TestApplyPlatformManagedLLMEnv_SelfHostGeminiGoogleAPIKeyIsUsable(t *testing.T) {
	clearPlatformProxyEnv(t)
	envVars := map[string]string{"GOOGLE_API_KEY": "user-google-key"}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-gemini-byok", "hermes", "gemini/gemini-4-hydrogen", nil)
	if res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = true, want false (self-host BYOK Gemini)")
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true: GOOGLE_API_KEY is in byok-gemini's auth_env")
	}
	if got := envVars["MOLECULE_RESOLVED_PROVIDER"]; got != "byok-gemini" {
		t.Errorf("MOLECULE_RESOLVED_PROVIDER = %q, want byok-gemini", got)
	}
	if got := envVars["GOOGLE_API_KEY"]; got != "user-google-key" {
		t.Errorf("GOOGLE_API_KEY = %q, want the tenant's own key kept", got)
	}
}

// Platform (proxy wired): the same workspace is BYOK — its own key is an
// explicit BYOK choice — instead of being flipped onto the metered proxy.
func TestApplyPlatformManagedLLMEnv_PlatformGeminiGoogleAPIKeyStaysBYOK(t *testing.T) {
	setPlatformProxyEnv(t)
	envVars := map[string]string{"GOOGLE_API_KEY": "user-google-key"}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-gemini-byok", "hermes", "gemini/gemini-4-hydrogen", nil)
	if res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = true, want false: an explicit GOOGLE_API_KEY makes the Gemini workspace BYOK")
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("HasUsableLLMCred = false, want true")
	}
	if _, injected := envVars["MOLECULE_LLM_USAGE_TOKEN"]; injected {
		t.Error("a BYOK workspace must not receive the platform usage token")
	}
}

// A platform-routed workspace never keeps a direct Google key.
func TestApplyPlatformManagedLLMEnv_PlatformWorkspaceDropsGoogleAPIKey(t *testing.T) {
	setPlatformProxyEnv(t)
	envVars := map[string]string{"GOOGLE_API_KEY": "stray", "GEMINI_API_KEY": "stray"}
	res := applyPlatformManagedLLMEnv(context.Background(), envVars, "ws-platform", "claude-code", "google/gemini-4-hydrogen", nil)
	if !res.RoutedToPlatform {
		t.Fatalf("RoutedToPlatform = false, want true for the platform arm google/gemini-4-hydrogen")
	}
	for _, k := range []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"} {
		if _, kept := envVars[k]; kept {
			t.Errorf("%s survived on a platform-routed workspace", k)
		}
	}
}
