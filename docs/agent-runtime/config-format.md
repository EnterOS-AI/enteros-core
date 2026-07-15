# Runtime `config.yaml` boundary

The workspace-runtime package, not Core, owns the accepted `config.yaml`
schema and reload behavior. This Core-local page previously duplicated that
parser and drifted into unsupported fields, runtime names, and hot-reload
claims.

Use the docs repository's [Config Format source
reference](https://git.moleculesai.app/molecule-ai/docs/src/branch/main/content/docs/agent-runtime/config-format.md)
and verify parser changes against these runtime sources. Publishing that source
to a public site is a separate deployment concern.

- `molecule_runtime/config.py`
- `molecule_runtime/prompt/builder.py`
- `molecule_runtime/skills/watcher.py`

Stable boundaries that Core consumers may rely on:

- `shared_context` and automatic parent-file prompt injection are retired;
- old top-level `memory:` and `env:` blocks are not the current runtime
  credential/configuration contract;
- `sub_workspaces` is compatibility data and does not provision children;
- changing `config.yaml`, prompt files, the selected skill list, or runtime
  settings requires a workspace restart; and
- only file changes inside a skill selected at startup use the narrow skill
  live-reload path.

Core stores and forwards workspace configuration, but the selected runtime
adapter decides which fields have an effect. Do not add a second field matrix
here; link the runtime-owned reference so future parser changes have one
documentation source of truth.
