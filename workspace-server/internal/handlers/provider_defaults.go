package handlers

// Mirror of the third-party LLM provider registry baked into adapter
// templates (workspace-configs-templates/openclaw/adapter.py +
// workspace-configs-templates/claude-code-default/adapter.py + the
// hermes counterparts). Must stay in sync — if a provider is added in
// any of those adapters' base_url defaults, add it here too.
//
// Why this exists (RFC internal#417):
//
// Operators saving a vendor-scoped API key (e.g. MINIMAX_API_KEY) into
// the platform `global_secrets` table expect the workspace to talk to
// the canonical regional endpoint for that key. Pre-fix, when no
// matching <PROVIDER>_BASE_URL was also saved, the adapter inside the
// workspace fell back to its hardcoded registry default — which for
// MiniMax is api.minimaxi.com (China), while every operator key issued
// against api.minimax.io (global) hits 401 there. The fix is platform-
// side: when we see the API key but no URL, we inject the canonical URL
// so the workspace's adapter precedence chain (operator URL > runtime
// config > adapter default) ends up pointing at the right region.
//
// This is a FALLBACK, never an override. If the operator already
// saved <PROVIDER>_BASE_URL we leave it alone; their explicit choice
// always wins. If the API key isn't set, we don't inject a stray URL
// for a provider the workspace never uses.
//
// MINIMAX_BASE_URL is set to api.minimax.io (NOT api.minimaxi.com) —
// the global endpoint matches the keys all operator-host secrets are
// issued against. See RFC internal#417 §Phase 1 for the regional
// ambiguity table.
var ProviderBaseURLDefaults = map[string]string{
	"OPENAI_BASE_URL":     "https://api.openai.com/v1",
	"GROQ_BASE_URL":       "https://api.groq.com/openai/v1",
	"OPENROUTER_BASE_URL": "https://openrouter.ai/api/v1",
	"QIANFAN_BASE_URL":    "https://qianfan.baidubce.com/v2",
	"MINIMAX_BASE_URL":    "https://api.minimax.io",
	"MOONSHOT_BASE_URL":   "https://api.moonshot.ai/v1",
}

// applyProviderBaseURLDefaults injects each provider's canonical
// BASE_URL into envVars when the matching <PROVIDER>_API_KEY is set
// and non-empty AND no <PROVIDER>_BASE_URL is already present.
//
// The function mutates envVars in place. It is a pure helper (no DB,
// no I/O) so unit tests drive it directly without sqlmock.
//
// Pairing rule: each entry in ProviderBaseURLDefaults names a key of
// the form <PREFIX>_BASE_URL; the paired API-key var is derived by
// swapping the suffix to _API_KEY (e.g. MINIMAX_BASE_URL ↔
// MINIMAX_API_KEY). This mirrors how every adapter registry pairs the
// two — see workspace-configs-templates/*/adapter.py for the canonical
// pairing and RFC internal#417 §Phase 2 for the design rationale.
//
// Returns the list of base-URL keys that were injected, so the caller
// can log which fallbacks fired without re-iterating the map.
func applyProviderBaseURLDefaults(envVars map[string]string) []string {
	if envVars == nil {
		return nil
	}
	var injected []string
	for baseURLKey, defaultURL := range ProviderBaseURLDefaults {
		// Derive the paired API-key var name. Every entry in the map
		// MUST end in `_BASE_URL`; if a future entry breaks that
		// convention this strip-and-swap silently misses it. The unit
		// test TestApplyProviderBaseURLDefaults_KeyShape pins the
		// shape so the miss surfaces immediately on next test run.
		const baseSuffix = "_BASE_URL"
		if len(baseURLKey) <= len(baseSuffix) ||
			baseURLKey[len(baseURLKey)-len(baseSuffix):] != baseSuffix {
			continue
		}
		apiKeyKey := baseURLKey[:len(baseURLKey)-len(baseSuffix)] + "_API_KEY"

		// API key must be present AND non-empty. An empty string is
		// treated as unset — operators sometimes save a placeholder
		// row and we don't want to silently route them to a real
		// endpoint with no credential.
		apiKeyVal, hasKey := envVars[apiKeyKey]
		if !hasKey || apiKeyVal == "" {
			continue
		}

		// Don't overwrite an explicit operator URL. The operator may
		// have legitimately scoped this provider to a custom or
		// regional endpoint (e.g. a corporate proxy, a different
		// MiniMax region). Their explicit value always wins — same
		// precedence rule as molecule_runtime.adapter_base's
		// resolve_provider_routing.
		if existing, ok := envVars[baseURLKey]; ok && existing != "" {
			continue
		}

		envVars[baseURLKey] = defaultURL
		injected = append(injected, baseURLKey)
	}
	return injected
}
