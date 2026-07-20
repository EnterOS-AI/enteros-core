package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

// DefaultMaxConcurrentTasks mirrors the workspaces.max_concurrent_tasks
// schema default. Handlers that resolve a 0/omitted payload value write
// this constant so the read-side (scheduler capacity check) sees a
// guaranteed non-zero column on every row.
const DefaultMaxConcurrentTasks = 1

type Workspace struct {
	ID     string         `json:"id" db:"id"`
	Name   string         `json:"name" db:"name"`
	Role   sql.NullString `json:"role" db:"role"`
	Tier   int            `json:"tier" db:"tier"`
	Status string         `json:"status" db:"status"`
	// Kind: "workspace" (default) or "platform". A "platform" workspace is the
	// org-level concierge (the platform agent) that sits at the org root and is
	// the user's default A2A target. See migration
	// 20260606000000_workspaces_kind + RFC docs/design/rfc-platform-agent.md.
	Kind               string          `json:"kind" db:"kind"`
	SourceBundleID     sql.NullString  `json:"source_bundle_id" db:"source_bundle_id"`
	AgentCard          json.RawMessage `json:"agent_card" db:"agent_card"`
	URL                sql.NullString  `json:"url" db:"url"`
	ParentID           *string         `json:"parent_id" db:"parent_id"`
	ForwardedTo        *string         `json:"forwarded_to" db:"forwarded_to"`
	LastHeartbeatAt    *time.Time      `json:"last_heartbeat_at" db:"last_heartbeat_at"`
	LastErrorRate      float64         `json:"last_error_rate" db:"last_error_rate"`
	LastSampleError    sql.NullString  `json:"last_sample_error" db:"last_sample_error"`
	ActiveTasks        int             `json:"active_tasks" db:"active_tasks"`
	// IsBusy is the self-healing successor to ActiveTasks (RFC #4402): a boolean
	// "is the agent in a turn right now" fed by the heartbeat. During the B1→B4
	// migration it is written on every beat — either from the runtime's own
	// is_busy (B2+) or derived from active_tasks>0 (older runtimes). Consumers
	// cut over to it in B3.
	IsBusy             bool            `json:"is_busy" db:"is_busy"`
	MaxConcurrentTasks int             `json:"max_concurrent_tasks" db:"max_concurrent_tasks"`
	UptimeSeconds      int             `json:"uptime_seconds" db:"uptime_seconds"`
	CreatedAt          time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at" db:"updated_at"`
	// DeliveryMode: "push" (synchronous to URL — default) or "poll" (logged
	// to activity_logs, agent reads via GET /activity?since_id=). See
	// migration 045 + RFC #2339.
	DeliveryMode string `json:"delivery_mode" db:"delivery_mode"`
	// BroadcastEnabled: when true the workspace may call POST /broadcast to
	// deliver a message to all non-removed agent workspaces in the org.
	// Default false — only privileged orchestrators should hold this ability.
	BroadcastEnabled bool `json:"broadcast_enabled" db:"broadcast_enabled"`
	// TalkToUserEnabled: when false the workspace's send_message_to_user calls
	// and POST /notify requests are rejected with HTTP 403 so the agent is
	// forced to route updates through a parent workspace. Default true
	// (preserves existing behaviour for all workspaces).
	TalkToUserEnabled bool `json:"talk_to_user_enabled" db:"talk_to_user_enabled"`
	// Canvas layout fields (from JOIN)
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Collapsed bool    `json:"collapsed"`
}

// Delivery mode constants. Matches the CHECK constraint in migration 045.
const (
	DeliveryModePush = "push"
	DeliveryModePoll = "poll"
)

// IsValidDeliveryMode reports whether s is one of the recognised
// delivery modes. Empty string is NOT valid here — callers must
// resolve the default ("push") before calling.
func IsValidDeliveryMode(s string) bool {
	return s == DeliveryModePush || s == DeliveryModePoll
}

// Workspace kind constants. Matches the CHECK constraint in migration
// 20260606000000_workspaces_kind. KindPlatform marks the org-level concierge
// (the platform agent) which sits at the org root; see
// docs/design/rfc-platform-agent.md.
const (
	KindWorkspace = "workspace"
	KindPlatform  = "platform"
)

// IsValidKind reports whether s is a recognised workspace kind. Empty string is
// NOT valid here — callers resolve the default (KindWorkspace) before calling.
func IsValidKind(s string) bool {
	return s == KindWorkspace || s == KindPlatform
}

type RegisterPayload struct {
	ID string `json:"id" binding:"required"`
	// URL is required for push-mode workspaces; optional / unused for
	// poll-mode (the platform never dispatches to it). The handler
	// enforces the conditional requirement based on the resolved
	// delivery mode (payload value, falling back to the row's existing
	// value, falling back to "push").
	URL       string          `json:"url"`
	AgentCard json.RawMessage `json:"agent_card" binding:"required"`
	// DeliveryMode is optional. Empty string means "keep the existing
	// value on the workspace row, or default to push for new rows".
	// When set, must be one of DeliveryModePush / DeliveryModePoll.
	DeliveryMode string `json:"delivery_mode,omitempty"`
	// Kind is optional. Empty string means "keep the existing value on the
	// workspace row, or default to KindWorkspace for new rows". When set, must
	// be one of KindWorkspace / KindPlatform. KindPlatform additionally requires
	// the row to be its own org root (parent_id IS NULL) and to be the only
	// platform agent in the org — enforced by the Register handler.
	Kind string `json:"kind,omitempty"`
	// MCPServerPresent is the runtime's declaration that the platform-agent
	// image's baked /opt/molecule-mcp-server binary is present. For platform
	// agents the controlplane treats nil/false as fail-closed (RCA #2970).
	// Non-platform workspaces may omit this field.
	MCPServerPresent *bool `json:"mcp_server_present,omitempty"`
}

type HeartbeatPayload struct {
	WorkspaceID   string  `json:"workspace_id" binding:"required"`
	ErrorRate     float64 `json:"error_rate"`
	SampleError   string  `json:"sample_error"`
	ActiveTasks   int     `json:"active_tasks"`
	// IsBusy is the runtime's authoritative turn-in-flight flag (RFC #4402 B2),
	// the self-healing successor to active_tasks. A POINTER so the three states
	// stay distinct: absent/nil (older image that doesn't send it → the handler
	// DERIVES is_busy from active_tasks>0, behavior-identical to B1) vs an
	// explicit true/false the handler TRUSTS over the derive (COALESCE in the
	// heartbeat UPDATE). Optional in the SDK workspace-comms contract
	// (`is_busy,omitempty`), so an old runtime simply omits it. B1 captured the
	// column by deriving in SQL with no new bind arg; B2 adds this parsed field
	// + the bump that puts is_busy in molcontracts.HeartbeatRequest.
	IsBusy        *bool   `json:"is_busy,omitempty"`
	UptimeSeconds int     `json:"uptime_seconds"`
	CurrentTask   string  `json:"current_task"`
	// MonthlySpend is cumulative USD spend for the current calendar month,
	// denominated in cents (e.g. 1500 = $15.00). Zero means "no update" —
	// the heartbeat handler never writes zero to avoid accidentally clearing
	// a previously-reported spend value. Any non-zero value is clamped to
	// [0, maxMonthlySpend] before the DB write. (#615)
	MonthlySpend int64 `json:"monthly_spend"`
	// RuntimeState is a self-reported runtime health flag separate from
	// "is the heartbeat task firing at all". The heartbeat task lives in
	// its own asyncio task and keeps pinging even when the agent runtime
	// is wedged (e.g. claude_agent_sdk's `Control request timeout:
	// initialize` leaves the SDK in a permanent error state for the
	// process lifetime). RuntimeState is how the workspace tells the
	// platform "I'm alive but my Claude runtime is broken — flip me to
	// degraded so the canvas can show a Restart hint."
	//
	// Empty string = healthy / no signal. The only currently-recognised
	// non-empty value is "wedged"; future values can extend this without
	// migration.
	RuntimeState string `json:"runtime_state"`

	// RuntimeMetadata is the adapter-declared capability map + per-
	// capability override values. The Python runtime builds this from
	// BaseAdapter.capabilities() + per-hook methods (e.g.
	// idle_timeout_override()) — see molecule-ai-workspace-runtime/
	// molecule_runtime/heartbeat.py:
	// _runtime_metadata_payload. Optional: missing means "use platform
	// defaults for everything", matching pre-2026-04 behavior.
	//
	// Pointer (not value) so a missing JSON field is nil rather than a
	// zero-value RuntimeMetadata{} that would falsely claim "all caps =
	// false declared explicitly". Lets the platform distinguish "adapter
	// said no native ownership" from "old runtime version, didn't say".
	RuntimeMetadata *RuntimeMetadata `json:"runtime_metadata,omitempty"`

	// AgentCard is sent by the runtime on heartbeat when the initial
	// /registry/register failed and the workspace has no persisted agent_card.
	// The heartbeat handler backfills NULL agent_card rows so the workspace
	// can come online without requiring a full re-register. (#2421)
	AgentCard json.RawMessage `json:"agent_card,omitempty"`
	// MCPServerPresent mirrors the register payload field on every heartbeat
	// so the fail-closed platform-agent gate can block recovery paths that
	// would otherwise resurrect an mcp-less concierge (RCA #2970).
	MCPServerPresent *bool `json:"mcp_server_present,omitempty"`

	// LoadedMCPTools is the list of namespaced MCP tool identifiers the
	// runtime reports as actually loaded for this workspace. For platform
	// concierges, core cross-checks this against the declared management
	// MCP so a missing plugin is surfaced as degraded instead of silent
	// (core#3082, CR2 #12653 fix). Each entry is a Claude Code dispatcher
	// id of the form `mcp__<server>__<tool>`; the platform MCP's required
	// tool is `mcp__molecule-platform__provision_workspace` (see
	// conciergePlatformMCPProvisionWorkspaceTool).
	//
	// On a heartbeat where mcp_server_present=true and LoadedMCPTools is
	// nil/omitted, the #3082 gate fails loud (degraded) — the runtime
	// spoke the #147 contract but omitted the new loaded_mcp_tools
	// producer, so we cannot verify the specific required tool is loaded.
	// Runtime needs a loaded_mcp_tools producer to make the deployed path
	// healthy (tracked separately — see PR #3101 PM flag).
	LoadedMCPTools []string `json:"loaded_mcp_tools,omitempty"`

	// MCPToolsReady is the EV2 POSITIVE readiness signal: the runtime's
	// MCPReadinessProber sets it true on the FIRST successful `tools/list`
	// (turn-independent, so it fires for codex/hermes without a synthetic
	// turn). Core flips a concierge provisioning->online on the first beat
	// carrying true — replacing the wall-clock fireConciergeWarmup nudge that
	// used to coax the per-turn loaded_mcp_tools capture. The 180s grace is
	// KEPT as the flap absorber.
	//
	// POINTER so the three states stay distinct (absent != false): nil =
	// unknown / probe not yet succeeded (older image, or before first
	// success), false = probed-not-ready, true = tools loaded. Optional in the
	// SDK workspace-comms contract (`mcp_tools_ready,omitempty`); runtime#273
	// landed the NEGATIVE half (launch_failure) — this is the positive half.
	MCPToolsReady *bool `json:"mcp_tools_ready,omitempty"`

	// FirstReadyAt is the RFC3339 timestamp of the first tools/list success
	// that set MCPToolsReady=true. Observability/latency only
	// (provisioning->ready wall-clock); the online-flip MUST NOT gate on it.
	// Absent until the first probe success. (EV2)
	FirstReadyAt string `json:"first_ready_at,omitempty"`
}

// RuntimeMetadata is the adapter-declared capability + override block
// the Python runtime sends in the heartbeat payload. New fields can be
// added with `omitempty` without breaking older runtime versions.
//
// See project memory `project_runtime_native_pluggable.md` for the
// principle and molecule-ai-workspace-runtime/molecule_runtime/
// adapter_base.py:RuntimeCapabilities for the Python source of truth.
type RuntimeMetadata struct {
	// Capabilities maps capability name → "adapter owns it natively".
	// Keys (heartbeat, scheduler, session, status_mgmt, retry,
	// activity_decoration, channel_dispatch) match
	// RuntimeCapabilities.to_dict() in adapter_base.py — keep in sync.
	Capabilities map[string]bool `json:"capabilities,omitempty"`

	// IdleTimeoutSeconds, when set, overrides the per-dispatch silence
	// window in a2a_proxy.go for this workspace's A2A traffic. Pointer
	// so nil means "no override; use the global default". Zero / negative
	// is treated as nil by the consumer (a2a_proxy.go).
	IdleTimeoutSeconds *int `json:"idle_timeout_seconds,omitempty"`
}

type UpdateCardPayload struct {
	WorkspaceID string          `json:"workspace_id" binding:"required"`
	AgentCard   json.RawMessage `json:"agent_card" binding:"required"`
}

// MemorySeed represents an initial memory to seed into a workspace at creation time.
// Used by both the POST /workspaces API and org template import to pre-populate
// agent memories from config (issue #1050).
type MemorySeed struct {
	Content string `json:"content" yaml:"content"`
	Scope   string `json:"scope" yaml:"scope"` // LOCAL, TEAM, GLOBAL
}

type WorkspaceComputeVolume struct {
	RootGB int `json:"root_gb,omitempty"`
}

type WorkspaceComputeDisplay struct {
	Mode     string `json:"mode,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

type WorkspaceCompute struct {
	InstanceType string                  `json:"instance_type,omitempty"`
	Volume       WorkspaceComputeVolume  `json:"volume,omitempty"`
	Display      WorkspaceComputeDisplay `json:"display,omitempty"`
	// DataPersistence is the per-workspace durable-data choice (internal#734):
	// "persist" keeps the workspace's data volume (browser profile / cookies /
	// downloads / agent memory) across recreate; "ephemeral" uses no durable
	// disk (wiped each recreate — privacy); "" = auto (desktop-control persists,
	// others follow the org flag). Forwarded verbatim to CP's data_persistence.
	DataPersistence string `json:"data_persistence,omitempty"`
	// Provider is the cloud/compute backend for this workspace box. Empty means
	// the deployment-selected default; recognized explicit values currently
	// include "aws", "hetzner", and "gcp". Distinct from the LLM/model provider
	// and forwarded to CP /cp/workspaces/provision as `provider`.
	Provider string `json:"provider,omitempty"`
}

type CreateWorkspacePayload struct {
	Name     string `json:"name" binding:"required"`
	Role     string `json:"role"`
	Template string `json:"template"` // workspace-configs-templates folder name
	Tier     int    `json:"tier"`
	Model    string `json:"model"`
	// LLMProvider is the optional provider slug paired with Model. Runtimes
	// such as claude-code need a bare model id plus explicit provider slug;
	// hermes can still derive provider from slash-prefixed model ids.
	LLMProvider string `json:"llm_provider"`
	Runtime     string `json:"runtime"`  // "hermes" (default), "claude-code", "codex", etc.
	External    bool   `json:"external"` // true = no Docker container, just a registered URL
	URL         string `json:"url"`      // for external workspaces: the A2A endpoint URL (push mode only — omit for poll)
	// DeliveryMode: "push" (default) sends inbound A2A to URL synchronously;
	// "poll" records inbound to activity_logs for the agent to consume via
	// GET /activity?since_id=. Poll mode does not require a URL. See #2339.
	DeliveryMode    string  `json:"delivery_mode,omitempty"`
	WorkspaceDir    string  `json:"workspace_dir"`    // host path to mount as /workspace (empty = isolated volume)
	WorkspaceAccess string  `json:"workspace_access"` // "none" (default), "read_only", or "read_write" — see #65
	ParentID        *string `json:"parent_id"`
	// BudgetLimit is the optional monthly spend ceiling in USD cents.
	// NULL (omitted) means no limit. budget_limit=500 means $5.00/month.
	BudgetLimit *int64 `json:"budget_limit"`
	// Secrets is an optional map of key→plaintext-value pairs to persist as
	// workspace secrets at creation time.  Stored encrypted (same path as
	// POST /workspaces/:id/secrets).  Nil/empty map is a no-op.
	Secrets map[string]string `json:"secrets"`
	// Plugins is an optional list of git-native plugin SOURCES to DECLARE for
	// this workspace at create (workspace_declared_plugins → the post-online
	// reconcile installs them). It is the API-level equivalent of a template's
	// `plugins:` block for callers that don't own a template — same DB channel
	// (seedTemplatePlugins → recordDeclaredPlugin), same idempotent ON CONFLICT
	// upsert, and the SAME privileged-plugin kind-gate (the org-management MCP
	// can never be declared here on a non-platform workspace). Empty is a no-op.
	// NOTE: declaring via this DB channel is the durable path; injecting
	// MOLECULE_DECLARED_PLUGINS via `secrets` does NOT survive, because provision
	// recomputes that env from the declared-set SSOT and overwrites it.
	Plugins []string `json:"plugins,omitempty"`
	// MaxConcurrentTasks caps parallel A2A + cron dispatch. 0 means use
	// DefaultMaxConcurrentTasks. Leaders typically set 3.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	// Compute is the product-facing per-workspace host shape/display
	// contract. It uses instance_type + volume.root_gb and persists
	// display for future desktop-control workspaces.
	Compute WorkspaceCompute `json:"compute,omitempty"`
	Canvas  struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"canvas"`
	// InitialMemories is an optional list of memories to seed into the
	// workspace immediately after creation. Each entry is inserted into
	// agent_memories under the workspace's v2 memory namespace
	// ("workspace:<id>"). Issue #1050.
	InitialMemories []MemorySeed `json:"initial_memories"`
}

type CheckAccessPayload struct {
	CallerID string `json:"caller_id" binding:"required"`
	TargetID string `json:"target_id" binding:"required"`
}
