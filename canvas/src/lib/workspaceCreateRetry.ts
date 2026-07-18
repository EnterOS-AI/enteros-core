import { api, ApiError, PlatformUnavailableError, RequestOptions } from "./api";

// workspaceCreateRetry.ts — the CLIENT-side mirror of the E2E's cold-origin
// `POST /workspaces` retry classifier (tests/e2e/lib/workspace_create_retry.sh).
//
// WHY THIS EXISTS (core#4307 RCA)
// The Cloudflare edge intermittently returns an EMPTY-body HTTP 503
// (content-length:0, retry-after:2, server:cloudflare) on `POST /workspaces`
// ~1s after the tenant health goes green but before the origin accepts writes
// — the cold-origin write window. The E2E runner already survives this via a
// bounded retry classifier; the CTO ruled client-retry the contract so real
// users (not just the E2E) survive the same window. This module holds the
// EXACT same decision table so client and E2E share ONE non-masking rule.
//
// THE NON-MASKING CONTRACT — retry ONLY the "never reached a handler"
// signatures, so a retry can neither mask a real create regression nor
// duplicate a non-idempotent POST:
//   • an EMPTY-body 503 — Service Unavailable synthesised by the edge before a
//     handler is up (the RCA'd cold-origin signature); and
//   • a connection reset / never-established fetch failure (the transport-layer
//     twin of the shell's curl `000`).
// NEVER retry:
//   • a NON-EMPTY body — a real app error (the Go handler ALWAYS emits a body,
//     so emptiness IS the "not the handler" signal). Surface it on the 1st try.
//   • 502 / 504 — bad-gateway / gateway-timeout may mean the origin already
//     PROCESSED the create; re-POSTing this non-idempotent request could
//     double-create. Also a client-side timeout/abort (maybe-processed).
// Attempts are bounded; the origin's Retry-After (integer seconds, default 2,
// cap 10) bounds the backoff.

/** Max create attempts (initial try + up to 3 retries). Mirrors the shell
 *  loop's bounded budget (~4). */
export const MAX_CREATE_ATTEMPTS = 4;

/** Default backoff when Retry-After is absent/non-integer, in seconds.
 *  Matches create_parse_retry_after's fallback. */
export const DEFAULT_RETRY_AFTER_S = 2;

/** Cap on an honored Retry-After (seconds) so a hostile/large value can't
 *  stall the create. Matches create_parse_retry_after's cap. */
export const MAX_RETRY_AFTER_S = 10;

/**
 * The retry decision — an EXACT port of `create_should_retry_cold` from
 * tests/e2e/lib/workspace_create_retry.sh.
 *
 * @param status HTTP status; `null` models the shell's empty status line /
 *               curl `000` (connection reset / never established).
 * @param body   the raw response body; emptiness is the "never reached a
 *               handler" signal.
 * @returns true iff this is a SAFE cold-origin transient to retry.
 */
export function shouldRetryColdCreate(status: number | null, body: string): boolean {
  // A non-empty body is a real response from the app (e.g. a 422/400 JSON
  // error). Never retry it — surface it so the caller names WHY. (Mirrors the
  // shell's `grep -q '[^[:space:]]'` whitespace-tolerant emptiness check.)
  if (body.trim().length > 0) return false;
  if (status === 503) return true; // edge/ingress "Service Unavailable" — handler not up yet
  if (status === null) return true; // connection refused/reset in the cold window (no status line)
  return false; // 502/504 (maybe-processed → non-idempotent) and all else
}

/**
 * Parse an honored Retry-After — an EXACT port of `create_parse_retry_after`.
 * Only a bare integer delta-seconds is honored; an HTTP-date, empty, or junk
 * falls back to the default rather than being mangled. The value is capped.
 *
 * @param raw the raw `Retry-After` header value (or null/undefined).
 * @returns delta-seconds to wait before the next attempt.
 */
export function parseRetryAfter(raw: string | null | undefined): number {
  const trimmed = (raw ?? "").trim();
  if (/^[0-9]+$/.test(trimmed)) {
    const n = parseInt(trimmed, 10);
    return n > MAX_RETRY_AFTER_S ? MAX_RETRY_AFTER_S : n;
  }
  return DEFAULT_RETRY_AFTER_S;
}

/** What the wrapper decides about a caught create error. */
interface CreateErrorDecision {
  retry: boolean;
  /** Raw Retry-After to honor if retrying. */
  retryAfter: string | null;
}

/**
 * Map a thrown create error onto the cold-origin decision, deriving the same
 * raw (status, body) signals the shell classifier sees:
 *  - PlatformUnavailableError → the platform's structured `platform_unavailable`
 *    JSON 503: a NON-empty body → a real error → surface (never retry).
 *  - an ApiError (non-ok HTTP) → classify on its status + bodyText.
 *  - a fetch-level rejection with NO status → it never reached a handler:
 *      · a genuine network failure (name "TypeError": connection refused/reset/
 *        never established) is the transport twin of curl `000` → retry.
 *      · a client-side timeout/abort ("TimeoutError"/"AbortError") is
 *        maybe-processed (like 502/504) → surface, never retry.
 *      · anything else (e.g. the session-expired redirect Error) → surface.
 */
function classifyCreateError(err: unknown): CreateErrorDecision {
  const NO_RETRY: CreateErrorDecision = { retry: false, retryAfter: null };

  // Structured platform-unavailable 503 carries a JSON body → real error.
  if (err instanceof PlatformUnavailableError) return NO_RETRY;

  const e = err as ApiError;
  if (typeof e?.status === "number") {
    return {
      retry: shouldRetryColdCreate(e.status, e.bodyText ?? ""),
      retryAfter: e.retryAfter ?? null,
    };
  }

  // No status → the fetch itself rejected before any response.
  if (isNetworkTypeError(e)) {
    // Browser/undici surface a FAILED FETCH (DNS, refused, reset) as a
    // TypeError — connection reset / never established → cold-origin, retry.
    return { retry: shouldRetryColdCreate(null, ""), retryAfter: null };
  }
  // TimeoutError/AbortError (maybe-processed), a non-network TypeError (a real
  // client bug), and any other throw → surface, never retry.
  return NO_RETRY;
}

/**
 * Is this a genuine fetch/network-failure TypeError (vs an incidental
 * TypeError from a real client bug)? Only a network failure is the transport
 * twin of curl `000` that may be safely retried; a `TypeError` from, e.g.,
 * calling a non-function or reading a property of undefined must NOT be retried
 * 4× — it will never succeed and the retries only delay surfacing the bug.
 *
 * `fetch` rejects a network failure with a `TypeError` whose message is one of
 * a small, cross-runtime set, and Node/undici additionally attaches the
 * underlying transport error as `.cause`. We treat EITHER signal as "network":
 *   - Chrome/Blink:   "Failed to fetch"
 *   - Node/undici:    "fetch failed"           (+ a `.cause`)
 *   - Firefox/Gecko:  "NetworkError when attempting to fetch resource."
 *   - Safari/WebKit:  "Load failed"
 *   - Safari/iOS:     "The Internet connection appears to be offline.",
 *                     "The network connection was lost.",
 *                     "A server with the specified hostname could not be found."
 *
 * The earlier form only matched fetch/network/"load failed", so a genuine
 * offline/DNS TypeError on Safari/iOS (none of which contain those substrings)
 * was mis-classified NO_RETRY — regressing the cold-origin retry this exists for
 * (#4456 code review). Broadened below to the offline/connection/hostname
 * variants; a bare programmer-error TypeError ("x is not a function", "Cannot
 * read properties of undefined") still matches none of these, and even a
 * mis-match is bounded by the 4-attempt cap.
 */
function isNetworkTypeError(e: {
  name?: string;
  message?: string;
  cause?: unknown;
}): boolean {
  if (e?.name !== "TypeError") return false;
  // undici/Node attaches the underlying system/transport error as `cause`; a
  // plain programmer-error TypeError has none.
  if (e.cause != null) return true;
  const msg = (e.message ?? "").toLowerCase();
  return (
    msg.includes("fetch") || // "Failed to fetch" / "fetch failed"
    msg.includes("network") || // "NetworkError…" / "The network connection was lost."
    msg.includes("load failed") || // Safari "Load failed"
    msg.includes("offline") || // Safari/iOS "The Internet connection appears to be offline."
    msg.includes("could not be found") // Safari/iOS DNS "…hostname could not be found."
  );
  // NOTE: deliberately NOT matching the bare token "connection" — it appears in
  // non-network programmer-error TypeErrors too (a field/property named
  // `connection`), which would burn all 4 retries on a real bug. The three
  // offline/DNS messages above are already covered by "network" ("network
  // connection was lost"), "offline", and "could not be found" respectively
  // (#4462 re-review).
}

/** Injectable seams so the bounded loop is unit-testable without real timers
 *  or network. Defaults are the real `api.post` and a setTimeout sleep. */
export interface CreateRetryDeps {
  post: <T>(path: string, body?: unknown, options?: RequestOptions) => Promise<T>;
  sleep: (ms: number) => Promise<void>;
}

const defaultDeps: CreateRetryDeps = {
  post: api.post,
  sleep: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
};

/**
 * `POST /workspaces` with the bounded cold-origin retry. This is the ONLY
 * create seam wrapped — every real-user create path (EmptyState, MobileSpawn,
 * useTemplateDeploy, CreateWorkspaceDialog) routes through here so the
 * non-masking rule holds uniformly. Non-cold failures (real app errors,
 * 502/504, timeouts) surface immediately, exactly as before this change.
 */
export async function createWorkspaceWithRetry<T>(
  body: unknown,
  options?: RequestOptions,
  deps: CreateRetryDeps = defaultDeps,
): Promise<T> {
  let lastErr: unknown;
  for (let attempt = 0; attempt < MAX_CREATE_ATTEMPTS; attempt++) {
    try {
      // Forward `options` only when supplied so the underlying call is
      // byte-identical to the pre-wrapper `api.post("/workspaces", body)`
      // (no trailing `undefined` positional) — keeps the create contract,
      // and existing exact-args tests, unchanged.
      return options === undefined
        ? await deps.post<T>("/workspaces", body)
        : await deps.post<T>("/workspaces", body, options);
    } catch (err) {
      lastErr = err;
      const decision = classifyCreateError(err);
      // Not a cold transient, or the budget is spent → surface the real error.
      if (!decision.retry || attempt === MAX_CREATE_ATTEMPTS - 1) throw err;
      await deps.sleep(parseRetryAfter(decision.retryAfter) * 1000);
    }
  }
  // Unreachable (the loop either returns or throws), but satisfies the type.
  throw lastErr;
}
