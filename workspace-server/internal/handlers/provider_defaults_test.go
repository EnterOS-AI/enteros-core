package handlers

import (
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
)

// providerDefaultsCatalogName maps each <PROVIDER>_BASE_URL fallback key to the
// providers.yaml catalog provider whose base_url_template it must equal. This is
// the SSOT tie: the fallback in provider_defaults.go is a hand-maintained copy,
// and this map is the seam that keeps it from silently diverging from the
// canonical catalog. Entries with an empty name are intentionally catalog-exempt
// (the container-env fallback covers a BYOK vendor the CP routing catalog does
// not track) and are asserted to be absent from the catalog so the exemption
// stays honest.
var providerDefaultsCatalogName = map[string]string{
	"OPENAI_BASE_URL":     "openai-api", // catalog splits openai into -api / -subscription
	"GROQ_BASE_URL":       "groq",
	"OPENROUTER_BASE_URL": "openrouter",
	"MINIMAX_BASE_URL":    "minimax", // GLOBAL .io — NOT minimax-cn (.minimaxi.com, China)
	"MOONSHOT_BASE_URL":   "moonshot",
	"QIANFAN_BASE_URL":    "", // catalog-exempt: BYOK-only, no CP proxy arm
}

// TestProviderBaseURLDefaults_MatchCatalog is the drift gate that pins every
// ProviderBaseURLDefaults value to the providers.yaml SSOT (catalog
// base_url_template). Without it the fallback map's URLs can silently diverge
// from canonical — exactly the class of bug that shipped MINIMAX_BASE_URL as the
// wrong host. Now: change the catalog and this test forces the fallback to
// follow (or fail RED). Also guards that the map covers no phantom providers and
// misses no catalog change for the vendors it claims to mirror.
func TestProviderBaseURLDefaults_MatchCatalog(t *testing.T) {
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("providers.LoadManifest() error = %v", err)
	}
	byName := make(map[string]providers.Provider, len(m.Providers))
	for _, p := range m.Providers {
		byName[p.Name] = p
	}
	// Every fallback entry must be accounted for by the mapping above.
	for envKey := range ProviderBaseURLDefaults {
		if _, ok := providerDefaultsCatalogName[envKey]; !ok {
			t.Errorf("ProviderBaseURLDefaults has %q but providerDefaultsCatalogName does not map it — add its catalog provider (or \"\" if catalog-exempt) so drift stays gated", envKey)
		}
	}
	for envKey, provName := range providerDefaultsCatalogName {
		fallback, ok := ProviderBaseURLDefaults[envKey]
		if !ok {
			t.Errorf("mapping references %q which is absent from ProviderBaseURLDefaults", envKey)
			continue
		}
		if provName == "" {
			if _, present := byName[envKey[:len(envKey)-len("_BASE_URL")]]; present {
				t.Errorf("%q is marked catalog-exempt but a matching provider exists in providers.yaml — wire it to the catalog instead", envKey)
			}
			continue
		}
		p, present := byName[provName]
		if !present {
			t.Errorf("catalog provider %q (for %q) not found in providers.yaml", provName, envKey)
			continue
		}
		if p.BaseURLTemplate != fallback {
			t.Errorf("DRIFT: ProviderBaseURLDefaults[%q]=%q but providers.yaml %q.base_url_template=%q — update the fallback to match the SSOT", envKey, fallback, provName, p.BaseURLTemplate)
		}
	}
}

// TestProviderCatalog_MinimaxDualRegion pins BOTH MiniMax endpoints so the
// .io-vs-.com distinction stays modeled and can't be lost while the global
// default is corrected. MiniMax ships two surfaces: the US/global host
// api.minimax.io (the `minimax` provider — the one we mostly use, and the
// container-env fallback default) and the China host api.minimaxi.com (the
// `minimax-cn` provider, selected by the `minimax-cn:` model prefix). Both are
// first-class catalog entries; conflating them is exactly the bug that bricked
// openclaw's default model, so both are gated here.
func TestProviderCatalog_MinimaxDualRegion(t *testing.T) {
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("providers.LoadManifest() error = %v", err)
	}
	byName := make(map[string]providers.Provider, len(m.Providers))
	for _, p := range m.Providers {
		byName[p.Name] = p
	}
	cases := []struct {
		name, wantHost, region string
	}{
		{"minimax", "https://api.minimax.io/v1", "US/global (default, we mostly use this)"},
		{"minimax-cn", "https://api.minimaxi.com/v1", "China"},
	}
	for _, c := range cases {
		p, ok := byName[c.name]
		if !ok {
			t.Errorf("catalog missing %q (%s) — both MiniMax regions must stay modeled", c.name, c.region)
			continue
		}
		if p.BaseURLTemplate != c.wantHost {
			t.Errorf("%q (%s) base_url_template=%q, want %q", c.name, c.region, p.BaseURLTemplate, c.wantHost)
		}
	}
}

// TestApplyProviderBaseURLDefaults_InjectsWhenKeySetAndURLAbsent — the
// happy path that closes RFC internal#417's reported bug: operator
// saves MINIMAX_API_KEY only, fallback fills in the canonical URL.
func TestApplyProviderBaseURLDefaults_InjectsWhenKeySetAndURLAbsent(t *testing.T) {
	envVars := map[string]string{
		"MINIMAX_API_KEY": "sk-real-key",
	}

	injected := applyProviderBaseURLDefaults(envVars)

	if got := envVars["MINIMAX_BASE_URL"]; got != "https://api.minimax.io/v1" {
		t.Fatalf("MINIMAX_BASE_URL: got %q, want %q", got, "https://api.minimax.io/v1")
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
