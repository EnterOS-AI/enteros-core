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

## References

- #2159 — gate auto-trigger not firing (root cause: stale PR heads lacking
the `pull_request_review` trigger, NOT a workflow code defect)
- #765 — static structural regression test for gate configuration
- #2157 — merged trigger addition (`pull_request_review` types: [submitted])
- #2020 — milestone confirming gate infrastructure is stable
- RFC#324 — qa-review + security-review design
