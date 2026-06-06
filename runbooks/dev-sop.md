# Developer SOP — PR review gate auto-fire and stale-head handling

> Last updated: 2026-06-03 (cp#2159 follow-up)
>
> Applies to: all core-PR authors and reviewers on `molecule-core` and sibling
> repos using the `qa-review` + `security-review` branch-protection gates.

---

## 1. Gitea PR-head workflow-selection rule

**Rule:** For `pull_request_target` and `pull_request_review` events, Gitea
loads the workflow definition from the **PR's HEAD branch**, not from the
base (`main`) branch.

This is different from GitHub Actions, where `pull_request_target` always
loads workflows from the base branch. Gitea's behaviour means:

- A PR that was opened **before** the `pull_request_review` trigger was added
to `qa-review.yml` / `security-review.yml` will **NOT** auto-fire on review,
because its HEAD still contains the old workflow YAML (no trigger).

- A PR that was opened **after** the trigger was added (or that has been
rebased onto a commit containing the trigger) **WILL** auto-fire, because its
HEAD contains the new workflow YAML.

### Ops implication

| PR head contains `pull_request_review` trigger? | Behaviour on APPROVED review |
|---|---|
| **Yes** (cut from current main, or rebased) | Workflows auto-queue, evaluate, and POST the `(pull_request_target)` context automatically. No slash-command needed. |
| **No** (stale head, opened before #2157) | Nothing fires. Use `/qa-recheck` + `/security-recheck` slash-commands in a PR comment, OR rebase onto current main. |

---

## 2. Standard core-PR flow (post-#2157)

```
1. Author opens PR from a branch based on current main
   → qa-review + security-review workflows run on pull_request_target
   → status contexts post (initial eval, usually red until reviews land)

2. Reviewers submit real APPROVED reviews
   → If PR head has the trigger: workflows AUTO-FIRE on pull_request_review
   → Contexts flip green (or stay red if reviewer is not in team)

3. [Optional] If contexts did not flip (stale head, event lost, etc.):
   → Anyone can comment `/qa-recheck` or `/security-recheck`
   → sop-checklist.yml refires the evaluator (read-only, idempotent)

4. Both qa-review + security-review contexts are green
   → Plain Do:merge (no force-merge needed)
```

### Key point

The `/qa-recheck` and `/security-recheck` commands are a **backstop**, not the
primary path. PRs cut from current main should auto-fire without manual
intervention.

---

## 3. Diagnosing a stale head

If a PR has real team-member APPROVED reviews but the qa/security contexts
remain red and no workflow run appears on the PR's "Actions" tab for the
review event, the PR head is likely stale.

### Quick check

```bash
# From the PR page, look at the head commit SHA, then:
curl -sS "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/contents/.gitea/workflows/qa-review.yml?ref=<HEAD_SHA>" \
  | jq -r '.content' | base64 -d | grep -c 'pull_request_review'
# 0  → stale head (no trigger in that version of the workflow)
# >0 → trigger present; auto-fire SHOULD work (if it didn't, file a tracker)
```

### Automated diagnostic

The test suite includes `test_gate_stale_head_diagnostic.py`, which reports
"auto-fire impossible for this PR" when the head lacks the trigger. Run it
in CI or locally with:

```bash
PR_NUMBER=123 python -m pytest .gitea/scripts/tests/test_gate_stale_head_diagnostic.py -v
```

---

## 4. Rebasing vs. slash-refire

| Approach | When to use | Trade-off |
|---|---|---|
| **Rebase onto current main** | PR is genuinely stale (head lacks trigger OR head is far behind main) | Clean history, gets all recent fixes, but requires force-push and re-approval if the branch was protected |
| **`/qa-recheck` + `/security-recheck`** | PR head is recent but the review event was missed, or you want to avoid rebase churn | Quick, no force-push, but does NOT fix a missing trigger in the head |

**Do not** use slash-refire as a substitute for rebasing a stale head. If the
workflow YAML in the PR head does not contain `pull_request_review`, no amount
of rechecking will make auto-fire work.

---

## 5. Live-fire verification

The `test_gate_auto_fire_live.py` regression test exercises the full runtime
path: it submits an APPROVED review to a test PR and polls for the
`(pull_request_target)` status contexts. It is skipped when no API token is
available, and is intended to catch runtime non-fire that static structural
tests (e.g. `test_gate_review_auto_fire.py`) cannot detect.

Run manually with:

```bash
export GITEA_HOST=git.moleculesai.app
export GITEA_TOKEN=<your-token>
export REPO=molecule-ai/molecule-core
export LIVEFIRE_PR_NUMBER=<test-pr-number>
python -m pytest .gitea/scripts/tests/test_gate_auto_fire_live.py -v
```

---

## 6. Fail-closed CI integrity — no fail-open gates (MERGE-BLOCKING)

**Rule:** No CI workflow, CI script, or test check may **FAIL OPEN** — i.e. it
must never report GREEN (exit 0, skip, warn-and-continue, `|| true`, or any
"return success") when it could **not actually verify its invariant**. A check
that cannot verify MUST **fail loud** (`::error::` annotation **and** a nonzero
exit) and **fail closed** (treat inability-to-verify as **FAILURE**, never as a
pass). An unverifiable check is a red check, full stop.

This is the same family of bug as the no-flakes rule (§ *No flakes*): a green
that isn't real. A flake is a green/red that flips for an unnamed reason; a
fail-open gate is a green that was never earned. Both let unverified code reach
`main`, and both are merge-blocking.

### Applies to

Required / hard gates on **protected contexts**: pushes to `main`, internal
protected branches, and **same-repo** PRs (`pull_request_target`). On these
contexts the *cause* of an unverifiable run is **irrelevant** — every one of the
following MUST fail closed:

- auth failure (401 / 403),
- missing token or identity,
- under-scoped credential,
- unreachable dependency (network, Infisical, control-plane, registry),
- a required test file that is absent or collects zero tests,
- any transient error the check cannot prove was benign.

"I couldn't check" is reported and scored exactly like "the check failed." A
gate that can be silently defanged by removing a secret is not a gate.

### The one allowed exception — explicit trust-boundary split

Legitimate degradation is permitted **only** where the secret genuinely cannot
exist — e.g. **fork PRs**, which by design have no access to repo secrets. Such
degradation is allowed **only** when it is:

1. gated behind an **explicit** fork / advisory branch in the workflow logic
   (an intentional trust-boundary split, not an incidental `if: secrets...`),
2. **clearly marked advisory** in its name and output, and
3. **NOT counted as a passing REQUIRED context** — it may inform, it may not
   satisfy the gate.

Silent degradation that satisfies a required gate is **forbidden**. If a fork PR
needs the real check, it must run via a maintainer-triggered same-repo path
(where the secret exists and the check therefore fails closed), not by quietly
passing the required context with no verification.

### Auth-failure vs. genuine-absence — do not conflate

Distinguish the two so a real finding is never masked and a masked finding is
never mistaken for real:

- **`403` (or 401) on a protected context → fail closed.** You could not verify;
  that is a check failure, not a finding about the resource.
- **A real `404` from a read made *with a valid, sufficiently-scoped token* →
  the real finding.** The resource is genuinely absent; report it as such.

A `403` reported as "resource not found" is itself a fail-open bug.

### Required practice

Every gate that depends on a token, an identity, or an external read MUST ship
with a test or workflow-lint covering the **absent-identity / unauthorized /
missing-file path** that asserts the gate **FAILS** (not skips, not passes).
Add or update that coverage in the **same PR** that adds or changes the gate.
A gate without a proven failure path is not yet a gate.

### Violations seen in this codebase (all merge-blocking if reintroduced)

- **serving-e2e** reporting vacuously GREEN when the Infisical identity is
  absent (no per-(provider × auth) completion was actually exercised).
- **branch-protection / BP-drift lints** returning `0` on a `403` instead of
  failing closed on the unverifiable response.
- **verify-template-models** run without `-strict`, so a drift it could not
  confirm passed silently.
- A **referenced-but-absent pytest file** that collects zero tests and reports
  green — silent pass with no assertions executed.

Each of these is a fail-open gate and is a merge blocker until it fails loud and
closed on protected contexts. See also the production fail-closed defaults in
`runbooks/sop-production-cicd.md` (*Production Defaults*), which apply the same
principle to deploy-time gates.

---

## References

- #2159 — gate auto-trigger not firing (root cause: stale PR heads lacking
the `pull_request_review` trigger, NOT a workflow code defect)
- #765 — static structural regression test for gate configuration
- #2157 — merged trigger addition (`pull_request_review` types: [submitted])
- #2020 — milestone confirming gate infrastructure is stable
- RFC#324 — qa-review + security-review design
