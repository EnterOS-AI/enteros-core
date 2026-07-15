import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const spec = readFileSync(
  new URL("../../../e2e/staging-slow-cold-greeting.spec.ts", import.meta.url),
  "utf8",
);

describe("slow-cold greeting browser harness", () => {
  it("does not apply Playwright's 30s default timeout to the 240s cold turn", () => {
    const start = spec.indexOf('context.route("**/workspaces/*/a2a"');
    const end = spec.indexOf("await gotoWithNetworkChangeRetry", start);
    const slowTurnInterceptor = spec.slice(start, end);

    expect(start).toBeGreaterThan(-1);
    expect(end).toBeGreaterThan(start);
    expect(slowTurnInterceptor).toContain("route.fetch({ timeout: 0 })");
    expect(slowTurnInterceptor).not.toMatch(/\broute\.fetch\(\)\s*;/);
  });
});
