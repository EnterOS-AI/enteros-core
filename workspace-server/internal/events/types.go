package events

// types.go — typed taxonomy of WebSocket event names emitted by the
// workspace-server.
//
// RFC #2945 PR-B. Pre-consolidation, every BroadcastOnly /
// RecordAndBroadcast call site passed a bare string literal:
//
//	h.broadcaster.BroadcastOnly(workspaceID, "AGENT_MESSAGE", payload)
//
// Producers (Go workspace-server, ~30 call sites across handlers/,
// scheduler/, registry/, bundle/) and consumers (canvas TS store +
// component listeners) duplicated the same string with no shared
// definition. A producer renaming an event silently broke every
// consumer — same drift class that produced the reno-stars data-loss
// regression on the persistence side. The fix on that side was the
// AgentMessageWriter SSOT (PR-A); the fix on this side is named
// constants.
//
// Why a typed string (not a plain enum / iota): the event name
// crosses the wire to TypeScript consumers as the literal string in
// `WSMessage.Event`. Iota integers would break the canvas store's
// switch (`case "AGENT_MESSAGE":`); a typed string preserves the
// wire contract while giving Go callers compile-time discipline.
//
// Canvas exposes a typed subset of the names it consumes in
// `canvas/src/lib/ws-events.ts`; it is not a second exhaustive taxonomy.

// EventType is the wire-typed name of a WebSocket event the platform
// broadcasts. Always emit constants from this file rather than bare
// strings — the AST gate in events_types_drift_test.go guards
// against bare-string usage in the broadcaster surfaces.
type EventType string

// Event constants — the canonical server taxonomy. New events MUST be added
// here. Add them to canvas/src/lib/ws-events.ts only when a TypeScript
// consumer handles that event. Group by semantic family so the list stays
// scan-friendly as it grows.
const (
	// Chat / agent messaging — surfaces in canvas chat panels.
	EventAgentMessage   EventType = "AGENT_MESSAGE"
	EventA2AResponse    EventType = "A2A_RESPONSE"
	EventActivityLogged EventType = "ACTIVITY_LOGGED"
	EventChannelMessage EventType = "CHANNEL_MESSAGE"
	// EventUserMessage echoes a canvas user's outbound chat message to
	// every connected device on the same workspace so a message typed on
	// device A surfaces on device B in real time (core#2697). Payload
	// shape mirrors AGENT_MESSAGE: {message_id, content, attachments?,
	// workspace_id}. Only broadcast when the client supplied a
	// messageId (the only path that has a stable identity for cross-
	// device dedup).
	EventUserMessage EventType = "USER_MESSAGE"
	// EventSessionReset signals that the user pressed "New session" on
	// one device; all other devices connected to the same workspace
	// clear their local chat view to match (core#2697). The server
	// also updates workspaces.chat_session_started_at so a fresh
	// chat-history fetch filters out pre-marker rows.
	EventSessionReset EventType = "SESSION_RESET"

	// Workspace lifecycle.
	EventWorkspaceProvisioning    EventType = "WORKSPACE_PROVISIONING"
	EventWorkspaceProvisionFailed EventType = "WORKSPACE_PROVISION_FAILED"
	EventWorkspaceOnline          EventType = "WORKSPACE_ONLINE"
	EventWorkspaceOffline         EventType = "WORKSPACE_OFFLINE"
	EventWorkspaceDegraded        EventType = "WORKSPACE_DEGRADED"
	EventWorkspaceHibernated      EventType = "WORKSPACE_HIBERNATED"
	EventWorkspacePaused          EventType = "WORKSPACE_PAUSED"
	EventWorkspaceRemoved         EventType = "WORKSPACE_REMOVED"
	EventWorkspaceAwaitingAgent   EventType = "WORKSPACE_AWAITING_AGENT"
	EventWorkspaceHeartbeat       EventType = "WORKSPACE_HEARTBEAT"

	// Agent assignment + identity.
	EventAgentAssigned    EventType = "AGENT_ASSIGNED"
	EventAgentReplaced    EventType = "AGENT_REPLACED"
	EventAgentRemoved     EventType = "AGENT_REMOVED"
	EventAgentMoved       EventType = "AGENT_MOVED"
	EventAgentCardUpdated EventType = "AGENT_CARD_UPDATED"

	// Delegation lifecycle.
	EventDelegationSent     EventType = "DELEGATION_SENT"
	EventDelegationStatus   EventType = "DELEGATION_STATUS"
	EventDelegationComplete EventType = "DELEGATION_COMPLETE"
	EventDelegationFailed   EventType = "DELEGATION_FAILED"

	// Task progression.
	EventTaskUpdated EventType = "TASK_UPDATED"

	// Approvals.
	EventApprovalRequested EventType = "APPROVAL_REQUESTED"
	EventApprovalEscalated EventType = "APPROVAL_ESCALATED"

	// User tasks (agent → user asks).
	EventUserTaskRequested EventType = "USER_TASK_REQUESTED"
	EventUserTaskResolved  EventType = "USER_TASK_RESOLVED"

	// Requests — the unified Tasks + Approvals inbox (RFC P1). REQUEST_CREATED
	// pokes a recipient agent's inbox; REQUEST_RESPONDED is the async signal-back
	// to the requester; REQUEST_MESSAGE is a More-Info thread reply.
	EventRequestCreated   EventType = "REQUEST_CREATED"
	EventRequestResponded EventType = "REQUEST_RESPONDED"
	EventRequestMessage   EventType = "REQUEST_MESSAGE"

	// Auth / credentials.
	EventExternalCredentialsRotated EventType = "EXTERNAL_CREDENTIALS_ROTATED"

	// Boot sequence — the per-step "Enter OS" boot animation the canvas
	// renders while a workspace is `provisioning`. The runtime emits these
	// as it walks its cold-boot checklist (provision compute → start
	// runtime → wire transport → install plugins → load identity → connect
	// management MCP → enumerate tools → go online), so the canvas can
	// replace the opaque provisioning spinner with a watchdog-driven,
	// per-step keycap animation that fails LOUDLY (shows the failing step's
	// reason) instead of hanging.
	//
	// One event type carries the whole family — a single BOOT_STEP with a
	// {step,total,key,label,status,message} payload is cleaner than one
	// event per boot phase and lets the runtime add/reorder steps without a
	// server or canvas release. The canvas is data-driven off the payload.
	//
	// Wire payload (BOOT_STEP) — the shape the runtime emitter MUST send;
	// the ingestion handler (boot_event.go) validates it and the canvas
	// store appends it to the workspace node's `bootSteps` array:
	//
	//	{
	//	  "step":    3,           // 1-based index of this step (>=1)
	//	  "total":   8,           // total steps in the boot plan (>= step)
	//	  "key":     "MCP",       // short keycap legend (<=8 chars), e.g. PWR/RT/MCP
	//	  "label":   "Connect management MCP",  // human step name
	//	  "status":  "running",   // one of: running | ok | failed
	//	  "message": "launching npx @molecule-ai/mcp-server…"  // optional log line;
	//	                          // on status=failed this is the red failure reason
	//	}
	//
	// Terminal signal: the runtime marks the LAST step status=ok (its
	// "go online" phase) and then flips the workspace row to `online`,
	// which emits the existing WORKSPACE_ONLINE — the canvas uses THAT to
	// fade the boot screen into chat. BOOT_STEP is presentation-only and is
	// broadcast-only (not persisted in structure_events): a mid-boot page
	// reload re-derives the step list from the workspace status + any
	// replayed steps, so there's nothing to persist. If NO BOOT_STEP events
	// arrive, the canvas degrades to a generic indeterminate boot.
	EventBootStep EventType = "BOOT_STEP"
)

// AllEventTypes lists every constant in this file. Used by the
// snapshot test (events_types_drift_test.go) to detect when a new
// constant is added without updating the server-side snapshot.
//
// Keep in lexicographic order so the snapshot diff is stable on
// renames and the parity-with-TS comparison is order-independent.
var AllEventTypes = []EventType{
	EventA2AResponse,
	EventActivityLogged,
	EventAgentAssigned,
	EventAgentCardUpdated,
	EventAgentMessage,
	EventAgentMoved,
	EventAgentRemoved,
	EventAgentReplaced,
	EventApprovalEscalated,
	EventApprovalRequested,
	EventBootStep,
	EventChannelMessage,
	EventDelegationComplete,
	EventDelegationFailed,
	EventDelegationSent,
	EventDelegationStatus,
	EventExternalCredentialsRotated,
	EventRequestCreated,
	EventRequestMessage,
	EventRequestResponded,
	EventSessionReset,
	EventTaskUpdated,
	EventUserTaskRequested,
	EventUserTaskResolved,
	EventUserMessage,
	EventWorkspaceAwaitingAgent,
	EventWorkspaceDegraded,
	EventWorkspaceHeartbeat,
	EventWorkspaceHibernated,
	EventWorkspaceOffline,
	EventWorkspaceOnline,
	EventWorkspacePaused,
	EventWorkspaceProvisionFailed,
	EventWorkspaceProvisioning,
	EventWorkspaceRemoved,
}
