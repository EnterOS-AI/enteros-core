/**
 * Pin tests for the workspace error-code TS mirror
 * (lib/workspace-error-codes.ts). The values are duplicated on purpose —
 * drift canary for the Go emit sites documented in the module (no Go const
 * block exists yet; each code is a raw string at its emit site).
 */
import { describe, expect, it } from "vitest";
import { WORKSPACE_ERROR_CODES } from "../workspace-error-codes";

describe("WORKSPACE_ERROR_CODES", () => {
  it("mirrors every documented emit-site code exactly", () => {
    expect(WORKSPACE_ERROR_CODES).toEqual({
      ModelRequired: "MODEL_REQUIRED",
      UnregisteredModelForRuntime: "UNREGISTERED_MODEL_FOR_RUNTIME",
      RuntimeUnsupported: "RUNTIME_UNSUPPORTED",
      MissingByokCredential: "MISSING_BYOK_CREDENTIAL",
      RuntimeUnresolved: "RUNTIME_UNRESOLVED",
      DerivedProviderNotInRegistry: "DERIVED_PROVIDER_NOT_IN_REGISTRY",
      MissingPlatformProxy: "MISSING_PLATFORM_PROXY",
      MissingModel: "MISSING_MODEL",
    });
  });

  it("codes are mutually non-substrings (free-text scanning stays unambiguous)", () => {
    // extractErrorCode (setup-scene-lib) scans arbitrary error text with
    // String.includes — that is only deterministic while no code is a
    // substring of another. Guard the invariant here so a future code
    // addition that breaks it fails loudly.
    const values = Object.values(WORKSPACE_ERROR_CODES);
    for (const a of values) {
      for (const b of values) {
        if (a === b) continue;
        expect(a.includes(b)).toBe(false);
      }
    }
  });
});
