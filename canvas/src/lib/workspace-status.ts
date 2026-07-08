/** Canonical workspace `status` wire values — the TS mirror of Go's
 *  models.Status* constants (workspace-server/internal/models/
 *  workspace_status.go), which in turn mirror the `workspace_status`
 *  Postgres enum (migrations 043 + 046, drift-test-pinned on the Go side
 *  by internal/db/workspace_status_enum_drift_test.go).
 *
 *  Single source of truth for the `status` magic strings used across the
 *  canvas. Kept in a leaf module (zero imports) so stores, components and
 *  e2e helpers can all import it without circular dependencies — the same
 *  pattern as `workspace-kind.ts`. `WorkspaceNodeData.status` stays a plain
 *  `string`; these are the well-known values to compare against, not an
 *  exhaustive TS enum.
 *
 *  DRIFT CONSEQUENCE: if the Go enum gains/renames a value and this file
 *  is not updated in the same change, canvas comparisons against the stale
 *  value silently stop matching — status-driven UI (badges, gates, the
 *  self-host setup scene's provision watch) misrenders the workspace as an
 *  unknown state instead of failing loudly. When touching the Go const
 *  block, update this mirror in the same PR. */
export const WORKSPACE_STATUS = {
  Provisioning: "provisioning",
  Online: "online",
  Offline: "offline",
  Degraded: "degraded",
  Failed: "failed",
  Removed: "removed",
  Paused: "paused",
  Hibernated: "hibernated",
  Hibernating: "hibernating",
  AwaitingAgent: "awaiting_agent",
} as const;

/** Union of the 10 Go/Postgres wire values. */
export type WorkspaceStatusValue =
  (typeof WORKSPACE_STATUS)[keyof typeof WORKSPACE_STATUS];

/** Canvas-only SYNTHETIC statuses — never sent by the workspace-server and
 *  never valid values for `workspaces.status`. They exist purely in canvas
 *  view-state and are kept in a separate const so nobody mistakes them for
 *  wire values (writing one to the API would be rejected by the Postgres
 *  enum):
 *
 *  - `starting`       — optimistic UI state between the user's create/start
 *                       action and the first server-confirmed status event.
 *  - `not_configured` — render-state for a workspace the canvas knows lacks
 *                       required configuration (e.g. no model/key yet); the
 *                       server-side row sits at a real enum value (usually
 *                       `offline`). */
export const CANVAS_SYNTHETIC_STATUS = {
  Starting: "starting",
  NotConfigured: "not_configured",
} as const;

/** Union of the canvas-only synthetic statuses. */
export type CanvasSyntheticStatusValue =
  (typeof CANVAS_SYNTHETIC_STATUS)[keyof typeof CANVAS_SYNTHETIC_STATUS];
