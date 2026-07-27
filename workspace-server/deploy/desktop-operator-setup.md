# Desktop sidecar — operator setup (decision-1 security prerequisites)

The agent-desktop / computer-use feature is **disabled by default**
(`MOLECULE_DESKTOP_ENABLED=false`). Before enabling it you MUST provision the
decision-1 isolation prerequisites below, then set the confirmation interlock.
Enabling the feature without these ships a verified cross-tenant exposure (a
compromised sidecar reaching backend infra by IP / the cloud metadata service).

Design reference: `docs/superpowers/specs/2026-07-27-agent-desktop-sidecar-design.md` §6.

## 1. Egress deny (RFC-1918 + cloud metadata) — the load-bearing control

A desktop sidecar runs untrusted web content. Its per-workspace network already
isolates it from other Docker networks by-IP, but NOT from host-published infra
ports or the cloud metadata IP. Close that gap:

**Self-host Docker:** run the verified firewall script on every host that runs
desktop sidecars, on a 60s timer (so new per-workspace networks are covered as
they appear). It is idempotent and label-driven.

```
# systemd timer, cron, or host-provisioning — as root:
/opt/molecule/workspace-server/scripts/desktop-egress-firewall.sh install
```

Verify (from inside any running sidecar): a host-published infra port and
`169.254.169.254` are unreachable, but `curl https://1.1.1.1` still works.

**CP / k8s backend:** apply the declarative equivalent (requires a
NetworkPolicy-enforcing CNI — Calico/Cilium):

```
kubectl -n <sidecar-namespace> apply -f workspace-server/deploy/desktop-egress-networkpolicy.yaml
```

## 2. Redis password

The platform's Redis currently runs passwordless on the shared network. Even
with egress-deny in place, set a password so isolation is defence-in-depth, not
the only layer:

- Set `requirepass` (or ACLs) on Redis.
- Provide the credential to the platform via its existing Redis URL env.
- Do NOT expose the credential to sidecars (they never need Redis).

## 3. userns-remap (daemon-level defence for privileged tenants)

Tenants can run privileged (host-root by design), so sidecar non-root alone is
not isolation. Enable user-namespace remapping on the Docker daemon so a
container uid 0 maps to an unprivileged host uid:

```
# /etc/docker/daemon.json
{ "userns-remap": "default" }
```

Restart the daemon and confirm `/etc/subuid` / `/etc/subgid` entries exist for
the `dockremap` user. (The sidecar image already runs as the non-root `desktop`
user; the per-container `--cap-drop ALL` + `no-new-privileges` + Chromium-tuned
seccomp hardening is applied automatically by the provisioner.)

## 4. Confirm the interlock

Only after 1–3 are in place, set the confirmation env so the platform will wire
the desktop feature:

```
MOLECULE_DESKTOP_ENABLED=true
MOLECULE_DESKTOP_EGRESS_CONFIRMED=true
```

If `MOLECULE_DESKTOP_ENABLED=true` but `MOLECULE_DESKTOP_EGRESS_CONFIRMED` is not
`true`, the platform logs the requirement and leaves the desktop backend
**unwired** — it will not ship the exposure by misconfiguration.

## Optional overrides

- `MOLECULE_DESKTOP_IMAGE` — the sidecar image (default: registry `…/molecule-desktop:latest`).
- `MOLECULE_DESKTOP_SECCOMP_PROFILE` — absolute path to a custom seccomp profile.
  Empty uses the embedded Chromium-tuned default
  (`internal/provisioner/seccomp/desktop-sidecar.json`). Do NOT set to
  `unconfined` outside debugging.
