/**
 * Live proof for the staging harness's browser WebSocket credential path.
 *
 * Production users keep using their HttpOnly WorkOS session cookie. This test
 * has no such session: it installs the fresh org's short-lived tenant bearer as
 * Playwright init data, opens the exact global /ws route, and requires the
 * server to select only the non-secret molecule-ws sentinel. The helper is
 * e2e-only; no token source is compiled into the public Canvas bundle.
 */
import { expect, test } from "@playwright/test";

import { installStagingWebSocketAuth } from "./support/stagingWebSocketAuth";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

test.skip(
  !STAGING,
  "CANVAS_E2E_STAGING not set — staging-only suite, not requested",
);

test("global Canvas WebSocket authenticates without a WorkOS test session", async ({
  context,
  page,
}) => {
  const tenantURL = process.env.STAGING_TENANT_URL;
  const tenantToken = process.env.STAGING_TENANT_TOKEN;
  if (!tenantURL || !tenantToken) {
    throw new Error(
      "staging-setup.ts did not export STAGING_TENANT_URL / " +
        "STAGING_TENANT_TOKEN; requested staging coverage must fail closed",
    );
  }

  await installStagingWebSocketAuth(context, {
    token: tenantToken,
    tenantURL,
  });
  await page.goto(`${tenantURL}/health`, { waitUntil: "domcontentloaded" });

  const result = await page.evaluate(async () => {
    const url = new URL("/ws", window.location.href);
    url.protocol = window.location.protocol === "https:" ? "wss:" : "ws:";

    return await new Promise<{
      opened: boolean;
      selectedProtocol: string;
      detail: string;
    }>((resolve) => {
      const socket = new WebSocket(url);
      let settled = false;
      const finish = (opened: boolean, detail: string) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        const selectedProtocol = socket.protocol;
        try {
          socket.close(1000, "staging auth proof complete");
        } catch {
          // The failure result below remains authoritative.
        }
        resolve({ opened, selectedProtocol, detail });
      };
      // Completion is event-driven; this deadline only prevents an infinite hang.
      // prettier-ignore
      const timeout = window.setTimeout( // lint-allow: env-coupling -- event-driven safety deadline
        () => finish(false, "timed out waiting for WebSocket open"),
        15_000,
      );
      socket.addEventListener("open", () => finish(true, "opened"), {
        once: true,
      });
      socket.addEventListener("error", () => finish(false, "socket error"), {
        once: true,
      });
      socket.addEventListener(
        "close",
        (event) =>
          finish(
            false,
            `closed before open (code=${event.code}, reason=${event.reason})`,
          ),
        { once: true },
      );
    });
  });

  expect(result, result.detail).toEqual({
    opened: true,
    selectedProtocol: "molecule-ws",
    detail: "opened",
  });
});
