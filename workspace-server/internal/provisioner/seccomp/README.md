# Desktop-sidecar seccomp profile

`desktop-sidecar.json` is the seccomp profile applied to every desktop sidecar
container by `LocalSidecarProvisioner.StartDesktop` (embedded via `go:embed`).

## Why a custom profile exists

The desktop sidecar runs Chromium, and we deliberately do **not** pass
`--no-sandbox` (that would collapse the browser's own isolation layer — design
§6.2). Chromium therefore relies on its **unprivileged user-namespace sandbox**,
which needs to `clone(CLONE_NEWUSER)` and then set up a chroot inside the new
namespace.

Docker's built-in seccomp profile blocks this for a container without
`CAP_SYS_ADMIN`:

- `clone`/`clone3`/`unshare` are only permitted **with** `CAP_SYS_ADMIN`, or
  **without** the namespace-creation mask bits (`CLONE_NEWUSER = 0x10000000` is
  inside the `0x7E020000` mask) — so an unprivileged `clone(CLONE_NEWUSER)` is
  denied.
- The chroot-setup syscalls the sandbox then needs (`chroot`, `pivot_root`,
  `mount`, `umount2`, `keyctl`) are gated behind `CAP_SYS_ADMIN` /
  `CAP_SYS_CHROOT`, which are **evaluated in the pre-userns context** and so are
  denied under our `--cap-drop ALL` hardening.

Empirically (Docker **29.5.3**, `molecule-desktop:test`, 2026-07-27) the stock
profile makes Chromium abort at boot with `zygote_host: No usable sandbox` (and,
once userns clone is allowed but chroot is not, `zygote_host:221 ENOENT`). The
browser process becomes a `<defunct>` zombie and every screenshot is the blank
Xvfb root.

## What the profile changes

`desktop-sidecar.json` is `moby-default.json` (the upstream Docker default,
`defaultAction: SCMP_ACT_ERRNO` — deny-by-default) with a **single prepended
allow rule** for the two syscall groups the userns sandbox needs:

```json
{ "names": ["clone","clone3","unshare","setns","chroot","pivot_root","mount","umount2","keyctl"],
  "action": "SCMP_ACT_ALLOW" }
```

Every other default deny is preserved verbatim.

### Why this is still safe

seccomp `ALLOW` is **not** a grant of privilege — it only stops seccomp from
pre-empting the kernel's own permission check. An unprivileged process that
calls `mount()`/`chroot()`/`pivot_root()` **outside** a user namespace still
gets `EPERM` straight from the kernel. These syscalls only succeed for a process
that is inside a user namespace it created (where it holds the namespace-scoped
capability). So relaxing them re-enables the Chromium sandbox without granting
the sidecar any real host privilege, and the container keeps `--cap-drop ALL`
and `no-new-privileges`.

## Verified-good container config

`StartDesktop` applies, and this profile is validated under:

- `CapDrop: ["ALL"]`
- `SecurityOpt: ["no-new-privileges", "seccomp=<this profile>"]`
- `Resources.Memory` + `Resources.MemorySwap` (equal → no swap past the cap)

Result: Chromium's userns sandbox is **active**, no `--no-sandbox`, no
`CAP_SYS_ADMIN`, screenshot pipeline works end-to-end.

## Operator override

An operator can point `MOLECULE_DESKTOP_SECCOMP_PROFILE` at a different profile
file (absolute path). If unset, the embedded `desktop-sidecar.json` is used.
Setting it to the literal `unconfined` disables the profile (NOT recommended;
for debugging only).

## Regenerating

`moby-default.json` is pinned from
`https://raw.githubusercontent.com/moby/moby/v24.0.9/profiles/seccomp/default.json`.
To refresh: re-fetch that file, then prepend the allow rule above as the first
element of `syscalls`.
