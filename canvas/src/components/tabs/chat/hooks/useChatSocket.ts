"use client";

import { useCallback, useEffect, useRef } from "react";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { createMessage, type ChatMessage } from "../types";

export interface UseChatSocketCallbacks {
  onAgentMessage?: (msg: ChatMessage) => void;
  onActivityLog?: (entry: string) => void;
  onSendComplete?: () => void;
  onSendError?: (error: string) => void;
  /** A request the user (or an agent) responded to — drives the live
   *  decision chip in My Chat (core#2636). */
  onRequestResponded?: (p: {
    status: string;
    responderType: string;
    title: string;
    kind: string;
  }) => void;
}

export function useChatSocket(
  workspaceId: string,
  callbacks: UseChatSocketCallbacks,
): void {
  const callbacksRef = useRef(callbacks);
  callbacksRef.current = callbacks;

  // Agent push messages from global store
  const pendingAgentMsgs = useCanvasStore((s) => s.agentMessages[workspaceId]);
  useEffect(() => {
    if (!pendingAgentMsgs || pendingAgentMsgs.length === 0) return;
    const consume = useCanvasStore.getState().consumeAgentMessages;
    const msgs = consume(workspaceId);
    for (const m of msgs) {
      callbacksRef.current.onAgentMessage?.(
        createMessage("agent", m.content, m.attachments),
      );
    }
    if (msgs.length > 0) {
      callbacksRef.current.onSendComplete?.();
    }
  }, [pendingAgentMsgs, workspaceId]);

  const resolveWorkspaceName = useCallback((id: string) => {
    const nodes = useCanvasStore.getState().nodes;
    const node = nodes.find((n) => n.id === id);
    return (node?.data as WorkspaceNodeData)?.name || id.slice(0, 8);
  }, []);

  useSocketEvent((msg) => {
    try {
      if (msg.event === "ACTIVITY_LOGGED") {
        if (msg.workspace_id !== workspaceId) return;

        const p = msg.payload || {};
        const type = p.activity_type as string;
        const method = (p.method as string) || "";
        const status = (p.status as string) || "";
        const targetId = (p.target_id as string) || "";
        const durationMs = p.duration_ms as number | undefined;
        const summary = (p.summary as string) || "";

        let line = "";
        if (type === "a2a_receive" && method === "message/send") {
          const targetName = resolveWorkspaceName(targetId || msg.workspace_id);
          if (status === "ok" && durationMs) {
            const sec = Math.round(durationMs / 1000);
            line = `← ${targetName} responded (${sec}s)`;
            const own = (targetId || msg.workspace_id) === workspaceId;
            if (own) callbacksRef.current.onSendComplete?.();
          } else if (status === "ok" && !durationMs) {
            // Task #227 — poll-mode (external/MCP workspace) queued receipt.
            // ws-server `logA2AReceiveQueued` writes a "received but no
            // reply yet" row with status="ok" and NO duration_ms, then
            // immediately returns the synthetic {status:"queued"} 200 to
            // the caller. Before this branch the row was silently dropped
            // by the (status==="ok" && durationMs) guard above — leaving
            // the chat UI with zero progress signal for the entire window
            // between "user typed" and "agent eventually polled and
            // replied". Surface the queued state explicitly so the user
            // sees acknowledgement (matches the queued-delegation
            // indicator in AgentCommsPanel.WaitingBubbles).
            //
            // We intentionally do NOT call onSendComplete here: the
            // outbound is not done — only acknowledged. The MyChatPanel
            // spinner stays up until the actual AGENT_MESSAGE reply lands
            // (poll path) or an explicit error fires (which still hits
            // the status==="error" branch below).
            line = `⧗ ${targetName} queued — agent will pick up on next poll`;
          } else if (status === "error") {
            line = `⚠ ${targetName} error`;
            const own = (targetId || msg.workspace_id) === workspaceId;
            if (own) {
              callbacksRef.current.onSendComplete?.();
              // internal#212 — surface the actionable, secret-safe
              // failure reason (provider HTTP status + error code +
              // human-readable message) the ws-server now puts on
              // ACTIVITY_LOGGED.error_detail. The old hardcoded
              // "Agent error (Exception) — see workspace logs for
              // details." is the fallback only — it pointed at a
              // workspace-logs tab that doesn't exist, telling the
              // user nothing they could act on.
              //
              // Graceful degradation: older ws-server builds don't
              // include error_detail, so the legacy boilerplate is
              // still the floor (never silently swallow).
              const detail = (p.error_detail as string) || "";
              const reason = detail
                ? detail
                : "Agent error (Exception) — see workspace logs for details.";
              callbacksRef.current.onSendError?.(reason);
            }
          }
        } else if (type === "a2a_send") {
          const targetName = resolveWorkspaceName(targetId);
          line = `→ Delegating to ${targetName}...`;
        } else if (type === "task_update") {
          if (summary) line = `⟳ ${summary}`;
        } else if (type === "agent_log") {
          if (summary) line = summary;
        }

        if (line) {
          callbacksRef.current.onActivityLog?.(line);
        }
      } else if (
        msg.event === "TASK_UPDATED" &&
        msg.workspace_id === workspaceId
      ) {
        const task = (msg.payload?.current_task as string) || "";
        if (task) {
          callbacksRef.current.onActivityLog?.(`⟳ ${task}`);
        }
      } else if (
        msg.event === "REQUEST_RESPONDED" &&
        msg.workspace_id === workspaceId
      ) {
        const p = msg.payload || {};
        callbacksRef.current.onRequestResponded?.({
          status: (p.status as string) || "",
          responderType: (p.responder_type as string) || "",
          title: (p.title as string) || "",
          kind: (p.kind as string) || "",
        });
      }
    } catch {
      /* ignore */
    }
  });
}
