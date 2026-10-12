# Plugin shapes and Agent Skills compatibility

Molecule plugins are delivered to a workspace by the platform and interpreted
inside the workspace by the installed `molecules-workspace-runtime` package.
The runtime implementation is authoritative; this page describes the current
file shapes without claiming compatibility for products Molecule does not test.

## Skills and rules

The built-in `AgentskillsAdaptor` understands this common shape:

```text
my-plugin/
├── plugin.yaml                 optional platform metadata
├── rules/*.md                  optional always-on instructions
├── *.md                        optional root prompt fragments
├── setup.sh                    optional setup hook
└── skills/
    └── my-skill/
        ├── SKILL.md
        ├── scripts/            optional
        ├── references/         optional
        └── assets/             optional
```

It copies skills into `/configs/skills/<name>/` and appends rules and eligible
root Markdown fragments to the runtime's configured memory file. README,
CHANGELOG, LICENSE, and CONTRIBUTING files are not prompt fragments. Setup
hooks run with known credential variables removed from their environment, but a
plugin is still executable code and must be reviewed like any dependency.

A standalone `SKILL.md` at the plugin root and a `skills/` tree are both
recognized by current platform classification. The exact runtime-native
activation behavior depends on the selected workspace runtime.

## Adapter resolution

For `(plugin name, workspace runtime)`, the runtime resolves adapters in this
order:

1. a curated adapter in `molecule_runtime/plugins_registry/<plugin>/<runtime>.py`;
2. an adapter shipped by the plugin at `adapters/<runtime>.py`;
3. `RawDropAdaptor`, which leaves files in place and returns a warning.

An adapter module exports either an `Adaptor` class or
`get_adaptor(plugin_name, runtime)`. The curated registry lives in the runtime
Python package, not in this repository.

## MCP-server plugins

The built-in `MCPServerAdaptor` currently consumes a
`settings-fragment.json` containing an `mcpServers` block and merges that block
into the Claude-compatible `.claude/settings.json` layer. It delegates skills,
rules, prompt fragments, and setup to `AgentskillsAdaptor`.

Do not infer Codex, Gemini, Hermes, or another runtime's MCP config shape from
this page. A plugin must ship or select an adapter that implements that
runtime's current native configuration. On uninstall, the current MCP adapter
does not remove merged MCP entries automatically because those entries may be
shared or manually maintained.

## Authoring guidance

- Keep each `skills/<name>/SKILL.md` valid for the Agent Skills convention if
  portability matters.
- Put Molecule-specific always-on instructions under `rules/`.
- Ship `adapters/<runtime>.py` only when the built-in shape is insufficient.
- Test install and uninstall on every runtime declared by the plugin.
- Pin remote plugin sources; see [Plugin install sources](./sources.md).

Current implementation authority:

- runtime: `molecule_runtime/plugins_registry/` in the
  `molecule-ai-workspace-runtime` repository;
- platform delivery and classification:
  `workspace-server/internal/handlers/plugins_*` in this repository.

The platform-management entitlement model is described in
[Platform MCP as a plugin](../design/rfc-platform-mcp-as-plugin.md); that RFC is
design context, while the checked handlers and runtime package define shipped
behavior.
