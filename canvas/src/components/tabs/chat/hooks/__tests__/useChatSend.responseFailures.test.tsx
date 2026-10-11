// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { useChatSend } from "../useChatSend";

const post = vi.hoisted(() => vi.fn<(path: string, body?: unknown) => Promise<unknown>>());
vi.mock("@/lib/api", () => ({ api: { post, get: vi.fn() } }));
vi.mock("../../uploads", () => ({
  uploadChatFiles: vi.fn(),
  FileTooLargeError: class FileTooLargeError extends Error {},
}));
beforeEach(() => post.mockReset());
afterEach(cleanup);

describe("useChatSend response failures", () => {
  it.each([
    [{ error: "agent returned empty response body" }, "agent returned empty response body"],
    [{ error: { code: -32603, message: "Provider is unavailable" } }, "Provider is unavailable"],
    [{ result: { status: { state: "failed", message: { parts: [{ kind: "text", text: "Model is unavailable" }] } } } }, "Model is unavailable"],
    [{ result: { status: { state: "rejected" } } }, "could not complete"],
    [{ result: { status: { state: "canceled" } } }, "canceled"],
    [{ result: { status: { state: "completed" } } }, "returned no reply"],
    [{}, "returned no reply"],
  ])("shows an error for an unsuccessful HTTP 200 response (%j)", async (response, message) => {
    post.mockResolvedValue(response);
    const onAgentMessage = vi.fn();
    const { result } = renderHook(() => useChatSend("response-errors", { onAgentMessage }));
    await act(async () => { await result.current.sendMessage("hello"); });
    expect(result.current.sending).toBe(false);
    expect(result.current.error).toContain(message);
    expect(onAgentMessage).not.toHaveBeenCalled();
  });

  it.each(["submitted", "working"])("keeps %s tasks pending until socket completion", async (state) => {
    post.mockResolvedValue({ result: { status: { state } } });
    const { result } = renderHook(() => useChatSend("response-pending", {}));
    await act(async () => { await result.current.sendMessage("hello"); });
    expect(result.current.sending).toBe(true);
    expect(result.current.error).toBeNull();
    act(() => result.current.releaseSendGuards());
    expect(result.current.sending).toBe(false);
  });

  it.each([{}, { error: { message: "Late error" } }])("does not undo a socket completion with a late response (%j)", async (response) => {
    let resolve!: (value: unknown) => void;
    post.mockReturnValue(new Promise((done) => { resolve = done; }));
    const { result } = renderHook(() => useChatSend("response-race", {}));
    await act(async () => { await result.current.sendMessage("hello"); });
    act(() => result.current.releaseSendGuards());
    await act(async () => { resolve(response); });
    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });
});
