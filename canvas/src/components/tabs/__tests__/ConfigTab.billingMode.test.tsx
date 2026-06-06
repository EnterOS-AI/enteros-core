// @vitest-environment jsdom
//
// internal#718 P4 closure — ConfigTab.billingMode.test.tsx is retired.
//
// This suite (255 lines, 8 tests) pinned the canvas-side provider →
// llm_billing_mode linkage from internal#703 Gap 2: when the operator
// changed the PROVIDER in the Config tab, ConfigTab.handleSave would
// PUT /admin/workspaces/:id/llm-billing-mode so the platform-vs-byok
// decision tracked the dropdown.
//
// That linkage is retired together with the LLM_PROVIDER override flow
// (see ConfigTab.provider.test.tsx retirement note). P2-B (#1972)
// moved the platform-vs-byok decision to
// `ResolveLLMBillingModeDerived(runtime, model, authEnv)` in
// workspace-server — the canvas can no longer override it via the
// provider dropdown, by design. The runtime+model selection IS the
// billing-mode selection now.
//
// The `/admin/workspaces/:id/llm-billing-mode` endpoint still exists
// as the operator override surface (`workspaces.llm_billing_mode`
// column); it is no longer driven by the provider dropdown.
// Coverage for the derived billing flow lives in
// workspace-server/internal/handlers/llm_billing_mode_derived_test.go.
//
// Restore from git history if the canvas-side provider→billing linkage
// needs to be revisited (it should not — the derived resolver is the
// single decision point).

import { describe, it } from "vitest";

describe("ConfigTab — provider → llm_billing_mode linkage (retired internal#718 P4)", () => {
  it.skip("LLM_PROVIDER → billing_mode wiring is retired; see file header for the replacement coverage", () => {
    // intentionally empty
  });
});
