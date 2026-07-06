# Testing Strategy

**Status:** Policy. Update when tier definitions or thresholds change.
**Audience:** Everyone writing or reviewing code in this repo.
**Cross-refs:** [backends.md](../architecture/backends.md), [pr-hygiene.md](./pr-hygiene.md), [postmortem-2026-04-23-boot-event-401.md](./postmortem-2026-04-23-boot-event-401.md)

## The short version

- **Don't chase 100% coverage.** The last 15-20% costs as much as the first 80% and mostly adds brittle tests of trivial getters, error branches that can't fire, and stdlib wrappers.
- **Different code classes have different floors.** Auth at 80% is scarier than a DTO at 50%. Match the test investment to the risk.
- **Tests should pay rent.** A test that runs lines but asserts nothing meaningful isn't catching bugs — it's just dragging refactors down.

## Tiered coverage floors

Every Go package, every TypeScript module, every Python module fits one of these tiers. The tier determines the minimum acceptable coverage — and the review standard.

| Tier | Examples | Line floor | Branch floor | Review standard |
|---|---|---|---|---|
| **1. Auth / secrets / crypto** | `tokens`, `session_auth`, `wsauth_middleware`, `crypto/envelope`, `cp_tenant_auth` | **90%** | **85%** | Every branch tested. Adversarial scenarios (cross-tenant, expired token, null origin, malformed header). Timing considered. |
| **2. Handlers with side effects** | `workspace_provision`, `workspace_crud`, `container_files`, `terminal`, `registry` | **75%** | 70% | Happy + main error paths. DB mocks. Ownership / tenant-isolation checks. |
| **3. State machines + workers** | `scheduler`, `provisioner`, `healthsweep`, `orphan-sweeper`, `boot_ready` | **75%** | 70% | Every state transition tested, plus the transitions that *shouldn't* fire. |
| **4. Config / business logic** | `budget`, `orgtoken` (validation), `templates`, `derive-provider`, `redaction` | **70%** | 65% | Standard unit-test territory. Table-driven preferred. |
| **5. Plain DTOs / generated** | `models/*`, proto-generated Go, TypeScript interfaces | none | none | Writing tests here is theatre. Don't. |
| **6. CLI glue / cmd/*** | `cmd/server`, `cmd/molecli` | smoke only | — | Integration tests / E2E cover these. One startup-smoke test per binary. |
| **7. Third-party wrappers** | `awsapi`, `cloudflareapi`, `stripeapi`, `neonapi` | integration | — | Unit tests mock vendor shape, not behavior. Real behavior covered by staging integration. |

### Why a blanket percentage is wrong

- A `models/` package at 90% means you wrote tests for `func (w Workspace) ID() string { return w.id }`. No bugs caught, but coverage number is green.
- A `tokens` package at 75% means some rejection branch isn't covered. Maybe the *exact* branch that lets a revoked token still authenticate.
- Blanket targets make the first case look equivalent to the second. They aren't.

## Current state (as of 2026-04-23)

Run `go test ./... -cover` in each repo for up-to-date numbers. Snapshot:

### workspace-server (Go)

| Package | Actual | Tier | Target | Gap |
|---|---:|---|---:|---:|
| `internal/handlers/tokens.go` | **0%** | 1 | 90% | 90 |
| `internal/handlers/workspace_provision.go` | **0%** | 2 | 75% | 75 |
| `internal/middleware/wsauth_middleware.go` | ~48% | 1 | 90% | 42 |
| `internal/provisioner` | 45% | 3 | 75% | 30 |
| `internal/scheduler` | 49% | 3 | 75% | 26 |
| `internal/channels` | 40% | 4 | 70% | 30 |
| `internal/orgtoken` | 88% | 4 | 70% | — |
| `internal/crypto` | 91% | 1 | 90% | — |
| `internal/supervised` | 93% | 3 | 75% | — |
| `internal/plugins` | 94% | 4 | 70% | — |
| `internal/envx` | 100% | 5 | none | — |

### molecule-controlplane (Go)

| Package | Actual | Tier | Target | Gap |
|---|---:|---|---:|---:|
| `internal/awsapi` | 18% | 7 | integration | — |
| `internal/provisioner` | 48% | 3 | 75% | 27 |
| `internal/handlers` | 60% | 2 | 75% | 15 |
| `internal/billing` | 60% | 4 | 70% | 10 |
| `internal/crypto` | 68-80% | 1 | 90% | 10-22 |
| `internal/auth` | 96% | 1 | 90% | — |
| `internal/middleware` | 97% | 1 | 90% | — |
| `internal/reserved` | 100% | 5 | none | — |
| `internal/httpx` | 100% | 4 | 70% | — |

### canvas (TypeScript)

**No coverage instrumentation today.** 900 tests / 58 files pass, but coverage isn't measured. See issue #1815 for the fix: set a 70% line floor in `vitest.config.ts` and gate CI on it.

### workspace (Python)

**No pytest/coverage config.** See issue #1818: set up `pytest-cov` with `--cov-fail-under=75` (ratchet from current baseline over 2-3 weeks).

## Writing a good test

A good test:
- **Asserts a specific outcome**, not that a function runs without error.
- **Covers the exact branch that bugs would live in** — cross-tenant access, revoked-but-cached token, race on state transition.
- **Uses table-driven patterns** when the code is a dispatch with N cases. One test row per case.
- **Mocks at system boundaries** (DB, HTTP, time), not at internal package boundaries.
- **Survives refactors** — tests behavior, not internal state.

A bad test:
- Tests a getter that just returns a field.
- Mocks the function under test itself.
- Relies on `time.Sleep` or clock timing to assert order.
- Asserts `nil == nil` to boost coverage.

## No flaky, no environmental: every CI/e2e red is a deterministic coupling bug

**Status: absolute operator principle. This section is enforced (see [CI gates](#ci-gates)) and part of the SOP-checklist governance gate (`no-flaky-env-coupling`).**

There is **no "flaky"** and **no "environmental."** Every CI or e2e red is a
*deterministic* bug in how the test is **coupled to its environment** — not an
act of God. The environment doing environmental things — a staging redeploy
mid-test, a busy box, a dependency restarting, live state changing under you —
is **normal and expected**. A correct test tolerates all of it. A test that
depends on the environment being **fast, quiet, unchanging, or perfectly-timed
is a broken test.** Fix the coupling; never tolerate the variance.

### The five coupling classes

Every red you are tempted to call "flaky" is one of these. Name the class, then
root-fix it:

| Class | What it is | The root fix |
|---|---|---|
| **A. Missing input** | The test path never injects an env/config the code needs, so the code can't do its job and the assertion fails 100% under that path (e.g. canvas e2e #2162: no `MOLECULE_LLM_*` proxy → agent can't boot → asserting agent content fails). | **Inject the input.** Provide the env/secret/fixture the real code requires. |
| **B. Race** | A background write or async state transition lands *after* you observed a "done" signal (e.g. dormant-clobber #3456: a status-write lands ~300 ms after verified-paused → resume/hibernate 404). | **Order on the real event**, not wall-clock. Wait for the specific state, or make the write idempotent/ordered. |
| **C. False-ready** | You gate on the wrong signal — a shared, always-up endpoint (`CP /health`) instead of the *specific* resource's ready signal — so you proceed into the boot window (create-502 → CP#1129). | **Poll the real ready signal** for the exact resource (this org/workspace/route), never a shared liveness proxy. |
| **D. Shared-live-state collision** | Concurrent e2e runs, or a CD roll, touch the **same** mutable staging object → cross-test interference. | **Isolate state.** Per-run org/workspace/namespace; never assert on a globally-shared mutable. |
| **E. Near-capacity fixed timeout** | A hard-coded `sleep N` / fixed deadline that passes on a quiet box and reds when staging is slow under load. | **Real-signal poll + a 10× safety net you never wait out.** The success path proceeds *instantly* on the real signal; the net only bounds a true hang. |

### The rule

> A test that depends on the environment being fast, quiet, unchanging, or
> perfectly-timed is **broken**. Fix the coupling — inject the input,
> poll the real signal, isolate the state, assert deterministically.

A hard timeout is legitimate **only** as a ~10× safety net you never actually
wait out: the success path polls the real signal and proceeds the moment it
appears. A failure *under load* is not "the environment" — it is a deterministic
bug (a near-capacity timeout, a false-ready gate, or a race). Slow is allowed;
**fail is not.** Headroom may only ever *slow* a test, never *fail* it.

### Bans

- **Never tolerate variance.** "It usually passes" is an unfixed bug, not a status.
- **Never retry-and-hope.** Re-running until green hides the coupling; it does not fix it. (Auto-retry as a *diagnostic* is fine; auto-retry as the *fix* is banned.)
- **Never disable or park a required producer** to make a red go away. An enforced
  required context whose producer is disabled is a **phantom gate** — it looks
  green because nothing runs. `Guard F` (the phantom-gate lint) catches this.
- **Never call a red "flaky" or "environmental"** in a review, PR, or COE to
  dismiss it without a stated deterministic root cause **and** a fix. The
  `lint-env-coupling-dismissal` check reds any COE/incident/postmortem doc that
  uses those words as a dismissal without naming a root cause.

### Gate mechanics you must know

- **`[*]` is a *merge* gate, not a *main-green* guarantee.** The `["*"]`
  branch-protection wildcard means "every emitted status on the PR head is
  success before merge." It does **not** promise `main`'s push-context stays
  green afterward. A red **push-context on `main` is a P-bug to root-fix**, not
  to tolerate.
- **`block_on_outdated_branch` MUST be `True`** (done on `core` + `controlplane`):
  a PR must be re-tested against current `main` before merge, so a green that was
  computed against a stale base can't land a main-red.

## Enforcement

### CI gates

- **Go**: `go test ./... -cover` + a pre-commit script that compares coverage to `.coverage-baseline` and fails on drops > 2 points in a tier-1 package.
- **TypeScript**: `vitest --coverage` with thresholds in `vitest.config.ts`. Fails CI if below.
- **Python**: `pytest --cov-fail-under=75` in the Python CI job.
- **No-flaky / env-coupling** (`lint-env-coupling-dismissal`, `.gitea/scripts/lint_env_coupling_dismissal.py`): reds a PR whose COE/incident/postmortem docs dismiss a red as "flaky"/"environmental" without a stated deterministic root cause, and reds a **changed** e2e file that introduces a fixed `sleep`/`setTimeout`/`time.Sleep` without an accompanying real-signal poll. Escape hatch for a genuinely-intended wait: a `lint-allow: env-coupling` marker on the line/near it. The [`no-flaky-env-coupling`](../../.gitea/sop-checklist-config.yaml) SOP-checklist item requires a non-author (or `ai-sop-ack`) peer to confirm the same at review time.

### Review expectations

- Any PR touching a tier-1 package that lowers its coverage needs an explicit reviewer sign-off and justification.
- New code should arrive at or above its tier's floor.
- Untested files in tier-1 or tier-2 should be flagged in review, not waved through.

## Related

- [Issue #1821](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1821) — policy tracking issue
- [Issue #1815](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1815) — Canvas coverage instrumentation
- [Issue #1818](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1818) — Python pytest-cov
- [Issue #1814](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1814) — workspace_provision_test.go unblock
- [Issue #1816](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1816) — tokens.go coverage
- [Issue #1819](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1819) — wsauth_middleware coverage
