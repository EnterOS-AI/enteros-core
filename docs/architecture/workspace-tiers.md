# Workspace tiers

A workspace tier expresses its requested capability and isolation posture. It
does not select a cloud provider or provisioning backend. The local Docker
backend maps tiers to the flags below; a control-plane backend is responsible
for enforcing equivalent intent with its own primitives.

## Local Docker mapping

| Tier | Local behavior |
|---|---|
| T1, sandboxed | Read-only root filesystem, `noexec` temporary storage, config mount only, no workspace mount. |
| T2, standard | Unprivileged container with workspace/config mounts and default 512 MiB / 1 CPU limits. |
| T3, privileged | Privileged container with host PID visibility, Docker networking, and default 2 GiB / 2 CPU limits. |
| T4, full host | T3 plus host networking and Docker-socket access, with default 4 GiB / 4 CPU limits. |

T2, T3, and T4 resource defaults can be changed through
`TIER<n>_MEMORY_MB` and `TIER<n>_CPU_SHARES`. `ApplyTierConfig` treats an
unknown or zero value as T2 when applying local container flags.

T3 and T4 execute agent-controlled code with broad host capabilities. T4 can
manage other containers through the Docker socket and should be granted only
when the workload requires it.

## Defaults and persistence

The request handlers currently choose a backend-aware default when the caller
does not specify a tier:

- control-plane-backed provisioning: T4;
- local/self-hosted Docker provisioning: T3.

The chosen tier is persisted in `workspaces.tier`. It is not read from
`config.yaml` as lifecycle authority. A tier change affects newly provisioned
compute or the next restart/reprovision; it does not mutate an already-running
container in place.

From A2A's perspective, tier does not change the protocol or Agent Card. It
changes the execution boundary and resource/capability policy.

See [Workspace provisioning](./provisioner.md) for backend dispatch and
lifecycle behavior. The implementation authority for local flags is
`workspace-server/internal/provisioner/provisioner.go`.
