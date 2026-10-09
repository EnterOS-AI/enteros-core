# CP Desktop-Sidecar Backend — Design Spec

**Status:** Draft for review
**Date:** 2026-07-30
**Author:** Claude (Opus 4.8), driving for Hongming
**Related:** `docs/superpowers/specs/2026-07-27-agent-desktop-sidecar-design.md` (the self-host feature this extends)

## 1. Problem

The agent-desktop-sidecar (computer-use) feature is wired **only when a local-Docker
provisioner exists** — `cmd/server/main.go`: `if prov != nil { … SetDesktopGateway(gw) }`.
On a **Control-Plane / SaaS tenant** the tenant-server delegates workspace provisioning
to the CP and has no local Docker (`prov == nil`), so the desktop block is skipped
entirely, `desktopGateway` is nil, and every `/desktop/*` call returns
`503 "desktop not available"`. The design left this as an explicit deferral:
*"CP/k8s backend is a separate follow-up"* (§5, §14, decision 4;
`sidecar_provisioner_api.go` `unavailableSidecarProvisioner`).

**Concrete impact:** the Enter OS Server (`reno-stars.enteros.ai`) is a CP tenant, so its
agents cannot use the computer even though the runtime (`0.4.61`), prompt wiring, gateway,
egress proxy, and desktop image are all built and correct. Verified 2026-07-30 by
inspecting the live deployment: tenant logs `Provisioner: Control Plane (auto-detected SaaS
tenant)`, no docker socket on the tenant, `desktopGateway` nil.

## 2. Key context (verified against the live Enter OS Server)

- **No cloud providers are in use.** Despite a `cp-instance-reconciler … against real EC2
  state` log line, there is no EC2 — the reconciler finds nothing. All workspaces run as
  **local Docker containers on the single Enter OS Server host.**
- **`molecule-cp-prod` mounts `/var/run/docker.sock`** and creates each workspace box as a
  local Docker container (e.g. `enteros-ws-reno-stars-<id>`) on a **per-org network**
  (`mol-reno-stars-<orgid>`).
- **The tenant-server is on that same per-org network** — so anything the CP puts on that
  network is reachable from the tenant's gateway by container name.
- Both the tenant and the CP have `SECRETS_ENCRYPTION_KEY` and `MOLECULE_CP_SHARED_SECRET`
  set — so both can derive the **same signing root** via `handlers.DesktopSigningRoot`.

This means the CP backend is NOT "build EC2/k8s scheduling." It is: **have the CP run the
existing `LocalSidecarProvisioner` (it already has docker.sock), on request from the
tenant.** ~90% of the code already exists.

## 3. Goal / non-goals

**Goal:** the agent (and human view) on a CP-provisioned workspace can use the desktop,
with the SAME structural egress isolation and coordinate/token contracts as self-host.

**Approach (operator directive, 2026-07-30): "the CP should work like self-host for display
for now, until the k8s cluster is fully implemented."** So we do NOT build a CP-delegation
layer. We make the desktop use **local Docker directly**, exactly as self-host does, and
swap to a k8s sidecar backend later.

**In scope (now):** wire the existing `LocalSidecarProvisioner` for the desktop on CP
tenants by giving the tenant a Docker client — the single-host, local-Docker reality.

**Out of scope (future, interface-compatible):** a `K8sSidecarProvisioner` that schedules
the sidecar as a pod when the k8s cluster lands. The `SidecarProvisioner` interface already
makes this a drop-in swap.

## 4. Design — desktop uses local Docker directly (like self-host)

The desktop sidecar only needs **a Docker client**. That is INDEPENDENT of how workspace
boxes are provisioned (`prov`, the box provisioner). Today the desktop is gated on
`if prov != nil`, which conflates "boxes are local-Docker" with "a Docker daemon is
reachable." We decouple those.

### 4.1 Decouple the desktop wiring from the box provisioner (molecule-core)

- `cmd/server/main.go`: change the desktop-enable condition from `if prov != nil` to
  **`if dockerAvailable() && MOLECULE_DESKTOP_DISABLE != "true" && DesktopSigningRoot() != ""`**,
  where `dockerAvailable()` returns a usable Docker client when `/var/run/docker.sock` is
  mounted OR `DOCKER_HOST` is set. Self-host is unchanged (it has both `prov` and Docker);
  CP tenants now qualify the moment they have a Docker client.
- The desktop sidecar backend stays the **existing `LocalSidecarProvisioner`**, constructed
  with that Docker client — no new provisioner type, no CP endpoints. (On self-host it
  already reuses the box provisioner's Docker client; here it takes the same client from
  `dockerAvailable()`.)

### 4.2 Give the CP tenant a Docker client (infra)

The tenant container currently has no Docker access. Give it one, exactly like a self-host
tenant has:

- **(A) Mount `/var/run/docker.sock`** into the tenant container. Simplest; full Docker
  access (the tenant IS the platform, same trust level as self-host).
- **(B) `DOCKER_HOST=tcp://molecule-docker-proxy:2375`** via the existing
  `tecnativa/docker-socket-proxy` (`molecule-docker-proxy` is already running). Least
  privilege — but the proxy must be permitted for CONTAINERS + NETWORKS + IMAGES (pull) +
  the exec/attach the control path needs. *Recommended if the proxy can be scoped.*

This is a per-tenant deploy change (the workflow/compose that creates the tenant container),
not code.

### 4.3 Everything else is unchanged / already correct

- **Gateway** derives `DESKTOP_CONTROL_TOKEN` from `DesktopSigningRoot()` and dials
  `wsdesk-<id>:<port>`. The sidecar the tenant creates lands on the workspace's per-org
  network, which the tenant is already on → resolves by name; SSRF allowlist
  (`ValidateSidecarTarget`) already pins the shape.
- **Egress isolation** — `LocalSidecarProvisioner` creates the per-workspace INTERNAL
  network + per-workspace egress proxy exactly as on self-host (`§6.1`,
  `cmd/desktop-egress-proxy`). No host firewall.
- **Signing root** — the SAME process (the tenant) signs and derives, so no cross-service
  token coordination is needed (this was the fragile part of the delegation approach — now
  gone).
- **Idle reaper / scale-to-zero** — runs in the tenant, same as self-host, since the tenant
  owns the sidecar's Docker lifecycle.

### 4.4 k8s future (swap, not rewrite)

When the k8s cluster is ready: implement `K8sSidecarProvisioner` satisfying the same
`SidecarProvisioner` interface (schedule the sidecar as a pod on the workspace's namespace,
NetworkPolicy for the internal-network isolation, the egress proxy as a sidecar container),
and select it in `main.go` when running on k8s. No gateway/token/handler changes.

## 5. Dependencies

- The **desktop-sidecar image** (`molecule-desktop`) must be pullable by the tenant. It is
  in `registry.moleculesai.app`; the Enter OS Server pulls box images from
  `registry.enteros.ai`, so `molecule-desktop:latest` must be available there
  (mirror/publish) unless the tenant can reach `registry.moleculesai.app` directly.
- The egress-proxy entrypoint ships in the same image (`Entrypoint: [tini,
  desktop-egress-proxy]`) — already provided.

## 6. Security

No new isolation surface: `LocalSidecarProvisioner` creates the SAME per-workspace
INTERNAL network + egress proxy. The proxy is the only egress path and denies
private/link-local/loopback/CGNAT (`cmd/desktop-egress-proxy/blocklist.go`). The control
token is derived, never stored. **The one new privilege is the tenant getting a Docker
client (§4.2)** — identical to what a self-host tenant already holds; scope it via the
socket-proxy (option B) if least-privilege is desired.

## 7. Open decisions (need Hongming's input before implementation)

1. **Docker access (§4.2):** mount `/var/run/docker.sock` into the tenant (A, simplest) or
   `DOCKER_HOST` → the existing `molecule-docker-proxy` (B, least-privilege)? Recommendation:
   **B if the proxy can be permitted for containers+networks+images; else A.**
2. **Desktop image location (§5):** mirror `molecule-desktop` to `registry.enteros.ai`, or
   let the tenant pull from `registry.moleculesai.app`?
3. **Rollout:** enable on reno-stars first (one tenant's deploy env), validate the live
   smoke, then apply the tenant-deploy change to other CP tenants.

## 8. Testing

- Unit: `CPSidecarProvisioner` HTTP contract (tenant side); CP handler provisions the
  sidecar + internal net + proxy (reuse the existing `local_sidecar_provisioner_test.go`
  fakes on the CP side).
- Integration: a CP-mode tenant → `/desktop/screenshot` returns a real PNG; human
  `/display` returns available and the noVNC proxy connects; egress proxy denies a private
  destination; idle reaper tears the sidecar down.
- Live smoke on the Enter OS Server: reprovision a hermes workspace, ask the agent to
  "take a screenshot" / "open a URL", confirm it works end-to-end.

## 9. Estimated shape

- **molecule-core (code):** decouple the desktop-enable check from `prov` (a
  `dockerAvailable()` helper + construct `LocalSidecarProvisioner` from that client) + tests.
  **Small — no new provisioner type, no controlplane changes.**
- **infra (deploy):** give the reno-stars tenant container a Docker client (socket mount or
  `DOCKER_HOST` → proxy) + ensure the desktop image is pullable. Per-tenant deploy config.

The entire desktop feature (provisioner logic, egress isolation, gateway, tokens, image,
runtime prompt) is DONE and reused as-is. This change is just "let the desktop use the local
Docker that's already on the host, regardless of who provisions the boxes" — the self-host
path, applied to CP tenants. k8s later is a clean interface swap (§4.4).
