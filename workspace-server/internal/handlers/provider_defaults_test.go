package handlers

import (
	"strings"
	"testing"
)

// TestApplyProviderBaseURLDefaults_InjectsWhenKeySetAndURLAbsent — the
// happy path that closes RFC internal#417's reported bug: operator
// saves MINIMAX_API_KEY only, fallback fills in the canonical URL.
func TestApplyProviderBaseURLDefaults_InjectsWhenKeySetAndURLAbsent(t *testing.T) {
	envVars := map[string]string{
		"MINIMAX_API_KEY": "sk-real-key",
	}

	injected := applyProviderBaseURLDefaults(envVars)

	if got := envVars["MINIMAX_BASE_URL"]; got != "https://api.minimax.io" {
		t.Fatalf("MINIMAX_BASE_URL: got %q, want %q", got, "https://api.minimax.io")
	}
	if len(injected) != 1 || injected[0] != "MINIMAX_BASE_URL" {
		t.Fatalf("injected list: got %v, want [MINIMAX_BASE_URL]", injected)
	}
}

// TestApplyProviderBaseURLDefaults_DoesNotOverrideExplicitURL — operator
// override wins. If they saved both MINIMAX_API_KEY and a custom URL
// (say a corporate-proxy or a different region), we leave their URL
// alone. This is the precedence rule from RFC §Phase 2.
func TestApplyProviderBaseURLDefaults_DoesNotOverrideExplicitURL(t *testing.T) {
	envVars := map[string]string{
		"MINIMAX_API_KEY":  "sk-real-key",
		"MINIMAX_BASE_URL": "https://custom.example.com",
	}

	injected := applyProviderBaseURLDefaults(envVars)

	if got := envVars["MINIMAX_BASE_URL"]; got != "https://custom.example.com" {
		t.Fatalf("MINIMAX_BASE_URL: got %q, want operator value %q",
			got, "https://custom.example.com")
	}
	for _, k := range injected {
		if k == "MINIMAX_BASE_URL" {
			t.Fatalf("MINIMAX_BASE_URL should not appear in injected list when operator set it explicitly; got %v", injected)
		}
	}
}

// TestApplyProviderBaseURLDefaults_NoKeyNoInjection — provider whose
// API key isn't set should not get a stray URL injected. Keeps the
// workspace env clean and avoids accidentally signalling that the
// provider is available when it isn't.
func TestApplyProviderBaseURLDefaults_NoKeyNoInjection(t *testing.T) {
	envVars := map[string]string{
		// Some unrelated key set, but no MINIMAX_API_KEY.
		"OPENAI_API_KEY": "sk-openai",
	}

	injected := applyProviderBaseURLDefaults(envVars)

	if _, ok := envVars["MINIMAX_BASE_URL"]; ok {
		t.Fatalf("MINIMAX_BASE_URL was injected without MINIMAX_API_KEY being set; envVars=%v", envVars)
	}
	// OPENAI_API_KEY is set, so OPENAI_BASE_URL should be injected —
	// keeps this test honest about what "no injection" means (it's
	// per-provider, not blanket).
	if envVars["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Fatalf("OPENAI_BASE_URL: got %q, want %q",
			envVars["OPENAI_BASE_URL"], "https://api.openai.com/v1")
	}
	for _, k := range injected {
		if k == "MINIMAX_BASE_URL" {
			t.Fatalf("MINIMAX_BASE_URL should not appear in injected list; got %v", injected)
		}
	}
}

// TestApplyProviderBaseURLDefaults_EmptyKeyTreatedAsUnset — operators
// sometimes save a placeholder empty-string row (e.g. they cleared the
// key but didn't delete the row). Treat that the same as the key being
// absent: don't route to a real endpoint with no credential.
func TestApplyProviderBaseURLDefaults_EmptyKeyTreatedAsUnset(t *testing.T) {
	envVars := map[string]string{
		"MINIMAX_API_KEY": "", // explicitly empty
	}

	injected := applyProviderBaseURLDefaults(envVars)

	if _, ok := envVars["MINIMAX_BASE_URL"]; ok {
		t.Fatalf("MINIMAX_BASE_URL should not be injected when MINIMAX_API_KEY=\"\"; envVars=%v", envVars)
	}
	if len(injected) != 0 {
		t.Fatalf("injected list should be empty when key is empty; got %v", injected)
	}
}

// TestApplyProviderBaseURLDefaults_KeyShape — every key in the
// registry must end in `_BASE_URL` so the strip-and-swap in
// applyProviderBaseURLDefaults can derive the paired API-key var
// name. If a future entry breaks that convention the fallback
// silently misses it; this test catches the drift at next CI run.
func TestApplyProviderBaseURLDefaults_KeyShape(t *testing.T) {
	for k := range ProviderBaseURLDefaults {
		if !strings.HasSuffix(k, "_BASE_URL") {
			t.Errorf("ProviderBaseURLDefaults key %q must end in _BASE_URL", k)
		}
	}
}

// TestApplyProviderBaseURLDefaults_NilMap — defensive: the helper is
// called from loadWorkspaceSecrets right before return, after both
// query loops. If a future refactor passes nil (e.g. an error path
// returns the bare nil map literal), the helper must not panic. The
// function's nil-guard pins this.
func TestApplyProviderBaseURLDefaults_NilMap(t *testing.T) {
	// Must not panic. Return value can be nil/empty either way.
	_ = applyProviderBaseURLDefaults(nil)
}
