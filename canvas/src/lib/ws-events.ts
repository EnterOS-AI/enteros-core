/**
 * Typed Canvas subset of the Go `events.EventType` taxonomy
 * (`workspace-server/internal/events/types.go`). The Go side is the SSOT;
 * this object lists only names consumed through typed Canvas paths, so a typo
 * in those handlers is a compile error rather than a silently dropped event.
 *
 * Add a name when Canvas begins consuming it; this is intentionally not an
 * exhaustive mirror of `AllEventTypes`. The unified request names are present
 * because the Tasks/Approvals UI reacts to them live.
 */
export const WS_EVENTS = {
  WorkspaceOnline: "WORKSPACE_ONLINE",
  WorkspaceOffline: "WORKSPACE_OFFLINE",
  WorkspacePaused: "WORKSPACE_PAUSED",
  WorkspaceDegraded: "WORKSPACE_DEGRADED",
  WorkspaceProvisioning: "WORKSPACE_PROVISIONING",
  WorkspaceProvisionFailed: "WORKSPACE_PROVISION_FAILED",
  WorkspaceRemoved: "WORKSPACE_REMOVED",
  AgentCardUpdated: "AGENT_CARD_UPDATED",
  AgentMessage: "AGENT_MESSAGE",
  TaskUpdated: "TASK_UPDATED",
  A2AResponse: "A2A_RESPONSE",
  // --- Unified requests inbox (RFC P1; mirrored from events/types.go) ---
  RequestCreated: "REQUEST_CREATED",
  RequestResponded: "REQUEST_RESPONDED",
  RequestMessage: "REQUEST_MESSAGE",
  // --- "Enter OS" boot sequence (mirrored from events/types.go) ---
  // A single BOOT_STEP carries the whole per-step boot animation; the
  // canvas appends each to the workspace node's `bootSteps` array while
  // the workspace is `provisioning`. Presentation-only + broadcast-only.
  BootStep: "BOOT_STEP",
} as const;

/** The event-name string union — the values of WS_EVENTS. */
export type WsEventName = (typeof WS_EVENTS)[keyof typeof WS_EVENTS];

/**
 * REQUEST_* event names the canvas Tasks/Approvals tabs refresh on. A single
 * Set keeps the ConciergeShell subscriber's membership test O(1) and the list
 * declarative — adding a fourth request event is one line here.
 */
export const REQUEST_EVENT_NAMES: ReadonlySet<string> = new Set<string>([
  WS_EVENTS.RequestCreated,
  WS_EVENTS.RequestResponded,
  WS_EVENTS.RequestMessage,
]);

/** True when a WS message's event is one of the unified-requests events. */
export function isRequestEvent(event: string): boolean {
  return REQUEST_EVENT_NAMES.has(event);
}
