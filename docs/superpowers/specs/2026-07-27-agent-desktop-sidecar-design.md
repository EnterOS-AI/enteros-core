# Agent Desktop Sidecar — Design Spec (v2, review-hardened)

- **Date:** 2026-07-27
- **Status:** Draft v2 — pending review. Supersedes v1 (same file; see §17 Revision history).
- **Scope:** workspace-server (Go) · canvas (Next.js) · molecule-ai-sdk (contracts) · molecule-controlplane (deferred, cross-repo)
- **Stack:** on-prem cluster is **Enter OS** (plain Docker now, Kubernetes soon).

## 0. Locked decisions (this drives everything below)

1. **Security first — per-workspace network isolation is a hard prerequisite**, not a hardening footnote. The desktop feature does not ship until a compromised sidecar cannot reach other tenants or platform infra.
2. **Reconcile with the existing runtime desktop implementation** (`molecule-ai-workspace-runtime/molecule_runtime/a2a_tools_desktop.py`) — do not build a parallel `desktop_*` surface.
3. **Build in workspace-server core, behind an SDK-SSOT contract** (Option A). The contract is the abstraction seam; extracting to a native plugin later (Option B) is a deferred RFC (`2026-07-27-computer-use-plugin-extraction-rfc-followup.md`).
4. **Backend-neutral abstraction, Local/Docker backend first.** Production (k8s/CP) backend is a clean **per-tier availability gate** — absent, not broken, until the control-plane wires its half. Self-host is fully functional standalone.

**Resolved in review (2026-07-27):** desktop is **co-located** with the workspace's compute unit (§7); governance is **capability-is-authorization + AI escalation**, no per-action human gates (§6.3); secrets-at-rest encryption **deferred** to a follow-up RFC (§6.4); reconcile the existing runtime tool by **re-point + 3-layer split** (§9).

## 1. Problem & reframe

Each workspace should be "a computer a human uses" — the agent opens a browser and clicks, not just calls APIs. The capability exists, welded to the **EC2 per-VM model**. Enter OS is per-container. This project re-homes the desktop to the container model **and** adds the agent's own eyes/hands — which never existed as a first-class, portable capability.

**What the review corrected about v1 (do not repeat these mistakes):**
- The control-lock does **not** already support agent+human coexistence — the acquire SQL can't preempt and the noVNC token is bound to the lock holder, so a human can neither take over nor even watch an agent-held desktop. **View must be split from control** (§8).
- Self-host display is **greenfield**, not reuse — the local provisioner never reads `cfg.Display` and the proxy is EC2-only (§14).
- The agent runtime is **multi-vendor and partly blind** (text-only adapters); `computer_use` native tooling is used nowhere. Computer-use must be **gated on a vision-capable adapter** (§9).
- The security isolation premise is **false today** — flat network, privileged tenants, no volume encryption (§6).
- The "orphan sweeper mis-parse" risk was wrong; the real bug is the **credential volume can never be reaped → leaks forever** (§10, §15).
- The image-result recommendation was backwards — the **additive attachment-URI path** is correct; base64 blocks would mutate a shared contract (§9).

## 2. Decisions locked during brainstorming (unchanged from v1)

| Decision | Choice |
|---|---|
| Desktop scope | Full Linux desktop |
| Packaging | Dedicated per workspace (not shared) |
| Lifecycle | Scale-to-zero + persistent profile volume |
| Agent control | Computer-use loop (screenshot + xdotool), **gated on vision-capable adapter** |
| Human path | Reuse noVNC proxy; **view split from control** |
| Resolution | One fixed native resolution, no scaling |
| Orchestration | Docker now → k8s soon; backend-neutral |

## 3. Coordinate-space contract (hard invariant, pinned by the SSOT contract §4)

1. One native resolution, default **1280×800**, config-driven.
2. Pinned identically in **X `-screen`**, the **tool declaration** (`display_width_px`/`display_height_px`), and the **framebuffer capture** — and this triple is enforced by the SSOT contract's value-pin, not by hand.
3. **DPI 96, device-pixel-ratio 1.** No HiDPI/fractional scaling.
4. **No coordinate math in the pipeline** — any resize/scale is a bug.
5. **Pin the window, not just the screen.** Launch Chrome in kiosk/app-fullscreen (`--kiosk`) with no decorations + a post-launch geometry assert. (The existing runtime normalizes only firefox/falkon; Chrome, our pick, is unhandled — this is a real gap to close.)
6. **Model regime:** the 1:1-pixel guarantee is a *native computer-use tool* property and does **not** automatically transfer to a generic MCP tool. The contract records the assumed regime; §9 gates the tool on a vision-capable adapter and defines the result path explicitly.

## 4. SSOT contract (the abstraction seam)

**Author `contracts/tool/computer-use.contract.json` in `molecule-ai-sdk`; codegen `molcontracts.ComputerUseContract`; have workspace-server consume + value-pin it** — the exact precedent of `internal/handlers/mcp_plugin_delivery_contract.go` (`MatchesSSOT`), riding the existing codegen + CI drift-gate (§16) + `go.mod` module-bump rails. **Zero new codegen machinery.**

The contract pins, as one source of truth shared across workspace-server, canvas, the runtime, and (later) the plugin:
- **Display geometry** (the §3 triple) — width/height/DPI.
- **Action enum** — `screenshot | click | type | key | scroll` (+ params).
- **Control-server protocol** — `GET /screenshot` → PNG, `POST /input` → actions; auth header shape.
- **Result shape** — the screenshot delivery form (attachment URI, §9) so every consumer agrees.
- **Lifecycle verbs** — the backend-neutral `SidecarProvisioner` operations (§5).

A `TestComputerUseContract_MatchesSSOT` value-pins the Go binding against the SDK JSON, mirroring `mcp_plugin_delivery_contract.go`'s `MatchesSSOT`.

## 5. Abstraction — the backend-neutral `SidecarProvisioner`

Mirrors the existing `LocalProvisionerAPI` / `CPProvisionerAPI` split. One interface, three eventual backends, selected by the existing `…Auto` dispatcher shape in `workspace_dispatchers.go`:

```
SidecarProvisioner interface {
    StartDesktop(ctx, cfg) (DesktopHandle, error)   // idempotent; returns reachable address
    StopDesktop(ctx, workspaceID) error             // graceful (§11)
    DesktopRunning(ctx, workspaceID) (bool, error)
    // WipeProfile is part of the erase/prune path (§11)
}
```

- **Local (Docker) backend — ships first, all in this repo.** Sibling container or co-located per §7.
- **CP (cloud) backend — deferred, cross-repo.** Returns a clean `ErrDesktopBackendUnavailable` until wired → the feature is a **per-tier availability gate**, not a break. Self-host is unaffected.
- **k8s backend — at migration.** Third impl behind the same interface.

**Per-tier availability:** `GET /workspaces/:id/display` returns `available:false, reason:"desktop_backend_unavailable"` when the active backend is unimplemented — canvas already renders a not-enabled empty state, so this degrades cleanly with no UI change.

## 6. Security & per-workspace isolation (decision 1 — the gating prerequisite)

The v1 isolation story was false against the real deployment. v2 makes isolation a **prerequisite**, verified by tests before any desktop ships.

**6.1 Per-workspace network (mandatory).** Today every container (platform, canvas, **passwordless Redis**, Postgres, LiteLLM holding provider keys, all tenants) shares one flat `molecule-core-net` with inter-container comms on. **v2 requires:**
- Each workspace + its desktop on a **dedicated per-workspace Docker network** (self-host) / NetworkPolicy-isolated namespace (k8s). The sidecar reaches only its own tenant's control endpoint and the internet.
- Backend infra (Postgres/Redis/MinIO/LiteLLM) on a **backend-only network** the platform joins but tenants/sidecars never do. **Set a Redis password now regardless.**
- Sidecar internet egress through an **egress proxy with a domain allowlist**; deny RFC-1918 + `169.254.169.254`.

**6.2 Privileged-tenant reality (document + mitigate).** Tenants default to **T3/T4** (privileged, host PID, docker.sock, host networking — host-root is a *designed* capability). The sidecar being non-root is not isolation on its own, because a compromised tenant can escalate to host root and read any volume. v2: enable daemon **`userns-remap`** so tenant/sidecar uids don't collide across volumes (also fixes the uid-collision cross-tenant read); keep the sidecar cap-dropped + seccomp + `no-new-privileges`, and **reconcile that with Chrome's sandbox** (unprivileged-userns seccomp permitting `clone(CLONE_NEWUSER)`; verify Chrome's sandbox stays on **without** `--no-sandbox`).

**6.3 Governance — capability *is* authorization, with AI escalation (decision 4).** The safety boundary is **what the agent is provisioned to reach**, not per-action human gates: if the agent isn't allowed to do something, it is never given the credential for it. There is **no human confirmation on individual actions.** The controls that make this model hold:
- **Credential scoping is the primary control.** Each task/agent is mounted **only** the credentials it needs — never one profile holding every login. (v2's per-task/origin partitioning, reframed: it's now load-bearing, because "has the credential ⇒ allowed" only holds if scope is tight.)
- **Domain allowlist per task** as a scope boundary, and treat page content as untrusted input to resist injection turning the agent against its own scope.
- **AI escalation chain instead of human gates.** On something genuinely serious or ambiguous, the agent escalates — to its **team-leader agent**, up the hierarchy, to the **platform agent**, which is the one channel that talks directly to the user. Reuses existing plumbing: the workspace `parent_id` hierarchy, A2A peer messaging (`delegate_task`/`reply_to_workspace`), and the platform-agent→user channel (`send_message_to_user`).
- **Full audit stream** of every navigation + input, so escalation decisions and post-hoc review have ground truth and a human/platform-agent can kill a session in real time.
- Explicit assumption: agents act in good faith (not trying to break things); tight scope + escalation + audit are the backstops, not per-click approval.

**6.4 Secrets at rest — deferred (decision 2).** No volume encryption exists in the codebase today, and we are **not** claiming it. The profile volume ships **unencrypted at rest for now**, with the honest posture stated to operators; volume/profile encryption is a **tracked follow-up** (`2026-07-27-desktop-profile-encryption-rfc-followup.md`), not a launch blocker. Acceptable near-term because (a) the volume is per-workspace on an isolated network (§6.1) and (b) host-level access is already the tenant's privilege reality (§6.2) — encryption at rest defends host theft, not the near-term threat. **Revisit before any deployment handling regulated data.**

**6.5 Sidecar inbound auth (fix the exemption).** The existing inbound-secret pattern treats a Docker-internal name as "internal" and **skips the secret** — so any container could POST synthetic input to another desktop. v2: the sidecar control server **independently authenticates every inbound** `/input`/`/screenshot` (per-sidecar bearer), bound to the per-workspace network only, and the sidecar name is **excluded from the internal-caller exemption** so the platform actually presents the secret.

## 7. Co-located desktop (decision 1)

The desktop is **co-located with the workspace's own compute unit**, not scheduled independently — realized per backend by the `SidecarProvisioner` abstraction:
- **k8s:** a sidecar container in the workspace **pod**.
- **EC2/cloud:** the desktop on the workspace's **VM** (matches today's `desktop-control`).
- **Self-host Docker:** a **lifecycle-coupled sibling container** on the workspace's per-workspace network — the tenant is a locked-down Alpine box, so the desktop is its own container but bound to the workspace's lifecycle/network, created and torn down with it.

This maximizes decision-4's "CP-not-wired ≠ a big deal": co-location adds display to an already-provisioned unit rather than needing a new independent scheduler. **Scale-to-zero** is start/stop *within* the unit: self-host gets **true container-level** scale-to-zero (remove the sibling, keep the volume → frees RAM); cloud/k8s co-location is closer to **process-level** (stop the desktop, RAM partially reclaimed) until/unless the CP backend adds independent sidecar scheduling — consistent with "cloud perf not fully optimized until CP does its wiring."

## 8. Control arbitration — split VIEW from CONTROL (rewrite)

v1's model was structurally impossible on the existing lock. v2:

- **View is not lock-gated.** Introduce a **viewer session token bound to `workspace_id`** (+ display-enabled + caller authz), **not** to `controlled_by`/`expires_at`. Stop keying `DisplaySession`'s token check on the lock holder. The human can **always watch**; the lock governs `/input` only.
- **Real preemption/handoff.** Add a takeover path where **human acquire preempts the agent** atomically (writes an `interrupted` marker), without needing `force` + admin token. The current upsert `WHERE` cannot express "human beats agent" — this is new SQL.
- **`/input` arbitration is atomic + event-driven.** Enforce the lock in the `computer` tool with a check-and-act against the DB in one conditional; add **`LISTEN/NOTIFY`** (or the existing broadcast bus) so a paused agent gets a **resume signal** instead of polling. **Fail-closed:** no live agent lock ⇒ `/input` refused. Specify lease-lapse behavior explicitly.
- **Pause is a first-class control-loop state, not a tool error.** The control server returns a structured "paused" and the agent gets a blocking `desktop_wait_for_control` tool (server long-polls the lock) so "pause without busy-loop" is a real primitive. Update the existing `tool_desktop_click` to consult the lock before acting so the agent never fights the human for the one cursor.

## 9. Computer-use tool & model integration (rewrite; reconcile with existing impl)

- **Reconcile via re-point + a 3-layer split (decision 3).** Keep `a2a_tools_desktop.py` as the **single agent-facing surface** (the agent knows these tools and they carry the vision-safe coordinate reasoning) — **re-point**, don't replace, so two contracts never coexist. Atomize into three single-responsibility layers with the SSOT contract as the seam:
  1. **Actuator = the sidecar control server** — dumb executor (`POST /input`→xdotool, `GET /screenshot`→framebuffer, fixed display; no coordinate/browser logic).
  2. **Enforcement gateway = the platform-Go layer** — auth, control-lock arbitration, scale-from-zero, SSRF; a thin authenticated gateway, **not** a competing agent tool.
  3. **Agent tool = the re-pointed `a2a_tools_desktop.py`** — same schema + coordinate reasoning; transport changes from `chroot /host DISPLAY=:99` to the gateway; its types are **generated from the SSOT contract** so it can't drift.
  Net-new for cross-language SSOT: the contract needs a **Python consumer** (codegen-py or schema-validate) for the runtime tool alongside the Go binding. This split *is* the plugin-extraction path — packaging layer 3 as a native plugin later is repackaging, nothing structural.
- **Gate on a vision-capable adapter.** The runtime is `claude-code | codex | openclaw | hermes | custom`, several text-only (screenshots degrade to a one-sentence description). Advertise the `computer`/desktop tools in `tools/list` **only** when the workspace's adapter is vision-capable. A text-only adapter must not see the tool.
- **Result path = additive attachment URI, not a shared-contract change.** `dispatch` returns `(string, error)` for every tool + a public `Dispatch`; emitting base64 image blocks would mutate that shared surface and its tests. Ship screenshots via the existing `workspace:/…png` attachment convention (what production already does). Count the extra screenshot→read round-trip in the cost budget. Base64 blocks are a later, optional fidelity upgrade — the SSOT contract (§4) pins the shape either way.
- **Harden the input bridge.** Move→settle→click with `--sync`; a clipboard-paste fallback for non-ASCII/IME; focus-verify; defined modifier-state ownership given the single cursor.
- **Per-task cost budget (new).** Model bytes/tokens/round-trips × expected steps (20–60/task, ~1.5k vision tokens/screenshot). This is the dominant recurring cost of computer-use and was unbudgeted in v1.

## 10. Lifecycle — scale-to-zero (rewrite)

- **Idle signal = agent control-server activity**, not VNC/X-idle. Bump `last_agent_activity_at` on **every** screenshot and input; hold an explicit **computer-use lease** that suppresses teardown for the life of the agent loop. Idle = all of {agent activity, VNC input, VNC count>0} cold. **Re-check liveness under the lifecycle lock immediately before teardown.** (The agent opens zero VNC connections — the v1 signal would reap a working desktop.)
- **Non-blocking cold start.** First call returns a structured "starting, retry" (needs §9's result shape), not a blocked HTTP call with no server timeout. **Pre-build the sidecar image at tenant-provision** so a wake is create-only, never a 12-min local build. Drive the boot with the existing stall-runner + boot telemetry; set a start deadline below the agent's tool timeout.
- **Admission control + memory limits.** Hard `Memory`/`MemorySwap` on the sidecar; a per-node desktop budget gate in `StartDesktop` that refuses/queues with a clear result; sidecar `oom_score_adj` **above** the tenant so memory pressure sheds the desktop, never the agent.
- **Graceful teardown.** `ContainerStop` (SIGTERM + flush window → then remove), **never** the tenant's force-remove — SIGKILL mid-write corrupts the Chrome profile SQLite and silently breaks "logins persist." Safe here because the sidecar is **not** `unless-stopped`.
- **Restart policy = `no`; recover crashes in the platform** (the `computer` tool detects a dead sidecar and re-runs `StartDesktop`). **`on-failure` is banned** — SIGTERM exit-143 would resurrect the container the graceful stop just killed.
- **Per-workspace lifecycle mutex** makes start idempotent across the agent + human triggers and closes the teardown-vs-start TOCTOU inherited from the hibernation monitor.

## 11. Persistent profile volume & the credential-leak reap (rewrite)

- Browser `user-data-dir` on a durable per-workspace volume; survives scale-to-zero; only prune/wipe deletes it.
- **The real sweeper bug (corrected):** the orphan sweeper's hex-guard (`isLikelyWorkspaceID`) means `ws-<id>-desktop` is **never reaped** → on workspace delete/DB-loss the **credential volume orphans forever.** v2 requires a **new reap path** in *both* `orphan_sweeper.go` and the inline delete in `workspace_crud.go`, plus extending `Stop`/`RemoveVolume`/erase to target `ws-<id>-desktop`. **Name sidecars `wsdesk-<id>`** (not `ws-<id>-desktop`) so no `TrimPrefix("ws-")` path ever mis-parses them, and filter the sweeper **by label** (`molecule.platform.role=desktop`), not by name.
- **WipeProfile / revoke** is a first-class action reusing the prune path — but must actually target this volume (v1's prune path does not).

## 12. Data model

- **Desired config** (idle-timeout, profile-volume policy, geometry) → `compute` jsonb (extend `WorkspaceCompute`/`…Display`, `validateWorkspaceCompute`, `workspaceComputeJSON`, forward via `cpProvisionRequest`).
- **Realized handle** (profile-volume id) → nullable column beside `instance_id`; `SetComputeInstance` preserves/repoints on migration.
- **Churning lifecycle state** (running/stopped, `last_agent_activity_at`, lease) → a side table keyed by `workspace_id`, modeled on `workspace_stall_state`, off the hot heartbeat row. Idle-timeout config mirrors `hibernation_idle_minutes`.

## 13. Display proxy re-home (corrected scope)

Not a one-var swap. v2 edits: (a) re-type `displayForward`/`realDisplayForward` to take a **workspace id / resolved target**, not `instanceID`; (b) change the readiness gate from `instance_id != ""` to **desktop-address / running** in **both** `DisplaySession:46` and the status handler `workspace_compute.go:664`; (c) the agent hop must pass the SSRF guard — add an **explicit hostname allowlist** in `isSafeURL` for the sidecar name (new code) or route via the un-guarded reverse-proxy-to-resolved-target the noVNC path uses; do not rely on the private-range relaxation being on in self-host prod. Reverse proxy, token subprotocol, routes, and the (now view-split) control-lock stay.

## 14. Cross-repo scope & sequencing (decision 4)

| Concern | Repo | When |
|---|---|---|
| SDK computer-use contract | molecule-ai-sdk | P0 (authored first — everything value-pins it) |
| Sidecar image + control server | new build | P0 |
| `SidecarProvisioner` iface + Local(Docker) impl + dispatchers | workspace-server | P1 |
| Per-workspace network isolation + Redis auth + egress | workspace-server + compose/infra | **P0/P1 (prerequisite, decision 1)** |
| Display re-home + view/control split + arbitration | workspace-server | P2 |
| `computer` tool (reconciled) + attachment-URI result + adapter gating | workspace-server | P3 |
| Lifecycle: agent-activity idle + pre-build + caps + graceful + reap | workspace-server | P4 |
| Governance: credential scoping + AI escalation chain + audit | workspace-server | P5 |
| CP/k8s backend + scale-to-zero primitive | molecule-controlplane | **P6 (deferred; per-tier availability gate)** |

**Self-host first is correct** (decision 4): P0–P5 live entirely in this repo + the SDK. Honest caveat: self-host display is **greenfield** (local provisioner never did display), so P0–P5 is real build, not reuse — and it needs the new local e2e (§15) as its only green signal.

## 15. Testing

**15.1 Unit tests (per component).**
- `TestComputerUseContract_MatchesSSOT` — Go binding value-pins the SDK JSON (§4).
- SSOT consistency for new `compute` fields (extend the existing `TestComputeMetadata_SSOTInternalConsistency` family).
- `SidecarProvisioner` Local impl against the `dockerClient` fake (start idempotency, graceful stop, memory/oom config, `wsdesk-` naming).
- Sweeper reap: a removed workspace's desktop container **and** volume are gone (regression for the credential-leak bug).
- Arbitration: view-token-not-lock-bound; human preempts agent; `/input` fail-closed on no-lock; lease-lapse behavior.
- Idle signal: agent-activity keeps desktop alive with zero VNC connections; teardown re-checks under lock.
- Coordinate/window: geometry triple equality; Chrome kiosk geometry assert.
- Adapter gating: text-only adapter does not see the `computer` tool in `tools/list`.
- Security: sidecar rejects unauthenticated `/input`; SSRF allowlist admits only the sidecar name.

**15.2 E2E tests.**
- **New local-Docker display e2e** (the missing guardrail): provision a `wsdesk-<id>` sidecar on the per-workspace network and assert a real frame arrives over the **local** proxy — the analog of `staging-display.spec.ts` pointed at Docker, not EIC. **This is the P2 exit criterion** (replaces the EC2-only spec that skips in CI).
- Computer-use loop e2e: screenshot → click a known target → verify effect, against a fixed test page, on a vision-capable adapter.
- Human takeover e2e: agent driving → human preempts → agent pauses → human releases → agent resumes, no cursor fight.
- Keep the existing EC2 `staging-display.spec.ts` for the cloud path; do not treat it as local coverage.

## 16. Per-PR CI wiring

- **Codegen drift gate** — the SDK contract runs through the existing `codegen-drift` workflow; a contract/binding mismatch fails the PR (the mechanism that makes the SSOT real).
- **Go gates** — `go test`, `go vet`, **`golangci-lint` / `staticcheck`** (the "Platform (Go)" gate catches SA-codes `go test`/`vet` miss; run locally before pushing).
- **New local-Docker display e2e runs in CI** (dind), gated to run on changes under the desktop/display paths — this is the only automated proof the re-homed local path works.
- **Migrations check** (up/down) for the new side table + columns.
- **Canvas** — typecheck + the existing display unit tests; the mocked `DisplayTab.test.tsx` stays green.
- Filter any Windows-local failures against clean `HEAD` before attributing them to the change.

## 17. Documentation updates

- This spec (SSOT for the design) — keep current as decisions land.
- The HTML review page (`2026-07-27-agent-desktop-sidecar-review.html`) — refresh to v2 (security-first banner, corrected findings, new sections).
- The plugin-extraction RFC follow-up — already filed; keep the seam description in sync.
- Operator docs: per-workspace network + egress proxy + Redis auth + `userns-remap` setup for Enter OS self-host.
- `compute.display` API docs (canvas ContainerConfig/CreateWorkspace) if the config surface changes.
- A short "computer-use for agents" runtime doc: adapter vision-capability requirement + the domain allowlist / sensitive-action policy.

## 18. Cleanup

- **Retire the parallel desktop surface:** once §9 reconciles with `a2a_tools_desktop.py`, remove/redirect the duplicate so only one coordinate/lifecycle contract exists.
- Remove the `instance_id`-as-readiness assumption from the display path (superseded by desktop-address, §13); leave the EC2 file-ops path untouched.
- Drop the display-mode cloud instance-type sizing maps' relevance for sidecars (moot on Docker) — keep for EC2.
- Delete v1's now-false claims wherever they were mirrored (e.g. any "encrypted at rest" / "isolated by default" copy in canvas or docs).
- Remove the EC2-only e2e from the "local guardrail" role (keep it for cloud).

## 19. Integration risks (updated)

1. **Credential-volume leak** (§11) — must land the reap path + `wsdesk-` naming **with** the sidecar. Highest.
2. **Per-workspace network is a real infra change** to compose/infra + provisioner network handling — prerequisite, not optional.
3. **Chrome sandbox vs cap-drop/seccomp tension** (§6.2) — resolve with an unprivileged-userns profile; verify no `--no-sandbox`.
4. **Adapter gating** — shipping the tool to a text-only adapter yields a blind, non-functional loop; gate hard.
5. **Cold-start** — pre-build the image or the first call hangs to timeout.
6. **CP backend is the largest, deferred, cross-repo piece** — flag to the control-plane team early; it is *not* a fast-follow.

## 20. Phasing

- **P0** — SDK contract + sidecar image/control server + **per-workspace network + Redis auth + egress** (security prerequisite) + uid/EACCES spike.
- **P1** — `SidecarProvisioner` Local(Docker) impl + dispatchers + `wsdesk-` naming + **real reap path**.
- **P1.5** — **local-Docker display e2e harness** (gate for P2).
- **P2** — display re-home + **view/control split** + preemption/handoff, green against P1.5.
- **P3** — `computer` tool (reconciled w/ `a2a_tools_desktop.py`) + attachment-URI result + **adapter gating** + input hardening.
- **P4** — lifecycle: agent-activity idle + pre-build + memory/admission caps + graceful teardown + mutex.
- **P5** — governance: per-task credential scoping + domain allowlist + AI escalation chain + audit stream. (Secrets-at-rest encryption deferred to a follow-up RFC.)
- **P6** — CP/k8s backend + scale-to-zero primitive (deferred, cross-repo, availability gate).

## 21. Full checklist

**SSOT**
- [ ] `contracts/tool/computer-use.contract.json` authored in molecule-ai-sdk
- [ ] `molcontracts.ComputerUseContract` generated; `go.mod` bumped
- [ ] `TestComputerUseContract_MatchesSSOT` value-pins the binding
- [ ] New `compute` fields added to the SSOT consistency test family

**Abstraction**
- [ ] `SidecarProvisioner` interface + compile-time assertions
- [ ] Local(Docker) impl; CP impl returns `ErrDesktopBackendUnavailable`
- [ ] `StartDesktopAuto`/`StopDesktopAuto` dispatchers (existing `…Auto` shape)
- [ ] Per-tier availability gate surfaced in `GET /display`
- [ ] Plugin-extraction seam matches the deferred RFC

**Security (prerequisite — decision 1)**
- [ ] Per-workspace Docker network / k8s NetworkPolicy
- [ ] Backend-only network for Postgres/Redis/MinIO/LiteLLM; **Redis password**
- [ ] Egress proxy + domain allowlist; deny RFC-1918 + metadata IP
- [ ] `userns-remap` enabled; sidecar cap-drop + seccomp + `no-new-privileges`
- [ ] Chrome sandbox stays on without `--no-sandbox` (verified)
- [ ] Sidecar independent inbound auth; name excluded from internal-caller exemption
- [ ] Secrets-at-rest: **deferred** — honest posture documented + follow-up RFC filed
- [ ] Per-task credential scoping (primary control) + domain allowlist
- [ ] AI escalation chain (parent hierarchy + A2A + platform-agent→user)
- [ ] Full navigation/input audit stream + real-time kill

**Arbitration**
- [ ] Viewer token bound to workspace, not lock holder; `DisplaySession` check de-coupled
- [ ] Human-preempts-agent takeover (no admin-token requirement)
- [ ] `/input` atomic check-and-act; fail-closed on no-lock
- [ ] `LISTEN/NOTIFY` resume signal; `desktop_wait_for_control` blocking tool
- [ ] Existing click tool consults the lock before acting

**Lifecycle**
- [ ] `last_agent_activity_at` + computer-use lease; teardown re-check under mutex
- [ ] Non-blocking first wake; **pre-built image**; start deadline
- [ ] Memory/oom limits + per-node admission gate
- [ ] Graceful `ContainerStop`; restart policy `no`; platform crash recovery
- [ ] Per-workspace lifecycle mutex; idempotent start

**Compute/tool**
- [ ] Reconcile with `a2a_tools_desktop.py` (replace vs re-point decided)
- [ ] Vision-capable-adapter gating in `tools/list`
- [ ] Attachment-URI screenshot result; per-task cost budget documented
- [ ] Chrome kiosk window pinning + geometry assert
- [ ] Input hardening (settle/sync, IME paste fallback, focus verify)

**Data model / proxy**
- [ ] `compute` jsonb + side table migrations (up/down)
- [ ] `displayForward` re-typed to workspace id/target; readiness gate updated (both sites)
- [ ] SSRF hostname allowlist for the sidecar

**Tests**
- [ ] Unit tests per §15.1
- [ ] Local-Docker display e2e (P2 gate) + computer-use loop e2e + takeover e2e

**CI (per-PR)**
- [ ] Codegen drift gate green
- [ ] `go test` / `vet` / `golangci-lint`/`staticcheck`
- [ ] Local display e2e in dind on desktop-path changes
- [ ] Migration up/down check; canvas typecheck

**Docs**
- [ ] Spec + HTML review page refreshed to v2
- [ ] Operator setup (network/egress/Redis/userns) + runtime computer-use doc + API docs

**Cleanup**
- [ ] Duplicate desktop surface retired
- [ ] `instance_id`-readiness assumption removed from display path
- [ ] v1 false claims purged from mirrors
- [ ] EC2 e2e removed from the "local guardrail" role

## 22. Revision history

- **v2 (2026-07-27)** — Hardened after a five-lens adversarial review + plugin/SSOT recon. Corrected: arbitration (view/control split), self-host display is greenfield, adapter vision-gating, security isolation is a prerequisite (per-workspace net), the real sweeper bug is a credential-volume leak, image-result via attachment URI. Added: SSOT contract, backend-neutral abstraction, sensitive-action governance, agent-activity idle signal, graceful teardown, testing/CI/docs/cleanup sections, full checklist, per-tier availability gate (decision 4).
- **v1 (2026-07-27)** — Initial design from brainstorming + five-subsystem recon.

## 23. Resolved decisions (2026-07-27)

1. **Co-located** desktop — pod (k8s) / VM (cloud) / lifecycle-coupled sibling (self-host) (§7).
2. **Secrets-at-rest encryption deferred** — honest unencrypted posture + follow-up RFC (§6.4).
3. **Re-point** `a2a_tools_desktop.py` via the 3-layer split; SSOT contract is the seam (§9).
4. **No per-action human confirmation** — capability-is-authorization + tight credential scoping + AI escalation chain + audit (§6.3).

## 24. Implementation status (2026-07-27, branch `feat/agent-desktop-sidecar`)

Honest ledger. "✅" = code committed **and** verified here (`go test`/`vet`/`build` green; pre-existing Windows symlink failures filtered out). "◻︎" = pending; the note says what it's blocked on.

**Done & verified (11 commits, ~40 tests):**
- ✅ `SidecarProvisioner` interface + availability-gate backend
- ✅ Local (Docker) backend — idempotent start, graceful profile-preserving stop, memory/oom caps, isolated-net attach, restart `no`, wipe
- ✅ Collision-safe `wsdesk-` naming + role labels; agent-activity idle decision
- ✅ `compute.display.idle_timeout_seconds` + validation; `workspace_desktop_lifecycle` migration
- ✅ Desktop control server + exec actuator + `cmd/desktop-control-server`
- ✅ `Dockerfile.desktop-sidecar` + entrypoint (image *artifact*; build/publish is a pipeline step)
- ✅ Computer-use gateway — fail-closed lock, scale-from-zero, activity, per-sidecar auth
- ✅ `WorkspaceHandler` wiring — `sidecarProv` + `…Auto` dispatchers (routing pins green)

**Pending — needs a running stack to verify *properly* (integration into DB/service-dependent files):**
- ◻︎ `computer` MCP-tool registration in `mcp.go` (text-only → attachment-URI result) + vision-adapter gating
- ◻︎ Display-proxy re-home (`displayForward` re-type) + `GET /display` availability gate
- ◻︎ Control-lock view/control split + human-preempts-agent + `LISTEN/NOTIFY` resume
- ◻︎ Lifecycle store + idle sweeper (reusing `DesktopIsIdle`) + `wsdesk-` reap path + SSRF allowlist

**Pending — cross-repo (own toolchains):** ◻︎ SDK contract + codegen + `MatchesSSOT` · ◻︎ runtime tool re-point · ◻︎ CP backend.

**Pending — infra/operator:** ◻︎ per-workspace network · egress proxy · `userns-remap` · Redis auth.

**Pending — terminal human/infra gates (not autonomous):** ◻︎ image build + registry publish · ◻︎ CI / local-Docker e2e in dind · ◻︎ **security review** · ◻︎ **human-authorized production deploy**.

**Update (14 commits across 2 repos):**
- ✅ SSOT `computer-use` contract authored in `molecule-ai-sdk-ssot` (branch `feat/computer-use-tool-contract`, JSON-valid). Codegen + workspace-server `MatchesSSOT` value-pin still ◻︎ (needs the SDK codegen toolchain + module bump).
- ✅ `computer` MCP tool registered + wired to the gateway (4 tests). Screenshot returns a base64 data-URI interim; the attachment-URI result and vision-adapter gating are ◻︎ (need the workspace-write path + adapter lookup).
- ◻︎ Still pending (needs a running stack): wire the gateway's DB-backed lock/activity/token adapters into `MCPHandler` construction · display-proxy re-home · control-lock view/control split · lifecycle store + idle sweeper · `wsdesk-` reap path · SSRF allowlist.
