# Skills: Current Usage Guide

Molecule does not currently ship a hosted skill marketplace or a workspace-CLI
command family for installing, upgrading, listing, scaffolding, bundling, or
removing skills. The available skills are the directories delivered with the
active workspace configuration or contributed by installed plugins.

The authoritative runtime behavior lives in
`molecule_runtime/skill_loader/loader.py`. See the maintained
[Skills reference](../agent-runtime/skills.md) for the loader contract and
frontmatter fields.

## What a skill is

A workspace skill is a directory below the active config directory's `skills/`
folder. It may be declarative-only or include executable helpers:

```text
skills/web-research/
├── SKILL.md
├── scripts/
│   └── search_sources.py
├── examples/             # optional reference material
└── references/           # optional reference material
```

Only these paths have automatic runtime behavior:

- `SKILL.md` supplies frontmatter metadata and instructions.
- Top-level, non-underscore `scripts/*.py` modules are imported and scanned for
  `langchain_core.tools.BaseTool` objects.

The skill loader does **not** import a `tools/` directory. Other files may
support the instructions, but the runtime does not automatically inject every
file in the package.

## Enable a workspace skill

Place the directory under `<config-path>/skills/` and list its directory name
in the active `config.yaml`:

```yaml
skills:
  - web-research
  - code-review
```

The value is a list of directory-name strings, not catalog records or source
objects. Do not put skill names under the separate `tools:` key.

The active config path is normally `/configs` in a workspace image. If runtime
config loading falls back to a baked template, the runtime adopts that
template's directory as the config base, so skills must be delivered alongside
the config that actually loaded.

There are three current delivery patterns:

1. A workspace template ships the skill directory and names it in
   `config.yaml`.
2. An installed plugin contributes one or more skill directories through the
   plugin lifecycle.
3. A local or self-hosted operator places files in the active config directory
   for development.

The workspace CLI starts and manages a workspace; it does not perform any of
these skill-package operations. For plugin delivery, use Core's plugin
lifecycle described in [Plugin install sources](../plugins/sources.md).

## Write `SKILL.md`

```markdown
---
name: web-research
description: Finds and compares primary technical sources
tags: [research, web]
examples:
  - Compare two API contracts and cite the differences.
runtime: [claude-code, codex, hermes, openclaw]
---

# Web Research

Use primary sources. Record the source URL for every claim that could change.
```

The Markdown body becomes the skill instructions in the agent's system prompt.
The current runtime consumes `name`, `description`, `tags`, `examples`, and
`runtime`. The directory name is the skill ID and is also the default display
name.

Use `runtime: ["*"]` or omit the field for a universal skill. A string or list
of adapter names can limit loading to compatible runtimes. An incompatible
skill is skipped rather than partially loaded.

Runtime parsing tolerates malformed frontmatter with warnings so a bad metadata
block does not crash workspace startup. Treat those warnings as authoring
errors; do not depend on the tolerant fallback as a packaging contract.

## Add executable helpers

Executable helpers belong in `scripts/`, not `tools/`:

```python
from langchain_core.tools import tool


@tool
def summarize_file(path: str) -> str:
    """Summarize a text file in the workspace."""
    ...
```

Each top-level `scripts/*.py` module executes when the skill loads. The loader
collects objects produced by `@tool` because they implement `BaseTool`.
Underscore-prefixed modules and nested Python modules are not discovered by
this loader path.

Skill scripts execute inside the agent process. The loader temporarily removes
a fixed set of common credential environment variables during import, and the
configured dependency scanner can warn or block before import, but neither is
a sandbox. Review code and dependencies before delivering an executable skill.

## Reload and removal behavior

The runtime watches files inside skill directories already named in
`config.yaml`. A content change is detected after the polling/debounce window
and the runtime attempts to replace that skill's instructions and tools through
the active adapter callback.

Changing the configured skill-name list is a different lifecycle event. Restart
the workspace after adding or removing an entry unless the active runtime or
management surface explicitly performs that restart.

To remove a directly configured skill:

1. Remove its name from `config.yaml`.
2. Restart the workspace.
3. Remove the directory when it is no longer referenced.

For a plugin-contributed skill, use the plugin lifecycle instead. Workspace
skills load before plugin skills, and a plugin skill with an already-loaded
skill ID is skipped.

## Troubleshooting

**`SKILL.md not found` in logs:** The configured string must match a directory
under `<config-path>/skills/`, and that directory must contain `SKILL.md`.

**Instructions load but executable helpers do not:** Check that the files are
top-level `scripts/*.py`, do not begin with `_`, import successfully, and expose
`BaseTool` objects. A `tools/` directory has no loader semantics.

**Skill is skipped for this runtime:** Check the `runtime` frontmatter against
the active adapter name.

**Security scan blocks the skill:** Review the scanner output and the skill's
dependencies. Do not disable scanning merely to force an unreviewed module to
load.

**A new configured skill does not appear after editing `config.yaml`:** Restart
the workspace. Hot reload watches the contents of names selected at startup; it
is not a package installer or config-list watcher.

## Skills and plugins

A skill contributes instructions and optional in-process tools. A plugin is a
separate Core-managed package that may contribute skills, rules, prompts, MCP
descriptors, adapters, or daemons. Do not use the terms interchangeably.

## Related docs

- [Skills reference](../agent-runtime/skills.md)
- [Runtime config](../agent-runtime/config-format.md)
- [Workspace runtime](../agent-runtime/workspace-runtime.md)
- [Plugin and agentskills.io compatibility](../plugins/agentskills-compat.md)
- [Plugin install sources](../plugins/sources.md)
