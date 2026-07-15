import type { BrowserContext } from "@playwright/test";

/**
 * Playwright-only bridge for the staging harness.
 *
 * Real users authenticate the global Canvas socket with their HttpOnly WorkOS
 * session cookie. The staging browser has no WorkOS session; it owns only the
 * fresh org's short-lived tenant admin bearer. Browser WebSocket constructors
 * cannot set Authorization, so this init script offers that ephemeral bearer
 * through the browser subprotocol contract already implemented by
 * canvas/src/store/socket.ts and workspace-server/internal/handlers/socket.go.
 *
 * This file lives under e2e/ and is never imported by the Canvas application:
 * no tenant token or token-reading hook is compiled into the public bundle.
 */
export function stagingWebSocketAuthInit(token: string): void {
  if (!token) {
    throw new Error("staging tenant token is required");
  }

  const NativeWebSocket = window.WebSocket;
  const encoded = Array.from(new TextEncoder().encode(token), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  const authProtocols = [
    `molecule-auth.${encoded}`,
    "molecule-ws",
  ];

  function StagingAuthenticatedWebSocket(
    this: WebSocket,
    url: string | URL,
    protocols?: string | string[],
  ): WebSocket {
    if (!new.target) {
      throw new TypeError("WebSocket constructor must be called with new");
    }

    let isTenantGlobalSocket = false;
    try {
      const parsed = new URL(String(url), window.location.href);
      const expectedProtocol =
        window.location.protocol === "https:" ? "wss:" : "ws:";
      isTenantGlobalSocket =
        parsed.protocol === expectedProtocol &&
        parsed.host === window.location.host &&
        parsed.pathname === "/ws";
    } catch {
      // Preserve the native constructor's own URL validation and error shape.
    }

    if (isTenantGlobalSocket && protocols === undefined) {
      return new NativeWebSocket(url, authProtocols);
    }
    return protocols === undefined
      ? new NativeWebSocket(url)
      : new NativeWebSocket(url, protocols);
  }

  // Preserve instanceof and the constructor's static CONNECTING/OPEN/etc.
  StagingAuthenticatedWebSocket.prototype = NativeWebSocket.prototype;
  Object.setPrototypeOf(StagingAuthenticatedWebSocket, NativeWebSocket);
  window.WebSocket =
    StagingAuthenticatedWebSocket as unknown as typeof WebSocket;
}

/** Register the auth bridge before the first tenant document is created. */
export async function installStagingWebSocketAuth(
  context: BrowserContext,
  token: string,
): Promise<void> {
  if (!token) {
    throw new Error("staging tenant token is required");
  }
  await context.addInitScript(stagingWebSocketAuthInit, token);
}
