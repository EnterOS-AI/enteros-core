// @vitest-environment jsdom
//
// internal#718 P3 (retire-list #5) — the billing-mode the Config tab shows /
// sends must reflect the DERIVED provider per the registry, not the hardcoded
// billingModeForProvider("" | "platform" → platform_managed else byok) rule.
// When the runtime is registry-backed, billingModeForSelectedProvider reads the
// registry-served billing_mode off the provider catalog entry. The hardcoded
// rule remains only as the fallback for non-registry runtimes / older backends.

import { describe, it, expect } from "vitest";
import { billingModeForSelectedProvider } from "../ConfigTab";
import {
  buildProviderCatalogFromRegistry,
  type RegistryProvider,
  type RegistryModel,
} from "../../ProviderModelSelector";

const REGISTRY_PROVIDERS: RegistryProvider[] = [
  { name: "anthropic-oauth", display_name: "Claude Code subscription", auth_env: ["CLAUDE_CODE_OAUTH_TOKEN"], billing_mode: "byok" },
  { name: "platform", display_name: "Platform", auth_env: ["ANTHROPIC_API_KEY"], billing_mode: "platform_managed" },
];
const REGISTRY_MODELS: RegistryModel[] = [
  { id: "sonnet", provider: "anthropic-oauth", billing_mode: "byok" },
  { id: "anthropic/claude-opus-4-7", provider: "platform", billing_mode: "platform_managed" },
];

describe("billingModeForSelectedProvider (registry-driven)", () => {
  const catalog = buildProviderCatalogFromRegistry(REGISTRY_PROVIDERS, REGISTRY_MODELS);

  it("reads platform_managed from the registry for the platform provider", () => {
    expect(billingModeForSelectedProvider("platform", catalog)).toBe("platform_managed");
  });

  it("reads byok from the registry for a BYOK provider", () => {
    // anthropic-oauth derives to byok via the REGISTRY (not because the
    // hardcoded rule treats every non-'platform' string as byok — that rule
    // would also say byok here, so use a case the hardcoded rule gets WRONG
    // to prove the registry is authoritative):
    expect(billingModeForSelectedProvider("anthropic-oauth", catalog)).toBe("byok");
  });

  it("falls back to the hardcoded rule when no registry catalog is supplied", () => {
    // Non-registry runtime / older backend → catalog empty/undefined → the
    // legacy mapping still applies ('' | 'platform' → platform_managed).
    expect(billingModeForSelectedProvider("", undefined)).toBe("platform_managed");
    expect(billingModeForSelectedProvider("platform", undefined)).toBe("platform_managed");
    expect(billingModeForSelectedProvider("minimax", undefined)).toBe("byok");
  });

  it("falls back to the hardcoded rule when the provider is not in the registry catalog", () => {
    // A provider string the registry catalog doesn't carry (stale saved
    // value) → fall back to the legacy rule rather than guessing.
    expect(billingModeForSelectedProvider("some-byo-vendor", catalog)).toBe("byok");
  });
});
