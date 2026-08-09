-- core#5137: workspaces.mcp_surface — the CONSUMER-side corroboration of the
-- runtime-reported loaded_mcp_tools inventory.
--
-- WHY a second column instead of correcting the first. loaded_mcp_tools is the
-- runtime's own claim and must keep carrying exactly what the runtime said;
-- rewriting a producer's claim in place destroys the ability to audit it later.
-- This column records what CORE observed about that claim, derived from core's
-- own turn record (activity_logs.tool_trace + the "tool-use" agent_log
-- summaries) — tool ids emitted by the model's dispatcher, which no enumeration
-- probe can synthesise.
--
-- Shape (handlers.mcpSurfaceReport):
--   {
--     "reported_count": 54,
--     "dispatch_corroborated_count": 0,
--     "advertised_only_count": 54,
--     "reported_namespaces": ["molecule-platform"],
--     "dispatched_namespaces": ["molecule"],
--     "dispatch_records": 504,
--     "verdict": "contradicted:dispatch_uses_only_other_namespaces",
--     "observed_at": "2026-08-09T00:00:00Z"
--   }
--
-- NULL means "core has not evaluated this row yet" (pre-deploy rows, and any
-- workspace whose runtime never publishes loaded_mcp_tools). NULL is NOT a
-- verdict and must never be read as one — the reader
-- (handlers.mcpSurfaceVerdictFromJSON) maps it to the neutral value.
--
-- Additive and idempotent: no default, no backfill, no rewrite of existing rows.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS mcp_surface JSONB;

-- The corroboration read runs on every concierge heartbeat that carries an
-- inventory, so both of its branches must be index-served. The agent_log branch
-- is already covered by idx_activity_ws_type_time
-- (workspace_id, activity_type, created_at DESC); the tool_trace branch is not —
-- without this it degrades to a sort over the workspace's whole activity
-- history, a per-heartbeat cost that grows with tenant age.
--
-- PARTIAL (WHERE tool_trace IS NOT NULL) so it indexes only the rows the query
-- can return; most activity_logs rows carry no trace. Plain CREATE INDEX, not
-- CONCURRENTLY, matching 048_activity_logs_peer_indexes — this repo's migrations
-- run inside a transaction and CONCURRENTLY cannot.
CREATE INDEX IF NOT EXISTS idx_activity_logs_ws_tooltrace_time
    ON activity_logs (workspace_id, created_at DESC)
    WHERE tool_trace IS NOT NULL;

COMMENT ON COLUMN workspaces.mcp_surface IS
  'core#5137 consumer-derived corroboration of loaded_mcp_tools: how many reported MCP tool ids sit in a namespace this workspace''s model has actually been observed dispatching from. NULL = not yet evaluated (not a verdict).';
