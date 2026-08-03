# RFC follow-up: Extract computer-use into a native plugin

- **Date filed:** 2026-07-27
- **Status:** Deferred — do NOT action now. Placeholder for a later cycle.
- **Parent design:** `2026-07-27-agent-desktop-sidecar-design.md`
- **Decision that created this:** Build computer-use in workspace-server **core** for now, behind an
  SDK-SSOT contract seam, and extract it to a plugin later. (User, 2026-07-27.)

## Why deferred

The platform MCP bridge (`internal/handlers/mcp.go`) is a **static, compile-time** tool surface —
a hand-maintained `mcpAllTools` slice + `dispatch` switch, advertising `listChanged:false`, with
**no plugin/config/DB path to register a tool**. Making a plugin contribute a tool to that bridge
("Option C") is net-new architecture (a `tools` contribution point + a dynamic tool registry +
plugin→platform dispatch routing) with no precedent. Not worth blocking the desktop feature on.

## The seam we ARE building now (so extraction is a refactor, not a rewrite)

- **Interface = an SDK-SSOT contract.** Author `contracts/tool/computer-use.contract.json` in
  `molecule-ai-sdk`, codegen `molcontracts.ComputerUseContract`, and have the core impl **consume +
  value-pin** it — the exact precedent of `internal/handlers/mcp_plugin_delivery_contract.go`
  (`MatchesSSOT`). The contract pins: the fixed display geometry (the §3 "one number in three
  places" invariant), the action enum (screenshot/click/type/key/scroll), the control-server
  protocol (`GET /screenshot`, `POST /input`), and the image-result block shape.
- **Impl stays platform-Go** (proxy, SSRF, scale-from-zero, control-lock arbitration) — these are
  inherently platform duties a plugin cannot perform alone.

## What the later extraction entails ("Option B")

Ship computer-use as a `kind:mcp` **native plugin** delivering an in-container / sidecar MCP server,
reusing the concierge-MCP pattern wholesale (native-plugins registry → declare → reconcile →
`mcpServers` delivery → runtime launches it). This aligns with **reconciling with the existing
runtime desktop impl** `molecule-ai-workspace-runtime/molecule_runtime/a2a_tools_desktop.py`
(the `desktop_*` tools already live in-container — that IS Option B's shape).

Open gaps to resolve at extraction time:
1. The platform-side duties (authed proxy, scale-from-zero, control-lock) can't move into an
   in-container plugin — they need the `audience`/inbound-secret credential seam, which is
   `self`-only / half-built today (`catalog_gen.go:171` + the MCP-delivery contract's
   audience→credential map). Finish that seam.
2. The text-only MCP result gap applies to whichever surface returns the screenshot — the contract
   (above) is the right place to pin the image-block shape so both consumers agree.
3. Decide replace-vs-repoint for the existing `a2a_tools_desktop.py` `desktop_*` tools so two
   coordinate/lifecycle contracts don't coexist.

## Acceptance for this follow-up

Extraction is "done" when the same `ComputerUseContract` governs BOTH the in-container tool surface
(delivered as a native plugin) AND the platform-side control-server/proxy, with no behavior change
visible to the agent or the canvas.
