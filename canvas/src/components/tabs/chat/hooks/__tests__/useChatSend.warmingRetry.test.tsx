// @vitest-environment jsdom
//
// core#3082 first-boot UX — a message sent while the concierge is still
// WARMING gets a structured 503 {"warming":true} + Retry-After from the A2A
// proxy: the turn was DEFERRED, not failed. Pre-fix the catch-all treated it
// as transport failure ("Failed to send message — agent may be unreachable",
// red banner) seconds before the agent came online — the exact 2026-07-18
// fresh-onboarding screenshot. Post-fix: the send auto-retries itself on the
// server's Retry-After cadence with the thinking state kept up, delivering
// the message the moment the boot finishes; only an exhausted retry budget
// surfaces a calm "still booting" error.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

const apiPostMock = vi.fn<
  (url: string, body?: unknown, opts?: unknown) => Promise<unknown>
>();
vi.mock("@/lib/api", () => ({
  api: {
    post: (url: string, body?: unknown, opts?: unknown) =>
      apiPostMock(url, body, opts),
    get: vi.fn(),
  },
}));
vi.mock("../../uploads", () => ({
  uploadChatFiles: vi.fn(),
  FileTooLargeError: class FileTooLargeError extends Error {},
}));

import { useChatSend } from "../useChatSend";

// The ApiError shape lib/api.ts attaches on a non-ok response.
const warming503 = () => {
  const e = new Error(
    'API POST /workspaces/ws-warm/a2a: 503 {"error":"concierge is warming up","warming":true,"retry_after":5}',
  ) as Error & { status: number; bodyText: string; retryAfter: string | null };
  e.status = 503;
  e.bodyText = '{"error":"concierge is warming up","warming":true,"retry_after":5}';
  e.retryAfter = "5";
  return e;
};

beforeEach(() => {
  vi.useFakeTimers();
  apiPostMock.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("useChatSend — warming 503 auto-retry", () => {
  it("retries on the Retry-After cadence and delivers once the agent is online", async () => {
    // First attempt: warming 503. Second attempt (after 5s): delivered.
    apiPostMock
      .mockRejectedValueOnce(warming503())
      .mockResolvedValueOnce({ result: { parts: [{ kind: "text", text: "hello!" }] } });

    const { result } = renderHook(() => useChatSend("ws-warm", {}));

    await act(async () => {
      await result.current.sendMessage("hi");
      await Promise.resolve();
    });

    // Deferred, not failed: no banner, thinking stays up.
    expect(result.current.error).toBeNull();
    expect(result.current.sending).toBe(true);
    expect(apiPostMock).toHaveBeenCalledTimes(1);

    // Advance past the Retry-After hint — the retry fires and succeeds.
    await act(async () => {
      vi.advanceTimersByTime(5_000);
      await Promise.resolve();
    });
    expect(apiPostMock).toHaveBeenCalledTimes(2);
    expect(result.current.error).toBeNull();
    expect(result.current.sending).toBe(false); // turn completed
  });

  it("surfaces a calm 'still booting' error when the retry budget is exhausted", async () => {
    apiPostMock.mockImplementation(() => Promise.reject(warming503()));

    const { result } = renderHook(() => useChatSend("ws-warm", {}));

    await act(async () => {
      await result.current.sendMessage("hi");
      await Promise.resolve();
    });

    // Burn through the whole budget (24 retries × 5s).
    for (let i = 0; i < 24; i++) {
      await act(async () => {
        vi.advanceTimersByTime(5_000);
        await Promise.resolve();
      });
    }

    expect(apiPostMock).toHaveBeenCalledTimes(25); // initial + 24 retries
    expect(result.current.error).toMatch(/still booting/);
    expect(result.current.error).not.toMatch(/unreachable/);
    expect(result.current.sending).toBe(false);
  });

  it("a plain 503 without the warming shape still fails as unreachable", async () => {
    const plain = new Error("API POST: 503 upstream dead") as Error & {
      status: number;
      bodyText: string;
    };
    plain.status = 503;
    plain.bodyText = '{"error":"upstream dead"}';
    apiPostMock.mockRejectedValueOnce(plain);

    const { result } = renderHook(() => useChatSend("ws-dead", {}));

    await act(async () => {
      await result.current.sendMessage("hello?");
      await Promise.resolve();
    });

    expect(result.current.error).toMatch(/unreachable/);
    expect(result.current.sending).toBe(false);
  });
});
