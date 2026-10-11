import { afterEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { middleware } from "../middleware";

afterEach(() => vi.unstubAllEnvs());

describe("locale entry URLs", () => {
  it.each(["/en", "/en/", "/zh", "/ja/"])("serves %s without redirecting the browser", (path) => {
    const response = middleware(new NextRequest(`http://127.0.0.1:3000${path}`));
    expect(response.status).toBe(200);
    expect(response.headers.get("location")).toBeNull();
    // NextURL normalizes loopback hosts to localhost in unit requests.
    expect(response.headers.get("x-middleware-rewrite")).toBe("http://localhost:3000/");
  });

  it("preserves a nested path and query", () => {
    const response = middleware(new NextRequest("http://localhost:3000/en/settings?tab=keys"));
    expect(response.headers.get("x-middleware-rewrite")).toBe("http://localhost:3000/settings?tab=keys");
  });

  it.each(["/", "/enough", "/settings"])("leaves %s untouched", (path) => {
    const response = middleware(new NextRequest(`http://localhost:3000${path}`));
    expect(response.headers.get("x-middleware-rewrite")).toBeNull();
    expect(response.headers.get("location")).toBeNull();
  });

  it("retains the nonce CSP and forwards the same nonce on rewrites", () => {
    vi.stubEnv("NODE_ENV", "production");
    vi.stubEnv("CSP_DEV_MODE", "0");
    const response = middleware(new NextRequest("https://canvas.example/en"));
    const nonce = response.headers.get("x-middleware-request-x-nonce");
    expect(nonce).toBeTruthy();
    expect(response.headers.get("Content-Security-Policy")).toContain(`'nonce-${nonce}'`);
    expect(response.headers.get("x-middleware-request-content-security-policy"))
      .toBe(response.headers.get("Content-Security-Policy"));
  });
});
