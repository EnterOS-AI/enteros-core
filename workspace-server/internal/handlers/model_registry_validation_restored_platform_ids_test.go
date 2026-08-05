package handlers

import "testing"

// Create-gate proof for the molecule-ai-sdk#204 adoption (2026-08-05).
//
// sdk#203 narrowed every runtime's `platform` arm to minimax-only after the
// platform's Moonshot vendor account was suspended. That narrowing withdrew five
// ids that were HEALTHY, and the consequence was asymmetric in a way that is easy
// to miss: workspaces ALREADY pinned to those ids kept serving (they were healed
// by the CP-side resolution), but the ids were no longer SELECTABLE — a new
// workspace could not be created on them, because this gate keys off
// ModelsForRuntime. sdk#204 restores them.
//
// The providers-package test (TestDeriveProvider_PlatformArmMembership) proves
// the REGISTRY says the right thing. This test proves the thing operators
// actually care about: the create-time gate — the one that emits 422
// UNREGISTERED_MODEL_FOR_RUNTIME — now ACCEPTS them. The two are not the same
// assertion: the gate is an OR over ModelsForRuntime and DeriveProvider, so it
// could in principle have accepted these ids via the BYOK routability path even
// under #203. It did not, and this test is what makes that claim checkable.
//
// Under the #203 pin every "restored" case below fails.
func TestValidateRegisteredModelForRuntime_Sdk204RestoredPlatformIDs(t *testing.T) {
	// The exact five ids sdk#204 restored, with the runtime whose platform arm
	// carries each.
	restored := []struct{ runtime, model string }{
		{"claude-code", "anthropic/claude-opus-4-7"},
		{"claude-code", "anthropic/claude-opus-4-8"},
		{"claude-code", "anthropic/claude-sonnet-4-6"},
		{"codex", "openai/gpt-5.4"},
		{"codex", "openai/gpt-5.4-mini"},
	}
	for _, tc := range restored {
		t.Run("restored/"+tc.runtime+"/"+tc.model, func(t *testing.T) {
			ok, why := validateRegisteredModelForRuntime(tc.runtime, tc.model)
			if !ok {
				t.Fatalf("validateRegisteredModelForRuntime(%q, %q) = false (%s) — "+
					"sdk#204 restored this id, so workspace-create must ACCEPT it. "+
					"A failure here means the sdk/gen/go pin in go.mod is still on "+
					"#203 or older (canonicalRegistrySHA256 would be stale too).",
					tc.runtime, tc.model, why)
			}
		})
	}

	// NEGATIVE CONTROL 1 — the genuinely dead ids. sdk#204 moved these to
	// `withdrawn_models`, NOT back onto the menu. If this half ever goes green
	// alongside the half above, the adoption re-opened selection on a suspended
	// vendor account: the exact "permanently dead workspace" hazard #203 existed
	// to close. This is what keeps the test above from being a rubber stamp on
	// "everything is allowed now".
	stillWithdrawn := []struct{ runtime, model string }{
		{"claude-code", "moonshot/kimi-k2.6"},
		{"claude-code", "moonshot/kimi-k2.5"},
		{"hermes", "moonshot/kimi-k2.6"},
		{"hermes", "moonshot/kimi-k2.5"},
		{"openclaw", "moonshot/kimi-k2.6"},
		{"openclaw", "moonshot/kimi-k2.5"},
	}
	for _, tc := range stillWithdrawn {
		t.Run("withdrawn/"+tc.runtime+"/"+tc.model, func(t *testing.T) {
			if ok, _ := validateRegisteredModelForRuntime(tc.runtime, tc.model); ok {
				t.Fatalf("validateRegisteredModelForRuntime(%q, %q) = true — the "+
					"suspended-account moonshot platform ids must stay REFUSED at "+
					"create (422 UNREGISTERED_MODEL_FOR_RUNTIME). Accepting one "+
					"provisions a workspace that 429s on every call.",
					tc.runtime, tc.model)
			}
		})
	}

	// NEGATIVE CONTROL 2 — a nonsense id. Guards against the failure mode where
	// the gate has degenerated into "accept everything" (registry load failure
	// makes it fail OPEN by design, which would turn the restored half above
	// green for the wrong reason). If this passes validation, the restored
	// assertions prove nothing at all.
	if ok, _ := validateRegisteredModelForRuntime("claude-code", "anthropic/claude-opus-9-9-not-a-real-model"); ok {
		t.Fatal("CONTROL BROKEN: a nonsense model id was ACCEPTED for claude-code — " +
			"the gate is failing open (registry load failure?), so the restored-id " +
			"assertions in this test are vacuous and prove nothing.")
	}
}
