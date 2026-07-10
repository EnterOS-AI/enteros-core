# ADR-004: The SDK owns the runtime-adapter contract + official registry; the shared runtime engine is runtime-agnostic

**Status:** Proposed — supersedes ADR-003 §2 (dispatch-location) pending principal ratification
**Date:** 2026-07-09
**Supersedes:** ADR-003 §2 "the plugin adapts to each runtime" — specifically the decision that per-runtime renderers/readers/present-probes live in `molecule_runtime/mcp_render.py` dispatched by runtime name.
**Related:** runtime#181 (loaded_mcp_tools under-emission); the 2026-07-09 drift where a fix *added* per-runtime code to the shared engine instead of removing it.

## Context

ADR-003 established the correct two-layer split (runtime adapts to platform; plugin adapts to each runtime) and made the **Go platform gate** (`registry.go`) runtime-agnostic — zero `if runtime==` branches. That half is right and stays.

But ADR-003 §2 placed the *per-runtime shape* — MCP-config **renderers, their inverse readers, and the present-probe** — in the **shared Python runtime engine** (`molecule_runtime/mcp_render.py`: `_RUNTIME_SPECS` / `_RUNTIME_READERS` / `render_for_runtime` / `read_mcp_servers_for` / `management_mcp_present_for`), dispatched by runtime name, with the rule "adding a runtime means adding its renderer **and** reader **and** present-probe together." `persona_render` (`_RUNTIME_PERSONA`) follows the same shape.

That means **the shared engine holds per-runtime (adapter) code**, and three consequences follow:

1. **"Runtime-agnostic core" is only true for the Go gate, not the engine.** The Python engine that every workspace runs still branches on runtime name via dispatch tables.
2. **A new runtime requires editing the shared engine.** Third-party runtime authors cannot add support without a PR to `molecule_runtime` — there is no "bring your own adapter" seam.
3. **The dispatch tables invite drift, and CI *rewards* it.** `runtime#181` was `_RUNTIME_READERS['hermes'] = {}` — a stub in the shared dict. Both "fixes" tried (add a concrete core reader; or an adapter override plus a `{}` stub) either grow the shared engine's per-runtime knowledge or split renderer-from-reader, violating ADR-003's own lockstep rule. The G6 metagate literally *enforces* that every runtime has a concrete `_RUNTIME_SPECS` entry **in the engine** — so passing CI means extending the very coupling we want gone.

The correct end state, which ADR-003 gestures at only for the tool-id ("*Target:* pin in the contract and derive… in progress"), is broader: the **contract + the registry of officially-supported runtimes belong to the SDK**, the engine holds nothing runtime-specific, and adapters (official or third-party) implement a **contract socket**.

## Decision

Adopt as committed architecture — enforced by guardrails so it cannot regress:

1. **The SDK owns the adapter *contract* (the socket).** A single SSOT declares the methods every runtime adapter MUST implement to satisfy the platform: identity (`name`/`display_name`/`description`), lifecycle (`setup`/`create_executor`), the MCP-config seam (**native-config path/format/key**, `render_mcp_config`, `read_mcp_servers`, `management_mcp_present`, `enumerate_loaded_mcp_tools`), and persona. "Critical" socket methods are mandatory; extra adapter methods are permitted but are *extra*, never depended on by the engine.

2. **The SDK owns the *official registry*.** The set of natively-supported runtimes and their contract metadata (native path pattern, config format, server-map key) lives in the SDK as data. **Third-party developers register their own adapters against the same contract** — the registry is a convenience for first-party support, not a gate on who may implement.

3. **The shared runtime engine (`molecule_runtime`) holds ZERO per-runtime dispatch.** `_RUNTIME_SPECS`, `_RUNTIME_READERS`, `_RUNTIME_PERSONA` are removed. The engine resolves the adapter once (`ADAPTER_MODULE` → `get_adapter`) and calls the socket. Per-runtime shape lives **in the adapter**: official adapters ship in their template repos (claude-code / codex / hermes / openclaw); third-party adapters ship wherever their author wants. The engine never spells a runtime name.

4. **Conformance CI ships FROM the SDK; every adapter inherits it.** The SDK publishes a conformance suite that, given any adapter, asserts it satisfies the socket: renders → reads → present-probes in lockstep (round-trips its own native config), enumerates `loaded_mcp_tools` including the required management tool, and fails **closed** when unmapped. An adapter that does not conform fails **its own** CI. First-party support is proven "e2e against the officially-supported ones" by running the suite across the official registry.

## Consequences

- **`mcp_render.py` / `persona_render.py` per-runtime functions move into their adapters.** Behavior is preserved (byte-identical native-config output — pinned by a golden test during migration). The engine keeps only generic helpers (JSON/TOML/YAML read-write, id normalization) that carry no runtime name.
- **The guardrails invert.** G6 ("every runtime has a concrete `_RUNTIME_SPECS` entry in the engine") and the prescribed `test_mcp_render_lockstep` are **replaced** by the SDK conformance suite (per-adapter, inherited). A new red-on-regression **ratchet** fails any change that *adds* a `_RUNTIME_*` entry to the engine — drift can only shrink.
- **Third-party runtimes become possible** without a `molecule_runtime` PR — they implement the SDK socket and register.
- **Migration is staged, one vertical slice first:** (P1) land the SDK socket + registry + conformance suite; (P2) migrate **hermes** as the reference adapter (already validated live — 46 tools incl. `provision_workspace`) and prove the conformance suite green; (P3) migrate claude-code / codex / openclaw; (P4) delete the engine dispatch tables and flip G6/lockstep → the conformance gate + the ratchet. Each phase is independently mergeable and leaves the fleet green.
- **`api-protocol` / the SDK `registry-contract.md`** remain the workspace↔platform wire contract; this ADR is about the *adapter* contract, a distinct seam.

## Enforcement (guardrail matrix — target)

| Rule | Guardrail | Status |
|------|-----------|--------|
| Engine holds no per-runtime dispatch | ratchet: CI fails if `_RUNTIME_SPECS`/`_RUNTIME_READERS`/`_RUNTIME_PERSONA` gain an entry (→ 0 entries at P4) | ◻ P2 |
| Every adapter satisfies the socket | SDK conformance suite, inherited + run by each adapter's CI | ◻ P1 |
| Officially-supported runtimes pass e2e | conformance suite run across the SDK official registry | ◻ P1 |
| Native-config output unchanged across the move | golden byte-parity test per migrated adapter | ◻ P2–P3 |
| Unmapped runtime fails closed | conformance suite (replaces G6) | ◻ P1 |
| The docs route you here | signpost comment on each dispatch table + `CLAUDE.md`/`AGENTS.md` pointer to this ADR | ◻ P1 |

Until each is green the ◻ items are tracked under the adapter-contract-SSOT workstream. The signpost + pointer (last row) land first so no one extends the dispatch tables while the migration is in flight.
