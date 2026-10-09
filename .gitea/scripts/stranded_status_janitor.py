#!/usr/bin/env python3
"""stranded-status-janitor — find commit-status contexts whose LAST row is a
merge-blocking artifact rather than a verdict about the code, and heal them by
RE-RUNNING the run (never by fabricating a green).

Two artifact classes, one healing mechanism:

  * PENDING that will never resolve — the underlying Actions run has already
    finished (the original defect, #4979).
  * FAILURE that reports nothing — the Actions engine's own
    `cancelled -> failure` stamp on a run that was cancelled, often before it
    executed a single step (core#5156). See CANCELLED_DESCRIPTION.

WHY THIS EXISTS
---------------
Under branch protection `status_check_contexts=["*"]` every posted context is
merge-blocking, so a single context whose LAST commit-status row is `pending`
blocks a fully-green, fully-approved PR forever. It looks like a slow job, not
a fault, so it costs a human hours before anyone notices.

The cancellation class is worse, because it looks like a REAL red. A cancelled
run's status is written by Gitea itself with `state=failure`, so a PR that is
otherwise green and `mergeable=true` reads as broken. Whether it clears on its
own depends entirely on the workflow's concurrency-group key:

  * group keyed on the head SHA / PR number -> the superseding run posts a new
    row on the SAME SHA and the red is overwritten. Self-healing.
  * group NOT keyed on the head SHA (e.g. `gitea-merge-queue`'s repo-global
    `gitea-merge-queue-${{ github.repository }}`) -> the evicting run belongs to
    a DIFFERENT SHA. Nothing ever posts to this SHA's timeline again and the red
    latches PERMANENTLY.

Note also that `cancel-in-progress: false` does not prevent this. It protects
the RUNNING member of a group; at most one run may sit QUEUED per group, so a
newer arrival cancels the older queued run. Measured (core#5156): run 644627
running, 644628 queued at 05:59:31Z, 644629 arrives 05:59:53Z — 644628 is
cancelled at exactly 05:59:53Z with `started_at=1970`, never having run.

The defects below were all measured on Gitea 1.26.4 against
molecule-core + molecule-controlplane (see molecule-core issue #4979 and the
evidence issue that landed this script):

  (A) CROSS-RUN MISATTRIBUTION (upstream Gitea bug, confirmed in source).
      `services/actions/job_emitter.go:checkJobsByRunID` builds one job slice
      out of the triggering run's own jobs PLUS jobs collected by
      `checkRunConcurrency` — which are jobs of a *different* run sharing the
      concurrency group — and then calls:

          CreateCommitStatusForRunJobs(ctx, run, jobs...)

      `CreateCommitStatusForRunJobs` resolves the target commit ONCE, from the
      single `run` argument (`getCommitStatusEventNameAndCommitID(run)`), and
      builds `TargetURL: fmt.Sprintf("%s/jobs/%d", run.Link(), job.ID)`.
      So the *other* run's jobs get their status written onto the TRIGGERING
      run's commit SHA, under the triggering run's URL, carrying the other
      run's still-in-flight `pending` state. Because the triggering run has no
      further job transitions, that pending row is never superseded.
      Observable signature: the pending row's `target_url` names run R but a
      job id that is NOT a member of run R ("phantom job id").

  (B) QUEUED-RUN EVICTION (core#5156). A newer run arriving in a concurrency
      group cancels the run already QUEUED there — `cancel-in-progress: false`
      only protects the RUNNING one. Gitea then writes `failure` /
      "Has been cancelled" for a job with `started_at=1970` and
      `run_attempt=0`, i.e. one that never executed. THIS IS WRITTEN BY THE
      SERVER, not by any workflow step: on the measured SHA, 148 of 149 status
      rows carry `creator: null` (the only row with a creator was a bot POST).
      No `if: always()` guard, no reporting step and no concurrency tweak can
      suppress it — a workflow-side fix for the WRITE does not exist. Only the
      READERS can be taught, and only a re-run can replace the row.
      Observable signature: state `failure`, description exactly
      "Has been cancelled", and the job named by `target_url` reporting
      `conclusion: cancelled`.

Healing is safe and simple: re-run the run. The re-run posts a REAL terminal
status produced by a job that actually reported.

SAFETY INVARIANTS (each has a unit test in
`.gitea/scripts/tests/test_stranded_status_janitor.py`)
-------------------------------------------------------
  1. This script NEVER creates or mutates a commit status. It issues GETs plus
     `POST .../actions/runs/{id}/rerun` and nothing else. A green context must
     always have come from a job that actually reported green.
  2. A run is only re-run when the run itself is `completed` AND every job in
     it is `completed`. A genuinely-running job is left alone.
  3. A pending row must be at least --min-age-minutes old (default 15) before
     it counts as stranded. This bounds churn and keeps normal in-flight CI
     untouched.
  4. Re-run attempts are capped (--max-attempts, default 4, measured against
     the run's highest job `run_attempt`) so a permanently-broken run cannot be
     re-run in a loop forever.
  5. At most one re-run per run per pass, and the pass is idempotent: once a
     context's latest row is terminal it is no longer selected.

READING COMMIT STATUSES CORRECTLY — READ THIS BEFORE "FIXING" ANYTHING
----------------------------------------------------------------------
Do NOT reduce `GET /repos/{o}/{r}/statuses/{sha}` yourself. That list endpoint
is LOSSY. Measured on molecule-core PR #4869 head b5015c2f: it returns 111 rows
containing only 110 distinct ids, with max(id)=111 — one row duplicated and one
DROPPED. The dropped row was id 62, the `success` for
`lint-bp-context-emit-match`, so a hand-rolled reduction reported that context
`pending` when Gitea considered it green. The loss reproduces identically at
page sizes 10, 25 and 50, because the endpoint sorts by timestamp and ties are
broken non-deterministically — classic unstable-sort offset pagination, where a
row can be served twice and its neighbour never served at all.

This is the trap that makes the whole bug class expensive: a lossy read
manufactures phantom "stranded" contexts that look exactly like real ones, so
you chase reruns for commits that were already green.

Use `GET /repos/{o}/{r}/commits/{sha}/status` instead. Gitea computes it
server-side as `max(id) GROUP BY context_hash`, i.e. exactly the latest row per
context, with no client-side reduction and no pagination hazard in the
reduction itself. Verified complete against three commits (55/49/50 contexts,
zero disagreement with a lossless list read).

Two caveats on that endpoint, both measured:
  * Its `total_count` field is UNRELIABLE — observed 5 when 55 contexts were
    returned, and 0 when 50 were. Never use it as a completeness check. Page
    until a short page.
  * It is still paginated at 50 contexts/page, so a commit with more than 50
    contexts needs more than one page. Both repos here exceed 50.
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

# ---------------------------------------------------------------------------
# Pure logic (no IO) — this is what the unit tests drive.
# ---------------------------------------------------------------------------

TARGET_URL_RE = re.compile(r"/actions/runs/(\d+)/jobs/(\d+)")

#: Commit-status states that mean "this context has reached a verdict".
#: `skipped` counts: the wildcard branch-protection gate treats a skipped
#: context as satisfied, so it is terminal for merge purposes.
TERMINAL_STATES = frozenset({"success", "failure", "error", "warning", "skipped"})

#: Red states that MAY be a cancellation artifact rather than a real verdict.
RED_STATES = frozenset({"failure", "error"})

#: Gitea's Actions engine writes this description when it maps a cancelled run
#: to a commit status. Measured on molecule-core PR #5156, head 626d2049da:
#:
#:   id=148  pending   05:59:31Z  "Blocked by required conditions"
#:   id=149  failure   05:59:53Z  "Has been cancelled"
#:   run 644628 / job 949071: conclusion=cancelled
#:                            started_at=1970-01-01T00:00:00Z run_attempt=0
#:
#: `started_at=1970` + `run_attempt=0` prove the job never started, so no step
#: wrote this; and every row on that SHA carries `creator: null` except a single
#: bot POST, the signature of a server-side write. The engine's own stamp cannot
#: be prevented workflow-side — it can only be detected and healed.
#:
#: This string is used ONLY as a cheap pre-filter so ordinary reds cost zero API
#: calls (real failures read "Failing after Ns"). The VERDICT is always the
#: authoritative `conclusion` field on the job named by `target_url`, never the
#: string — so a red that merely quotes the phrase is still treated as real.
CANCELLED_DESCRIPTION = "Has been cancelled"


def parse_run_and_job(target_url):
    """Return (run_id, job_id) parsed out of a status row's target_url.

    Returns (None, None) when the row has no Actions target (e.g. a status a
    workflow POSTed for itself). Such rows are never healed by re-running,
    because there is no run to re-run.
    """
    match = TARGET_URL_RE.search(target_url or "")
    if not match:
        return None, None
    return int(match.group(1)), int(match.group(2))


def latest_status_per_context(rows):
    """Reduce every commit-status row for one SHA to the newest row per context.

    Ordering of the API response is NOT guaranteed, so never take "the first
    row for this context". Reduce by ``max((updated_at, id))``: `updated_at`
    is the primary key so clock order wins, and `id` breaks the frequent ties
    where two rows for one context land inside the same second (Gitea's own
    reducer is `max(id) GROUP BY context_hash`, so the tiebreak agrees with the
    server).
    """
    best = {}
    for row in rows:
        context = row.get("context") or ""
        key = (row.get("updated_at") or "", int(row.get("id") or 0))
        if context not in best or key > best[context][0]:
            best[context] = (key, row)
    return {context: row for context, (_, row) in best.items()}


def _parse_ts(value):
    if not value:
        return None
    try:
        return datetime.datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=datetime.timezone.utc
        )
    except ValueError:
        return None


def looks_like_cancellation_stamp(row):
    """Cheap pre-filter: does this red row have the ENGINE's cancellation shape?

    Exact description match after strip — a substring test would swallow a
    genuine failure whose description quotes the phrase (e.g. a test named
    "Has been cancelled by the user unexpectedly"), the same trap
    `main-red-watchdog._is_cancel_cascade` avoids. Answering True only buys the
    row a run lookup; `classify_context` still refuses to act unless the job's
    own `conclusion` says `cancelled`.
    """
    if (row.get("status") or "").lower() not in RED_STATES:
        return False
    description = row.get("description")
    if not isinstance(description, str):
        return False
    if description.strip() != CANCELLED_DESCRIPTION:
        return False
    run_id, job_id = parse_run_and_job(row.get("target_url"))
    return run_id is not None and job_id is not None


def _classify_cancellation_latch(row, run, jobs, now, min_age_minutes, max_attempts):
    """Sub-classifier for a red row that has the engine's cancellation shape.

    Under `status_check_contexts=["*"]` this red is merge-blocking even though
    the job never reported anything about the code. It does NOT always clear
    itself: when the run's concurrency group is not scoped to the head SHA (e.g.
    `gitea-merge-queue`'s repo-global group), the run that evicted it belongs to
    a DIFFERENT SHA, so nothing ever posts to this SHA's timeline again and the
    red latches permanently.

    Healing is the same as the pending class and just as safe: re-run, so a job
    that actually executes posts the verdict. Nothing here fabricates a status.
    """
    run_id, job_id = parse_run_and_job(row.get("target_url"))

    age_seconds = None
    updated = _parse_ts(row.get("updated_at"))
    if updated is not None and now is not None:
        age_seconds = (now - updated).total_seconds()
    if age_seconds is not None and age_seconds < min_age_minutes * 60:
        # A cancellation whose superseding run is still in flight will be
        # overwritten by that run's own status within seconds. Only an AGED one
        # is latched.
        return "too-fresh", "cancelled %.0fs ago (< %dm grace)" % (
            age_seconds,
            min_age_minutes,
        )

    if run is None:
        return "unknown-run", "run %d could not be read" % run_id
    if (run.get("status") or "").lower() != "completed":
        return "run-active", "run %d is %s" % (run_id, run.get("status"))
    if not jobs:
        return "no-jobs", "run %d reported no jobs" % run_id

    # AUTHORITATIVE check. The description got us here; the job's `conclusion`
    # decides. This is "option A" from mc#1564 — resolve the underlying run
    # status instead of trusting the string — and it is what makes the guard
    # non-vacuous in the other direction: a genuinely FAILED job is never
    # selected, no matter what its description says.
    matched = [j for j in jobs if str(j.get("id")) == str(job_id)]
    if not matched:
        # Gitea's cross-run misattribution (see the module docstring, defect A)
        # can name a job that is not a member of this run. Without the job we
        # cannot prove cancellation, so we refuse to act.
        return "phantom-job", "job %s is not a member of run %d" % (job_id, run_id)
    conclusion = (matched[0].get("conclusion") or "").lower()
    if conclusion != "cancelled":
        return "real-failure", (
            "run %d job %s concluded %r — a genuine red, left alone"
            % (run_id, job_id, conclusion or "unknown")
        )

    unfinished = [j for j in jobs if (j.get("status") or "").lower() != "completed"]
    if unfinished:
        return "jobs-active", "run %d has %d unfinished job(s)" % (
            run_id,
            len(unfinished),
        )

    attempt = max((int(j.get("run_attempt") or 0) for j in jobs), default=0)
    if attempt >= max_attempts:
        return "attempts-exhausted", "run %d already at attempt %d (cap %d)" % (
            run_id,
            attempt,
            max_attempts,
        )

    return "stranded", (
        "run %d job %s was CANCELLED (conclusion=cancelled, started_at=%s); its "
        "`failure` blocks merge under wildcard protection but reports nothing "
        "about the code"
        % (run_id, job_id, matched[0].get("started_at") or "unknown")
    )


def classify_context(row, run, jobs, now, min_age_minutes, max_attempts):
    """Decide what to do about one context's newest commit-status row.

    Returns a (verdict, detail) pair. Only the verdict ``"stranded"`` is ever
    acted on; everything else is a documented reason to leave the row alone.
    """
    state = (row.get("status") or "").lower()
    if looks_like_cancellation_stamp(row):
        return _classify_cancellation_latch(
            row, run, jobs, now, min_age_minutes, max_attempts
        )
    if state in TERMINAL_STATES:
        return "terminal", "context already has a verdict (%s)" % state
    if state != "pending":
        return "skip", "unhandled state %r" % state

    run_id, _job_id = parse_run_and_job(row.get("target_url"))
    if run_id is None:
        # A workflow POSTed this pending itself; there is no run to re-run and
        # inventing a verdict for it is exactly what we refuse to do.
        return "orphan", "pending row has no Actions target_url"

    age_seconds = None
    updated = _parse_ts(row.get("updated_at"))
    if updated is not None and now is not None:
        age_seconds = (now - updated).total_seconds()
    if age_seconds is not None and age_seconds < min_age_minutes * 60:
        return "too-fresh", "pending for %.0fs (< %dm grace)" % (
            age_seconds,
            min_age_minutes,
        )

    if run is None:
        return "unknown-run", "run %d could not be read" % run_id
    if (run.get("status") or "").lower() != "completed":
        return "run-active", "run %d is %s" % (run_id, run.get("status"))

    # Invariant 2: the run AND every job in it must be finished. A run can read
    # `completed` while a re-run of one job is still going; never touch that.
    if not jobs:
        return "no-jobs", "run %d reported no jobs" % run_id
    unfinished = [j for j in jobs if (j.get("status") or "").lower() != "completed"]
    if unfinished:
        return "jobs-active", "run %d has %d unfinished job(s)" % (
            run_id,
            len(unfinished),
        )

    # Invariant 4: never loop forever on a run that re-runs into the same hole.
    attempt = max((int(j.get("run_attempt") or 0) for j in jobs), default=0)
    if attempt >= max_attempts:
        return "attempts-exhausted", "run %d already at attempt %d (cap %d)" % (
            run_id,
            attempt,
            max_attempts,
        )

    return "stranded", "run %d completed (%s) but context is still pending" % (
        run_id,
        run.get("conclusion"),
    )


def plan_reruns(findings):
    """Collapse findings to at most one re-run per run id, preserving order.

    Several contexts of the same run strand together (measured: 5 contexts of
    one run in a single event), and one re-run repairs all of them.
    """
    seen = set()
    plan = []
    for finding in findings:
        run_id = finding["run_id"]
        if run_id in seen:
            continue
        seen.add(run_id)
        plan.append(finding)
    return plan


# ---------------------------------------------------------------------------
# Gitea client
# ---------------------------------------------------------------------------


class ApiError(RuntimeError):
    pass


class Gitea:
    """Minimal Gitea client. Deliberately exposes NO status-creating method."""

    def __init__(self, base_url, token, repo):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.repo = repo

    def _request(self, method, path, params=None, body=None):
        url = "%s/api/v1%s" % (self.base_url, path)
        if params:
            url += "?" + urllib.parse.urlencode(params)
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(url, data=data, method=method)
        request.add_header("Authorization", "token %s" % self.token)
        request.add_header("Accept", "application/json")
        if data is not None:
            request.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                payload = response.read().decode()
                return response.status, (json.loads(payload) if payload else None)
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read().decode()[:400]
        except Exception as exc:  # network/DNS/timeout
            raise ApiError("%s %s: %s" % (method, path, exc)) from exc

    def get(self, path, **params):
        status, body = self._request("GET", path, params=params)
        if status != 200:
            raise ApiError("GET %s -> %s %s" % (path, status, body))
        return body

    def latest_statuses(self, sha, page_limit=50, max_pages=40):
        """The newest commit status per context for one SHA.

        Reads `/commits/{sha}/status`, which Gitea reduces server-side to one
        row per context. We page until a short page — `total_count` is measured
        unreliable and must not be used as a stop condition. See the module
        docstring for why the `/statuses/{sha}` list endpoint is NOT used here.
        """
        rows, page = [], 1
        while page <= max_pages:
            payload = self.get(
                "/repos/%s/commits/%s/status" % (self.repo, sha),
                page=page,
                limit=page_limit,
            )
            batch = (payload or {}).get("statuses") or []
            rows.extend(batch)
            if len(batch) < page_limit:
                break
            page += 1
        # Defensive: the server should already return one row per context, but
        # reduce anyway so a future server change cannot reintroduce ambiguity.
        return list(latest_status_per_context(rows).values())

    def run(self, run_id):
        try:
            return self.get("/repos/%s/actions/runs/%s" % (self.repo, run_id))
        except ApiError:
            return None

    def run_jobs(self, run_id):
        try:
            payload = self.get("/repos/%s/actions/runs/%s/jobs" % (self.repo, run_id))
        except ApiError:
            return []
        return payload.get("workflow_jobs", payload.get("jobs", [])) or []

    def rerun(self, run_id):
        """The ONLY mutating call this tool is permitted to make."""
        status, body = self._request(
            "POST", "/repos/%s/actions/runs/%s/rerun" % (self.repo, run_id)
        )
        return status, body

    def open_pull_heads(self, limit=50):
        """(number, head_sha) for every open PR, newest first."""
        out, page = [], 1
        while True:
            batch = self.get(
                "/repos/%s/pulls" % self.repo, state="open", limit=limit, page=page
            )
            if not batch:
                break
            for pull in batch:
                sha = (pull.get("head") or {}).get("sha")
                if sha:
                    out.append((pull["number"], sha))
            if len(batch) < limit:
                break
            page += 1
        return out

    def branch_head(self, branch):
        payload = self.get("/repos/%s/branches/%s" % (self.repo, branch))
        return ((payload.get("commit") or {}).get("id")) or None


# ---------------------------------------------------------------------------
# Sweep
# ---------------------------------------------------------------------------


def sweep_sha(client, sha, now, args, label=""):
    """Return the stranded findings for one SHA."""
    rows = client.latest_statuses(sha)
    if not rows:
        return []
    findings = []
    for row in sorted(rows, key=lambda r: r.get("context") or ""):
        context = row.get("context") or ""
        # Cheap pre-filter. `classify_context` evaluates everything that does
        # not need the run (terminal / orphan / too-fresh) BEFORE it looks at
        # `run`, so a first pass with run=None tells us whether this context is
        # even a candidate. Only then do we spend two API calls on it — most
        # commits carry 40+ contexts and nearly all are already terminal.
        verdict, detail = classify_context(
            row, None, [], now, args.min_age_minutes, args.max_attempts
        )
        if verdict == "unknown-run":
            run_id, _ = parse_run_and_job(row.get("target_url"))
            run = client.run(run_id)
            jobs = client.run_jobs(run_id) if run is not None else []
            verdict, detail = classify_context(
                row, run, jobs, now, args.min_age_minutes, args.max_attempts
            )
        else:
            run_id = None
        if verdict == "stranded":
            findings.append(
                {
                    "sha": sha,
                    "label": label,
                    "context": context,
                    "run_id": run_id,
                    "row_id": row.get("id"),
                    "updated_at": row.get("updated_at"),
                    "description": row.get("description"),
                    "detail": detail,
                }
            )
        elif args.verbose and verdict not in ("terminal",):
            print("    . %-60s %s: %s" % (context[:60], verdict, detail))
    return findings


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--repo", required=True, help="owner/name")
    parser.add_argument(
        "--base-url", default=os.environ.get("GITEA_HOST_URL", "https://git.moleculesai.app")
    )
    parser.add_argument(
        "--branch", action="append", default=[], help="also sweep this branch head"
    )
    parser.add_argument("--sha", action="append", default=[], help="sweep an explicit SHA")
    parser.add_argument("--skip-open-prs", action="store_true")
    parser.add_argument("--min-age-minutes", type=int, default=15)
    parser.add_argument("--max-attempts", type=int, default=4)
    parser.add_argument("--max-reruns", type=int, default=10)
    parser.add_argument(
        "--heal",
        action="store_true",
        help="POST rerun for stranded runs. Without it the pass only reports.",
    )
    parser.add_argument("--verbose", action="store_true")
    args = parser.parse_args(argv)

    token = os.environ.get("GITEA_TOKEN", "").strip()
    if not token:
        print("::error::GITEA_TOKEN is empty — failing closed")
        return 2

    client = Gitea(args.base_url, token, args.repo)
    now = datetime.datetime.now(datetime.timezone.utc)

    targets = []
    if not args.skip_open_prs:
        for number, sha in client.open_pull_heads():
            targets.append((sha, "PR #%d" % number))
    for branch in args.branch:
        head = client.branch_head(branch)
        if head:
            targets.append((head, "branch %s" % branch))
    for sha in args.sha:
        targets.append((sha, "explicit"))

    # De-duplicate: several PRs can share a head SHA.
    seen, ordered = set(), []
    for sha, label in targets:
        if sha in seen:
            continue
        seen.add(sha)
        ordered.append((sha, label))

    print("stranded-status-janitor: repo=%s targets=%d heal=%s min_age=%dm" % (
        args.repo, len(ordered), args.heal, args.min_age_minutes))

    findings = []
    for sha, label in ordered:
        if args.verbose:
            print("  %s %s" % (sha[:10], label))
        try:
            findings.extend(sweep_sha(client, sha, now, args, label))
        except ApiError as exc:
            # Per-SHA isolation: one unreadable commit must not strand the rest.
            print("::warning::%s (%s): %s" % (sha[:10], label, exc))

    if not findings:
        print(
            "OK no stranded pending / cancellation-latched contexts across %d "
            "commit(s)." % len(ordered)
        )
        return 0

    print("")
    print("Found %d stranded context(s):" % len(findings))
    for finding in findings:
        print(
            "  %s %-10s run=%-8s %r\n      %s (last row #%s, %s, %r)"
            % (
                finding["sha"][:10],
                finding["label"],
                finding["run_id"],
                finding["context"],
                finding["detail"],
                finding["row_id"],
                finding["updated_at"],
                finding["description"],
            )
        )

    plan = plan_reruns(findings)[: args.max_reruns]
    if not args.heal:
        print("")
        print(
            "::warning::%d stranded context(s) across %d run(s). Re-run those runs to "
            "heal them, or run this script with --heal." % (len(findings), len(plan))
        )
        return 1

    print("")
    failed = 0
    for finding in plan:
        status, body = client.rerun(finding["run_id"])
        if status in (200, 201, 202, 204):
            print("  healed: re-ran run %s (%s)" % (finding["run_id"], finding["sha"][:10]))
        else:
            failed += 1
            print(
                "::warning::rerun of run %s failed: %s %s"
                % (finding["run_id"], status, body)
            )
    print("")
    print(
        "re-ran %d/%d run(s); %d stranded context(s) should clear on the next pass."
        % (len(plan) - failed, len(plan), len(findings))
    )
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
