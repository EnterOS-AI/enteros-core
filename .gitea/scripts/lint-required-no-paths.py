#!/usr/bin/env python3
"""lint-required-no-paths — structural enforcement of
`feedback_path_filtered_workflow_cant_be_required`.

The invariant: A REQUIRED CHECK MUST NOT BE ABLE TO GO GREEN WITHOUT
RUNNING. This lint enforces it in TWO arms, because the repo expresses
the same defect in two syntaxes:

  ARM A (declarative, BLOCKING) — `on: paths:` / `on: paths-ignore:`
    on a required workflow. The original arm; see below.

  ARM B (imperative, REPORT-ONLY today) — the SAME filter hand-rolled in
    shell inside a `detect-changes` job, whose boolean output then gates
    every substantive step of the required-context job. When the
    predicate misses, every real step skips and the job posts SUCCESS
    for a merge-required context WITHOUT RUNNING. e2e-staging-canvas.yml
    literally described its detect-changes job as an "Inline replacement
    for dorny/paths-filter". Arm A caught the declarative form of the
    mistake and was blind to every REAL instance of it in this repo.

  Both arms guard ONE property: `required context posted success` must
  IMPLY `the check actually executed`.

Enumeration (the bug that made this lint a no-op) — READ THIS
--------------------------------------------------------------
This script used to enumerate the required set SOLELY from
`branch_protections/<branch>.status_check_contexts`. On molecule-core
that field is **`["*"]`** — the all-green WILDCARD meta-gate (every
posted status must be success). `"*"` is not an enumerable context: it
does not parse as `<workflow> / <job> (<event>)`, so the script warned
"could not parse context '*'", resolved ZERO workflows, printed
"OK — all 0 resolvable required workflow(s) clean" and exited 0.

    ::notice::Linting 1 required context(s) ...
      - *
    ::warning::could not parse context '*' ...; skipping
    ::notice::OK — all 0 resolvable required workflow(s) clean.
    EXIT=0

That green is exactly what an ABSENT input produces — the lint was
itself the vacuous gate it exists to prevent, and had been since the
BP flipped to `["*"]`. Fixed here: the enforced set is now read from
the checked-in SSOT `.gitea/required-contexts.txt` (entries ABOVE the
first `# pending-#NNNN` marker), which is what the merge queue actually
enforces; `"*"` in live BP is recognized as the sanctioned wildcard
meta-gate rather than an unparseable context. An EMPTY enforced set is
now a HARD FAILURE (exit 4), not a green.

A required-check workflow with a paths filter silently degrades the
merge gate:

  - If the PR's diff doesn't match the `paths:` glob, the workflow
    never fires.
  - This repository's protection gate keeps the missing required context
    `pending` (never `skipped == success`), so the PR cannot merge.
  - For a docs-only PR against `paths: ['**.go']`, the PR is
    blocked forever — no human action can produce a green.

The class was previously prevented only by reviewer vigilance + the
saved memory `feedback_path_filtered_workflow_cant_be_required`. This
script makes it a hard CI gate so a future PR adding `paths:` to a
required workflow fails fast at PR time, not after merge when the next
docs PR wedges main.

The lint runs as `.gitea/workflows/lint-required-no-paths.yml` on every
PR. The lint workflow ITSELF must not have a paths-filter (otherwise it
could be circumvented by a paths-non-matching PR) — that's enforced by
self-reference and by the workflow's own `on:` block deliberately
omitting filters.

Sources of truth:
  - `branch_protections/<branch>` `status_check_contexts` (the merge gate)
  - `.gitea/workflows/*.yml` `name:` + `on:` (the workflow set)

Repository context-format note:
  Status-check contexts are formatted `{workflow_name} / {job_name_or_key} ({event})`.
  We parse the workflow_name prefix and walk `.gitea/workflows/*.yml` for
  a file whose `name:` attr matches. (The filename is NOT the source of
  truth; `name:` is, because Gitea formats the context from `name:`.)

Arm B enforcement mode (`NOOP_GATE_ENFORCE`)
--------------------------------------------
Four ENFORCED lanes carry the Arm-B shape TODAY (e2e-api.yml,
e2e-peer-visibility.yml, handlers-postgres-integration.yml,
template-delivery-e2e.yml). Because this lint's own job posts a commit
status and BP is `["*"]`, making Arm B blocking on day one would RED
every PR in the repo. So Arm B ships REPORT-ONLY (`::warning::`,
exit code unaffected) and is promoted to blocking by setting
`NOOP_GATE_ENFORCE=1` once those four lanes are converted to
always-run. Arm A is and stays BLOCKING. Tracking: task #105.

Exit codes:
  0 — no required workflow has a paths/paths-ignore filter (clean).
      Arm-B findings may be present as ::warning:: (report-only mode).
  1 — at least one required workflow has a paths/paths-ignore filter
      (Arm A, the gate-degrading defect class), or — when
      NOOP_GATE_ENFORCE=1 — at least one required-context job can post
      success without running (Arm B).
  2 — env contract violation (missing GITEA_TOKEN/HOST/REPO/BRANCH).
  3 — workflows directory missing, workflow YAML unparseable, or the
      required-contexts SSOT file is missing.
  4 — FAIL-CLOSED verification failure: branch_protections 401/403
      auth failure (token can't read BP), 5xx transient (propagated
      ApiError), unexpected response shape, or an EMPTY enforced-context
      set (nothing to lint == the absent-input green; see above). This
      is a HARD gate on a protected context — it MUST NOT green when it
      cannot verify.

Auth note: the token used for `GET /repos/.../branch_protections/{branch}`
needs repo-admin access. The workflow-default `GITHUB_TOKEN` is non-admin;
we re-use `DRIFT_BOT_TOKEN` (same persona that powers
ci-required-drift.yml). A 401/403 from a missing-scope token is an
AUTH FAILURE that FAILS CLOSED (exit 4) — fix the token, not the
lint. Only an authenticated 404 (genuinely-absent protection) is a
tolerated graceful skip.
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

import yaml  # PyYAML 6.0.2 — installed by the workflow before this runs.


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------
def _env(key: str, *, required: bool = True, default: str | None = None) -> str:
    val = os.environ.get(key, default)
    if required and not val:
        sys.stderr.write(f"::error::missing required env var: {key}\n")
        sys.exit(2)
    return val or ""


GITEA_TOKEN = _env("GITEA_TOKEN", required=False)
GITEA_HOST = _env("GITEA_HOST", required=False)
REPO = _env("REPO", required=False)
BRANCH = _env("BRANCH", required=False, default="main")
WORKFLOWS_DIR = _env(
    "WORKFLOWS_DIR", required=False, default=".gitea/workflows"
)
# The ENFORCED required-context SSOT. Entries ABOVE the first
# `# pending-#NNNN` marker are what the merge queue actually blocks on
# (gitea-merge-queue.py); entries below are documented-but-parked. Live
# BP cannot supply this list because it holds the `["*"]` wildcard.
REQUIRED_CONTEXTS_FILE = _env(
    "REQUIRED_CONTEXTS_FILE",
    required=False,
    default=".gitea/required-contexts.txt",
)
# Arm B (imperative green-by-no-op) blocking switch. Default OFF —
# report-only — because four ENFORCED lanes still carry the shape and
# this lint's own context is merge-required under BP ["*"]. See the
# module docstring.
NOOP_GATE_ENFORCE = os.environ.get("NOOP_GATE_ENFORCE", "0").strip().lower() in (
    "1",
    "true",
    "yes",
)

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# Cloudflare's WAF in front of git.moleculesai.app returns HTTP 403 /
# "error code: 1010" (banned-browser-signature) to the default
# `Python-urllib/<ver>` User-Agent, so this BP reader was failing closed
# at the edge (CF-1010) before ever reaching Gitea. The ban is a
# UA-denylist (any non-urllib UA passes — empirically verified); send an
# explicit non-urllib UA. Transport-only change; auth/method/semantics
# unchanged. (curl-based gates like review-check.sh are unaffected for
# the same reason.)
_GITEA_UA = "molecule-ci-gate/1.0 (+gitea-api)"


def _require_runtime_env() -> None:
    """Enforce env contract — called from `run()` only. Tests import
    individual functions without setting the full env contract."""
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "BRANCH"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper (mirrors ci-required-drift.py contract:
# raise on non-2xx and on JSON-decode-fail when JSON expected, per
# `feedback_api_helper_must_raise_not_return_dict`).
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API call cannot be trusted to have succeeded."""


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    url = f"{API}{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    data = None
    headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Accept": "application/json",
        "User-Agent": _GITEA_UA,
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        status = e.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {path} → HTTP {status}: {snippet}")

    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as e:
        if expect_json:
            raise ApiError(
                f"{method} {path} → HTTP {status} but body is not JSON: {e}"
            ) from e
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


# --------------------------------------------------------------------------
# Status-check context parser
# --------------------------------------------------------------------------
# Format: "<workflow_name> / <job_name_or_key> (<event>)"
# Examples observed on molecule-core/main:
#   "Secret scan / Scan diff for credential-shaped strings (pull_request)"
#   "sop-checklist / all-items-acked (pull_request)"
#
# Split strategy: peel off the trailing ` (<event>)` first, then split
# the leading `<workflow> / <rest>` on the FIRST ` / ` (workflow names
# come from `name:` attrs which conventionally don't embed ' / '; job
# names CAN, so we keep the rest of the slash-divided text as the job
# name). This matches Gitea's `name: ` semantics.
_CONTEXT_RE = re.compile(r"^(?P<workflow>.+?) / (?P<job>.+) \((?P<event>[^)]+)\)$")


def parse_context(ctx: str) -> tuple[str, str, str] | None:
    """Parse `<workflow> / <job> (<event>)` → (workflow, job, event) or None."""
    if not ctx:
        return None
    m = _CONTEXT_RE.match(ctx)
    if not m:
        return None
    return m.group("workflow"), m.group("job"), m.group("event")


# --------------------------------------------------------------------------
# workflow file enumeration
# --------------------------------------------------------------------------
# (The old `resolve_workflow_file(workflow_name)` helper is gone: it resolved a
# context only as far as its WORKFLOW, which was enough for Arm A but not for
# Arm B — green-by-no-op is a per-JOB property. `build_job_index()` below
# resolves a context all the way to the job that emits it and supersedes it.)
def _iter_workflow_files() -> list[Path]:
    d = Path(WORKFLOWS_DIR)
    if not d.is_dir():
        sys.stderr.write(f"::error::workflows directory not found: {d}\n")
        sys.exit(3)
    # `.yml` and `.yaml` — Gitea accepts both (rarely used `.yaml`, but
    # don't silently miss it if a future port uses it).
    return sorted(list(d.glob("*.yml")) + list(d.glob("*.yaml")))


# --------------------------------------------------------------------------
# paths-filter detection
# --------------------------------------------------------------------------
# Triggers that accept `paths:` / `paths-ignore:` (per GitHub Actions /
# Gitea Actions docs): pull_request, pull_request_target, push.
# We don't enumerate — any sub-key named `paths` or `paths-ignore`
# inside an event mapping is flagged.
_PATHS_KEYS = ("paths", "paths-ignore")


def detect_paths_filters(workflow_path: Path) -> list[str]:
    """Walk the workflow's `on:` block and return a list of findings, one
    per offending `paths`/`paths-ignore` key.

    Returns:
        Empty list if the workflow has no paths/paths-ignore filter
        anywhere in its `on:` block. Otherwise, a list of human-readable
        strings naming the event and filter key + the filter contents.
    """
    try:
        doc = yaml.safe_load(workflow_path.read_text(encoding="utf-8"))
    except yaml.YAMLError as e:
        sys.stderr.write(f"::error::YAML parse error in {workflow_path}: {e}\n")
        sys.exit(3)
    if not isinstance(doc, dict):
        return []

    on_block = doc.get("on") or doc.get(True)  # PyYAML 6 quirk: `on:`
    # under default constructor sometimes becomes the bool key `True`
    # because YAML 1.1 treats `on` as a boolean. Tolerate both.
    if on_block is None:
        return []

    findings: list[str] = []

    # Shape A: `on: pull_request` (string shorthand) — cannot carry filters.
    if isinstance(on_block, str):
        return []
    # Shape B: `on: [pull_request, push]` (list shorthand) — cannot carry filters.
    if isinstance(on_block, list):
        return []
    # Shape C: `on: { event: { ... } }` — the standard mapping case.
    if isinstance(on_block, dict):
        # Defensive: top-level malformed `on.paths` (someone wrote
        # `on: { paths: ['x'] }` thinking it's a workflow-level filter).
        # This is invalid syntax, but if present, flag it — it might
        # not block the workflow from registering (Gitea may ignore the
        # unknown key) and would create a false sense of "filter exists"
        # the lint should still surface.
        for k in _PATHS_KEYS:
            if k in on_block:
                v = on_block[k]
                findings.append(
                    f"top-level `on.{k}` filter (malformed but present): {v!r}"
                )
        for event, event_body in on_block.items():
            if event in _PATHS_KEYS:
                continue  # already handled above
            if not isinstance(event_body, dict):
                # `pull_request: null` / `pull_request: [opened]` shapes —
                # no place for a paths filter to live; skip.
                continue
            for k in _PATHS_KEYS:
                if k in event_body:
                    v = event_body[k]
                    findings.append(
                        f"`on.{event}.{k}` filter present: {v!r}"
                    )
    return findings


# --------------------------------------------------------------------------
# ENFORCED-context SSOT  (.gitea/required-contexts.txt)
# --------------------------------------------------------------------------
# Live BP holds `["*"]` (the all-green wildcard meta-gate), which is NOT an
# enumerable context — see the module docstring. The enumerable enforced set
# lives in the checked-in SSOT, and is what gitea-merge-queue.py blocks on.
WILDCARD_CONTEXTS = {"*"}

_EVENT_SUFFIX_RE = re.compile(
    r"\s*\((?:pull_request|push|pull_request_target)\)\s*$"
)
# The sequencing marker: entries AT OR BELOW the FIRST one are
# documented-required but NOT merge-enforced.
_PENDING_MARKER_RE = re.compile(r"^#\s*pending-#\d+")


def strip_event(ctx: str) -> str:
    """`CI / all-required (pull_request)` → `CI / all-required`."""
    return _EVENT_SUFFIX_RE.sub("", ctx).strip()


def load_enforced_contexts(path: str) -> list[str]:
    """Read the ENFORCED (merge-blocking) contexts from the SSOT file.

    Only lines ABOVE the first `# pending-#NNNN` marker count — that is
    the file's own documented contract ("Entries AT OR BELOW the first
    `# pending-#NNNN (not yet enforced)` marker line are
    DOCUMENTED-required but NOT enforced by the merge queue").

    Exits 3 if the file is absent: the SSOT is checked in, so its
    absence means we cannot enumerate the enforced set, and a lint that
    cannot enumerate must not green.
    """
    p = Path(path)
    if not p.is_file():
        sys.stderr.write(
            f"::error::required-contexts SSOT not found: {path} — cannot "
            f"enumerate the enforced set, so this lint FAILS CLOSED rather "
            f"than greening a gate it could not verify.\n"
        )
        sys.exit(3)
    out: list[str] = []
    for raw in p.read_text(encoding="utf-8").splitlines():
        if _PENDING_MARKER_RE.match(raw.strip()):
            break  # everything from here down is parked, not enforced
        line = raw.split("#", 1)[0].strip()
        if line:
            out.append(strip_event(line))
    return out


# --------------------------------------------------------------------------
# context → (workflow file, job key) index
# --------------------------------------------------------------------------
# Gitea formats the per-job commit status as
# `{workflow name} / {job `name:` or job key}{ (event)}`. Index every job in
# every workflow under that key so an enforced context resolves to the exact
# JOB (not just the workflow) — Arm B is a per-job property.
def build_job_index() -> dict[str, tuple[Path, str, dict]]:
    index: dict[str, tuple[Path, str, dict]] = {}
    for f in _iter_workflow_files():
        try:
            doc = yaml.safe_load(f.read_text(encoding="utf-8"))
        except yaml.YAMLError as e:
            sys.stderr.write(f"::error::YAML parse error in {f}: {e}\n")
            sys.exit(3)
        if not isinstance(doc, dict):
            continue
        wf_name = doc.get("name") or f.stem
        jobs = doc.get("jobs")
        if not isinstance(jobs, dict):
            continue
        for job_key, job in jobs.items():
            if not isinstance(job, dict):
                continue
            job_name = job.get("name") or job_key
            index[strip_event(f"{wf_name} / {job_name}")] = (f, job_key, doc)
    return index


# --------------------------------------------------------------------------
# ARM B — imperative green-by-no-op detection
# --------------------------------------------------------------------------
# The property being guarded (NOT a syntax shape):
#
#   A required-context job J must have NO reachable configuration in which
#   it posts SUCCESS without executing its substantive work.
#
# The repo's violating construction:
# Precondition for both signatures: J `needs:` a job D that computes a boolean
# output from a repo DIFF (detect-changes.py / `git diff` / paths-filter).
#
# Then EITHER signature is INDEPENDENTLY SUFFICIENT — we do not require both,
# and neither is necessary for the other:
#
#   B1 — EXPLICIT NO-OP ARM. J has an INERT step (body only echoes / exits 0)
#        conditioned on the NEGATIVE of D's predicate (`!= 'true'`). Such a
#        step has exactly one purpose: produce a green when the diff predicate
#        MISSED. On a job that emits a merge-required context this is decisive
#        — if that step is the one that runs, a required check went green
#        having done nothing. The workflows say so out loud: "gate satisfied
#        without running the E2E."
#
#   B2 — ALL SUBSTANTIVE STEPS GATED. Every substantive (non-inert, non-setup)
#        step of J is conditioned on D's predicate being 'true'. When it is
#        false, all of them skip and J — having no failing step — rolls up to
#        SUCCESS. This catches the shape even if the B1 echo step is deleted,
#        so deleting the echo step does NOT satisfy this lint.
#
# Why BOTH are needed (learned the hard way — this detector's own first draft
# used B2 alone and MISSED e2e-peer-visibility.yml): that lane carries one
# UNGATED substantive step, a `bash -n` script-syntax preflight. So B2 alone
# says "innocent" — yet the lane still posts SUCCESS for the required context
# `E2E Peer Visibility` without ever running the peer-visibility E2E. A rule
# that asks "does the job run ANY step?" is the wrong question; the right one
# is "can the diff predicate make it green WITHOUT running the check it is
# required for?". B1 answers that even when a trivial preflight step runs.

# `needs.<job>.outputs.<name>` referenced in an `if:` expression.
_NEEDS_OUTPUT_RE = re.compile(r"needs\.([A-Za-z0-9_\-]+)\.outputs\.([A-Za-z0-9_\-]+)")
# The POSITIVE arm: step runs only when the diff predicate HIT.
_POSITIVE_GATE_RE = re.compile(
    r"needs\.(?P<job>[A-Za-z0-9_\-]+)\.outputs\.(?P<out>[A-Za-z0-9_\-]+)\s*=="
    r"\s*(?:'true'|\"true\"|true)"
)
# The NEGATIVE arm: step runs only when the diff predicate MISSED — this is
# where the "No-op pass" lives.
_NEGATIVE_GATE_RE = re.compile(
    r"needs\.(?P<job>[A-Za-z0-9_\-]+)\.outputs\.(?P<out>[A-Za-z0-9_\-]+)\s*"
    r"(?:!=\s*(?:'true'|\"true\"|true)|==\s*(?:'false'|\"false\"|false))"
)

# A `run:` body is INERT if every effective line is one of these — i.e. it
# cannot perform the check it is standing in for. `echo`/`printf` produce
# log output; `:`/`true`/`exit 0` produce a zero exit; `set -e` etc. are
# shell hygiene. Anything else (a test binary, curl, docker, a script) is
# substantive.
_INERT_LINE_RE = re.compile(
    r"^\s*(?:#.*|echo(?:\s.*)?|printf(?:\s.*)?|:|true|exit\s+0|set\s+[-+][A-Za-z]+.*)?\s*$"
)
# Steps that only PREPARE the runner. They are not the substantive work of a
# required check, so a job whose only ungated steps are these is still
# green-by-no-op.
_SETUP_ACTION_PREFIXES = (
    "actions/checkout",
    "actions/setup-",
    "actions/cache",
    "docker/setup-",
    "docker/login-action",
)
# Markers that a job's boolean output is derived from a REPO DIFF.
_DIFF_PREDICATE_MARKERS = (
    "detect-changes.py",
    "git diff",
    "paths-filter",
    "changed-files",
)


def _step_if(step: dict) -> str:
    cond = step.get("if")
    return "" if cond is None else str(cond)


def _is_inert_step(step: dict) -> bool:
    """True if the step cannot perform substantive work."""
    if step.get("uses"):
        return False  # an action is substantive unless it's pure setup
    body = step.get("run")
    if body is None:
        return True  # no `uses:` and no `run:` — nothing happens
    return all(_INERT_LINE_RE.match(line) for line in str(body).splitlines())


def _is_setup_step(step: dict) -> bool:
    uses = str(step.get("uses") or "")
    return any(uses.startswith(p) for p in _SETUP_ACTION_PREFIXES)


def _diff_predicate_reason(job: Any) -> str | None:
    """If `job` computes a boolean output from a repo diff, say how.

    Requires BOTH: the job exports `outputs:` wired to a step output, AND
    one of its steps derives that from a diff. Either alone is innocent.
    """
    if not isinstance(job, dict):
        return None
    outputs = job.get("outputs")
    if not isinstance(outputs, dict) or not outputs:
        return None
    if not any("steps." in str(v) for v in outputs.values()):
        return None
    steps = job.get("steps")
    if not isinstance(steps, list):
        return None
    for step in steps:
        if not isinstance(step, dict):
            continue
        blob = f"{step.get('run') or ''}\n{step.get('uses') or ''}"
        for marker in _DIFF_PREDICATE_MARKERS:
            if marker in blob:
                return marker
    return None


def detect_noop_gate(doc: dict, job_key: str) -> list[str]:
    """Arm B. Return findings if required-context job `job_key` can post
    SUCCESS without running its substantive work.

    Returns [] when the job always executes its work (or when the job is
    gated on something that is not a diff-derived predicate — an
    `if: github.event_name == 'push'` lane is a DIFFERENT question and is
    deliberately out of scope here).
    """
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return []
    job = jobs.get(job_key)
    if not isinstance(job, dict):
        return []

    needs = job.get("needs") or []
    if isinstance(needs, str):
        needs = [needs]
    if not isinstance(needs, list):
        return []

    # 1. Which of this job's dependencies are diff-predicate producers?
    diff_jobs: dict[str, str] = {}
    for dep in needs:
        reason = _diff_predicate_reason(jobs.get(str(dep)))
        if reason:
            diff_jobs[str(dep)] = reason
    if not diff_jobs:
        return []

    def _gated_on_diff(cond: str, regex: re.Pattern) -> str | None:
        for m in regex.finditer(cond):
            if m.group("job") in diff_jobs:
                return f"needs.{m.group('job')}.outputs.{m.group('out')}"
        return None

    steps = job.get("steps")
    if not isinstance(steps, list):
        return []

    dep_desc = ", ".join(
        f"`{d}` (diff predicate via `{r}`)" for d, r in sorted(diff_jobs.items())
    )
    findings: list[str] = []

    # ---- B1: the explicit no-op arm (sufficient on its own) --------------
    for step in steps:
        if not isinstance(step, dict) or not _is_inert_step(step):
            continue
        out = _gated_on_diff(_step_if(step), _NEGATIVE_GATE_RE)
        if out:
            name = step.get("name") or "<unnamed>"
            findings.append(
                f"[B1 no-op arm] job `{job_key}` emits a REQUIRED context and "
                f"has step '{name}' gated `if: {_step_if(step).strip()}` whose "
                f"body only echoes and exits 0. When {out} is false THIS is the "
                f"step that runs, and the required context posts SUCCESS having "
                f"run nothing. It is a hand-rolled `on: paths:` filter — exactly "
                f"what Arm A forbids declaratively."
            )

    # ---- B2: every substantive step is diff-gated (sufficient on its own) -
    substantive: list[dict] = [
        s
        for s in steps
        if isinstance(s, dict) and not _is_inert_step(s) and not _is_setup_step(s)
    ]
    if substantive and all(
        _gated_on_diff(_step_if(s), _POSITIVE_GATE_RE) for s in substantive
    ):
        findings.append(
            f"[B2 all-steps-gated] job `{job_key}` emits a REQUIRED context, but "
            f"ALL {len(substantive)} of its substantive step(s) are gated on "
            f"{dep_desc}. When that predicate is false every step SKIPS and the "
            f"job — having no failing step — rolls up to SUCCESS without running."
        )

    return findings


# --------------------------------------------------------------------------
# Driver
# --------------------------------------------------------------------------
def run() -> int:
    """Main lint entrypoint. Returns the process exit code.

    Exit semantics (see module docstring for full table):
      0 — clean (no offending paths-filter on any required workflow),
          OR protection unreadable (403/404) — surfaced as ::error::
          but treated as non-fatal so token-scope issues don't red-X
          every PR.
      1 — at least one required workflow carries a paths/paths-ignore
          filter — the regression class this lint exists to prevent.
    """
    _require_runtime_env()

    protection_path = f"/repos/{OWNER}/{NAME}/branch_protections/{BRANCH}"
    protection: Any = {}
    try:
        _, protection = api("GET", protection_path)
    except ApiError as e:
        msg = str(e)
        m = re.search(r"HTTP (\d{3})", msg)
        http_status = int(m.group(1)) if m else None
        # FAIL-CLOSED contract (was fail-open: 403 AND 404 both exit 0 —
        # fixed). This is a HARD gate (no continue-on-error → false) on a
        # PROTECTED context: pull_request (same-repo; fork PRs can't carry
        # DRIFT_BOT_TOKEN) + workflow_dispatch. We split auth-failure from
        # genuinely-absent:
        #   401/403 → AUTH FAILURE: the token cannot read branch
        #     protections, so we CANNOT enumerate the required-check set
        #     and CANNOT verify the no-paths-filter invariant. Fail loud /
        #     fail closed (exit 4) — do NOT green an unverifiable gate.
        #   404 → authenticated absent resource: branch genuinely has no
        #     protection. Nothing to enumerate; tolerated degradation,
        #     surfaced loudly (exit 0 with ::warning::).
        if http_status in (401, 403):
            sys.stderr.write(
                f"::error::GET {protection_path} returned HTTP "
                f"{http_status} — DRIFT_BOT_TOKEN cannot read branch "
                f"protections (needs repo-admin scope). AUTH FAILURE: "
                f"cannot enumerate required checks, so this lint FAILS "
                f"CLOSED rather than greening a gate it could not verify. "
                f"Fix: grant repo-admin to mc-drift-bot (org team "
                f"`drift-bot`, perm=admin) — fix the token, not the lint.\n"
            )
            return 4
        if http_status == 404:
            # Authenticated absent resource: branch genuinely has no
            # protection. This used to `return 0` — another absent-input
            # green. We now CONTINUE and lint the checked-in SSOT anyway:
            # the enforced set does not depend on BP being readable (BP only
            # holds the `["*"]` wildcard on this repo), so an unprotected
            # branch is no reason to stop verifying the invariant.
            sys.stderr.write(
                f"::warning::GET {protection_path} returned HTTP 404 — "
                f"branch '{BRANCH}' has no protection configured "
                f"(authenticated absent resource). Falling back to the "
                f"checked-in enforced set ({REQUIRED_CONTEXTS_FILE}). If "
                f"'{BRANCH}' SHOULD be protected, this is a real finding.\n"
            )
            protection = {}
        else:
            raise

    if not isinstance(protection, dict):
        sys.stderr.write(
            f"::error::protection response for {BRANCH} not a JSON object\n"
        )
        return 4

    # ---- Enumerate the ENFORCED set --------------------------------------
    # Live BP holds the `["*"]` wildcard meta-gate, which is NOT enumerable.
    # The enumerable enforced set is the checked-in SSOT (what the merge
    # queue actually blocks on). Any NON-wildcard context live BP names is
    # unioned in, so a context added to BP but not to the SSOT is still
    # linted rather than silently skipped.
    bp_contexts = list(protection.get("status_check_contexts") or [])
    wildcard = [c for c in bp_contexts if c in WILDCARD_CONTEXTS]
    bp_named = {strip_event(c) for c in bp_contexts if c not in WILDCARD_CONTEXTS}
    if wildcard:
        print(
            f"::notice::branch_protections/{BRANCH} carries the all-green "
            f"WILDCARD gate {wildcard} — every posted status must be success. "
            f"It is a meta-gate, not an enumerable context, so the enforced "
            f"set is read from {REQUIRED_CONTEXTS_FILE}."
        )

    ssot_contexts = load_enforced_contexts(REQUIRED_CONTEXTS_FILE)
    contexts = sorted(set(ssot_contexts) | bp_named)

    if not contexts:
        # FAIL-CLOSED. "0 contexts to lint" is precisely the green an ABSENT
        # input produces — the exact vacuity this lint exists to prevent. It
        # must never be the pass arm.
        sys.stderr.write(
            f"::error::enforced-context set is EMPTY (BP {BRANCH} named "
            f"{sorted(bp_named)}, SSOT {REQUIRED_CONTEXTS_FILE} named none "
            f"above the first `# pending-#NNNN` marker). A lint with nothing "
            f"to lint CANNOT verify its invariant, so it FAILS CLOSED rather "
            f"than greening.\n"
        )
        return 4

    print(f"::notice::Linting {len(contexts)} ENFORCED context(s):")
    for c in contexts:
        print(f"  - {c}")

    job_index = build_job_index()

    offenders: list[tuple[str, Path, list[str]]] = []   # Arm A — blocking
    noop_offenders: list[tuple[str, Path, list[str]]] = []  # Arm B
    unresolved: list[str] = []

    for ctx in contexts:
        entry = job_index.get(ctx)
        if entry is None:
            print(
                f"::warning::no job in {WORKFLOWS_DIR} emits the context "
                f"'{ctx}'; skipping. "
                f"(orphaned-context detection is ci-required-drift's job.)"
            )
            unresolved.append(ctx)
            continue
        wf_path, job_key, doc = entry
        workflow_name = str(doc.get("name") or wf_path.stem)

        # ---- ARM A: declarative `on: paths:` (BLOCKING) -------------------
        a_findings = detect_paths_filters(wf_path)
        if a_findings:
            offenders.append((ctx, wf_path, a_findings))

        # ---- ARM B: imperative green-by-no-op (report-only by default) ----
        b_findings = detect_noop_gate(doc, job_key)
        if b_findings:
            noop_offenders.append((ctx, wf_path, b_findings))

        if not a_findings and not b_findings:
            print(
                f"::notice::OK {wf_path.name} :: job `{job_key}` "
                f"({workflow_name}) — no declarative paths filter, no "
                f"green-by-no-op arm"
            )

    # ---- ARM A report (BLOCKING) -----------------------------------------
    if offenders:
        print("")
        print(
            f"::error::ARM A — {len(offenders)} required workflow(s) with "
            f"paths/paths-ignore filters:"
        )
        for ctx, wf_path, findings in offenders:
            for finding in findings:
                # ::error file=... lets Gitea Actions surface a per-file
                # annotation in the PR UI (when annotations are wired).
                print(
                    f"::error file={wf_path}::Required context '{ctx}' "
                    f"({wf_path.name}) has a paths filter that would degrade "
                    f"the merge gate to a silent indefinite pending: {finding}. "
                    f"See feedback_path_filtered_workflow_cant_be_required. "
                    f"Fix: remove the filter and make the job always run."
                )

    # ---- ARM B report ----------------------------------------------------
    if noop_offenders:
        sev = "error" if NOOP_GATE_ENFORCE else "warning"
        print("")
        print(
            f"::{sev}::ARM B — {len(noop_offenders)} required-context job(s) "
            f"can post SUCCESS WITHOUT RUNNING (hand-rolled paths filter in a "
            f"detect-changes job):"
        )
        for ctx, wf_path, findings in noop_offenders:
            for finding in findings:
                print(
                    f"::{sev} file={wf_path}::Required context '{ctx}' "
                    f"({wf_path.name}): {finding} "
                    f"Fix: make the job ALWAYS run its check (delete the "
                    f"detect-changes gate), or stop treating the context as "
                    f"merge-required. A required check that greens without "
                    f"running is a vacuous gate."
                )
        if not NOOP_GATE_ENFORCE:
            print(
                "::warning::ARM B is REPORT-ONLY (NOOP_GATE_ENFORCE unset). "
                "The lanes above are pre-existing and would RED-BLOCK every "
                "PR if this arm were blocking today. Convert them to "
                "always-run, then set NOOP_GATE_ENFORCE=1 in "
                ".gitea/workflows/lint-required-no-paths.yml to make this "
                "blocking. Tracking: task #105."
            )
        elif not offenders:
            return 1

    if offenders:
        return 1

    print("")
    print(
        f"::notice::OK — all {len(contexts) - len(unresolved)} resolvable "
        f"enforced context(s) clean (Arm A: no paths/paths-ignore filters"
        + ("; Arm B: no green-by-no-op arms)." if not noop_offenders
           else f"; Arm B: {len(noop_offenders)} REPORTED, see warnings).")
    )
    if unresolved:
        print(
            f"::notice::{len(unresolved)} enforced context(s) were not "
            f"resolved to a job (warn-not-fail); see warnings above."
        )
    return 0


if __name__ == "__main__":
    sys.exit(run())
