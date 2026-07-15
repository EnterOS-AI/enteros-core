# Contributing to Molecule AI

Thanks for your interest in contributing to Molecule AI! This guide covers the
development workflow, conventions, and how to get your changes merged.

## Getting Started

### Prerequisites

- **Go 1.25+** — platform backend
- **Node.js 20+** — canvas frontend
- **Python 3.11+** — workspace runtime
- **Docker** — infrastructure services (Postgres, Redis)
- **Git** — with hooks path set to `.githooks`
- **jq** — parses `manifest.json` during `setup.sh` to clone the
  template/plugin registry. Install via `brew install jq` (macOS) or
  `apt install jq` (Debian). Without it, setup.sh prints a note and
  leaves the registry dirs empty (recoverable by installing jq and
  re-running).

### Setup

```bash
# Clone the repo
git clone https://git.moleculesai.app/molecule-ai/molecule-core.git
cd molecule-core

# Install git hooks
git config core.hooksPath .githooks

# Copy and edit .env (generate ADMIN_TOKEN + SECRETS_ENCRYPTION_KEY)
cp .env.example .env

# Start infrastructure (Postgres, Redis, MinIO, Langfuse)
./infra/scripts/setup.sh

# Build and run the platform — applies pending migrations on first boot
cd workspace-server
go run ./cmd/server

# In a separate terminal, run the canvas
cd canvas
npm install
npm run dev
```

### Environment Variables

Copy `.env.example` to `.env` and fill in your values:
```bash
cp .env.example .env
```

See `CLAUDE.md` for a full list of environment variables and their purposes.

## What goes where (content vs code)

This repo is scoped to **code** (canvas, workspace, workspace-server, related
infra). Public content (blog posts, marketing copy, OG images, SEO briefs,
DevRel demos) lives in [`molecule-ai/docs`](https://git.moleculesai.app/molecule-ai/docs).
The `Block forbidden paths` CI gate fails any PR that writes to `marketing/`
or other removed paths — open against `molecule-ai/docs` instead.

| Content type | Target |
|---|---|
| Blog posts | `molecule-ai/docs` → `content/blog/<YYYY-MM-DD-slug>/` |
| Doc pages | `molecule-ai/docs` → `content/docs/` |
| Marketing copy / PMM positioning | `molecule-ai/docs` → `marketing/` |
| OG images, visual assets | `molecule-ai/docs` → `app/` or `marketing/` |
| SEO briefs | `molecule-ai/docs` → `marketing/` |
| DevRel demos (runnable code) | Standalone repo under `molecule-ai/`, OR embedded in `molecule-ai/docs` |
| Launch checklists, internal tracking | Gitea Issues — **not** committed files |
| Engineering docs (`docs/adr/`, `docs/architecture/`, `docs/incidents/`) | This repo (internal, not published) |
| Live product pages (e.g. `canvas/src/app/pricing/page.tsx`) | This repo (these are app code, not marketing copy) |

If a PR fails the `Block forbidden paths` check, the contents belong in
`molecule-ai/docs`. No CI drag, no Canvas E2E, content lands in minutes.

## Development Workflow

### Branch Naming

Use prefixed branches:
- `feat/` — new features
- `fix/` — bug fixes
- `chore/` — maintenance, deps, CI
- `docs/` — documentation only

**Never push directly to `main`.** All changes go through pull requests.

### Commits

Write concise commit messages that focus on the "why":
```
fix(canvas): prevent infinite re-render on WebSocket reconnect

The useEffect dependency array included the entire nodes object,
causing a render loop when any node position changed.
```

### Pull Requests

- Keep PRs focused — one concern per PR
- Include a test plan in the PR description
- PRs are merged with **merge commits** (not squash or rebase)

#### Auto-merge & the "extra commit" trap

**Two system guards protect against pushing commits after auto-merge has been enabled.** Don't try to work around them — they exist because we shipped a half-merged PR on 2026-04-27 (`#2174` merged with only its first commit; the second was orphaned on a branch the host had already deleted).

1. **Repo-wide:** "Automatically delete head branches" is on. Once a PR merges, the branch is deleted server-side. Any subsequent `git push` to that branch fails with `remote rejected — no such branch`.

2. **CI:** the `pr-guards` workflow (calling [molecule-ci `disable-auto-merge-on-push`](https://git.moleculesai.app/molecule-ai/molecule-ci/src/branch/main/.github/workflows/disable-auto-merge-on-push.yml)) fires on every push to an open PR. If auto-merge was already enabled, it's disabled and a comment is posted. You must explicitly re-enable after verifying the new commit.

**Workflow rules that follow from the guards:**
- Push **all** commits before enabling auto-merge on the PR (Gitea's "Merge When Checks Succeed").
- If you realize you need another commit after enabling auto-merge: push it, then **re-enable** auto-merge — the guard will already have disabled it. The disable + re-enable is the verification step.
- For changes that depend on each other across PRs (e.g. a build-script change + a workflow that consumes it), prefer a **stack** of PRs (PR-B branched off PR-A's branch, opened only after PR-A is in queue) over amending one in-flight PR.

### Running Tests

```bash
# Go (platform)
cd workspace-server && go test -race ./...

# Canvas (Next.js)
cd canvas && npm test

# Workspace runtime (Python)
# Runtime code is SSOT in molecule-ai-workspace-runtime, not molecule-core/workspace.
cd ../molecule-ai-workspace-runtime
python -m venv .venv && source .venv/bin/activate
python -m pip install -e '.[test]'
pytest -q

# E2E API tests (requires running platform)
bash tests/e2e/test_api.sh
```

### Pre-commit Hooks

The `.githooks/pre-commit` hook enforces:
- `'use client'` directive on React hook files
- Dark theme only (no white/light CSS classes)
- No SQL injection patterns (`fmt.Sprintf` with SQL)
- No leaked secrets (`sk-ant-`, `ghp_`, `AKIA`)

Fix violations before committing — the hook will reject the commit.

### Development pipeline (SOP-24)

The canonical development pipeline is defined in
[`internal/runbooks/dev-sop.md` §SOP-24](https://git.moleculesai.app/molecule-ai/internal/src/branch/main/runbooks/dev-sop.md)
(private repo). It specifies five ordered stages:

1. **local lint** — pre-commit hooks + linters (stage 1)
2. **compile** — `platform-build` / `canvas-build` build the binaries (stage 2)
3. **local e2e** — the `e2e-api` suite (**merge-blocking**)
4. **staging e2e** — the staging E2E gate run by CP's deploy pipeline (**merge-blocking**)
5. **production monitor** — post-deploy canary soak + rollback authority

Stages 1–2 are implemented by the CI jobs in the table below. Stages 3–4
(local e2e + staging e2e) are **merge-blocking**: a PR cannot merge until both
are green. The provisioner-parity / `e2e-smoke` staging jobs that gate stage 4
run in CP's deploy pipeline, not in this repo's CI — so the table below is the
stage-1/2 job set, **not** the complete merge-gate set. See
`internal/runbooks/dev-sop.md` §SOP-24 for the authoritative gate list.

### CI Pipeline

CI runs on Gitea Actions with self-hosted runners. External contributors:
PRs from forks will not trigger CI automatically. A maintainer will review
and run CI manually.

The jobs below implement **stages 1–2** of the SOP-24 pipeline (local lint +
compile). They are not the full gate set — local-e2e and staging-e2e (SOP-24
stages 3–4) are merge-blocking and partly live in CP's deploy pipeline.

| Job | What it checks |
|-----|---------------|
| platform-build | Go build + vet + `go test -race` |
| canvas-build | npm build + vitest |
| python-lint | pytest with coverage |
| e2e-api | Full API test suite (62 tests) |
| shellcheck | Shell script linting |
| ops-scripts | Python unittest suite for `scripts/*.py` |

### Workspace runtime SSOT

Runtime code lives in
[`molecule-ai-workspace-runtime`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime).
Do not reintroduce `molecule-core/workspace/` or vendored `molecule_runtime/`
copies in consumers. Core and templates consume the published runtime package
from the Gitea package registry.

For local external MCP agents, the multi-workspace config key depends on the
bridge: the maintained Python runtime/MCP CLI uses `MOLECULE_WORKSPACES`, while
`parseWorkspaceTargets` in `@molecule-ai/mcp-server` uses
`MOLECULE_WORKSPACES_JSON`. The shared entry shape and the bridge-specific keys
are documented once, canonically, in
[Multiple workspaces and tenants](docs/guides/external-agent-registration.md#multiple-workspaces-and-tenants);
the operator-facing snippets are generated from
`workspace-server/internal/handlers/external_connection.go`. Do not restate the
shape in a new place — a second copy is how the key names drift, and a snippet
built against the wrong key fails silently.
`platform_url` selects the tenant; `org_id` is not part of this config.
Workspace IDs can differ across orgs.

## Local Testing

### CI ops-script suite
```bash
python3 -m pytest .gitea/scripts/tests/ -q
bash .gitea/scripts/tests/test_ci_status.sh
```
Runs the governance/merge-queue script regression suites. No network access required.

## Code Style

### Go (Platform)
- Standard `gofmt` formatting
- `go vet` must pass
- No `fmt.Sprintf` in SQL queries (use parameterized queries)
- Prefer function injection over import cycles

### TypeScript (Canvas)
- Strict mode enabled
- No `any` types (use `unknown` or proper types)
- Use `ConfirmDialog` component, never native `confirm/alert/prompt`
- Dark theme only — no white/light CSS classes

### Python (Workspace Runtime)
- Type hints on public functions
- pytest for all tests

## External integrations

Code in this repo lands in molecule-core. Some related runtime artifacts
live in their own repos:

- [`molecule-ai/molecule-ai-workspace-runtime`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime) — Python runtime (`molecule_runtime`) that runs inside containerized Molecule workspaces. Its runtime-owned adapter registry, rather than a copied list here, defines the maintained adapters bridged to the A2A queue.
- [`molecule-ai/molecule-ai-sdk`](https://git.moleculesai.app/molecule-ai/molecule-ai-sdk) — `A2AServer` + `RemoteAgentClient` for external agents that register over the public `/registry/register` flow.
- [`molecule-ai/molecule-mcp-claude-channel`](https://git.moleculesai.app/molecule-ai/molecule-mcp-claude-channel) — Claude Code channel plugin. Bridges A2A traffic into a running Claude Code session via MCP `notifications/claude/channel`. Polling-based (no tunnel required); install inside Claude Code via `/plugin marketplace add https://git.moleculesai.app/molecule-ai/molecule-mcp-claude-channel.git` → `/plugin install molecule@molecule-channel`, then launch with `claude --dangerously-load-development-channels=plugin:molecule@molecule-channel`.

When extending the **A2A surface** in molecule-core (`workspace-server/internal/handlers/a2a_proxy.go` etc.), consider whether the change has a downstream impact on the runtime SDK or the channel plugin — they're versioned independently but share the wire shape.

## Architecture Overview

See `CLAUDE.md` for detailed architecture documentation, including:
- Component diagram (Platform, Canvas, Workspace Runtime)
- Key architectural patterns
- Database schema and migrations
- API route reference

## Reporting Issues

Use Gitea Issues with a clear title and reproduction steps. Include:
- What you expected
- What actually happened
- Platform/OS version
- Relevant logs or screenshots

## Security

If you discover a security vulnerability, please report it privately by
opening an issue against `molecule-ai/internal` (a private repo only
maintainers can see) rather than filing a public issue here.

## License

By contributing, you agree that your contributions will be licensed under the
same [Business Source License 1.1](LICENSE) that covers this project.
