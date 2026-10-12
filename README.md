<div align="center">

<p>
  <img src="./docs/assets/branding/molecule-icon.svg" alt="Molecule AI" width="160" />
</p>

<p>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./docs/assets/branding/molecule-text-white.png">
    <img src="./docs/assets/branding/molecule-text-black.png" alt="Molecule AI" width="420" />
  </picture>
</p>

<p>
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<h3>The org-native control plane for heterogeneous AI-agent workspaces</h3>

[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-orange.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-1.25+-00ADD8?logo=go)](https://go.dev/)
[![Python Version](https://img.shields.io/badge/python-3.11+-3776AB?logo=python)](https://www.python.org/)
[![Next.js](https://img.shields.io/badge/Next.js-15-black?logo=next.js)](https://nextjs.org/)

<p>
  <a href="./docs/index.md"><strong>Docs</strong></a> •
  <a href="./docs/quickstart.md"><strong>Quick start</strong></a> •
  <a href="./docs/architecture/molecule-technical-doc.md"><strong>Technical reference</strong></a> •
  <a href="./docs/api-protocol/platform-api.md"><strong>Platform API</strong></a>
</p>

</div>

## Quick start

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-core.git
cd molecule-core
./scripts/dev-start.sh
```

Open [http://localhost:3000](http://localhost:3000). See the
[quick-start guide](./docs/quickstart.md) for prerequisites, manual startup,
first-run configuration, and troubleshooting.

## OpenAI Codex & GPT-5.6

Codex is both a product surface and a build tool for EnterOS:

- **Codex as a first-class agent runtime.** The
  [`codex` workspace template](./workspace-configs-templates/codex/) wraps
  Codex CLI (`@openai/codex`) as an EnterOS workspace runtime: each tenant
  session holds a long-lived `codex app-server` child bound to one thread, so
  agent-to-agent messages process in order with full conversation continuity.
  Provisioning a Codex workspace is a single `provision_workspace` call from
  the platform agent — the same runtime-contract SDK drives Codex and five
  other runtimes identically.
- **GPT-5.x model routing.** A provider registry in the template's
  `config.yaml` routes auth via a ChatGPT/Codex subscription
  (`CODEX_AUTH_JSON`), a direct `OPENAI_API_KEY`, or any endpoint speaking the
  OpenAI Responses API; GPT-5-family models are selectable per workspace.
- **Codex in the build loop.** During OpenAI Build Week (Jul 13–21, 2026),
  Codex CLI sessions running GPT-5.6-codex were used to implement and review
  changes shipped to this repository; our CI/merge pipeline (all-green status
  gate plus reviewer approval) applied to that agent-authored work the same as
  to any human contribution.

The canonical public mirror of this repository is
[github.com/EnterOS-AI/enteros-core](https://github.com/EnterOS-AI/enteros-core).

## What this repository owns

`molecule-core` contains the tenant workspace server and Canvas. Together they
provide:

- authenticated workspace lifecycle and backend dispatch;
- a `parent_id` organization hierarchy used for peer discovery and
  communication authorization;
- registry, heartbeat, Agent Card, A2A proxy, poll-delivery, activity, and
  approval surfaces;
- scoped agent memory and key/value workspace-memory APIs;
- encrypted secrets, files, terminal, templates, bundles, schedules, and
  operational views; and
- live Canvas updates through WebSocket fanout.

A workspace is a durable organizational role, not a task node. Teams are
composed by creating or reparenting workspace rows. Canvas's **Expand Team
View** and **Collapse Team View** controls only show or hide existing
descendants; they do not provision or delete workspaces.

## Runtime boundary

Agent execution lives in the maintained workspace-runtime and workspace-template
repositories. Core stores and forwards supported configuration, supplies
authenticated platform and hierarchy context, and dispatches lifecycle work to
the configured backend.

[`manifest.json`](./manifest.json) is the checked-in source of truth for the
template and plugin repositories Core currently offers. Every entry is pinned to
an immutable commit. Do not copy a fixed runtime count or a mutable `main` ref
into documentation; inspect the manifest and the runtime-owned parser instead.

The retired `shared_context` parent-file injection model and destructive team
expand/collapse routes are not current runtime contracts.

## Architecture at a glance

```text
Canvas (Next.js)  <-- HTTP / WebSocket -->  Workspace server (Go / Gin)
                                              |            |
                                           Postgres      Redis
                                              |
                                   configured lifecycle backend
                                              |
                              pinned workspace-template image
                                              |
                                    workspace runtime / agent
```

- Postgres domain tables are authoritative for durable current state.
- Redis supports liveness, cache, and fanout; it is not the workspace source of
  truth.
- `structure_events` is append-only selected lifecycle history, not a complete
  event source.
- Local and control-plane provisioning are implementations behind shared
  dispatchers. Tier does not select a cloud vendor.
- Deployment topology is environment-specific. This repository does not promise
  a universal EC2, Railway, Render, ECR, Neon, or co-location shape.

See the [current technical reference](./docs/architecture/molecule-technical-doc.md)
for the code-backed boundaries and source files.

## Repository layout

| Path | Purpose |
|---|---|
| `workspace-server/` | Go APIs, auth, lifecycle, registry, hierarchy, A2A, memory, bundles, and backend dispatch |
| `canvas/` | Next.js operational UI |
| `workspace-server/migrations/` | Durable schema |
| `.gitea/workflows/` | Active CI, release, and deployment automation |
| `manifest.json` | Immutable template/plugin catalog |
| `docs/` | Focused architecture, protocol, development, and runbook references |

## Deployment and verification

Canonical SCM and automation are on
[`git.moleculesai.app`](https://git.moleculesai.app/molecule-ai/molecule-core).
Changes ship through the active Gitea Actions workflows after normal review and
merge; there is no documented operator-host or one-click Railway/Render deploy
path.

A merged PR is not, by itself, proof that a user-visible environment is current.
Verify the exact commit's terminal workflow results and the relevant staging or
runtime health surface.

## Documentation map

- [Docs home](./docs/index.md)
- [Quick start](./docs/quickstart.md)
- [Core technical reference](./docs/architecture/molecule-technical-doc.md)
- [Platform API](./docs/api-protocol/platform-api.md)
- [Communication rules](./docs/api-protocol/communication-rules.md)
- [Registry and heartbeat](./docs/api-protocol/registry-and-heartbeat.md)
- [Event log](./docs/architecture/event-log.md)
- [Runtime config boundary](./docs/agent-runtime/config-format.md)
- [Canvas](./docs/frontend/canvas.md)
- [Local development](./docs/development/local-development.md)

## License

[Business Source License 1.1](LICENSE), copyright © 2025 Molecule AI. The
license converts to Apache 2.0 on January 1, 2029; see the license text for the
complete terms.
