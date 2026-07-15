import { describe, expect, it, vi } from "vitest";
import type { APIResponse, Route } from "@playwright/test";

import { fulfillStagingFetchedResponse } from "../../../e2e/support/stagingRouteFulfill";

function fixtures(error?: Error) {
  const fulfill = error
    ? vi.fn().mockRejectedValue(error)
    : vi.fn().mockResolvedValue(undefined);
  const route = { fulfill } as unknown as Route;
  const response = {} as APIResponse;
  return { fulfill, route, response };
}

describe("fulfillStagingFetchedResponse", () => {
  it("fulfills an active intercepted response", async () => {
    const { fulfill, route, response } = fixtures();

    await fulfillStagingFetchedResponse(route, response);

    expect(fulfill).toHaveBeenCalledWith({ response });
  });

  it("ignores the exact disposed-response teardown race", async () => {
    const disposed = new Error(
      "route.fulfill: Fetch response has been disposed",
    );
    const { route, response } = fixtures(disposed);

    await expect(
      fulfillStagingFetchedResponse(route, response),
    ).resolves.toBeUndefined();
  });

  it("ignores Playwright's exact target-closed teardown error", async () => {
    const closed = new Error(
      "route.fulfill: Target page, context or browser has been closed",
    );
    const { route, response } = fixtures(closed);

    await expect(
      fulfillStagingFetchedResponse(route, response),
    ).resolves.toBeUndefined();
  });

  it("rethrows every unrelated route failure", async () => {
    const other = new Error("route.fulfill: protocol violation");
    const { route, response } = fixtures(other);

    await expect(
      fulfillStagingFetchedResponse(route, response),
    ).rejects.toBe(other);
  });
});
