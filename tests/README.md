# Tests

This repo uses the standard monorepo testing convention: **unit tests live with their package, cross-component E2E tests live here.**

## Where to find tests

| Scope | Location |
|---|---|
| Go unit + integration (platform, CLI, handlers) | `workspace-server/**/*_test.go` — run with `cd workspace-server && go test -race ./...` |
| TypeScript unit (canvas components, hooks, store) | `canvas/src/**/__tests__/` — run with `cd canvas && npm test -- --run` |
| TypeScript unit (MCP server handlers) | `mcp-server/src/__tests__/` — run with `cd mcp-server && npx jest` |
| Python unit (workspace runtime, adapters) | `molecule-ai-workspace-runtime/tests/` in the standalone runtime repo |
| Python unit (SDK: plugin + remote agent) | `sdk/python/tests/` — run with `cd sdk/python && python3 -m pytest` |
| **Cross-component E2E** (spans platform + runtime + HTTP) | `tests/e2e/` ← **you are here** |

## Why split this way

- **Go** requires co-located `_test.go` files to access unexported symbols.
- **Per-package test commands** keep the inner loop fast — changing canvas doesn't re-run Go tests.
- **`tests/e2e/`** covers scenarios that no single package owns: a full workspace lifecycle, A2A across two provisioned agents, delegation chains, bundle round-trips.

## Running E2E

Every E2E script here assumes the platform is running at `localhost:8080` and (where noted) provisioned agents are online. See the header comment of each `.sh` for specifics.

## Cleaning up an interrupted test workspace

If an E2E run aborts before its teardown runs (Ctrl-C, crash, CI timeout),
the platform can be left with a workspace whose config volume is stale or
empty. Use the exact workspace ID printed by that test and delete it through
the authenticated workspace API. Do not run prefix-based or fleet-wide cleanup
against a shared staging or production tenant.

```bash
curl -fsS -X DELETE \
  -H "Authorization: Bearer ${WORKSPACE_ADMIN_TOKEN}" \
  "${MOLECULE_URL}/workspaces/${EXACT_WORKSPACE_ID}"
```

Local-only cleanup may additionally remove that exact workspace's container
after the API delete. Never infer a destructive selector from a name prefix.
