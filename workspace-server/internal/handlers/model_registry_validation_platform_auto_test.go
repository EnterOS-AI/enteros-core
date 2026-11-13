package handlers

import "testing"

// platform/auto (Auto model routing, Plan 1) is the sentinel model id that the
// CP LLM proxy resolves per-call to a concrete model. In the registry it is a
// member of every runtime's closed `platform` arm (molecule-ai-sdk
// feat/auto-model-routing 1d3f835). These tests pin core's side of that
// contract: create-time acceptance, provider derivation, and that derivation ignores vendor keys.
// They require the sdk/gen/go pin to be at or after that SDK commit.

var autoRuntimes = []string{"claude-code", "hermes", "codex", "openclaw"}

func TestValidateRegisteredModelForRuntime_PlatformAuto(t *testing.T) {
	for _, rt := range autoRuntimes {
		t.Run(rt, func(t *testing.T) {
			if ok, why := validateRegisteredModelForRuntime(rt, "platform/auto"); !ok {
				t.Fatalf("validateRegisteredModelForRuntime(%q, platform/auto) = false (%s); sdk pin predates platform/auto?", rt, why)
			}
		})
	}
}

// The create gate is an OR of (platform menu) and (DeriveProvider resolves), so
// acceptance alone does not prove platform/auto is OFFERED. Pin menu membership
// explicitly: this is what the pickers and defaultModelForRuntime read, and it
// is what fails on a pre-auto sdk pin.
func TestModelsForRuntime_PlatformAutoOnPlatformMenu(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Fatalf("providerRegistry: %v", err)
	}
	for _, rt := range autoRuntimes {
		models, err := m.ModelsForRuntime(rt)
		if err != nil {
			t.Fatalf("ModelsForRuntime(%q): %v", rt, err)
		}
		found := false
		for _, id := range models {
			if id == "platform/auto" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: platform/auto missing from platform menu %v; sdk pin predates platform/auto?", rt, models)
		}
	}
}

func TestDeriveProvider_PlatformAutoIsPlatform(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Fatalf("providerRegistry: %v", err)
	}
	for _, rt := range autoRuntimes {
		t.Run(rt, func(t *testing.T) {
			p, err := m.DeriveProvider(rt, "platform/auto", nil)
			if err != nil {
				t.Fatalf("DeriveProvider(%q, platform/auto) error: %v", rt, err)
			}
			if !p.IsPlatform() {
				t.Fatalf("DeriveProvider(%q, platform/auto) = %q, want platform", rt, p.Name)
			}
		})
	}
}

// A vendor key present alongside platform/auto must NOT flip the derivation to
// a BYOK arm: the exact-id match is authoritative over auth-env tie-breaks, so
// auto can never be billed against (or silently reroute to) a customer key.
func TestDeriveProvider_PlatformAutoIgnoresVendorKeys(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Fatalf("providerRegistry: %v", err)
	}
	keys := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "MINIMAX_API_KEY"}
	for _, rt := range autoRuntimes {
		p, err := m.DeriveProvider(rt, "platform/auto", keys)
		if err != nil || !p.IsPlatform() {
			t.Fatalf("%s: with vendor keys DeriveProvider = (%q, %v), want platform", rt, p.Name, err)
		}
	}
}

// The BYOK create preflight treats platform/auto as platform-derived: allowed
// with no DB work and no credential needed. (This proves derivation, not that a
// BYOK workspace is refused; see auto_model_guard_test.go for that.)
func TestValidateBYOKCredentialSatisfiable_PlatformAutoIsPlatformDerived(t *testing.T) {
	for _, rt := range autoRuntimes {
		if ok, why := validateBYOKCredentialSatisfiable(t.Context(), rt, "platform/auto", nil); !ok {
			t.Fatalf("%s: platform/auto rejected by BYOK gate: %s", rt, why)
		}
	}
}
