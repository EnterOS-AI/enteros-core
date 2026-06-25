# Runtime ↔ Platform ↔ Plugin: division of responsibilities

> **Status:** Committed architecture (see [ADR-003](/adr/ADR-003-runtime-platform-plugin-responsibilities)).
> This page is the single canonical statement of *who adapts to whom*. If you are
> wiring a new runtime, a new plugin, or anything the online/degraded gate reads,
> start here.

Molecule runs the **same agent code across many runtimes** (claude-code, codex,
hermes, openclaw, gemini-cli, …) and exposes the **same capabilities** (the
management MCP, A2A, memory) regardless of which runtime an org picks. Two
adapter layers make that work, and they have **opposite directions**:

| Layer | Adapts… | …to… | Lives in |
|-------|---------|------|----------|
| **Runtime** | the agent | **the platform** | `molecule-ai-workspace-runtime` (shared pip pkg) |
| **Plugin**  | its abilities | **each runtime** | the plugin (descriptor) + per-runtime renderers |

Stated as one sentence each:

- **The runtime's job is to adapt the agent to the platform.** It registers,
  heartbeats, and reports the status the platform's online/degraded gate reads —
  **runtime-agnostically, in one place**. The platform never asks "which runtime
  is this?"; it reads a fixed status contract.
- **The plugin's job is to adapt its abilities to each runtime.** It ships one
  runtime-agnostic descriptor; a per-runtime renderer writes that descriptor into
  the runtime's *native* MCP config. The same plugin works on every runtime, and
  the agent stays runtime-switchable.

## 1. Runtime → Platform: the status contract

The runtime reports to the platform on **register + every heartbeat** via one
payload builder (`platform_agent_identity.identity_gate_payload`). The platform's
fail-closed online/degraded gate (`workspace-server/internal/handlers/registry.go`)
consumes it. The load-bearing fields:

| Field | Meaning | Consumed by |
|-------|---------|-------------|
| `mcp_server_present` | the management `molecule-platform` MCP is **declared/wired** in the active runtime's native config | RCA#2970 online gate |
| `loaded_mcp_tools` | the management MCP's tools **actually loaded** into the model (the `mcp__<server>__<tool>` ids) | core#3082 degrade gate |

**Both halves are required.** "Declared" (`mcp_server_present`) is necessary but
not sufficient — a declared-but-dead server is the exact false-green core#3082
catches, so the runtime must also report what *loaded* (`loaded_mcp_tools`). The
runtime produces `loaded_mcp_tools` **at init** (it enumerates the connected MCP
servers itself — `loaded_mcp_tools_probe.py`), so the gate can flip
`degraded → online` **without waiting for a user turn**. This enumeration is
runtime-agnostic: it speaks the MCP wire protocol directly, not any one SDK's
tool-list message.

**Rule:** any field the gate reads MUST have a runtime-side **producer** that is
actually wired (not just a scaffold) and a **liveness test**. A producer that is
unreferenced serializes (`omitempty`) identically to a correctly-warming-up one —
that is how a half-wired producer silently degrades every concierge (the
2026-06-25 incident). The contract field + the required tool id
(`mcp__molecule-platform__create_workspace`) are pinned in
`contracts/mcp-plugin-delivery.contract.json`; **both sides derive from it**,
neither hardcodes the literal.

## 2. Plugin → Runtime: per-runtime rendering

The plugin declaration is the **single source of truth** for its MCP descriptor
(server name, command, args, env). A per-runtime **shape adapter** renders that
one descriptor into the runtime's native config:

| Runtime | Native MCP config |
|---------|-------------------|
| claude-code | `.claude/settings.json` (`mcpServers`) |
| codex | `~/.codex/config.toml` |
| gemini-cli | `settings.json` |
| openclaw | `~/.openclaw/openclaw.json` |

Renderers + their inverse readers + the present-probe live in
`molecule-ai-workspace-runtime/molecule_runtime/mcp_render.py`
(`_RUNTIME_SPECS` / `render_for_runtime` / `read_mcp_servers_for` /
`management_mcp_present_for`), dispatched **by runtime name** — never by a baked
image. See [agentskills-compat](/plugins/agentskills-compat) and
[cli-runtime](/agent-runtime/cli-runtime) for the plugin author's view.

**Rule:** adding a runtime means adding its renderer **and** reader **and**
present-probe together (kept in lockstep by a test). An unmapped runtime must
fail **closed**, never silently fall back to `.claude/settings.json` (the #3159
mis-attribution class).

## 3. Corollaries

- **No logic in the wrong layer.** The platform-facing gate has zero
  `if runtime == …` branches; runtime selection happens once via `ADAPTER_MODULE`
  → `get_adapter`, and per-runtime shape happens via the `mcp_render` dispatch.
  No platform logic is baked into a plugin.
- **Tool ids are SSOT.** `mcp__molecule-platform__create_workspace` is defined
  once (the contract) and derived everywhere — core's gate, the runtime emitter,
  and the e2e all read it. Re-spelling it per layer is how the codex#142
  `mcp__platform__` vs `mcp__molecule-platform__` drift happened.
- **Platform-ness is a composition, not an image.** A platform concierge is an
  **ordinary runtime image** plus (a) the org-admin key and (b) the management
  MCP plugin — *not* a special baked `molecule-platform-agent` image. The baked
  image structurally binds the concierge to claude-code; for any non-claude-code
  concierge it is not just redundant but **wrong**
  ([rfc-platform-mcp-as-plugin](/design/rfc-platform-mcp-as-plugin) §3.4). Detect
  "is this a concierge?" via the **composition** (`mcp_server_present()`), never
  the baked-image marker (`MOLECULE_PLATFORM_AGENT_IMAGE_BAKED`), which is absent
  on a de-baked concierge.

## 4. How this is enforced (guardrails)

Each rule above is (or is becoming) a red-on-regression test, so the principle
can't drift back into tribal knowledge:

| Rule | Guardrail |
|------|-----------|
| Plugin renders per-runtime native config | `test_mcp_plugin_delivery_contract` (codex writes `config.toml`, not claude settings) |
| No runtime branching in the gate | gate has zero `if runtime==`; `…RoutesThroughPort` |
| Renderer/reader/present-probe lockstep | `test_mcp_render_lockstep` (`set(_RUNTIME_SPECS)==set(_RUNTIME_READERS)`) |
| Unmapped runtime fails closed | G6 `test_mcp_render_completeness_g6` |
| Tool-id / prompt SSOT | G0/G1 + `test_mcp_ssot`; contract-derivation tests both sides |
| `loaded_mcp_tools` producer is live | producer-liveness boot test (real gate, no `force`) |
| `loaded_mcp_tools` contract pinned both sides | `mcp-plugin-delivery-contract-drift` (branch-protection blocking, runtime copy in the compare set) |
| Concierge reaches online + has create_workspace | deterministic e2e asserting `status=online` + heartbeat `loaded_mcp_tools` ∋ `create_workspace` (not LLM self-enumeration) |
| Baked image cannot return | de-bake absence guard (fails if `Dockerfile.platform-agent` / `resolvePlatformAgentImage` / baked publish job reappear) |

See the guardrail audit (2026-06-25) for the current enforced/gap status; items
not yet green are tracked under the guardrail/SSOT workstream.
