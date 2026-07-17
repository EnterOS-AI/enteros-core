import { describe, it, expect, vi } from "vitest";
import {
  shouldRetryColdCreate,
  parseRetryAfter,
  createWorkspaceWithRetry,
  MAX_CREATE_ATTEMPTS,
  type CreateRetryDeps,
} from "../workspaceCreateRetry";
import { PlatformUnavailableError, type ApiError } from "../api";

// ---------------------------------------------------------------------------
// This is the CLIENT-side twin of tests/e2e/lib/workspace_create_retry.sh's
// offline unit test (tests/e2e/test_workspace_create_retry_unit.sh). It asserts
// the SAME decision table so the canvas create path and the E2E runner share
// ONE non-masking cold-origin rule (core#4307). Every fixture below mirrors a
// case in the shell test.
// ---------------------------------------------------------------------------

/** Build an ApiError exactly as api.ts's request() throws it on a non-ok
 *  response: message + status + raw bodyText + Retry-After. */
function apiError(status: number, bodyText: string, retryAfter: string | null = null): ApiError {
  const e = new Error(`API POST /workspaces: ${status} ${bodyText}`) as ApiError;
  e.status = status;
  e.bodyText = bodyText;
  e.retryAfter = retryAfter;
  return e;
}

/** A genuine fetch-level network failure (connection refused/reset/never
 *  established) — browsers/undici reject with a TypeError. The transport twin
 *  of the shell's curl `000`. */
function networkError(): Error {
  return new TypeError("Failed to fetch");
}

/** A client-side timeout/abort (AbortSignal.timeout) — maybe-processed, so it
 *  must NOT be retried (the 502/504 safety, at the transport layer). */
function timeoutError(): Error {
  const e = new Error("The operation timed out.");
  e.name = "TimeoutError";
  return e;
}

// ===========================================================================
// classifier: shouldRetryColdCreate  (port of create_should_retry_cold)
// ===========================================================================
describe("shouldRetryColdCreate", () => {
  // Cold-origin "never reached a handler" → RETRY
  it("empty-body 503 → retry", () => expect(shouldRetryColdCreate(503, "")).toBe(true));
  it("whitespace-only 503 → retry", () => expect(shouldRetryColdCreate(503, "   ")).toBe(true));
  it("null status (conn reset) → retry", () => expect(shouldRetryColdCreate(null, "")).toBe(true));

  // Maybe-processed gateway errors → NEVER retry (non-idempotent create)
  it("empty-body 502 → no retry", () => expect(shouldRetryColdCreate(502, "")).toBe(false));
  it("empty-body 504 → no retry", () => expect(shouldRetryColdCreate(504, "")).toBe(false));

  // Real app errors: NON-empty body → NEVER retry, even on a 503 status.
  it("JSON 422 body → no retry", () =>
    expect(shouldRetryColdCreate(422, '{"error":"RUNTIME_UNSUPPORTED"}')).toBe(false));
  it("JSON 400 body → no retry", () =>
    expect(shouldRetryColdCreate(400, '{"error":"invalid template"}')).toBe(false));
  it("503 WITH json body → no retry", () =>
    expect(shouldRetryColdCreate(503, '{"error":"boom"}')).toBe(false));

  // Other non-cold empties → no retry.
  it("empty 404 → no retry", () => expect(shouldRetryColdCreate(404, "")).toBe(false));
});

// ===========================================================================
// header parser: parseRetryAfter  (port of create_parse_retry_after)
// ===========================================================================
describe("parseRetryAfter", () => {
  it("integer 2 → 2", () => expect(parseRetryAfter("2")).toBe(2));
  it("integer 5 → 5", () => expect(parseRetryAfter("5")).toBe(5));
  it("whitespace-padded '  3 ' → 3", () => expect(parseRetryAfter("  3 ")).toBe(3));
  it("absent (null) → default 2", () => expect(parseRetryAfter(null)).toBe(2));
  it("absent (undefined) → default 2", () => expect(parseRetryAfter(undefined)).toBe(2));
  it("hostile 900 → capped at 10", () => expect(parseRetryAfter("900")).toBe(10));
  it("HTTP-date → default 2", () =>
    expect(parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT")).toBe(2));
  it("garbage → default 2", () => expect(parseRetryAfter("soon")).toBe(2));
});

// ===========================================================================
// wrapper: createWorkspaceWithRetry  (the bounded loop over the classifier)
// ===========================================================================
describe("createWorkspaceWithRetry", () => {
  /** Deps with a no-op sleep and a scripted post. */
  function deps(post: CreateRetryDeps["post"]) {
    const sleep = vi.fn(async () => {});
    return { deps: { post, sleep } as CreateRetryDeps, sleep };
  }

  it("empty-body 503 then 200 → retries once and succeeds", async () => {
    const post = vi
      .fn()
      .mockRejectedValueOnce(apiError(503, "", "2"))
      .mockResolvedValueOnce({ id: "ws-new" });
    const { deps: d, sleep } = deps(post as CreateRetryDeps["post"]);

    const res = await createWorkspaceWithRetry<{ id: string }>({ name: "A" }, undefined, d);

    expect(res).toEqual({ id: "ws-new" });
    expect(post).toHaveBeenCalledTimes(2);
    expect(post).toHaveBeenCalledWith("/workspaces", { name: "A" });
    // Honored the origin's Retry-After: 2s → 2000ms.
    expect(sleep).toHaveBeenCalledWith(2000);
  });

  it("network TypeError then 200 → retries and succeeds", async () => {
    const post = vi
      .fn()
      .mockRejectedValueOnce(networkError())
      .mockResolvedValueOnce({ id: "ws-2" });
    const { deps: d, sleep } = deps(post as CreateRetryDeps["post"]);

    const res = await createWorkspaceWithRetry<{ id: string }>({ name: "B" }, undefined, d);

    expect(res).toEqual({ id: "ws-2" });
    expect(post).toHaveBeenCalledTimes(2);
    // No Retry-After on a transport failure → default 2s.
    expect(sleep).toHaveBeenCalledWith(2000);
  });

  it("persistent empty-503 → exhausts the bounded budget and throws", async () => {
    const post = vi.fn().mockRejectedValue(apiError(503, "", "1"));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "C" }, undefined, d)).rejects.toThrow("503");
    expect(post).toHaveBeenCalledTimes(MAX_CREATE_ATTEMPTS);
  });

  it("503 WITH a JSON app-error body → NO retry, surfaces on first try", async () => {
    const post = vi.fn().mockRejectedValue(apiError(503, '{"error":"boom"}'));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "D" }, undefined, d)).rejects.toThrow("boom");
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("502 → NO retry (maybe-processed, non-idempotent), surfaces on first try", async () => {
    const post = vi.fn().mockRejectedValue(apiError(502, ""));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "E" }, undefined, d)).rejects.toThrow("502");
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("504 → NO retry, surfaces on first try", async () => {
    const post = vi.fn().mockRejectedValue(apiError(504, ""));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "F" }, undefined, d)).rejects.toThrow("504");
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("JSON 422 app-error → NO retry, surfaces on first try", async () => {
    const post = vi.fn().mockRejectedValue(apiError(422, '{"error":"RUNTIME_UNSUPPORTED"}'));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "G" }, undefined, d)).rejects.toThrow("422");
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("PlatformUnavailableError (structured 503 JSON body) → NO retry", async () => {
    const post = vi.fn().mockRejectedValue(new PlatformUnavailableError("datastore down"));
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "H" }, undefined, d)).rejects.toBeInstanceOf(
      PlatformUnavailableError,
    );
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("client TimeoutError → NO retry (maybe-processed), surfaces on first try", async () => {
    const post = vi.fn().mockRejectedValue(timeoutError());
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "I" }, undefined, d)).rejects.toThrow(
      "timed out",
    );
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("non-network TypeError (a real client bug) → NO retry, surfaces on first try", async () => {
    // A programmer-error TypeError (e.g. calling a non-function) is NOT a fetch
    // failure. It must surface immediately — retrying it 4× only delays the bug
    // and never succeeds. Regression guard for the over-broad `name==="TypeError"`.
    const bug = new TypeError("obj.doesNotExist is not a function");
    const post = vi.fn().mockRejectedValue(bug);
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    await expect(createWorkspaceWithRetry({ name: "K" }, undefined, d)).rejects.toThrow(
      "is not a function",
    );
    expect(post).toHaveBeenCalledTimes(1);
  });

  it("undici network TypeError (message 'fetch failed' + cause) → retries", async () => {
    // Node/undici rejects a network failure with `TypeError: fetch failed` and
    // attaches the transport error as `.cause` — still a genuine network reset.
    const netErr = new TypeError("fetch failed");
    (netErr as unknown as { cause: unknown }).cause = new Error("ECONNRESET");
    const post = vi
      .fn()
      .mockRejectedValueOnce(netErr)
      .mockResolvedValueOnce({ id: "ws-net" });
    const { deps: d } = deps(post as CreateRetryDeps["post"]);

    const res = await createWorkspaceWithRetry<{ id: string }>({ name: "L" }, undefined, d);

    expect(res).toEqual({ id: "ws-net" });
    expect(post).toHaveBeenCalledTimes(2);
  });

  it("first-try 200 → no retry, single POST", async () => {
    const post = vi.fn().mockResolvedValueOnce({ id: "ws-fast" });
    const { deps: d, sleep } = deps(post as CreateRetryDeps["post"]);

    const res = await createWorkspaceWithRetry<{ id: string }>({ name: "J" }, undefined, d);

    expect(res).toEqual({ id: "ws-fast" });
    expect(post).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });
});
