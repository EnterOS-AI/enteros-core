// @vitest-environment jsdom
//
// internal#718 P4 closure — ConfigTab.provider.test.tsx is retired.
//
// This 574-line suite exercised the canvas-side LLM provider override
// flow: load the existing override from GET /workspaces/:id/provider,
// edit the dropdown, Save → PUT /workspaces/:id/provider, and the
// provider→billing_mode linkage on Save. All three server endpoints
// behind those flows are retired in internal#718 P4 closure:
//
//   - workspace-server SetProvider / GetProvider (PUT/GET
//     /workspaces/:id/provider) → both return 410 Gone with a
//     PROVIDER_ENDPOINT_RETIRED structured body.
//   - workspace-server setProviderSecret (the writer into
//     workspace_secrets.LLM_PROVIDER) — removed; row never written.
//   - The LLM_PROVIDER workspace_secret itself — migrated away in
//     20260528000000_drop_llm_provider_workspace_secret.up.sql.
//
// ConfigTab still renders the provider dropdown for display (the user
// can preview the derived provider locally), but Save no longer
// round-trips the value. The replacement contract is that the provider
// is DERIVED at every decision point from (runtime, model) via the
// registry — see internal/providers/derive_provider.go.
//
// The original suite's coverage is replaced by:
//
//   - workspace-server: TestPutProvider_410Gone +
//     TestGetProvider_410Gone + TestProviderEndpointGone_BodyShape in
//     internal/handlers/llm_provider_removal_p4_test.go.
//   - workspace-server: TestWorkspaceCreate_FirstDeploy_OnlyPersistsMODEL
//     in internal/handlers/workspace_provision_shared_test.go.
//   - registry: TestDeriveProvider_RealManifest in
//     internal/providers/derive_provider_test.go.
//
// Restore from git history if any aspect of the legacy LLM_PROVIDER
// flow needs to be revisited (it should not — the retirement is
// permanent).

import { describe, it } from "vitest";

describe("ConfigTab provider override — retired (internal#718 P4)", () => {
  it.skip("LLM_PROVIDER override flow is retired; see file header for the replacement coverage", () => {
    // intentionally empty
  });
});
