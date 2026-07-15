# Database schema

The executable schema is the ordered migration set under
`workspace-server/migrations/`. This page documents durable invariants and
important relationships; it is not a copy-paste replacement for the current
migrations.

## Workspace state

`workspaces` is the authoritative mutable record for each workspace. In
addition to identity and hierarchy, the current table carries lifecycle state,
runtime and template selection, tier/capability settings, backend compute
metadata, registration/heartbeat data, delivery mode, budget configuration,
and runtime capability observations.

Important invariants:

- IDs are UUIDs and must be used in full for backend resource names.
- `runtime` in Postgres is lifecycle authority; restart does not infer it from
  `config.yaml`.
- `tier` expresses capability/isolation, not a provider choice.
- `instance_id` is an opaque backend identifier. Its shape may help a legacy
  compatibility path dispatch, but documentation and new code must not equate
  every non-empty value with one cloud provider.
- valid lifecycle values are owned by `internal/models/workspace_status.go` and
  the corresponding status migration/check constraint.
- `parent_id` defines organization hierarchy. Selected historical/event tables
  do not replace the current workspace row as a replay source.

`agents` records model assignments and removal history. `canvas_layouts` and
`canvas_viewport` hold UI state. `structure_events` is append-only lifecycle and
audit history, while `activity_logs` records operational activity with its own
retention policy.

## Secrets

`workspace_secrets` stores values scoped to one workspace; `global_secrets`
stores tenant-wide values. Both tables persist:

```text
encrypted_value BYTEA
encryption_version INTEGER
```

Current version semantics are:

- `0`: legacy plaintext bytes;
- `1`: AES-256-GCM ciphertext produced with `SECRETS_ENCRYPTION_KEY`.

Production startup requires a valid encryption key and new writes are
encrypted. Non-production can run without the key for compatibility, in which
case rows are explicitly tagged version 0. Decryption follows the row's version
instead of guessing from the bytes. The key is supplied as process secret
configuration and is never stored in Postgres.

Secret values are excluded from exported bundles. Provisioning resolves global
and workspace values, decrypts them according to `encryption_version`, and
delivers only the credentials needed by the target workspace.

See [Secrets key custody](./secrets-key-custody.md) for operating rules.

## Organization and workspace credentials

`org_api_tokens` stores hashes, display prefixes, optional org ownership and
expiry, provenance, usage metadata, and revocation state. `org_id` anchors a
token to a tenant workspace; `expires_at` is enforced during validation.
Plaintext tokens are not persisted.

Workspace-scoped token tables bind a bearer to one workspace and are separate
from full tenant-admin organization tokens. See
[Organization API keys](./org-api-keys.md).

## Plugin, queue, and memory state

Plugin desired/installed state, durable A2A queue and delegation records,
pending uploads, approvals, schedules, memories, and display locks are all
schema-owned features with dedicated migrations. Their expiry and retention
columns have feature-specific semantics; do not apply the Redis liveness TTL to
them.

## Redis

Redis contains ephemeral liveness and routing caches:

| Key | Current purpose | TTL |
|---|---|---|
| `ws:<id>` | Workspace liveness marker | 180 seconds |
| `ws:<id>:url` | Platform-reachable URL cache | 5 minutes |
| `ws:<id>:internal_url` | Local container-network URL cache | 5 minutes |

Postgres remains authoritative. Clearing Redis must not erase durable workspace
state. Heartbeats repopulate liveness, and backend-aware health checks correct
stale status.

## Changing the schema

Add a forward and rollback migration, update the Go query/model code, and add a
test for the migration-dependent behavior. Do not update a prose `CREATE TABLE`
snapshot and assume the live schema changed.

Related documentation:

- [Event log](./event-log.md)
- [Registry and heartbeat](../api-protocol/registry-and-heartbeat.md)
- [Workspace provisioning](./provisioner.md)
- [Workspace tiers](./workspace-tiers.md)
