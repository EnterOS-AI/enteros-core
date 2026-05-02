# Runtime native-MCP push parity — status

**Goal:** every workspace runtime delivers Molecule A2A inbox messages
with the same UX as claude-code's MCP `notifications/claude/channel`
push: session continuity + queued or interrupted handling of new
messages mid-thread, no fresh subprocess per message.

Tracked across four runtime streams. Updated 2026-05-02.

---

## claude-code

**Status:** ✅ Done. Native MCP `notifications/claude/channel` push
shipped via `workspace/a2a_mcp_server.py`. Requires the host to launch
with `--dangerously-load-development-channels server:molecule`.

No further work.

---

## OpenClaw

**Status:** Scaffolded; awaiting validation + companion adapter rewrite.

**Path:** Channel-plugin SDK (`openclaw/plugin-sdk`), auto-discovered
from `~/.openclaw/plugins/<name>/` or workspace `.openclaw/`. Plugin
registers an HTTP webhook on `openclaw gateway`; Molecule workspace
adapter POSTs A2A messages to it; gateway dispatches through the same
`dispatchReplyWithBufferedBlockDispatcher` kernel call native channels
(Telegram, Lark, Slack, Discord) use.

**Artifacts landed:**
- `molecule-ai-workspace-template-openclaw/packages/openclaw-channel-plugin/`
  - `package.json`, `openclaw.plugin.json` (manifest), `index.ts`
    (channel + webhook handler), `README.md`, `tsconfig.json`
- Pre-release `v0.1.0-pre`. Mirrors `rabbit-lark-bot` reference
  plugin shape.

**Remaining (task #84, #87):**
1. Validate against a running OpenClaw gateway. Open questions in the
   plugin README: `resolveAgentRoute` peer-id shape,
   `dispatchReplyWithBufferedBlockDispatcher` async semantics,
   `outbound.sendText` no-op safety.
2. Rewrite Python adapter (`adapter.py`) to stop shelling out
   `openclaw agent --message ...` and instead POST to the plugin's
   webhook + run `/agent-reply` callback HTTP server. **Post-demo
   work** (touches a working integration).

---

## hermes

**Status:** Upstream PR drafted; short-term shim deemed unnecessary.

**Path:** Open the upstream `BasePlatformAdapter` system to external
plugins. Hermes already ships a working plugin discovery system for
memory backends (`plugins/memory/`, `register(ctx)` collector pattern,
`$HERMES_HOME/plugins/<name>/` user-installed tier). The PR extends
the same shape to platforms — `register_platform_adapter(...)` on the
existing collector, new `plugins/platforms/` discovery directory,
3-line fallback in `_create_adapter()`. Symmetric, not novel.

**Artifacts landed:**
- `docs/integrations/hermes-platform-plugins-upstream-pr.md` — full
  PR draft including problem, prior art, proposal, code shape,
  backward compat, test plan, and four open questions to resolve in
  Discord before submitting.

**Why no short-term polling shim:** earlier framing was wrong. Molecule
runtime already polls the inbox via `wait_for_message` per turn; each
polled message fires a fresh `execute()` on the adapter, which
proxies to hermes's stateless `/v1/chat/completions`. Adding adapter-
side polling would be duplicate work. The genuine short-term gap is
**session continuity** (hermes daemon doesn't see a single
conversation across turns because chat/completions is stateless), not
push latency. That gap is solved by the upstream PR; no
intermediate shim earns its complexity.

**Remaining (task #83):**
1. Reach out in Nous Research Discord to validate open questions
   (Platform enum-vs-string refactor, naming, example-plugin scope).
2. Submit PR to `NousResearch/hermes-agent`. **Requires user
   confirmation** — opening an upstream PR is an action visible to
   others.
3. Once merged: ship `hermes-platform-molecule-a2a` as the first
   external consumer, bump our hermes workspace template to enable
   it, remove any transitional code.

---

## Codex (OpenAI Codex CLI)

**Status:** Template structurally complete (12 files, 12/12 tests passing,
validated against real codex-cli 0.72.0). Awaiting molecule-core
registry integration + E2E.

**Path:** Persistent `codex app-server` stdio JSON-RPC client
(NDJSON-framed, v2 protocol). One app-server child per workspace
session; one `thread/start` per session; each A2A message becomes a
`turn/start` RPC; agent responses arrive as
`agent_message_delta` notifications. Per-thread serialization for
mid-turn arrivals (matches OpenClaw's per-chat sequentializer).
Optional `turn/interrupt` for "latest message wins" workspaces.

**Artifacts landed:**
- `docs/integrations/codex-app-server-adapter-design.md` — full design
  including RPC sequence, executor skeleton, eight open questions.
- `molecule-ai-workspace-template-codex/` — full template repo
  scaffolded:
  - `app_server.py` (286 LOC) — async JSON-RPC over NDJSON stdio
  - `executor.py` (~270 LOC) — thread bootstrap, turn dispatch,
    notification accumulation, mid-turn serialization
  - `adapter.py` — thin `BaseAdapter` shell + preflight
  - `Dockerfile`, `start.sh`, `config.yaml`, `requirements.txt`,
    `README.md`
  - `tests/` — **12/12 tests pass** (7 vs NDJSON mock child, 5 vs
    fake AppServerProcess covering executor logic)

**Validated against live `codex-cli 0.72.0`:** NDJSON framing,
`initialize` handshake, AND `thread/start` all work end-to-end.
**Schema-runtime drift caught:** real binary returns `thread.id`,
not `thread.threadId` as the JSON schema claims. Executor now
accepts both shapes; without the smoke test this would have been
a production bug.

**Remaining (task #85, #86):**
1. Register `codex` in molecule-core's `manifest.json` +
   `workspace-server/internal/handlers/runtime_registry.go`.
   **Defer to post-demo** — touches working live registry.
2. E2E verification with a real Molecule workspace + peer A2A
   traffic, per `feedback_close_on_user_visible_not_merge`.

---

## Cross-cutting (task #86)

End-to-end verification per `feedback_close_on_user_visible_not_merge`.
For each runtime, the closure criterion is not "code merged" but
"observed: real workspace boots → A2A message from peer agent →
delivered to running session → reply returned through A2A response
queue → peer agent receives". No runtime stream closes until that
chain is observed.

---

## What's blocking what

| Stream | Blocked on |
|---|---|
| claude-code | (done) |
| OpenClaw plugin | live gateway validation, then post-demo adapter rewrite |
| OpenClaw adapter rewrite | post-demo timing |
| hermes upstream PR | user confirmation to submit + Discord pre-validation |
| hermes consumer plugin | upstream PR merging |
| codex implementation | resolve 8 open questions, then post-demo eng time |
| E2E verification | each runtime stream completing |

Three of four runtime streams are at decision points needing user
input. Pre-demo (T-4d to 2026-05-06), the safe move is to land the
remaining design + scaffolding work and defer all behavioral changes to
post-demo.
