import { describe, it, expect } from "vitest";
import {
  parseComputeOptions,
  FALLBACK_COMPUTE_OPTIONS,
  defaultInstanceForProvider,
  displayDefaultForProvider,
  instanceTypesForProvider,
  normalizeProvider,
  providerLabel,
} from "../compute-options";

describe("compute-options", () => {
  const serverResponse = {
    providers: [
      {
        id: "aws",
        label: "AWS (default)",
        default_instance: "t3.medium",
        display_default: "t3.xlarge",
        instances: ["t3.medium", "t3.large", "t3.xlarge"],
      },
      {
        id: "hetzner",
        label: "Hetzner",
        default_instance: "cpx31",
        display_default: "cpx41",
        instances: ["cpx31", "cpx41"],
      },
      {
        id: "gcp",
        label: "GCP",
        default_instance: "e2-standard-2",
        display_default: "e2-standard-4",
        instances: ["e2-standard-2", "e2-standard-4"],
      },
    ],
  };

  it("parses /compute/metadata into ComputeOptions", () => {
    const opts = parseComputeOptions(serverResponse);
    expect(opts).not.toBeNull();
    expect(opts?.providers).toEqual(["aws", "hetzner", "gcp"]);
    expect(opts?.defaults).toEqual({ aws: "t3.medium", hetzner: "cpx31", gcp: "e2-standard-2" });
    expect(opts?.displayDefaults).toEqual({
      aws: "t3.xlarge",
      hetzner: "cpx41",
      gcp: "e2-standard-4",
    });
    expect(opts?.labels).toEqual({ aws: "AWS (default)", hetzner: "Hetzner", gcp: "GCP" });
    expect(opts?.instanceTypes.aws).toEqual(["t3.medium", "t3.large", "t3.xlarge"]);
  });

  it("falls back to in-bundle defaults when display_default is missing", () => {
    const partial = {
      providers: [
        {
          id: "aws",
          default_instance: "t3.medium",
          instances: ["t3.medium"],
        },
      ],
    };
    const opts = parseComputeOptions(partial);
    expect(opts?.displayDefaults).toEqual(FALLBACK_COMPUTE_OPTIONS.displayDefaults);
  });

  it("returns null for malformed payloads", () => {
    expect(parseComputeOptions(null)).toBeNull();
    expect(parseComputeOptions({})).toBeNull();
    expect(parseComputeOptions({ providers: [] })).toBeNull();
    expect(parseComputeOptions({ providers: [{ id: "" }] })).toBeNull();
  });

  it("helpers resolve per-provider values", () => {
    const opts = parseComputeOptions(serverResponse)!;
    expect(defaultInstanceForProvider(opts, "aws")).toBe("t3.medium");
    expect(displayDefaultForProvider(opts, "aws")).toBe("t3.xlarge");
    expect(instanceTypesForProvider(opts, "hetzner")).toEqual(["cpx31", "cpx41"]);
    expect(providerLabel(opts, "gcp")).toBe("GCP");
    expect(normalizeProvider(undefined)).toBe("aws");
  });
});
