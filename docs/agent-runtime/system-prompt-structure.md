# System-prompt assembly boundary

The workspace-runtime package owns prompt assembly. Use the docs repository's
[System Prompt Structure source
reference](https://git.moleculesai.app/molecule-ai/docs/src/branch/main/content/docs/agent-runtime/system-prompt-structure.md)
and verify behavior against `molecule_runtime/prompt/builder.py`. Publishing
that source to a public site is a separate deployment concern.

Current boundaries relevant to Core:

- the base prompt is assembled during runtime startup and shared by managed
  adapters;
- configured prompt files, durable framework memory files, plugin fragments,
  selected skills, A2A/memory instructions, reachable-peer summaries, and
  delegation guidance are distinct ordered inputs;
- the old `shared_context` field and parent-file injection endpoint are
  retired; team knowledge is recalled through scoped Memory v2;
- editing `config.yaml`, prompt files, plugins, or the selected skill list
  requires a restart to rebuild the complete prompt; and
- the skill watcher can reload contents of an already selected skill, but it
  does not rebuild arbitrary prompt/config state.

Core supplies authenticated workspace, hierarchy, platform-instruction, and
peer data. It does not own a separate prompt-builder algorithm, so pseudo-code
for one must not be maintained in this repository as if it were executable.
