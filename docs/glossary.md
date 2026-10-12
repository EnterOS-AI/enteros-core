# Molecule glossary

| Term | Current meaning |
|---|---|
| **workspace** | A durable organizational role with its own identity, runtime, lifecycle, credentials, and memory boundaries. The backing workload may be local or control-plane managed; it is not defined as always being a Docker container. |
| **agent** | The runtime process/session representing a workspace. Documentation should distinguish the workspace identity from one agent invocation. |
| **team** | A hierarchy of workspace rows connected by `parent_id`, not an execution sequence and not a destructive expand/collapse lifecycle. |
| **runtime** | The execution implementation selected for a workspace. Current choices come from the runtime/template catalog and `manifest.json`; this glossary does not duplicate a fixed list. |
| **template** | A reproducible workspace configuration source pinned by immutable commit in `manifest.json`. |
| **plugin** | An installable extension with a registered source scheme and runtime compatibility. It is not a Canvas node. |
| **skill** | A reusable instruction/asset bundle selected by the runtime. An already selected skill may have a narrow runtime-owned reload path; that does not imply arbitrary config hot reload. |
| **A2A** | Authenticated agent-to-agent JSON-RPC traffic and delivery semantics. Depending on the runtime, delivery can be push or poll. |
| **activity** | Operational message/tool/lifecycle data stored in `activity_logs`; it is separate from selected structure events. |
| **structure event** | An append-only selected lifecycle/history record. It is not a complete event source for current state. |
| **channel** | A runtime-specific inbound/outbound integration. Its credentials and delivery semantics belong to the channel implementation, not to a generic social-conversation object. |
| **bundle** | A portable supported subset of workspace configuration and descendants. It excludes secrets, Memory v2, activity/chat history, arbitrary files, and provider state. |

The [technical reference](./architecture/molecule-technical-doc.md),
[`manifest.json`](../manifest.json), router, migrations, and current runtime code
override older ecosystem comparisons or historical blog terminology.
