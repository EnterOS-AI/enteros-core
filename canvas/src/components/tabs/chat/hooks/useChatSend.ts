"use client";

import { useCallback, useRef, useState } from "react";
import { api } from "@/lib/api";
import { uploadChatFiles, FileTooLargeError } from "../uploads";
import { createMessage, type ChatMessage, type ChatAttachment } from "../types";
import { extractFilesFromTask } from "../message-parser";

interface A2APart {
  kind?: string;
  /** A2A v0.2 used `type` as the Part discriminator; v0.3 uses `kind`.
   *  Real runtimes and third-party agents still emit both, so accept
   *  either when `kind` is absent. */
  type?: string;
  text?: string;
  file?: {
    name?: string;
    mimeType?: string;
    uri?: string;
    size?: number;
  };
}

interface A2AResponse {
  result?: {
    parts?: A2APart[];
    artifacts?: Array<{ parts: A2APart[] }>;
    /** Standard A2A Task status; real runtimes place the agent's final
     *  reply inside status.message.parts. */
    status?: { message?: { parts?: A2APart[] } };
  };
  /** Set by ws-server's poll-mode short-circuit in `proxyA2ARequest`
   *  (a2a_proxy.go:416-431) when the target workspace is registered as
   *  `delivery_mode=poll` — e.g. an operator's laptop running
   *  `molecule-mcp-claude-channel`, a hermes/codex MCP bridge, or a
   *  Cursor MCP client. The HTTP 200 carries the synthetic envelope
   *  `{status:"queued", delivery_mode:"poll", method:"message/send"}`
   *  immediately (~50ms), BEFORE the agent has produced a reply.
   *
   *  Task #227 routing: when this field is "queued" the caller must NOT
   *  treat the 200 as "agent done" — there are no `result.parts` yet
   *  (the reply will arrive separately via the AGENT_MESSAGE WS event
   *  after the agent's next poll). Keep the spinner up; the eventual
   *  AGENT_MESSAGE flips `sending` off via the existing useChatSocket
   *  `onSendComplete` path. Without this distinction the spinner
   *  disappeared immediately and external/MCP workspaces had no progress
   *  UX between send and reply. */
  status?: string;
  /** Companion to `status` — "poll" when the queued short-circuit fired.
   *  Defensive: we key the poll-mode-skip decision on status==="queued"
   *  (the canonical signal) rather than on this field, but it's surfaced
   *  here so future debugging / tests can assert on the full envelope. */
  delivery_mode?: string;
}

export function extractReplyText(resp: A2AResponse): string {
  const collect = (parts: A2APart[] | undefined): string => {
    if (!parts) return "";
    return parts
      .filter((p) => p.kind === "text" || p.type === "text")
      .map((p) => p.text ?? "")
      .filter(Boolean)
      .join("\n");
  };
  const result = resp?.result;
  const collected: string[] = [];

  // Standard A2A JSON-RPC: {result: {parts: [{kind: "text", text: "..."}]}}
  const fromParts = collect(result?.parts);
  if (fromParts) collected.push(fromParts);

  // Standard A2A Task shape: the agent's reply can live in
  // result.status.message.parts rather than directly on result.parts.
  // Real runtimes (Claude Code, Hermes, etc.) commonly return this shape.
  const fromStatusMessage = collect(result?.status?.message?.parts);
  if (fromStatusMessage) collected.push(fromStatusMessage);

  if (result?.artifacts) {
    for (const a of result.artifacts) {
      const t = collect(a.parts);
      if (t) collected.push(t);
    }
  }
  return collected.join("\n");
}

/** Map a thrown error from `uploadChatFiles` to the user-facing reason
 *  shown in the chat error banner.
 *
 *  Cases (per `feedback_surface_actionable_failure_reason_to_user` —
 *  user-facing failures MUST tell the user WHY):
 *
 *    1. FileTooLargeError → use the error's message verbatim. The
 *       pre-flight already built the actionable string with the actual
 *       size + the cap; don't re-wrap it (which would prepend a
 *       redundant "Upload failed:" prefix).
 *
 *    2. DOMException name="TimeoutError" → AbortSignal.timeout fired
 *       during the fetch. Pre-flight already excluded file-size, so
 *       this CANNOT mean "file too large". Surface a connection-speed
 *       message — the user's actionable next step is retry or check
 *       network, NOT shrink the file.
 *
 *    3. Other Error → use the wrapped form so the server's reason
 *       (e.g. "upload failed: 413 ...") reaches the user instead of
 *       being swallowed.
 *
 *    4. Non-Error throw → generic fallback.
 *
 *  Exported for unit testing — the case-by-case mapping is the
 *  load-bearing contract this PR ships. */
export function mapUploadErrorToReason(e: unknown): string {
  if (e instanceof FileTooLargeError) {
    // Already a complete, user-facing sentence — surface verbatim.
    return e.message;
  }
  // DOMException with name="TimeoutError" is what AbortSignal.timeout
  // produces on abort. Browsers represent it as a DOMException, not a
  // regular Error subclass — feature-detect via .name to avoid coupling
  // to a global that's missing in test envs.
  if (
    e !== null && typeof e === "object" &&
    "name" in e && (e as { name: unknown }).name === "TimeoutError"
  ) {
    return "Upload timed out — your connection is too slow for this file. Try again, or reduce file size.";
  }
  if (e instanceof Error) {
    return `Upload failed: ${e.message}`;
  }
  return "Upload failed";
}

export interface UseChatSendOptions {
  getHistoryMessages: () => ChatMessage[];
  onUserMessage?: (msg: ChatMessage) => void;
  onAgentMessage?: (msg: ChatMessage) => void;
}

export function useChatSend(workspaceId: string, options: UseChatSendOptions) {
  const [sending, setSending] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const sendInFlightRef = useRef(false);
  const sendingFromAPIRef = useRef(false);
  const sendTokenRef = useRef(0);
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const releaseSendGuards = useCallback(() => {
    setSending(false);
    sendingFromAPIRef.current = false;
    sendInFlightRef.current = false;
  }, []);

  const clearError = useCallback(() => setError(null), []);

  const sendMessage = useCallback(
    async (text: string, files: File[] = []) => {
      const trimmed = text.trim();
      // Multi-send (core#2697 feature 2): do NOT block on `sending`. A user
      // must be able to fire a follow-up while a prior message's agent reply
      // is still pending — the server-side A2A queue is durable and orders
      // them. `sendInFlightRef` is now only a brief re-entrancy guard for the
      // synchronous setup (file upload + optimistic add + POST dispatch); it
      // is released the moment the POST is FIRED (below), NOT held across the
      // multi-minute reply wait, so the next send proceeds immediately.
      if ((!trimmed && files.length === 0) || uploading) return;
      if (sendInFlightRef.current) return;
      sendInFlightRef.current = true;

      let uploaded: ChatAttachment[] = [];
      if (files.length > 0) {
        setUploading(true);
        try {
          uploaded = await uploadChatFiles(workspaceId, files);
        } catch (e) {
          setUploading(false);
          sendInFlightRef.current = false;
          // Error-reason routing (CTO 2026-05-19 on forensic a99ab0a1:
          // "if its file size issue, should have error that instead
          // saying timeout which is wrong"). Each cause maps to ITS
          // OWN message — NO conflation between file-size and
          // connection-too-slow.
          setError(mapUploadErrorToReason(e));
          return;
        }
        setUploading(false);
      }

      // One id, threaded through both the optimistic bubble and the A2A
      // payload's messageId, so the server's USER_MESSAGE broadcast echo
      // dedups against this bubble on the origin device (core#2697 —
      // otherwise the sender saw its own message twice).
      const messageId = crypto.randomUUID();
      const userMsg = createMessage("user", trimmed, uploaded, undefined, messageId);
      optionsRef.current.onUserMessage?.(userMsg);

      setSending(true);
      sendingFromAPIRef.current = true;
      setError(null);
      const myToken = ++sendTokenRef.current;

      const history = optionsRef.current
        .getHistoryMessages()
        .filter((m) => m.role === "user" || m.role === "agent")
        .slice(-20)
        .map((m) => ({
          role: m.role === "user" ? "user" : "agent",
          parts: [{ kind: "text", text: m.content }],
        }));

      const parts: A2APart[] = [];
      if (trimmed) parts.push({ kind: "text", text: trimmed });
      for (const att of uploaded) {
        parts.push({
          kind: "file",
          file: {
            name: att.name,
            mimeType: att.mimeType,
            uri: att.uri,
            size: att.size,
          },
        });
      }

      api
        .post<A2AResponse>(
          `/workspaces/${workspaceId}/a2a`,
          {
            method: "message/send",
            params: {
              message: {
                role: "user",
                messageId,
                parts,
              },
              metadata: { history },
            },
          },
          // 30min — must exceed the server-side canvas idle watchdog
          // (workspace-server/internal/handlers/a2a_proxy.go:1002 const
          // defaultIdleTimeoutDuration = 30 * time.Minute, core#2723 / #2727
          // raise). Pre-#2727 the server-side default was 5min; the 120s
          // client-side timeout was BELOW the server idle and the
          // isClientTimeout handler at useChatSend.ts:261-280 silently
          // swallowed it (delivered; reply (and guard release) arrives via
          // the AGENT_MESSAGE WS event). The 5min server raise moved the
          // window to 5min, but a 300s+ chain still died client-side at
          // 120s — half-fixed (#2723 ship didn't include the client
          // alignment). The 30min server raise in #2727 makes a 30min
          // client timeout safe, and the same isClientTimeout swallow
          // path means a client timeout still doesn't surface "agent may
          // be unreachable" — it just falls through to the WS-event
          // delivery.
          { timeoutMs: 30 * 60 * 1000 },
        )
        .then((resp) => {
          if (sendTokenRef.current !== myToken) return;
          // Always process a synchronous push-mode 200 response. An earlier
          // early-return on !sendingFromAPIRef.current dropped agent replies
          // that arrived before the WebSocket path released the guards,
          // leaving the user message visible with no Echo/reply bubble.
          // releaseSendGuards is idempotent, and ChatTab deduplicates
          // appendMessageDeduped within a short window, so processing here is
          // safe even if a WS event already handled the reply.
          //
          // Task #227 — poll-mode (external/MCP workspace) queued-200
          // short-circuit. ws-server's `proxyA2ARequest` returns
          // `{status:"queued", delivery_mode:"poll", ...}` immediately
          // when the target has no URL (delivery_mode=poll), BEFORE the
          // agent has produced any reply. There is no `result.parts`
          // payload here — the actual reply will arrive separately via
          // the AGENT_MESSAGE WebSocket event after the agent's next
          // `wait_for_message` poll.
          //
          // Keep the spinner up by deliberately NOT calling
          // releaseSendGuards: the user-facing "thinking" state must
          // persist until the AGENT_MESSAGE lands (handled by the
          // useChatSocket `onAgentMessage`/`onSendComplete` path) or an
          // explicit error fires (`onSendError` from an ACTIVITY_LOGGED
          // status="error"). Don't synthesise an empty agent bubble.
          //
          // sendInFlightRef stays true intentionally — it's the dedup
          // guard for the user typing two messages back-to-back; for
          // poll mode the second message would race the first agent's
          // reply, so blocking is correct (matches push-mode behaviour
          // where `sending` blocks the textarea).
          if (resp?.status === "queued") {
            return;
          }
          const replyText = extractReplyText(resp);
          const replyFiles = extractFilesFromTask(
            (resp?.result ?? {}) as Record<string, unknown>,
          );
          if (replyText || replyFiles.length > 0) {
            optionsRef.current.onAgentMessage?.(
              createMessage("agent", replyText, replyFiles),
            );
          }
          releaseSendGuards();
        })
        .catch((e: unknown) => {
          if (sendTokenRef.current !== myToken) return;
          if (!sendingFromAPIRef.current) {
            sendInFlightRef.current = false;
            return;
          }
          // CLIENT TIMEOUT ≠ UNREACHABLE (jrs-auto, 2026-06-09). The A2A
          // proxy holds this POST open for the agent's WHOLE turn; a long
          // tool-calling turn routinely outlives the 120s client budget.
          // AbortSignal.timeout firing after the server ACCEPTED and held
          // the connection means the message was DELIVERED and the agent is
          // still working — showing "agent may be unreachable" here is a
          // false alarm (the user watches the agent run tools in the
          // activity feed while the chat claims failure). Keep the thinking
          // state up; the reply lands via the AGENT_MESSAGE WebSocket event,
          // which releases the guards — exactly the documented poll-mode
          // contract above. Genuine unreachability fails FAST (connection
          // refused / 4xx / 5xx) and still takes the error branch; a truly
          // dead agent is surfaced by the reactive-health path
          // (maybeMarkContainerDead), not by this client timeout.
          const isClientTimeout =
            e !== null && typeof e === "object" &&
            "name" in e && (e as { name: unknown }).name === "TimeoutError";
          // CLOUDFLARE EDGE TIMEOUT ≠ UNREACHABLE (jrs-auto, 2026-06-13). The
          // canvas→agent A2A POST is held open for the whole turn; a turn that
          // runs longer than Cloudflare's ~100s edge limit gets a 524 (also
          // 522/504) from CF — NOT from a dead agent. The agent is still
          // working (visible in the activity feed) and its reply arrives via
          // the AGENT_MESSAGE WebSocket event, exactly like the client-timeout
          // case above. Treat these gateway timeouts the same: keep the
          // thinking state, don't flash "agent may be unreachable".
          const status = (e as { status?: number } | null)?.status;
          const isGatewayTimeout = status === 524 || status === 522 || status === 504;
          if (isClientTimeout || isGatewayTimeout) {
            return; // delivered; reply (and guard release) arrives via WS
          }
          releaseSendGuards();
          setError("Failed to send message — agent may be unreachable");
        });

      // The POST is now in flight (the .then/.catch above run later, off the
      // microtask queue). Release the re-entrancy guard immediately so the
      // user can fire the NEXT message while this one's reply is still
      // pending — true multi-send. `sending` stays true to keep the thinking
      // indicator up; it no longer gates new sends.
      sendInFlightRef.current = false;
    },
    [workspaceId, uploading],
  );

  return {
    sending,
    uploading,
    sendMessage,
    error,
    clearError,
    releaseSendGuards,
    sendingFromAPIRef,
  };
}
