// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import { useCanvasStore } from "@/store/canvas";
import { handleCanvasEvent } from "@/store/canvas-events";
import type { WSMessage } from "@/store/socket";
import { useChatSend } from "../useChatSend";
import { useChatSocket } from "../useChatSocket";

const post = vi.hoisted(() => vi.fn<(path: string, body?: unknown) => Promise<unknown>>());
vi.mock("@/lib/api", () => ({ api: { post, get: vi.fn() } }));
let socketHandler: (msg: WSMessage) => void;
vi.mock("@/hooks/useSocketEvent", () => ({
  useSocketEvent: (handler: typeof socketHandler) => { socketHandler = handler; },
}));

beforeEach(() => {
  post.mockReset();
  useCanvasStore.setState({ nodes: [], agentMessages: {}, agentMessageConsumers: {} });
});
afterEach(cleanup);

function setup() {
  const onAgentMessage = vi.fn();
  const onUserMessage = vi.fn();
  const hook = renderHook(() => {
    const send = useChatSend("ws", { onAgentMessage, onUserMessage });
    useChatSocket("ws", {
      onAgentMessage,
      onSendComplete: send.releaseSendGuards,
      onSendError: send.failSend,
    });
    return send;
  });
  return { ...hook, onAgentMessage, messageId: () => onUserMessage.mock.calls.at(-1)![0].id as string };
}

function emit(event: string, payload: WSMessage["payload"], workspace = "ws") {
  const msg: WSMessage = { event, workspace_id: workspace, timestamp: new Date().toISOString(), payload };
  act(() => {
    handleCanvasEvent(msg, useCanvasStore.getState, useCanvasStore.setState);
    socketHandler(msg);
  });
}

describe("chat delivery across HTTP and WebSocket", () => {
  it("does not let transport success hide a later HTTP error", async () => {
    let resolve!: (body: unknown) => void;
    post.mockReturnValue(new Promise((done) => { resolve = done; }));
    const { result, messageId } = setup();
    await act(async () => { await result.current.sendMessage("hi"); });
    emit("ACTIVITY_LOGGED", { activity_type: "a2a_receive", method: "message/send", status: "ok", duration_ms: 50, message_id: messageId() });
    expect(result.current.sending).toBe(true);
    await act(async () => { resolve({ error: { message: "Provider unavailable" } }); });
    expect(result.current.error).toBe("Provider unavailable");
    expect(result.current.sending).toBe(false);
  });

  it.each([
    [{ error: { message: "Provider unavailable" } }, "Provider unavailable"],
    [{ result: { status: { state: "failed", message: { parts: [{ kind: "text", text: "Model unavailable" }] } } } }, "Model unavailable"],
    [{ result: { status: { state: "completed" } } }, "returned no reply"],
  ])("shows a queued send's socket failure without adding a successful reply (%j)", async (body, error) => {
    post.mockResolvedValue({ status: "queued" });
    const { result, messageId, onAgentMessage } = setup();
    await act(async () => { await result.current.sendMessage("hi"); });
    emit("ACTIVITY_LOGGED", { activity_type: "a2a_receive", method: "message/send", status: "ok", duration_ms: 50, message_id: messageId() });
    emit("A2A_RESPONSE", { method: "message/send", message_id: messageId(), response_body: body });
    expect(result.current.error).toContain(error);
    expect(result.current.sending).toBe(false);
    expect(onAgentMessage).not.toHaveBeenCalled();
  });

  it("keeps a socket error when the queued HTTP acknowledgement arrives late", async () => {
    let resolve!: (body: unknown) => void;
    post.mockReturnValue(new Promise((done) => { resolve = done; }));
    const { result, messageId } = setup();
    await act(async () => { await result.current.sendMessage("hi"); });
    emit("ACTIVITY_LOGGED", { activity_type: "a2a_receive", method: "message/send", status: "error", message_id: messageId(), error_detail: "Provider 503" });
    await act(async () => { resolve({ status: "queued" }); });
    expect(result.current.error).toBe("Provider 503");
    expect(result.current.sending).toBe(false);
  });

  it("delivers a standard Task status.message reply over the socket", async () => {
    post.mockResolvedValue({ status: "queued" });
    const { result, messageId, onAgentMessage } = setup();
    await act(async () => { await result.current.sendMessage("hi"); });
    emit("A2A_RESPONSE", { message_id: messageId(), response_body: { result: { status: { state: "completed", message: { parts: [{ kind: "text", text: "Hello" }] } } } } });
    expect(onAgentMessage).toHaveBeenCalledOnce();
    expect(onAgentMessage.mock.calls[0][0].content).toBe("Hello");
    expect(result.current.sending).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("an error for one concurrent send leaves the other pending", async () => {
    post.mockResolvedValue({ status: "queued" });
    const { result, messageId } = setup();
    await act(async () => { await result.current.sendMessage("first"); });
    const first = messageId();
    await act(async () => { await result.current.sendMessage("second"); });
    emit("A2A_RESPONSE", { message_id: first, response_body: { error: { message: "First failed" } } });
    expect(result.current.error).toBe("First failed");
    expect(result.current.sending).toBe(true);
    act(() => result.current.releaseSendGuards(messageId()));
    expect(result.current.sending).toBe(false);
  });
});
