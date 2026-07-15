# Workspace provisioning

The tenant workspace-server owns workspace lifecycle orchestration. It prepares
configuration, secrets, tokens, and templates, then sends lifecycle operations
through one backend dispatcher.

## Backend boundary

`WorkspaceHandler` can be wired with either:

- `LocalProvisionerAPI`, which manages containers on the local Docker daemon;
- `CPProvisionerAPI`, which delegates create, stop, restart, and state checks to
  the control plane.

The handler prefers the control-plane backend when it is configured, otherwise
it uses local Docker. A deployment with neither backend fails the workspace
loudly instead of leaving it in `provisioning`. The control plane owns the
provider implementation behind its API; tenant code must not infer a provider
from an `instance_id` or hard-code a cloud deployment model.

All create, import, resume, and restart paths use the dispatchers in
`internal/handlers/workspace_dispatchers.go`. Backend-specific code must not be
called directly from a new handler path.

## Provisioning flow

1. Validate the request and persist the workspace's current state in Postgres.
2. Resolve the template and runtime, gather global and workspace secrets, and
   prepare the boot configuration.
3. Serialize provision/restart work for the workspace so concurrent lifecycle
   requests cannot create two instances.
4. Dispatch to the configured backend.
5. Persist the backend identifier and wait for registration/heartbeat.
6. Move to `online`, `awaiting_agent`, or `failed` according to the runtime and
   provisioning result.

The `workspaces.runtime` column is authoritative after creation. Restart does
not rediscover the runtime from `config.yaml` or a running container.

## Local Docker behavior

The canonical local container name is `ws-<full-workspace-id>` and the internal
URL is `http://ws-<full-workspace-id>:8000`. Do not reintroduce the retired
12-character ID truncation; collision-safe full IDs are required. Legacy names
are consulted only where migration compatibility is explicit in code.

Local containers join the configured Docker network unless the selected tier
uses host networking. The provisioner publishes an ephemeral loopback port for
host-side access and separately caches an internal container URL for peer
traffic. Persistent config, workspace, and session volumes use the full
workspace ID.

Tier flags are a local-Docker implementation detail. See
[Workspace tiers](./workspace-tiers.md). A control-plane backend can enforce the
same capability intent through its own provider primitives.

## Lifecycle and health

Current workspace states are defined in `internal/models/workspace_status.go`:

```text
provisioning  online  offline  degraded  failed
paused  hibernating  hibernated  awaiting_agent  removed
```

Not every state transition is valid. Lifecycle handlers use conditional
updates, per-workspace gates, and backend checks to avoid reviving paused,
hibernated, or removed workspaces.

Health has backend-aware layers:

- each heartbeat refreshes `ws:<id>` with a 180-second Redis TTL;
- local container-backed workspaces are checked through the Docker API;
- control-plane-backed workspaces use the control-plane `IsRunning` contract;
- `runtime=external` uses heartbeat age, with a 180-second default stale window;
- a failed A2A forward can trigger a reactive backend state check.

Redis is a liveness/cache layer, not the source of workspace state. Postgres is
authoritative, and cache keys are cleared when a workspace is declared offline.

## Stop, restart, pause, and delete

- Restart reads the persisted runtime, stops through the selected backend, then
  provisions through the same dispatcher.
- Pause/hibernate deliberately stop compute and are excluded from automatic
  dead-instance recovery.
- Delete stops compute and removes runtime state according to the requested
  deletion mode. Permanent erase uses the backend's explicit prune path.
- Config and data persistence depend on the backend and selected deletion mode;
  callers must not assume every backend exposes a host Docker volume.

## Implementation authority

- dispatch and lifecycle: `internal/handlers/workspace_dispatchers.go` and
  `workspace_restart.go`;
- local Docker: `internal/provisioner/provisioner.go`;
- control-plane client: `internal/provisioner/cp_provisioner.go`;
- state names: `internal/models/workspace_status.go`;
- liveness: `internal/db/redis.go` and `internal/registry/healthsweep.go`.
