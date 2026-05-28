package providers

import (
	"testing"
)

// TestLoadParses asserts the embedded manifest parses and is non-empty.
func TestLoadParses(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(ps) == 0 {
		t.Fatal("Load() returned an empty provider slice")
	}
}

// TestRequiredFieldsPopulated asserts every entry has the fields the
// validate invariant requires (name, protocol, auth_mode, auth_env,
// display_name, model_prefix_match), and that protocol is one of the
// two legal wire formats.
func TestRequiredFieldsPopulated(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, p := range ps {
		if p.Name == "" {
			t.Errorf("provider with display_name %q has empty name", p.DisplayName)
		}
		if p.DisplayName == "" {
			t.Errorf("provider %q has empty display_name", p.Name)
		}
		if p.AuthMode == "" {
			t.Errorf("provider %q has empty auth_mode", p.Name)
		}
		if len(p.AuthEnv) == 0 {
			t.Errorf("provider %q has empty auth_env", p.Name)
		}
		if p.ModelPrefixMatch == "" {
			t.Errorf("provider %q has empty model_prefix_match", p.Name)
		}
		switch p.Protocol {
		case ProtocolOpenAI, ProtocolAnthropic:
		default:
			t.Errorf("provider %q has invalid protocol %q", p.Name, p.Protocol)
		}
	}
}

// TestUniqueNames asserts provider names are unique (Load enforces this;
// this test guards the manifest data itself).
func TestUniqueNames(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	seen := make(map[string]bool, len(ps))
	for _, p := range ps {
		if seen[p.Name] {
			t.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
	}
}

// providerByName is a test helper.
func providerByName(t *testing.T, ps []Provider, name string) Provider {
	t.Helper()
	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("provider %q not found in manifest", name)
	return Provider{}
}

// TestMatchesModel maps representative slugs from each source (proxy
// prefixes, canvas BARE_VENDOR_PATTERNS, adapter model_prefixes, DB
// catalog ids) to the provider that should own them.
func TestMatchesModel(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	cases := []struct {
		slug   string
		expect string // provider name that must match
	}{
		// Moonshot vs Kimi-coding — corrected serving split (internal#718
		// P0, CTO 2026-05-27, empirically verified): the BYOK api.kimi.com/
		// coding gateway owns the BARE kimi-* ids; the moonshot endpoint owns
		// the moonshot-namespaced/prefixed ids. Bare kimi-k2.6 / kimi-k2.5 /
		// kimi-for-coding therefore belong to kimi-coding; only the explicit
		// moonshot/ (proxy/platform) and moonshot- (bare moonshot model)
		// prefixes belong to moonshot.
		{"kimi-k2.6", "kimi-coding"},
		{"kimi-k2.5", "kimi-coding"},
		{"kimi-latest", "kimi-coding"},
		{"moonshot/kimi-k2.6", "moonshot"},
		{"moonshot-v1-128k", "moonshot"},
		// Anthropic — proxy "claude"->anthropic + DB claude-* + canvas /^claude-/.
		{"claude-sonnet-4-6", "anthropic-api"},
		{"claude-opus-4-7", "anthropic-api"},
		{"claude-haiku-4-5-20251001", "anthropic-api"},
		// Anthropic OAuth aliases.
		{"sonnet", "anthropic-oauth"},
		{"opus", "anthropic-oauth"},
		{"haiku", "anthropic-oauth"},
		// MiniMax — DB MiniMax-M2.7 (mixed case) + canvas /^MiniMax-/.
		{"MiniMax-M2.7", "minimax"},
		{"MiniMax-M2", "minimax"},
		{"minimax-m2.5", "minimax"},
		// OpenAI — DB gpt-5.x + canvas /^gpt-/.
		{"gpt-5.5", "openai"},
		{"gpt-5.4-mini", "openai"},
		// Xiaomi MiMo — adapter mimo- + canvas /^mimo-/.
		{"mimo-v2.5-pro", "xiaomi-mimo"},
		// Z.ai GLM — adapter glm- + canvas /^GLM-/ (mixed case).
		{"GLM-4.6", "zai"},
		{"glm-4.5", "zai"},
		// DeepSeek.
		{"deepseek-v4-pro", "deepseek"},
		// Kimi coding-tuned gateway (distinct from moonshot).
		{"kimi-for-coding", "kimi-coding"},
		// Canvas-only slash-prefixed vendors.
		{"openrouter/anthropic/claude-3.5", "openrouter"},
		{"huggingface/mistralai/Mistral-7B", "huggingface"},
		{"custom/my-local-model", "custom"},
		{"gemini-2.5-pro", "google"},
		{"qwen-3-max", "alibaba"},
		{"nousresearch/hermes-4-70b", "nousresearch"},
	}

	for _, tc := range cases {
		p := providerByName(t, ps, tc.expect)
		if !p.MatchesModel(tc.slug) {
			t.Errorf("slug %q: expected provider %q to match, but it did not (regex %q)", tc.slug, tc.expect, p.ModelPrefixMatch)
		}
	}
}

// TestNoAmbiguousModelMatch is the RFC §8.5 overlap guard: no two
// providers may claim the same representative slug. A bad regex that
// over-broadly matches another vendor's ids breaks routing across three
// runtimes, so we catch overlap at PR-1 load time.
func TestNoAmbiguousModelMatch(t *testing.T) {
	ps, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Representative slug corpus spanning every source. Each slug must be
	// claimed by exactly one provider.
	corpus := []string{
		"kimi-k2.6", "kimi-k2.5", "moonshot-v1-128k", "moonshot/kimi-k2.6",
		"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001",
		"sonnet", "opus", "haiku",
		"MiniMax-M2.7", "MiniMax-M2", "minimax-m2.5", "MiniMax-M2.7-highspeed",
		"gpt-5.5", "gpt-5.4", "gpt-5.4-mini",
		"mimo-v2.5-pro", "mimo-v2-flash",
		"GLM-4.6", "glm-4.5",
		"deepseek-v4-pro", "deepseek-v4-flash",
		"kimi-for-coding",
		"openrouter/x", "huggingface/y", "custom/z",
		"gemini-2.5-pro", "qwen-3-max", "nousresearch/hermes-4-70b",
		"ai-gateway/m", "opencode-zen/m", "opencode-go/m", "kilocode/m",
		"minimax-cn/m2", "ollama-cloud/m", "ollama/llama4", "nvidia/m", "arcee/m",
		"platform/anything",
	}

	for _, slug := range corpus {
		var matched []string
		for _, p := range ps {
			if p.MatchesModel(slug) {
				matched = append(matched, p.Name)
			}
		}
		if len(matched) > 1 {
			t.Errorf("slug %q ambiguously matched %d providers: %v", slug, len(matched), matched)
		}
	}
}

// TestMatchesModelZeroValue exercises the lazy on-demand compile path of
// a Provider not produced by Load.
func TestMatchesModelZeroValue(t *testing.T) {
	p := Provider{ModelPrefixMatch: "^claude-"}
	if !p.MatchesModel("claude-opus-4-7") {
		t.Error("zero-value Provider should match claude-opus-4-7")
	}
	if p.MatchesModel("gpt-5.5") {
		t.Error("zero-value Provider should not match gpt-5.5")
	}

	bad := Provider{ModelPrefixMatch: "([unterminated"}
	if bad.MatchesModel("anything") {
		t.Error("Provider with an invalid regex must never match")
	}

	empty := Provider{}
	if empty.MatchesModel("anything") {
		t.Error("Provider with an empty regex must never match")
	}
}
