// @vitest-environment jsdom
//
// core#2697 feature 2 — multi-send. The user MUST be able to fire a
// follow-up message while a prior message's agent reply is still pending
// (the original ship blocked this: sendMessage early-returned on
// `|| sending`, and held sendInFlightRef across the whole reply wait, so
// the 2nd message was silently dropped). The fix removes the `sending`
// gate and releases the re-entrancy guard the moment the POST is FIRED.
//
// These tests pin: (1) a 2nd send while the 1st is in flight goes through
// (user bubble + a 2nd POST); (2) the single-keystroke re-entrancy guard
// still prevents a literal double-fire within the same synchronous tick.

import { describe, it, expect, vi, beforeEach } from "vitest";
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

beforeEach(() => {
  apiPostMock.mockReset();
});

describe("useChatSend — multi-send (core#2697 feature 2)", () => {
  it("allows a SECOND send while the FIRST reply is still pending", async () => {
    // Both POSTs hang (agent hasn't replied) — simulates the real
    // "reply pending" window during which the old code dropped send #2.
    apiPostMock.mockImplementation(() => new Promise(() => {}));

    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );

    await act(async () => {
      await result.current.sendMessage("first message");
      await Promise.resolve(); // let the dispatch lock release post-fire
    });
    // After #1 fires, `sending` is true (reply pending) — the old gate.
    expect(result.current.sending).toBe(true);

    await act(async () => {
      await result.current.sendMessage("second message while first pending");
      await Promise.resolve();
    });

    // BOTH user bubbles rendered + BOTH POSTs dispatched — the 2nd was
    // NOT dropped despite `sending` being true.
    expect(onUserMessage).toHaveBeenCalledTimes(2);
    expect(apiPostMock).toHaveBeenCalledTimes(2);
    // Each send carries its own unique messageId.
    const id1 = (apiPostMock.mock.calls[0][1] as any).params.message.messageId;
    const id2 = (apiPostMock.mock.calls[1][1] as any).params.message.messageId;
    expect(id1).not.toBe(id2);
  });

  it("does not block on `sending` (an empty/whitespace send is still a no-op)", async () => {
    apiPostMock.mockImplementation(() => new Promise(() => {}));
    const onUserMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [], onUserMessage }),
    );
    await act(async () => {
      await result.current.sendMessage("   ");
      await Promise.resolve();
    });
    expect(onUserMessage).not.toHaveBeenCalled();
    expect(apiPostMock).not.toHaveBeenCalled();
  });
});
