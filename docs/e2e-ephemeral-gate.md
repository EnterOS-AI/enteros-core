# The Ephemeral-CP Happy-Path Gate

*RFC "one pre-merge ephemeral gate" §04 — as built. Landed 2026-07-12
(core #4036 → `2efd5e6d8`; dind mode core #4116; CP enablers #1526 + #1549).
GATING as of 2026-07-13: the mc#4081 soak is complete (12 consecutive green
dind runs on main) and `continue-on-error` is gone — a red happy path now
BLOCKS the merge. FULL MODE as of core #4274 (2026-07-13): the gate no longer
runs `smoke`, so steps 9b/10/10b (activity, delegation provenance, cascade
guard, pause/resume/hibernate lifecycle) execute pre-merge.*

## What it is

A per-PR gate that spins up a **throwaway control-plane**, provisions a
**fresh tenant** on it, runs the core happy path (org → tenant → workspace →
live A2A LLM completion) with **zero shared staging**, and tears everything
down. It is the intended pre-merge successor to the post-merge `E2E Staging
Platform Boot` lane: failures within its proven coverage can now red the PR
instead of appearing only after merge. The post-merge lane remains active
until the residuals in [Roadmap](#roadmap-mc4081) are closed.

One scenario runner, with local and CI launch wrappers:

| Where | Command |
|---|---|
| Laptop (fast, direct) | `make e2e-ephemeral-happy-path` (or `bash local-e2e/ephemeral-cp-happy-path.sh all`) |
| Laptop, phase-by-phase | `… boot` → `… scenario` (repeatable ~90s) → `… down` |
| Laptop, dind-topology parity | `bash tests/harness/dind.sh up` → `EPHEMERAL_DIND=1 … all` |
| CI | `.gitea/workflows/e2e-ephemeral-happy-path.yml` (always posts, internally scoped, **gating**, per-job dind) |

The image-substitution matrix: a **core** PR tests `molecule-tenant:pr-<sha>`
(built from the PR) against `controlplane:baseline-dockerprov` (built from CP
at the workflow's exact `CP_EPHEMERAL_REF`, using `Dockerfile.dockerprov` — the
multi-stage image that ships the `docker` CLI the local-docker provisioner
shells out to). The local wrapper exercises the same scenario but defaults to
the current sibling CP checkout and direct Docker; select that pinned checkout
and dind explicitly when exact CI-launch parity is required.

## The topology contract (every line is a debugged failure)

The runner (`tests/e2e/ephemeral_cp_happy_path.sh`) assembles a boot-env that
makes the CP **fully self-contained**. Each element below was added after a
live failure — do not remove one without re-running the gate locally:

1. **`SECRETS_ENCRYPTION_KEY` = base64(32B), never hex-64.** The tenant parser
   (`workspace-server/internal/crypto/aes.go`) accepts "32 bytes raw or
   base64"; a 64-char hex key is valid base64 alphabet and decodes to 48
   bytes → the tenant fatals at birth. The CP's own parser accepts hex, so
   only tenants die — symptom: `Tenant provisioning timed out (last: )`.
2. **`MOLECULE_TOPO_CP_APP_DOMAIN=lvh.me` + `LOCAL_TENANT_URL_TEMPLATE=
   http://{slug}.lvh.me:8080`.** The CP's provision-readiness canary probes
   the public tenant route; on the staging domain it dials the *real* edge
   (404 for a throwaway org → provision marked failed). `lvh.me` is public
   wildcard DNS → `127.0.0.1`, so the probe loops back into **this CP's own
   wildcard proxy** and exercises the same Host→slug→org→proxy→tenant chain
   staging uses, minus the CF edge. `hostSlugFromRequest` requires the Host
   suffix to equal the app domain — the two values must agree.
3. **`CP_BASE_URL` + `MOLECULE_TOPO_CP_BASE_URL` = `http://controlplane:8080`
   (self-referential).** CP `tenant_config.go` delivers
   `MOLECULE_CP_URL := os.Getenv("CP_BASE_URL")`; unset → it delivers `""`,
   the tenant's boot refresh blanks its injected URL, and `cpurl.Base()`
   falls through to the managed default — the ephemeral tenant sent its
   workspace-provision POST **to prod** (401). Fail-open filed as **CP #1515**.
   The topo base also feeds `LLMProxyBaseURL` (workspace LLM egress).
4. **`E2E_LLM_PATH=platform` + `E2E_MODE=full` +
   `E2E_INFRA_BACKEND=local-docker` + `E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1`.**
   The backend value selects the only active harness topology. The loopback
   opt-in admits only `http://127.0.0.1:<numeric-port>` for this throwaway CP;
   without it, the shared harness accepts only the canonical staging origin.
   The gate shares the Platform Boot lane's platform-managed LLM path, but not
   its runtime or scenario breadth: this gate pins Hermes in full mode, while
   Platform Boot uses its configured runtime in smoke mode. Hermes' default
   `minimax/MiniMax-M2.7` (slash form) is platform-managed; completions flow
   workspace → tenant proxy env → this CP's `/cp/internal/llm` proxy →
   `api.minimax.io` with the CP's own `MINIMAX_API_KEY`. (`pick_model_slug`
   has **no** hermes MiniMax-BYOK arm — a stray key routes to
   `openai/gpt-4o` → `MISSING_BYOK_CREDENTIAL`.)

   **`E2E_MODE` was `smoke` until core #4274 (2026-07-13); it is now `full`.**
   Smoke skipped steps 9b/10/10b — activity log, delegation provenance, cascade
   guard, and the pause/resume/hibernate lifecycle — which made the gate a
   strictly NARROWER lane than the post-merge staging job it is meant to replace.
   That is not a theoretical gap: a dead `/activity?workspace_id=` route reached
   main *through a green gate* because the gate never executed the step that
   calls it. A gate that runs less than the lane it replaces cannot replace it.
   The reason for `smoke` (rotten full-matrix infra, core #4114) was fixed and
   the pin outlived it.
5. **`LOCAL_TENANT_BIND_ADDR=0.0.0.0`** (CP #1526). On Linux, a
   `127.0.0.1`-bound host port is unreachable from inside a container
   (`host.docker.internal` = the gateway IP, which a loopback bind refuses);
   Docker Desktop/macOS special-cases it — the canonical local-vs-CI
   divergence this gate exists to surface, caught on its first CI run.
6. **`seed_workspace_image`.** The CP's local-docker workspace provisioner
   runs the **bare tag** `workspace-template-<runtime>:latest` (self-host
   store model) — pull the registry ref and retag before boot.

## DIND mode (the CI posture)

The gate's first CI runs on the shared docker-host died to **runner
interference**: the host-loopback docker-proxy for the CP's published port
stopped answering mid-run while every container stayed healthy. An early
`pr-ephemeral-cp.sh down` implementation also swept only the leg network and
leaked per-org-net tenants across runs. Per the no-sweepers principle, the fix
is structural: the CI job runs the **whole topology inside a per-job disposable `docker:27-dind`**
(`tests/harness/dind.sh`, the harness-replays pattern from core #4057). One
atomic `docker rm -fv` destroys everything — even on cancel.

Inside the dind (`EPHEMERAL_DIND=1`):
- published ports bind `0.0.0.0` (inside the dind, `host.docker.internal` is
  the dind's own gateway — the CP #1526 lesson one level down);
- the CP publishes the dind's **fixed :8080** (`CP_PUBLISH_ADDR`, CP #1549),
  pre-forwarded to the job's host loopback at dind-create time;
- the caller-visible URL is that forward (`CP_HOST_BASE_OVERRIDE=$BASE`), so
  `up`'s boot verify proves reach *through* the forward.

## Failure diagnosis

On failure the runner emits a **run-scoped** diagnostic burst: the CP's logs
plus every container created *after* our CP (`docker ps --filter since=`) —
never a host-wide name grep, which on a shared runner picks up leaked
crash-looping containers from other runs and misdirects the RCA (it did).

## Roadmap (mc#4081)

1. ~~Soak: ≥5 consecutive green dind runs.~~ **DONE** — 12 consecutive green on
   main (`ed6209ac`..`8b35a673`, 2026-07-11..13). The only non-green entries in
   that window are `not run` (diff missed the paths filter), not failures.
2. ~~Flip `continue-on-error: true → false`.~~ **DONE 2026-07-13.** When the gate
   runs, a red happy path now blocks the merge: main's branch protection is the
   wildcard `['*']`, so every context that reports must be green.
3. ~~Drop the `paths:` filter; adopt always-fire + `detect-changes`; add the
   context to `.gitea/required-contexts.txt`.~~ **DONE** (task #85). The context
   `E2E Ephemeral CP Happy Path / E2E Ephemeral CP Happy Path` is now listed in
   `.gitea/required-contexts.txt` and fires on every PR.
4. **OPEN — the real residual.** Move the happy-path contract into
   `molecule-ai-sdk` (task #74) so core, CP, and the SDK run the *same* gate —
   then demote the corresponding post-merge E2E Staging jobs (task #86).

   **Do not execute the demotion half yet.** Two current boundaries remain:

   * **Busy force-hibernate coverage is still open** (task #92; PR #4384).
     Step 10b resumes the leaf, calls `/hibernate?force=true`, and verifies both
     the response and the durable `hibernated` row. It does **not** start a
     long-running turn, wait for `active_tasks > 0`, or prove that non-force
     hibernate returns 409 first. The current green path therefore proves an
     idle force-hibernate transition, not the force-on-busy class that exposed
     core #4293.
   * **Binary identity is now explicit in the gated staging-CD path, but not in
     the legacy push lane** (task #93). `staging-tenant-cd.yml` waits for the
     immutable `:staging-<sha>` artifact, advances the staging pin, rolls the
     fleet canary-first, checks `/buildinfo` against that SHA, and then runs its
     hard E2E. The independent `e2e-staging-saas.yml` push run still has no
     ordering edge against that rollout, so do not treat that legacy run alone
     as proof of the triggering SHA.

   Prove the PR path covers each class live and deterministically before
   retiring its post-merge coverage. Task #92 must add the busy witness and make
   it mandatory before this lifecycle residual is closed.

## Bugs this gate caught before it could gate anything

CP #1515 (empty `CP_BASE_URL` routes tenants to prod, fail-open) · CP #1526
(Linux loopback publish) · CP #1530 (CP-side gate never ran; wedged every CP
PR) · core #4065-gap (interrupt-ack arriving via the queue) · the
settling-URL SSRF race · core #4114 (staging memory plugin, unmasked) ·
runtime #284 (fresh parent auto-runs a 90-iteration task).

Since full mode (#4274): core #4293 — `POST /hibernate?force=true` skipped the
handler's active-tasks 409 but never reached the atomic claim's
`active_tasks = 0` predicate, so it stopped nothing and still answered
`200 {"status":"hibernated"}`. A cost-control endpoint that silently did nothing
and reported success. Note the honest asterisk: full mode is what put a
*lifecycle* step in front of a real tenant, but the bug was caught by the
post-merge STAGING lane, not by this gate — see Roadmap 4 / task #92.
