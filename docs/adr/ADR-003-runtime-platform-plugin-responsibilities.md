# ADR-003: Runtime adapts to the platform; the plugin adapts to each runtime

**Status:** Accepted — committed architecture
**Date:** 2026-06-25
**Supersedes context:** RFC `rfc-platform-mcp-as-plugin` §2b/§3.4 (platform-MCP-as-plugin, de-bake)
**Related incident:** 2026-06-25 fleet-wide concierge `degraded` (half-wired `loaded_mcp_tools` producer)

## Context

Molecule runs one agent codebase across many runtimes (claude-code, codex,
hermes, openclaw, gemini-cli) and exposes the same capabilities (management MCP,
A2A, memory) on all of them. Which component adapts to which was, until now,
tribal knowledge: the *plugin → runtime* half was documented, but the
*runtime → platform* status contract lived only in source docstrings, the named
contract doc (`api-protocol/registry-and-heartbeat.md`) was stale, and the
de-bake rationale lived only in an RFC. There was no single canonical statement
and no ADR.

That gap had teeth. On 2026-06-25 every de-baked concierge was marked
`degraded` because the runtime reported `mcp_server_present=true` but never
produced `loaded_mcp_tools` — the producer was a scaffold with **zero callers**,
`omitempty` masked it, the unit tests bypassed the production gate with
`force=True`, and the only end-to-end check was a non-deterministic LLM
self-enumeration that was also `continue-on-error` and didn't run on PRs. A
half-wired producer crossing the runtime↔platform seam shipped silently.

## Decision

Adopt as **committed architecture** the two-layer, opposite-direction
responsibility split, and **enforce it with guardrails** so it cannot regress to
tribal knowledge:

1. **The runtime adapts the agent to the platform.** It owns the register/
   heartbeat **status contract** (`mcp_server_present` *and* `loaded_mcp_tools`),
   reported runtime-agnostically in one place. Every gate-consumed field must
   have a wired producer + a liveness test. The required tool id is pinned in a
   shared contract and **derived** on both sides, never hardcoded per layer.

2. **The plugin adapts its abilities to each runtime.** One runtime-agnostic
   descriptor (the plugin = SSOT); per-runtime renderers write it into native
   config. Renderer + reader + present-probe move in lockstep; an unmapped
   runtime fails **closed**.

3. **Platform-ness is a composition, not an image.** A concierge is an ordinary
   runtime image + the org-admin key + the management MCP plugin. The baked
   `molecule-platform-agent` image is removed and guarded against return;
   "is this a concierge?" is detected via `mcp_server_present()`, not the
   baked-image marker.

The full statement, the field tables, and the guardrail matrix live in
[`architecture/runtime-platform-plugin-responsibilities.md`](/architecture/runtime-platform-plugin-responsibilities).

## Consequences

- New runtimes and new plugins have a single doc to conform to; reviewers have a
  named contract to check against.
- A set of red-on-regression guardrails becomes required (producer-liveness boot
  test, contract-drift blocking with `loaded_mcp_tools` pinned, renderer/reader
  lockstep, deterministic online+`create_workspace` e2e, de-bake absence guard).
  Until each is green it is tracked under the guardrail/SSOT workstream.
- The stale `api-protocol/registry-and-heartbeat.md` "five fields" status model
  is corrected to include the MCP status fields.
