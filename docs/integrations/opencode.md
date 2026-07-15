# Molecule AI + OpenCode integration

OpenCode can connect to a workspace's authenticated Streamable HTTP MCP
endpoint. The exact tool list is server-controlled and may be filtered by the
workspace's `can_delegate` setting and platform feature flags.

## Prerequisites

- A tenant base URL such as `https://<tenant-slug>.moleculesai.app`, or
  `http://localhost:8080` for self-hosted development.
- The target workspace UUID.
- A workspace bearer returned by the platform. Never put it in the URL or
  commit it to `opencode.json`.

## 1. Mint a workspace bearer

An administrator can bootstrap a bearer for an existing workspace:

```bash
export MOLECULE_MCP_URL='https://<tenant-slug>.moleculesai.app'
export WORKSPACE_ID='<workspace-uuid>'
export ADMIN_TOKEN='<tenant-admin-token>'

MOLECULE_MCP_TOKEN=$(
  curl -fsS -X POST \
    "$MOLECULE_MCP_URL/admin/workspaces/$WORKSPACE_ID/tokens" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
  | jq -r '.auth_token'
)
export MOLECULE_MCP_TOKEN
```

The plaintext token is returned once. A caller that already has a workspace
bearer may rotate through `POST /workspaces/:id/tokens` with that existing
bearer. Token creation has no per-token `mcp:read` or `mcp:delegate` scope
payload; authorization is bound to the workspace and enforced by
`WorkspaceAuth`, tool filtering, and the MCP rate limiter.

## 2. Configure OpenCode

OpenCode's current configuration key is `mcp` (not the older `mcpServers`
shape), and environment interpolation uses `{env:NAME}`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "molecule": {
      "type": "remote",
      "url": "{env:MOLECULE_MCP_URL}/workspaces/{env:WORKSPACE_ID}/mcp",
      "enabled": true,
      "headers": {
        "Authorization": "Bearer {env:MOLECULE_MCP_TOKEN}"
      }
    }
  }
}
```

The platform also exposes
`GET /workspaces/:id/mcp/stream` for the backwards-compatible SSE transport;
new clients should use the POST endpoint above.

## 3. Available tools

OpenCode discovers tools through MCP `tools/list`; do not maintain a fixed
client-side allowlist. The current bridge includes these groups:

- topology and delegation: `list_peers`, `get_workspace_info`,
  `delegate_task`, `delegate_task_async`, and `check_task_status`;
- user-action tasks: `request_user_action`, `list_user_tasks`,
  `update_user_task`, and `delete_user_task`; and
- memory: the compatibility `commit_memory`/`recall_memory` pair plus the
  namespace-aware v2 search, summary, list, and forget tools.

`delegate_task` and `delegate_task_async` disappear when the workspace has
`can_delegate=false`. `send_message_to_user` is absent unless the platform
explicitly sets `MOLECULE_MCP_ALLOW_SEND_MESSAGE=true`.

## 4. Security and lifecycle

- `WorkspaceAuth` binds the bearer to the `:id` workspace before MCP dispatch.
- The bridge rate-limits calls per token.
- GLOBAL writes are rejected on the compatibility memory tools; v2 namespace
  access is resolved server-side and content passes the SAFE-T1201 redactor.
- Revoke a token by listing `GET /workspaces/:id/tokens` with a valid
  workspace bearer, then calling
  `DELETE /workspaces/:id/tokens/:tokenId` with the same bearer.
- Treat any bearer committed to source control or pasted into a persistent
  channel as burned and rotate it.

## Environment variables

```bash
MOLECULE_MCP_URL=https://<tenant-slug>.moleculesai.app
MOLECULE_MCP_TOKEN=
WORKSPACE_ID=
```

See [OpenCode's MCP server documentation](https://opencode.ai/docs/mcp-servers/)
for the client configuration format and [the platform API](../api-protocol/platform-api.md)
for Molecule routes.
