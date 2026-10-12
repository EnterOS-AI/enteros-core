// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { BrowserContext } from "@playwright/test";

import {
  installStagingWebSocketAuth,
  stagingWebSocketAuthInit,
} from "../../../e2e/support/stagingWebSocketAuth";

type CreatedSocket = {
  url: string | URL;
  protocols: string | string[] | undefined;
};

const created: CreatedSocket[] = [];
let originalWebSocket: typeof WebSocket;

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  constructor(url: string | URL, protocols?: string | string[]) {
    created.push({ url, protocols });
  }
}

beforeEach(() => {
  created.length = 0;
  originalWebSocket = window.WebSocket;
  Object.defineProperty(window, "WebSocket", {
    configurable: true,
    writable: true,
    value: FakeWebSocket as unknown as typeof WebSocket,
  });
});

afterEach(() => {
  Object.defineProperty(window, "WebSocket", {
    configurable: true,
    writable: true,
    value: originalWebSocket,
  });
});

describe("stagingWebSocketAuthInit", () => {
  it("adds the existing browser auth protocols only to this tenant's exact /ws route", () => {
    stagingWebSocketAuthInit({
      token: "A-1",
      tenantOrigin: window.location.origin,
    });

    const socket = new window.WebSocket(`ws://${window.location.host}/ws`);

    expect(socket).toBeInstanceOf(FakeWebSocket);
    expect(created).toEqual([
      {
        url: `ws://${window.location.host}/ws`,
        protocols: ["molecule-auth.412d31", "molecule-ws"],
      },
    ]);
  });

  it("does not attach the tenant credential to another origin or route", () => {
    stagingWebSocketAuthInit({
      token: "do-not-leak",
      tenantOrigin: window.location.origin,
    });

    new window.WebSocket("wss://example.test/ws");
    new window.WebSocket(`ws://${window.location.host}/ws/events`);

    expect(created.map((entry) => entry.protocols)).toEqual([
      undefined,
      undefined,
    ]);
  });

  it("does not retain or attach the credential in a hostile-origin frame", () => {
    const nativeWebSocket = window.WebSocket;

    stagingWebSocketAuthInit({
      token: "hostile-frame-must-not-receive-this",
      tenantOrigin: "https://pinned-tenant.example",
    });
    new window.WebSocket(`ws://${window.location.host}/ws`);

    expect(window.WebSocket).toBe(nativeWebSocket);
    expect(created).toEqual([
      {
        url: `ws://${window.location.host}/ws`,
        protocols: undefined,
      },
    ]);
  });

  it("preserves caller-supplied protocols instead of overriding product behavior", () => {
    stagingWebSocketAuthInit({
      token: "test-token",
      tenantOrigin: window.location.origin,
    });

    new window.WebSocket(`ws://${window.location.host}/ws`, [
      "caller-protocol",
    ]);

    expect(created[0]?.protocols).toEqual(["caller-protocol"]);
  });
});

describe("installStagingWebSocketAuth", () => {
  it("registers the short-lived token as test init data, not bundle configuration", async () => {
    const addInitScript = vi.fn().mockResolvedValue(undefined);
    const context = { addInitScript } as unknown as BrowserContext;

    await installStagingWebSocketAuth(context, {
      token: "fresh-org-token",
      tenantURL: "https://fresh-org.example/health",
    });

    expect(addInitScript).toHaveBeenCalledOnce();
    expect(addInitScript).toHaveBeenCalledWith(stagingWebSocketAuthInit, {
      token: "fresh-org-token",
      tenantOrigin: "https://fresh-org.example",
    });
  });

  it("fails closed instead of registering an empty credential", async () => {
    const addInitScript = vi.fn().mockResolvedValue(undefined);
    const context = { addInitScript } as unknown as BrowserContext;

    await expect(
      installStagingWebSocketAuth(context, {
        token: "",
        tenantURL: "https://fresh-org.example",
      }),
    ).rejects.toThrow("staging tenant token is required");
    expect(addInitScript).not.toHaveBeenCalled();
  });

  it("fails closed instead of registering a non-HTTP tenant URL", async () => {
    const addInitScript = vi.fn().mockResolvedValue(undefined);
    const context = { addInitScript } as unknown as BrowserContext;

    await expect(
      installStagingWebSocketAuth(context, {
        token: "fresh-org-token",
        tenantURL: "javascript:alert('wrong origin')",
      }),
    ).rejects.toThrow("staging tenant URL must use HTTP(S)");
    expect(addInitScript).not.toHaveBeenCalled();
  });
});
