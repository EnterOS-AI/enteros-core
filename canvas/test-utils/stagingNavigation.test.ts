import { describe, expect, it, vi } from "vitest";

import { gotoWithNetworkChangeRetry } from "./stagingNavigation";

describe("gotoWithNetworkChangeRetry", () => {
  it("retries one page.goto failure containing net::ERR_NETWORK_CHANGED", async () => {
    const response = { status: 200 };
    const goto = vi
      .fn()
      .mockRejectedValueOnce(
        new Error("page.goto: net::ERR_NETWORK_CHANGED at https://tenant.example/"),
      )
      .mockResolvedValueOnce(response);
    const page = { goto };
    const options = { waitUntil: "domcontentloaded" as const };

    await expect(
      gotoWithNetworkChangeRetry(page, "https://tenant.example/", options, 0),
    ).resolves.toBe(response);
    expect(goto).toHaveBeenCalledTimes(2);
    expect(goto).toHaveBeenNthCalledWith(1, "https://tenant.example/", options);
    expect(goto).toHaveBeenNthCalledWith(2, "https://tenant.example/", options);
  });

  it("rethrows every other page.goto failure without retrying", async () => {
    const refused = new Error(
      "page.goto: net::ERR_CONNECTION_REFUSED at https://tenant.example/",
    );
    const goto = vi.fn().mockRejectedValue(refused);

    await expect(
      gotoWithNetworkChangeRetry(
        { goto },
        "https://tenant.example/",
        { waitUntil: "domcontentloaded" },
        0,
      ),
    ).rejects.toBe(refused);
    expect(goto).toHaveBeenCalledTimes(1);
  });

  it("rethrows a second net::ERR_NETWORK_CHANGED failure", async () => {
    const first = new Error(
      "page.goto: net::ERR_NETWORK_CHANGED at https://tenant.example/",
    );
    const second = new Error(
      "page.goto: net::ERR_NETWORK_CHANGED at https://tenant.example/",
    );
    const goto = vi
      .fn()
      .mockRejectedValueOnce(first)
      .mockRejectedValueOnce(second);

    await expect(
      gotoWithNetworkChangeRetry(
        { goto },
        "https://tenant.example/",
        { waitUntil: "domcontentloaded" },
        0,
      ),
    ).rejects.toBe(second);
    expect(goto).toHaveBeenCalledTimes(2);
  });
});
