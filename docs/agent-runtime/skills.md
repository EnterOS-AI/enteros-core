# Skills boundary

A skill is a runtime-consumed instruction/asset bundle, conventionally rooted
at `SKILL.md`. Core exposes skill, plugin, file, and lifecycle surfaces, while
the selected runtime owns discovery, prompt injection, invocation, and reload
semantics.

## Current rules

- Template/plugin sources are pinned through `manifest.json`.
- Plugin install sources must use a scheme registered by Core's source
  registry; see [Plugin Sources](../plugins/sources.md).
- Runtime compatibility is explicit. Installing a plugin does not guarantee
  that every runtime can load its skills or tools.
- Editing files inside an already selected skill may use the runtime's narrow
  watcher path. Adding/removing selected skills or changing arbitrary config or
  prompt files requires restart unless the exact runtime version says otherwise.
- A memory record is not automatically a skill. Promotion into reusable
  procedure is a separate reviewed lifecycle.

Do not document a LangGraph decorator, Claude-specific harness path, or other
adapter implementation as the universal skill contract.

The current runtime code, plugin manifest, Core plugin compatibility endpoints,
and template pin are authoritative.
