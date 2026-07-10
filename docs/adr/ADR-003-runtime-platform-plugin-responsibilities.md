# ADR-003: Runtime adapts to the platform; the plugin adapts to each runtime

**Status:** Accepted — committed architecture (§2 dispatch-location superseded by [ADR-004](/adr/ADR-004-sdk-owns-adapter-contract-and-registry))
**Date:** 2026-06-25
**Supersedes context:** RFC `rfc-platform-mcp-as-plugin` §2b/§3.4 (platform-MCP-as-plugin, de-bake)
**Related incident:** 2026-06-25 fleet-wide concierge `degraded` (half-wired `loaded_mcp_tools` producer)

> **⚠ Superseded in part by ADR-004.** The two-layer, opposite-direction split
> (runtime adapts to platform; plugin adapts to each runtime) STANDS. What
> changes: §2 placed the per-runtime renderers/readers/present-probes in the
> **shared engine** (`molecule_runtime/mcp_render.py` `_RUNTIME_SPECS`/
> `_RUNTIME_READERS`, `persona_render._RUNTIME_PERSONA`). ADR-004 moves the
> per-runtime *shape* into the **adapter socket** (SDK-owned contract + official
> registry) so the shared engine holds **zero** runtime-specific code and names no
> runtime. Read ADR-004 before touching the dispatch tables — they are being
> deleted, not extended.

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
   have a wired producer + a liveness test. The required tool id should be pinned
   in a shared contract and **derived** on both sides rather than hardcoded per
   layer — *target state*; today core holds it as a literal const guarded by a
   drift test and the runtime enumerates it live (contract-pin in progress).

2. **The plugin adapts its abilities to each runtime.** One runtime-agnostic
   descriptor (the plugin = SSOT); per-runtime renderers write it into native
   config. Renderer + reader + present-probe move in lockstep; an unmapped
   runtime fails **closed**.

3. **Platform-ness is a composition, not an image.** A concierge is an ordinary
   runtime image + the org-admin key + the management MCP plugin. "Is this a
   concierge?" is detected via `mcp_server_present()`, not the baked-image marker
   (runtime#181). The baked `molecule-platform-agent` image is being removed and
   guarded against return — *in progress* (artifact deletion + absence guard,
   #78); until then the CP still carries vestigial `resolvePlatformAgentImage`
   references.

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
