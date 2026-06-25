-- core#3082: persist the runtime's loaded MCP tool inventory on the workspace
-- row so GET /workspaces/:id can return it deterministically (molecule-core#3256).
-- The field is a JSONB array of namespaced tool ids (`mcp__<server>__<tool>`);
-- NULL means the runtime has not yet reported an inventory.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS loaded_mcp_tools JSONB;
