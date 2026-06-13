// @vitest-environment jsdom
//
// core#2725 — concurrent sends must not drop, misroute, or starve the
// thinking indicator. The hook now tracks every in-flight request by a
// unique token (Set). Replies are processed only when their token is still
// in the set, and `sending` stays true while ANY token is pending.

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

function deferred<T = unknown>() {
  let resolve: (value: T) => void = () => {};
  let reject: (reason?: unknown) => void = () => {};
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("useChatSend — concurrent replies (core#2725)", () => {
  it("routes each HTTP reply to the correct send when replies resolve out of order", async () => {
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const onAgentMessage = vi.fn();
    const { result } = renderHook(() =>
      useChatSend("ws-1", {
        getHistoryMessages: () => [],
        onAgentMessage,
      }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve(); // let the setup guard release
      result.current.sendMessage("second");
      await Promise.resolve();
    });

    // Resolve the SECOND send before the first.
    await act(async () => {
      second.resolve({ result: { parts: [{ kind: "text", text: "reply-two" }] } });
      await Promise.resolve();
    });

    // Resolve the FIRST send afterwards.
    await act(async () => {
      first.resolve({ result: { parts: [{ kind: "text", text: "reply-one" }] } });
      await Promise.resolve();
    });

    expect(onAgentMessage).toHaveBeenCalledTimes(2);
    const contents = onAgentMessage.mock.calls.map(
      (c) => (c[0] as { content: string }).content,
    );
    expect(contents).toContain("reply-one");
    expect(contents).toContain("reply-two");
  });

  it("keeps the thinking indicator active until the LAST pending send completes", async () => {
    const first = deferred();
    const second = deferred();
    apiPostMock
      .mockImplementationOnce(() => first.promise)
      .mockImplementationOnce(() => second.promise);

    const { result } = renderHook(() =>
      useChatSend("ws-1", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("first");
      await Promise.resolve(); // let the setup guard release
      result.current.sendMessage("second");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    await act(async () => {
      first.resolve({ result: { parts: [{ kind: "text", text: "reply-one" }] } });
      await Promise.resolve();
    });
    // Send #2 is still pending → spinner stays up.
    expect(result.current.sending).toBe(true);

    await act(async () => {
      second.resolve({ result: { parts: [{ kind: "text", text: "reply-two" }] } });
      await Promise.resolve();
    });
    // Last send done → spinner off.
    expect(result.current.sending).toBe(false);
  });

  it("cleans up the poll-mode token when releaseSendGuards is called", async () => {
    apiPostMock.mockResolvedValue({
      status: "queued",
      delivery_mode: "poll",
      method: "message/send",
    });

    const { result } = renderHook(() =>
      useChatSend("ws-poll", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      await result.current.sendMessage("poll me");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // The AGENT_MESSAGE / onSendComplete WS event arrives.
    act(() => {
      result.current.releaseSendGuards();
    });

    expect(result.current.sending).toBe(false);
  });

  it("keeps the spinner up when releaseSendGuards prunes one poll-mode send but another is still in flight", async () => {
    const pushSend = deferred();
    apiPostMock
      .mockImplementationOnce(() => pushSend.promise)
      .mockResolvedValueOnce({
        status: "queued",
        delivery_mode: "poll",
        method: "message/send",
      });

    const { result } = renderHook(() =>
      useChatSend("ws-mixed", { getHistoryMessages: () => [] }),
    );

    await act(async () => {
      result.current.sendMessage("push-mode send");
      result.current.sendMessage("poll-mode send");
      await Promise.resolve();
    });
    expect(result.current.sending).toBe(true);

    // Poll-mode send completes via WS; push-mode send is still HTTP pending.
    act(() => {
      result.current.releaseSendGuards();
    });

    expect(result.current.sending).toBe(true);

    await act(async () => {
      pushSend.resolve({ result: { parts: [{ kind: "text", text: "push reply" }] } });
      await Promise.resolve();
    });

    expect(result.current.sending).toBe(false);
  });
});
