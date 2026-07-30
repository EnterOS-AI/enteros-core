# Desktop sidecar — operator notes

The agent-desktop / computer-use feature is **on by default** on the self-host
Docker backend. **There is no enablement flag and no required operator step** —
network isolation is structural (see below). Set `MOLECULE_DESKTOP_DISABLE=true`
to opt out.

Design reference: `docs/superpowers/specs/2026-07-27-agent-desktop-sidecar-design.md` §6.

## Why there's no egress-firewall step anymore

Isolation is enforced by the topology, not by a host firewall an operator has to
remember to install:

- Each workspace's desktop sidecar runs on a **per-workspace INTERNAL Docker
  network** (`wsnet-<id>`, created with `Internal: true`) — it has **no external
  route of its own**.
- Its only way out is a **per-workspace egress proxy** (`wsdeskproxy-<id>`,
  `cmd/desktop-egress-proxy`) that the sidecar's browser is pointed at via
  `--proxy-server`. The proxy **denies every private / link-local / loopback
  destination** (RFC-1918, `169.254.0.0/16` = cloud metadata + Docker host
  gateway, `127.0.0.0/8`, IPv6 ULA/link-local) and allows only the public
  internet.

So a compromised browser cannot reach backend infra (Postgres/Redis/MinIO/
LiteLLM), the Docker host, other tenants, or the cloud metadata service — **with
no host iptables and no operator affirmation.** Verified end-to-end on real
Docker (2026-07-28): the sidecar has no default route; the browser loads public
pages only through the proxy; the proxy returns 403 for metadata/private.

## Optional overrides

- `MOLECULE_DESKTOP_DISABLE=true` — turn the feature off.
- `MOLECULE_DESKTOP_IMAGE` — the sidecar/proxy image (one image serves both).
- `MOLECULE_DESKTOP_EGRESS_NETWORK` — the network the egress proxy uses for its
  internet leg (default `bridge`). Point it at any network that has internet and
  does NOT carry backend infra.
- `MOLECULE_DESKTOP_PLATFORM_CONTAINER` — the platform's own container id/name
  (defaults to `HOSTNAME`); the provisioner joins each per-workspace network so
  the gateway can reach the sidecar by name.
- `MOLECULE_DESKTOP_SECCOMP_PROFILE` — absolute path to a custom seccomp profile
  (empty = the embedded Chromium-tuned default,
  `internal/provisioner/seccomp/desktop-sidecar.json`).

## Defense-in-depth (recommended, not required)

These harden the platform generally; the desktop feature no longer depends on
them for its isolation:

- **Redis password** (`requirepass`/ACLs) — good practice regardless; the desktop
  proxy already denies the sidecar any path to Redis.
- **`userns-remap`** on the Docker daemon — maps container uid 0 to an
  unprivileged host uid; useful for the privileged-tenant threat model.

## CP / k8s backend

The self-host mechanism above (internal network + egress proxy) is Docker-
specific. The CP/k8s backend is a separate follow-up; the declarative equivalent
of the egress deny for a NetworkPolicy-enforcing CNI is in
`deploy/desktop-egress-networkpolicy.yaml`.
