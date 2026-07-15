# Workspace bundle boundary

Core owns the `/bundles/export/:id` and `/bundles/import` handlers. The docs
repository owns the fuller [Workspace Bundles source
reference](https://git.moleculesai.app/molecule-ai/docs/src/branch/main/content/docs/agent-runtime/bundle-system.md).
Publishing that source to a public site is a separate deployment concern.

Current verified boundaries:

- export serializes workspace-row metadata, the stored Agent Card, supported
  prompt/config files, selected skill trees, and non-removed descendants;
- export does not serialize secrets, memory records, activity/chat history,
  arbitrary workspace files, or provider/container state;
- some compatibility fields exist in the JSON type but are not populated by
  every exporter path;
- import gives every node a fresh ID, records `source_bundle_id`, restores the
  supported configuration subset, and provisions asynchronously; and
- an accepted import is not proof that every descendant reached a healthy
  runtime state.

Implementation sources of truth:

- `workspace-server/internal/bundle/exporter.go`
- `workspace-server/internal/bundle/importer.go`
- the bundle handlers registered in `workspace-server/internal/router/router.go`

Do not reintroduce marketplace plans, recursive rollback promises, or claims
that every workspace file/tool is captured unless those contracts are first
implemented and tested.
