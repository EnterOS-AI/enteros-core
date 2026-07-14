# Security-reviewer agent — SSOT for the `security-review` gate

This file is the checked-in, reviewable specification of the **agent that
clears the `security-review` merge gate**. It exists so the security gate is
resolved by a *genuine, adversarial, security-lens* review performed by a
reviewer **agent** — not by a human, and **not** by the same identity that
clears `qa-review`. The human is not the gate; the gate stays a real security
review, just performed by an agent with a distinct security lens.

Nothing in this file changes how a review is *evaluated* — that is owned by the
fail-closed SSOT predicate in `.gitea/scripts/_approval_validator.py` and the
evaluator `.gitea/scripts/review-check.sh` (invoked by
`.gitea/workflows/security-review.yml` with `TEAM=security TEAM_ID=21`). This
file documents **who** clears the gate, **with what lens**, and **how the agent
is invoked** — the producer side of the producer/consumer split.

---

## 1. Identity — a DISTINCT security lens

| | Gitea login | Gitea id | team `security` (id 21) | team `qa` (id 20) |
|---|---|---|---|---|
| **Security reviewer (this agent)** | `core-security` | 68 | ✅ member | ❌ not a member |
| General QA reviewer | `molecule-code-reviewer` | 109 | member (see bootstrap) | ✅ member |

`security-review` is **team-keyed, not login-keyed**: `review-check.sh` passes
iff there is ≥ 1 genuine `APPROVED` review on the PR head by a non-author who is
a member of Gitea team `security` (id 21). `core-security` (id 68) is **already
a member of team 21 and is NOT in team 20**, so it is the natural *distinct*
security-lens identity.

The rubber-stamp this removes: `molecule-code-reviewer` (id 109) is currently in
**both** team 20 (qa) and team 21 (security), so a single `APPROVE` from it
clears `qa-review` **and** `security-review` at once — one identity, one lens,
two gates. Binding `security-review` to `core-security` makes the security gate a
**second, orthogonal** judgement. See [§7 Bootstrap](#7-bootstrap-one-time-org-owner-action)
for the one-time roster action that makes this diversity *enforced* rather than
merely *available*.

The `security-review` *status gate* and the merge-queue `REVIEWER_SET` *floor*
are two separate things. `security-review` is team-keyed to team `security`
(id 21) and is cleared by a genuine non-author `core-security` approval — that
is a **gate**, and it stays orthogonal to the QA lens regardless of the peer
floor below.

Separately, the merge-queue enforces a genuine-peer floor of
`required_approvals = 1` from accounts in `REVIEWER_SET`. **CTO 2026-07-14:**
`REVIEWER_SET` is the union of the review, management, and owner teams
(`code-reviewers`/`qa`/`security`/`managers`/`Owners`) — reviewers, managers,
and owners may all satisfy it. This *widened* the earlier rule (which excluded
owner accounts and named only `{agent-reviewer, agent-researcher,
agent-reviewer-cr2}`): those three accounts were driven by no automation and
produced **0** genuine approvals across 29 merges, so the floor was
unsatisfiable and every merge fell through to a manual owner/admin path. Because
`core-security` is now inside `REVIEWER_SET`, its approval **can** count toward
the floor — but a merge still needs a distinct non-author merger, and the
`security-review` gate remains a separate required judgement.
See `.gitea/scripts/gitea-merge-queue.py` for the roster + the team-keyed
follow-up (making `REVIEWER_SET` resolve from team membership instead of a login
list, once the merge-actor token's org:read scope is confirmed).

---

## 2. Persona — adversarial, security-lens ONLY

The security reviewer runs a **different system prompt** from the general QA
reviewer. It is adversarial and *verify-don't-buy*: it reproduces the threat
model against the **actual diff** and never accepts a "looks fine" without
grounding it in the changed lines. It reviews with a **security lens only** and
deliberately does **not** re-run the QA correctness/test/style pass — that is the
`qa-review` lane's job. This guarantees `security-review` is a second orthogonal
lens, not a duplicate of the QA approval.

### Rubric

1. **Authn / authz & privilege boundaries** — who can call what; missing
   authorization checks; privilege escalation; tenant/workspace isolation.
2. **Secret handling & token scope** — least-privilege tokens; no token in
   `argv` (use the `curl -K <mode-600 authfile>` pattern, never
   `-H "Authorization: token $T"`); no secret echoed to logs; correct
   read-vs-write credential separation (evaluator token read-only, status/review
   POST on a distinct write-scoped token).
3. **Injection** — SQL / command / template / **prompt** injection; untrusted
   input reaching a shell, a query, or a model context.
4. **Supply chain** — pinned action SHAs (not floating tags); dependency / image
   integrity; no unpinned fetch-and-exec.
5. **Trust boundary** — `pull_request_target` must not execute PR-head code;
   BASE-ref checkout; no untrusted PR input flowing into a privileged step.

### Verdict behavior

- On **any** real security finding: submit a genuine Gitea **`REQUEST_CHANGES`**
  review (event `REQUEST_CHANGES`, exact enum) — never a comment. A
  `REQUEST_CHANGES` genuinely blocks the merge because branch protection has
  `block_on_rejected_reviews = true`.
- Submit **`APPROVED`** (exact enum) **only** when the security posture is
  genuinely clean. The `APPROVE` is a substantive security judgement, not a
  rubber stamp.
- **Never** approve via an issue comment / `LGTM` / `[agent-prefix]` text.
  Comment-based approval was explicitly removed as a bypass in
  `review-check.sh`; only a real `APPROVED` review object from the reviews API
  counts.

---

## 3. Invocation

### Primary — operator reviewer harness (security lane)

Reuse the same operator-conductor reviewer harness that already drives
`molecule-code-reviewer` to post genuine reviews, and add a **second security
lane** running under the `core-security` bot credential. Per open-PR event
(`opened` / `synchronize`) plus a `/security-recheck` comment refire, the
harness:

1. Pulls the PR **diff + changed files** via the Gitea API — **no PR-head code
   execution** (same read-only trust posture as the evaluator workflow).
2. Runs the security-lens model pass using the persona/rubric in §2.
3. POSTs a **real** Gitea review to
   `POST /repos/molecule-ai/molecule-core/pulls/{n}/reviews` with
   `event = APPROVED` or `REQUEST_CHANGES` (exact enum, never a comment), bound
   to the **current head sha**, using the `core-security` token.

This path is preferred because the harness already holds the review-posting
tokens and the `REVIEWER_SET` / merge plumbing, and it never touches PR-head
code.

### Fallback — in-CI producer workflow

If the harness lane is unavailable, a thin `security-reviewer.yml`
(`pull_request_target`, **BASE-ref checkout** — trusted context, no PR-head
execution) can shell the same security-lens harness and emit an identical
`core-security` `APPROVED` / `REQUEST_CHANGES` review object. The output is the
same real review; only the driver differs.

### Consumer (unchanged)

`.gitea/workflows/security-review.yml` stays a **pure evaluator** (read-only,
BASE-ref): it reads the reviews via `review-check.sh`, confirms a genuine
team-21 `APPROVED` on head, and maps it to the branch-protection status. Correct
producer/consumer separation — the producer decides, the evaluator records.

---

## 4. How the approval maps to the branch-protection status

1. `core-security` submits `APPROVED` on the current head.
2. `security-review.yml` fires on `pull_request_review [submitted]`.
3. `review-check.sh` runs `_review_check_filter.py` →
   `_approval_validator.is_genuine_approval`, finds `core-security` as a genuine
   non-author approver, then probes `GET /teams/21/members/core-security` → 200.
4. The job succeeds → `security-review / approved (pull_request_review) = success`
   **and** the dedicated re-post step publishes the branch-protection-required
   `security-review / approved (pull_request_target) = success` via
   `CI_STATUS_TOKEN` (fetched from the Infisical SSOT, wired to the
   `claude-status-reaper` identity).
5. Branch protection's `security-review / approved (pull_request_target)`
   context is satisfied.

If `core-security` submits `REQUEST_CHANGES`: there is no genuine `APPROVED`
candidate, so `security-review` stays red **and** `block_on_rejected_reviews =
true` independently blocks the merge — a real security objection holds the line.

This is exactly the path already proven end-to-end on PR #3369, where
`molecule-code-reviewer` (also in team 21) cleared
`security-review / approved (pull_request_target)`. Swapping the approver login
to `core-security` changes nothing mechanically because the gate keys on team-21
membership.

---

## 5. Genuineness safeguards (why the APPROVE is not a no-op)

- The gate consumes a **real** Gitea `APPROVED` review object, validated by the
  fail-closed SSOT predicate in `_approval_validator.py`: `state == "APPROVED"`
  (exact enum, no case coercion), `official is True`, not `dismissed`, not
  `stale`, and `commit_id` present **and** equal to the current head sha. A
  missing/old `commit_id` is rejected.
- Branch protection `dismiss_stale_approvals = true`: any new push invalidates
  the prior `APPROVE`, forcing a **fresh** security pass on the new head.
- Branch protection `block_on_rejected_reviews = true`: a security
  `REQUEST_CHANGES` genuinely blocks the merge.
- The predicate is mutation-resistant: `.gitea/scripts/tests/` trips if anyone
  removes the `commit_id` check, re-adds a "no commit_id is accepted" escape
  hatch, or flips `!=`/`==` in the head comparison.
- The entire governance surface (`.gitea/workflows/`, `.gitea/scripts/`,
  `.gitea/reserved-paths.txt`) is **self-reserved** (see
  `.gitea/reserved-paths.txt`), so the gate cannot be quietly gutted — a change
  to it must clear the very gates it changes.

---

## 6. Separation guarantees (verified)

- `qa-review` keys on team 20, `security-review` on team 21 — **distinct
  rosters**.
- `core-security` ∈ team 21, ∉ team 20 → its `APPROVE` cannot clear `qa-review`.
- `core-security` ∉ `REVIEWER_SET` → its `APPROVE` does not satisfy the merge
  floor; a `REVIEWER_SET` peer approval is still required to merge.
- `reserved-path-review` (author ≠ approver on head) and the merge-queue
  author ≠ merger rule, plus the `audit-force-merge.sh` detective backstop
  (`incident.reserved_self_merge`), are all unchanged.

---

## 7. Bootstrap (one-time, org-Owner action)

Because the enabling mechanism already keys on team-21 membership and
`core-security` is already in team 21, `security-review` is **agent-resolvable
today with zero workflow / branch-protection / script edits**. The only steps to
make the **distinct-persona** upgrade *enforced* are org-Owner roster /
harness-config actions — never a branch-protection force-change:

1. Keep `core-security` (id 68) in team `security` (id 21). *(Already true.)*
2. Point the operator reviewer harness **security lane** at the `core-security`
   bot credential (harness / Infisical config).
3. **Recommended for enforced diversity:** remove `molecule-code-reviewer`
   (id 109) from team `security` (id 21) — keep it in team `qa` (id 20). After
   this, clearing `qa-review` + `security-review` requires two **distinct**
   persona `APPROVE`s, and the security gate can only be cleared by the
   security-lens identity.

This first governance PR (which introduces this file and documents the
mechanism at the gate) touches `.gitea/workflows/` and is therefore
self-reserved — it must clear `qa-review`, `security-review`,
`reserved-path-review`, and CI, and be merged by the merge-queue bot
(author ≠ merger). Merging it — and performing the org-Owner roster action
above — is the **one human step**. From then on, every future PR's
`security-review` is cleared by the `core-security` security-lens agent with **no
human in the loop**; `dismiss_stale_approvals`, `block_on_rejected_reviews`, and
the fail-closed predicate keep it honest.
