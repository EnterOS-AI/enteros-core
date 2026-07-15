# Quick-start guide

This guide follows the current repository and does not require an operator host,
GitHub organization, Railway/Render wrapper, or production credential bundle.

## Prerequisites

- Docker with Compose v2
- Node.js 20+
- Go 1.25+
- `jq`
- a model-provider credential supported by the runtime you select

## One-command local start

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-core.git
cd molecule-core
./scripts/dev-start.sh
```

The script owns local environment generation, fail-closed admin authentication,
infrastructure startup, manifest-backed template/plugin setup, workspace-server
startup, and Canvas startup. Follow the URLs it prints; the Canvas default is
`http://localhost:3000` when that port is available.

On first load, complete the server-derived platform-agent setup shown by Canvas.
Runtime/provider/model choices come from current APIs and template metadata; do
not copy a hard-coded runtime list from this page.

## Manual component start

For focused development, start infrastructure and the two application
components separately:

```bash
./infra/scripts/setup.sh

cd workspace-server
ADMIN_TOKEN=dev-local-admin-token MOLECULE_ENV=development go run ./cmd/server

# separate terminal
cd canvas
npm ci
NEXT_PUBLIC_ADMIN_TOKEN=dev-local-admin-token npm run dev
```

`NEXT_PUBLIC_ADMIN_TOKEN` must match the workspace server's `ADMIN_TOKEN`.
Authentication remains fail-closed in local development; see the [admin-auth
runbook](./runbooks/admin-auth.md).

## Create and use a workspace

1. Complete the first-run platform-agent configuration if Canvas presents it.
2. Choose a template or create a blank workspace.
3. Add the required provider credential through the authenticated secrets UI.
4. Wait for registration/heartbeat to make the workspace `online`.
5. Send a message from Chat and confirm the response appears in Canvas activity.

A created row or accepted provision request is not proof of a healthy agent.
Registration, heartbeat, and a real message/reply are the end-to-end check.

## Build a hierarchy

Create a child with the desired `parent_id`, or drag an existing workspace onto
its parent in Canvas. **Expand Team View** and **Collapse Team View** only show
or hide descendants. Delete is a separate explicit lifecycle operation.

## Connect an external agent

1. Create/select an external workspace in Canvas.
2. Open **Config → External Connection**.
3. Rotate credentials if no usable one-time token is available.
4. Choose the runtime-specific or universal setup tab and copy the server-stamped
   block exactly.
5. Run it on the external machine and verify registration, heartbeat, inbound
   delivery, and reply routing.

The token is shown once. Re-opening the panel deliberately renders guarded
non-runnable placeholders so pasting it cannot overwrite a working credential.
Different runtimes use different push/poll/channel bridges; do not substitute
the retired `molecule-agent-sdk` command or assume an inbound public URL is
always required.

See the [External Agent Registration Guide](./guides/external-agent-registration.md)
for the current contracts.

## Verification commands

```bash
cd workspace-server
go test ./...

cd ../canvas
npm test
npm run build
```

Integration suites may need sibling repositories or local services; follow the
active workflow's setup and exact pinned dependency.

## Troubleshooting

| Symptom | Check |
|---|---|
| Canvas receives 401 | `ADMIN_TOKEN` and `NEXT_PUBLIC_ADMIN_TOKEN` match |
| Workspace stays provisioning/offline | backend availability, provision logs, registration, heartbeat, and runtime credentials |
| Template palette is empty | `manifest.json` sources were fetched and the template cache can read them |
| Chat has no reply | workspace is online, the selected runtime booted, and the real provider credential is valid |
| External agent loops on auth | rotate and use the newly shown one-time token; do not paste a re-show placeholder |
| External poll bridge stops after retention | current bridge handles HTTP 410 by resetting to a bounded cold start; refresh an older generated bridge |

For ownership and current architecture, see the [Core technical
reference](./architecture/molecule-technical-doc.md).
