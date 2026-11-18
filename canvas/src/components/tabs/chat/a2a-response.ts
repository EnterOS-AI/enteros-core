import { extractFilesFromTask, extractResponseText } from "./message-parser";

export interface A2APart {
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

export interface A2AResponse {
  error?: { code?: number; message?: string } | string;
  result?: {
    parts?: A2APart[];
    artifacts?: Array<{ parts: A2APart[] }>;
    /** Standard A2A Task status; real runtimes place the agent's final
     *  reply inside status.message.parts. */
    status?: { state?: string; message?: { parts?: A2APart[] } };
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
    if (!Array.isArray(parts)) return "";
    return parts
      .filter((p) => p && (p.kind === "text" || p.type === "text"))
      .map((p) => typeof p.text === "string" ? p.text : "")
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

  if (Array.isArray(result?.artifacts)) {
    for (const a of result.artifacts) {
      const t = collect(a?.parts);
      if (t) collected.push(t);
    }
  }
  return collected.join("\n");
}

/** Apply the same completion rules to HTTP replies and WebSocket deliveries. */
export function parseA2AResponse(raw: unknown) {
  const resp: A2AResponse = raw && typeof raw === "object" ? raw : {};
  const state = resp.result?.status?.state;
  const text = extractReplyText(resp) || extractResponseText(resp as Record<string, unknown>);
  const attachments = extractFilesFromTask((resp.result ?? resp) as Record<string, unknown>);
  const failed = !!resp.error || state === "failed" || state === "rejected" || state === "canceled";
  const pending = !failed && (resp.status === "queued" || state === "submitted" || state === "working");
  const rpcError = typeof resp.error === "string" ? resp.error : resp.error?.message;
  const error = failed
    ? (typeof rpcError === "string" && rpcError.trim()) || text.trim() || (state === "canceled" ? "The agent canceled this request." : "The agent could not complete this request.")
    : !pending && !text.trim() && attachments.length === 0
      ? "The agent returned no reply. Check the activity log for details."
      : null;
  return { pending, error, text, attachments };
}
