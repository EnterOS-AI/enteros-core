#!/usr/bin/env python3
"""lint_no_coe_on_required — forbid continue-on-error on REQUIRED jobs.

Forbidden shape
---------------
A job in `.gitea/workflows/*.yml` that BOTH:
  - has `continue-on-error: true` (job-level), AND
  - emits a commit-status context that is in the repo's required
    branch-protection set.

`continue-on-error: true` makes a failed step roll up to a *success*
job status (Gitea Quirk #10). On a job whose context branch-protection
treats as REQUIRED, that converts a real failure into a green gate —
exactly the mc#1982 masking incident (continue-on-error on platform-build
hid regressions for ~3 weeks; SOP#765). This makes SOP#765 mechanical.

What "REQUIRED" means here (widened 2026-07-14 — finding H3)
-----------------------------------------------------------
This lint USED to scope its required-set to the ~8 contexts enumerated in
`.gitea/required-contexts.txt`. That was a VACUOUS SCOPE: main's branch
protection is `status_check_contexts: ["*"]` — the all-green WILDCARD gate
— which means EVERY POSTED commit status must be `success` before a PR can
merge. So every job on a PR-triggered workflow is de-facto merge-REQUIRED,
not just the eight the file happens to enumerate.

The consequence was the exact bug this lint exists to prevent, one level up:
the lint that forbids `continue-on-error` from turning a real failure into a
green gate was itself BLIND to every job outside those eight lines. On
2026-07-14 twelve jobs on PR-triggered workflows carried
`continue-on-error: true` and could not go red at all; this lint reported
"OK" on all of them.

The required-set is therefore now the UNION of:
  (a) every context enumerated in `.gitea/required-contexts.txt`, and
  (b) EVERY job on a workflow triggered by `pull_request` /
      `pull_request_target` — because each such job posts a commit status
      context, and `["*"]` makes every posted context merge-blocking.

Fail-closed
-----------
Anything this lint cannot prove safe is a FAILURE, not a pass:
  - a masked job in the required-set that is not explicitly waived -> FAIL
  - a workflow whose YAML does not parse -> FAIL (previously `continue`,
    which silently made the jobs in a broken file invisible to the lint —
    a hole of exactly the same shape)
  - a missing allowlist file -> FAIL

Waivers (MASK_WAIVERS)
----------------------
A mask may only survive if it is listed in MASK_WAIVERS below with a reason
and a tracking reference. The list is a DEBT REGISTER: it is enumerated,
reviewable, and meant to shrink. A NEW mask on any PR-posting job is an
immediate red — which is the property the lint was supposed to have all
along. A waiver that no longer corresponds to a real mask is reported as a
prune-me WARNING (not a failure) so this lint cannot dead-lock against the
in-flight PRs that are removing those masks.

Required-context SSOT
---------------------
A checked-in allowlist (REQUIRED_CONTEXTS_FILE, default
.gitea/required-contexts.txt — one context per line, `#` comments). This
is authoritative because the CI token cannot always read
branch_protections (cp returns 403). When a token IS available
(GITEA_TOKEN + repo admin) the script ALSO live-reads branch_protections
and fails if the checked-in allowlist has drifted from live BP — but a
403/absent token degrades gracefully to allowlist-only (warn, don't fail
on the read).

Context derivation
------------------
Gitea emits the per-job status context as `"{workflow_name} / {job_name
or job_key}{suffix}"` where suffix is ` (pull_request)` / ` (push)` on
those events. The allowlist stores the bare `workflow / job` form; we
match a required context if its event-stripped form equals a job's
`workflow / job`.
"""
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("FAIL: PyYAML not available", file=sys.stderr)
    sys.exit(2)

WORKFLOWS_DIR = os.environ.get("WORKFLOWS_DIR", ".gitea/workflows")
REQUIRED_FILE = os.environ.get("REQUIRED_CONTEXTS_FILE", ".gitea/required-contexts.txt")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", "")
GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
REPO = os.environ.get("REPO", "")

EVENT_SUFFIX = re.compile(r"\s*\((pull_request|push|pull_request_target)\)\s*$")

# The all-green WILDCARD gate. Gitea branch-protection accepts "*" in
# status_check_contexts to mean "every posted commit status must be
# success" (the [*] gate rolled out per gate-model-require-all-individually
# / wildcard-gate-enforces-not-a-bug). "*" is a META-gate, NOT an enumerable
# job context — no job emits a status literally named "*", so it cannot (and
# must not) live in the enumerated allowlist: putting it in the merge-queue's
# enforced list would look for a `*` success on every PR head and freeze the
# queue. Recognize it here so the live-BP drift check treats it as the
# sanctioned wildcard rather than "a context missing from the allowlist".
WILDCARD_CONTEXTS = {"*"}

# Events that cause Gitea to post a per-job commit status onto a PR head.
# Under BP ["*"] every one of those statuses is merge-blocking, so every job
# on such a workflow is de-facto REQUIRED (see module docstring, finding H3).
PR_EVENTS = {"pull_request", "pull_request_target"}

# ── MASK DEBT REGISTER ──────────────────────────────────────────────────
# Jobs allowed to keep `continue-on-error: true` DESPITE posting a
# merge-blocking PR context. Every entry needs a reason and a tracking
# reference. This list must only ever SHRINK. Adding to it is a deliberate,
# reviewable act — which is the whole point: the debt is now enumerated
# instead of invisible.
#
# NOT waived (masks REMOVED in this PR — each proven green AND blessed by the
# repo's own pre-flip gate, lint_pre_flip_continue_on_error.py):
#   Harness Replays / Harness Replays
#   Lint workflow YAML (repository compatibility policy) / Lint workflow YAML ...
MASK_WAIVERS = {
    # ── Blocked by the PRE-FLIP GATE, not by a red job ──────────────────
    # The two below are PROVEN GREEN (see each workflow's inline comment),
    # but lint_pre_flip_continue_on_error.py — which is itself an unmasked,
    # merge-blocking required context — refuses to bless the flip, so
    # un-masking them would red every PR. Left masked deliberately.
    # (A third, `Harness Replays / detect-changes`, was in this bucket until
    # #4521 unmasked it — see the note just below.)
    #
    # `Harness Replays / detect-changes` was UNMASKED by #4521 (proven green
    # 15/15 step-level, and still green on current main) — its waiver has been
    # pruned from this register. The pre-flip-gate blind spot that kept it masked
    # (lint_pre_flip_continue_on_error.py:426 reads the un-paginated
    # `/commits/{sha}/status` combined endpoint, 30-status cap while the repo
    # posts 60 contexts) is tracked separately in task #106.
    #
    # The next two are PATHS-FILTERED workflows: they essentially never run on a
    # push to main, and the pre-flip gate only accepts main-PUSH runs from the
    # last RECENT_COMMITS_N (=5) commits as proof. So they can NEVER satisfy it —
    # a structural catch-22, not a red suite. FIX = let the pre-flip gate accept
    # PR-event runs (or widen the window) for paths-filtered workflows.
    "lint-mask-pr-atomicity / lint-mask-pr-atomicity":
        "PROVEN GREEN locally (lint exit 0 + unit tests pass) but paths-filtered, so it "
        "has no main-push runs for the pre-flip gate to read — structurally unverifiable "
        "by that gate today. Follow-up filed.",
    "SECRET_PATTERNS drift lint / Detect SECRET_PATTERNS drift":
        "PROVEN GREEN locally (all consumers aligned, exit 0) but paths-filtered — same "
        "pre-flip catch-22. PRIORITY: a credential-hygiene gate must not be unable to "
        "fail. Follow-up filed.",
    # In-flight: these masks are being removed by other open PRs. Waived here
    # ONLY so this lint can land without a merge-order dead-lock against them.
    # When those PRs merge, the waiver goes stale and is reported as prune-me.
    "E2E Staging SaaS (full lifecycle) / Prune stale e2e DNS records":
        "mask removed by PR #4326 (in flight) — prune this waiver once #4326 lands",
    "Ops Scripts Tests / Ops scripts (unittest)":
        "mask removed by PR #4325 (in flight) — prune this waiver once #4325 lands",
    # Pre-existing debt, NOT yet proven green — deliberately left masked
    # rather than un-masked blind (un-masking a red suite wedges every PR).
    # Follow-ups filed; prove-then-unmask, one lane at a time.
    "design-token-drift / Canvas ↔ app design-token SSOT drift":
        "pre-existing mask; not yet proven green — follow-up task filed",
    "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (real image + MiniMax LLM, advisory)":
        "pre-existing mask on the explicitly-advisory real-image lane; "
        "known-flaky (heartbeat host.docker.internal, task #77) — follow-up task filed",
}


def strip_event(ctx):
    return EVENT_SUFFIX.sub("", ctx).strip()


def workflow_events(doc):
    """Return the set of trigger event names for a parsed workflow doc.

    PyYAML resolves an UNQUOTED `on:` key to the boolean True (YAML 1.1
    treats on/off/yes/no as booleans). Workflows in this repo use BOTH the
    bare `on:` form (e.g. lint-workflow-yaml.yml) and the quoted `"on":` form
    (e.g. harness-replays.yml), so a naive `doc.get("on")` silently returns
    None for half the repo — which would make this lint blind to exactly the
    jobs it is supposed to police. Handle both spellings.
    """
    on = doc.get("on")
    if on is None:
        on = doc.get(True)
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return {e for e in on if isinstance(e, str)}
    if isinstance(on, dict):
        return {k for k in on.keys() if isinstance(k, str)}
    return set()


def load_required_allowlist(path):
    if not os.path.isfile(path):
        return None
    out = set()
    with open(path) as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                out.add(strip_event(line))
    return out


def job_contexts(workflows_dir, parse_errors=None):
    """Return dict context -> (file, job_key, continue_on_error_bool, pr_triggered).

    `parse_errors` (optional list) collects "<path>: <error>" for workflows
    whose YAML does not parse. The caller MUST fail on a non-empty list: a
    file we cannot parse is a file whose jobs we cannot police, and silently
    skipping it is a vacuous pass of precisely the shape this lint exists to
    kill. (This function previously `continue`d on YAMLError.)
    """
    contexts = {}
    for fn in sorted(os.listdir(workflows_dir)):
        if not (fn.endswith(".yml") or fn.endswith(".yaml")):
            continue
        path = os.path.join(workflows_dir, fn)
        try:
            with open(path) as f:
                doc = yaml.safe_load(f)
        except yaml.YAMLError as e:
            if parse_errors is not None:
                parse_errors.append(f"{path}: {e}")
            continue
        if not isinstance(doc, dict):
            continue
        wf_name = doc.get("name") or os.path.splitext(fn)[0]
        # Does this workflow post commit statuses onto a PR head?
        pr_triggered = bool(workflow_events(doc) & PR_EVENTS)
        jobs = doc.get("jobs") or {}
        if not isinstance(jobs, dict):
            continue
        for jkey, jval in jobs.items():
            if not isinstance(jval, dict):
                continue
            jname = jval.get("name") or jkey
            coe = jval.get("continue-on-error", False)
            # Gitea coerces string "true" truthy.
            coe_bool = coe is True or (isinstance(coe, str) and coe.strip().lower() == "true")
            ctx = f"{wf_name} / {jname}"
            contexts[strip_event(ctx)] = (path, jkey, coe_bool, pr_triggered)
    return contexts


def pr_posted_contexts(ctxs):
    """Every context posted on a PR-triggered workflow == de-facto required
    under branch protection ["*"]. This is the widened required-set (H3)."""
    return {ctx for ctx, info in ctxs.items() if info[3]}


def live_required_contexts():
    """Best-effort live BP read. Returns set or None (degrade)."""
    if not (GITEA_TOKEN and REPO):
        return None
    try:
        import json
        import urllib.request
        url = f"https://{GITEA_HOST}/api/v1/repos/{REPO}/branch_protections"
        # CF WAF 1010-bans the default Python-urllib UA; send a non-urllib
        # UA so this BP read reaches Gitea (transport-only).
        req = urllib.request.Request(
            url,
            headers={
                "Authorization": f"token {GITEA_TOKEN}",
                "User-Agent": "molecule-ci-gate/1.0 (+gitea-api)",
            },
        )
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.load(r)
        out = set()
        for b in data:
            if b.get("branch_name") in ("main", None):
                for c in (b.get("status_check_contexts") or []):
                    out.add(strip_event(c))
        return out
    except Exception as e:
        print(f"::warning:: live branch_protections read failed ({e}); using checked-in allowlist only")
        return None


def main():
    if not os.path.isdir(WORKFLOWS_DIR):
        print(f"OK: no {WORKFLOWS_DIR}")
        return 0
    required = load_required_allowlist(REQUIRED_FILE)
    if required is None:
        print(f"FAIL: required-contexts allowlist {REQUIRED_FILE} is missing — "
              f"this file is the SSOT for which contexts are merge-required.")
        return 1

    # Optional live-BP drift check (graceful). The "*" all-green wildcard is
    # the sanctioned meta-gate, not drift — exclude it from the comparison
    # (see WILDCARD_CONTEXTS). It is not an enumerable context and must never
    # be added to the enforced allowlist.
    live = live_required_contexts()
    if live is not None:
        only_live = (live - required) - WILDCARD_CONTEXTS
        if only_live:
            print("FAIL: branch-protection required contexts NOT in the checked-in allowlist "
                  f"({REQUIRED_FILE}) — allowlist has drifted from live BP:")
            for c in sorted(only_live):
                print(f"  - {c}")
            print("  Add them to the allowlist (or remove from BP).")
            return 1

    # Fail-closed: a workflow we cannot parse is a workflow whose jobs we
    # cannot police. Never silently skip it.
    parse_errors = []
    ctxs = job_contexts(WORKFLOWS_DIR, parse_errors=parse_errors)
    if parse_errors:
        print("FAIL: workflow YAML did not parse — cannot prove these files are mask-free:")
        for e in parse_errors:
            print(f"  - {e}")
        return 1

    # H3: the required-set is the UNION of the enumerated allowlist and EVERY
    # job posting a context on a PR-triggered workflow. BP is ["*"] — every
    # posted context is merge-blocking, so scoping to the enumerated ~8 left
    # this lint blind to every other gate in the repo.
    pr_ctxs = pr_posted_contexts(ctxs)
    effective_required = required | pr_ctxs

    fails = []
    waived = []
    for ctx in sorted(effective_required):
        info = ctxs.get(ctx)
        if info is None:
            # The context is required but no job currently emits it — that's
            # a different lint's concern (required-context-exists). Skip.
            continue
        path, jkey, coe, _pr = info
        if not coe:
            continue
        if ctx in MASK_WAIVERS:
            waived.append(f"{path}: `{ctx}` — {MASK_WAIVERS[ctx]}")
            continue
        fails.append(f"{path}: job `{jkey}` (context `{ctx}`) posts a merge-blocking "
                     f"status but has continue-on-error: true")

    # A waiver for a mask that no longer exists is dead weight. Report it so
    # the debt register stays honest — but do NOT fail on it: the in-flight
    # PRs removing those masks would otherwise dead-lock against this lint
    # (whichever merged second would red main).
    for ctx in sorted(MASK_WAIVERS):
        info = ctxs.get(ctx)
        if info is None or not info[2]:
            print(f"::warning::stale mask waiver — `{ctx}` no longer carries "
                  f"continue-on-error (or no longer exists). Prune it from "
                  f"MASK_WAIVERS in {os.path.basename(__file__)}.")

    for w in waived:
        print(f"::warning::WAIVED mask (debt, must shrink) — {w}")

    if fails:
        print("FAIL: continue-on-error: true on a merge-REQUIRED job (mc#1982 / SOP#765):")
        for f in fails:
            print(f"  - {f}")
        print()
        print("Why: continue-on-error makes a failed step roll up to a SUCCESS")
        print("  job status (Gitea Quirk #10). main's branch protection is")
        print("  status_check_contexts=[\"*\"] — EVERY posted context must be")
        print("  success — so every job on a pull_request workflow is REQUIRED.")
        print("  A masked one cannot go red at all: it is a gate whose PASS arm")
        print("  is what a BROKEN job produces. That is the mc#1982 incident.")
        print()
        print("  Fix: PROVE the job green (run it locally / read recent real runs),")
        print("  then remove continue-on-error. If it is genuinely red, do NOT")
        print("  un-mask it — fix the job, or add an explicit, tracked entry to")
        print("  MASK_WAIVERS so the debt is at least visible.")
        return 1

    print(f"OK: no unwaived continue-on-error on any of the {len(effective_required)} "
          f"merge-required contexts "
          f"({len(required)} enumerated + {len(pr_ctxs)} PR-posted under BP [\"*\"]; "
          f"{len(waived)} waived).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
