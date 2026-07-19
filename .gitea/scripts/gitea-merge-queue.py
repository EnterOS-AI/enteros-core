#!/usr/bin/env python3
"""gitea-merge-queue — conservative serialized merge bot for Gitea.

Gitea's `pull_auto_merge` does not implement this repository's exact-head,
one-at-a-time validation policy. This script enforces that policy in user space:

1. Scan open same-repo PRs that are NOT opted out (auto-discovery, see below),
   oldest-first, skipping drafts, until an ACTIONABLE one is found. A non-ready
   candidate (REQUEST_CHANGES, mergeable!=True, insufficient genuine approvals,
   or red required CI) is SKIPPED so it cannot head-of-line block newer ready
   PRs; the scan continues to the next candidate.
2. Refuse to act unless main's BP-required contexts are green. This is also
   the serialized backstop for direct-merge (see below): after a direct merge,
   main re-runs push CI and this gate PAUSES the queue if main goes red, so no
   merge piles onto an unverified/red main (issue #2358).
3. Refuse fork PRs; the queue may only mutate same-repo branches.
4. Honor branch protection's `block_on_outdated_branch` field. When enabled,
   a behind-main PR is updated first and must earn fresh CI/review before merge;
   this avoids Gitea's expected HTTP 405 for a stale head. When disabled, the
   queue retains the #2358 throughput path and directly merges a conflict-free
   (`mergeable is True`) behind-main PR after the full merge bar passes. For a
   current-base PR, `mergeable=None/missing` is a fail-closed WAIT and literal
   False is a conflict WAIT. A real /update conflict returns HTTP 409 and the PR
   is best-effort HELD per #2352.
5. Merge ONLY when, on the PR's CURRENT head sha:
     - >= REQUIRED_APPROVALS distinct GENUINE official APPROVED reviews from
       the recognised reviewer set (not stale, not dismissed, commit_id ==
       current head), AND
     - no open official REQUEST_CHANGES on the current head, AND
     - every BP-required status context is green, AND
     - the PR is mergeable (Gitea reports it conflict-free).

Authoritative gates (fail-closed):
  - The REQUIRED status contexts come from BRANCH PROTECTION
    (`status_check_contexts`). If branch protection cannot be enumerated,
    the queue HOLDS (does not merge blindly). (The old uniform SOP gate —
    qa-review/security-review/sop-checklist — was removed 2026-07-14; see
    GOVERNANCE_REQUIRED_CONTEXTS below.)
  - NON-required reds (E2E Chat, Staging SaaS, ci-arm64-advisory, any
    continue-on-error job) MUST NOT block. They are reported, never gating.
  - Every merge uses Gitea's normal protected endpoint with the reviewed head
    SHA pinned. The queue never requests Gitea's administrator force override;
    Gitea re-checks branch protection and approvals at write time.

Auto-discovery (opt-OUT, label-optional):
  The queue is SELF-SUSTAINING — a ready PR does NOT need a human (or an agent)
  to add the `merge-queue` label first. When AUTO_DISCOVER is on (default), the
  queue enumerates ALL open same-repo PRs and considers any that meets the full
  merge bar (genuine approvals on current head + BP-required green + mergeable +
  no open REQUEST_CHANGES). The merge bar above is UNCHANGED; auto-discovery only
  changes WHICH PRs are considered, not whether they are mergeable.

  This deliberately removes the historical dependency on an agent adding the
  `merge-queue` label — agent Gitea tokens lack `write:issue` (labels are
  issue-scoped), so they could never self-label and the queue stalled. The label
  is now OPTIONAL metadata, not a gate.

  SAFETY is preserved as opt-OUT: any PR carrying an opt-out label
  (OPT_OUT_LABELS — `merge-queue-hold`, `do-not-auto-merge`, `wip`, `draft` by
  default) is skipped (never auto-considered, never merged). Draft PRs
  (draft=true STATE) are also skipped; the literal `draft` LABEL is an
  additional explicit opt-out a human can apply without converting to a draft.
  A human who wants to keep a PR out of autonomous merging just adds one of
  those labels. Setting AUTO_DISCOVER=0 restores the legacy opt-IN behaviour
  (only PRs already carrying QUEUE_LABEL are considered).

Head-of-line (HOL) safety has two complementary layers:
  (a) The queue SCANS THROUGH the FIFO candidate list and skips any non-ready
      PR (REQUEST_CHANGES, mergeable!=True, insufficient genuine approvals, or
      red required CI) instead of locking on the oldest and waiting, so a PR
      that can never become ready without human action does not block newer
      ready PRs.
  (b) For the candidate the scan acts on, two permanent failure modes HOLD the
      PR (apply HOLD_LABEL) and let the scan CONTINUE to the next candidate
      rather than re-selecting the same wedged PR every tick:
        - a normal protected-merge refusal (403/404/405), and
        - a persistent branch-update conflict (the /update endpoint returns
          HTTP 409 because the PR branch cannot be merged with main without a
          manual rebase). A conflict will not self-resolve, so retrying it
          every tick would HOL-block every ready PR behind it (issue #2352).
      If the scoped queue token cannot persist HOLD_LABEL, the tick FAILS RED
      instead of claiming success without a durable hold (internal#1082).

Status-fetch is fail-closed: if the combined status for a sha cannot be
fetched, the PR is skipped this tick (never treated as green).

The script is intentionally one-PR-per-run. Workflow/cron concurrency should
serialize invocations so two green PRs cannot merge against the same main.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

# SSOT fail-closed approval predicate (SEV-1 internal#812). This is now the
# sole consumer of the module (the review-check.sh chain was removed with the
# SOP review gate 2026-07-14) — do NOT duplicate the predicate here. See
# _approval_validator.py for the fail-closed contract.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _approval_validator import classify_reviews as _classify_reviews_ssot  # noqa: E402


def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
WATCH_BRANCH = _env("WATCH_BRANCH", default="main")
QUEUE_LABEL = _env("QUEUE_LABEL", default="merge-queue")
HOLD_LABEL = _env("HOLD_LABEL", default="merge-queue-hold")
UPDATE_STYLE = _env("UPDATE_STYLE", default="merge")
# Auto-discovery (opt-OUT). When truthy (default), the queue considers ALL open
# same-repo PRs that meet the merge bar, not only PRs already carrying
# QUEUE_LABEL — so the queue is self-sustaining without any human/agent labeling
# (agent tokens lack write:issue and cannot self-label). Set AUTO_DISCOVER=0 to
# restore the legacy opt-IN behaviour (QUEUE_LABEL required to be considered).
AUTO_DISCOVER = _env("AUTO_DISCOVER", default="1").strip().lower() not in {
    "0",
    "false",
    "no",
    "off",
    "",
}
# Opt-OUT labels. A PR carrying ANY of these is skipped (never auto-considered,
# never merged) — the human escape hatch from autonomous merging. HOLD_LABEL is
# always included so the existing hold semantics keep working. `do-not-auto-merge`
# and `wip` let a human keep a PR out of the auto-merge path without removing it.
# `draft` is included as a literal label too: Gitea draft STATE (draft=true) is
# already skipped via _issue_is_draft, but a "draft" LABEL is an additional,
# explicit opt-out signal a human can apply without converting the PR to a draft.
OPT_OUT_LABELS = {
    name.strip()
    for name in _env(
        "OPT_OUT_LABELS",
        default="do-not-auto-merge,wip,draft",
    ).split(",")
    if name.strip()
} | ({HOLD_LABEL} if HOLD_LABEL else set())
# Governance checks that are ALWAYS required for every PR, regardless of
# branch-protection configuration. These are the uniform-gate checks that
# must pass before any PR can merge (SOP tier removal makes them mandatory
# for all PRs, not just tier:medium/tier:high).
#
# Context names use the (pull_request_target) suffix (not pull_request)
# to match the workflow event_type that actually emits them — verified
# live against PR#2419/#2331/etc.: the qa-review/security-review
# workflows run on pull_request_target (their `on:` block uses
# pull_request_target, not pull_request), and sop-checklist's
# all-items-acked job also uses pull_request_target. The previous
# (pull_request) suffix never matched the live emitted contexts,
# which is what was painting ~16 ready PRs red (gate appeared
# "missing" qa-review/security-review even after both passed).
# Verified against the lint-bp-context-emit-match test which already
# asserts (pull_request_target) for these names. No requirement
# dropped; just a name correction.
# SOP review-gate contexts (qa-review / security-review / sop-checklist) were
# fully removed 2026-07-14 (CTO directive): the producing workflows are deleted
# and no PR emits these contexts anymore, so keeping them here would make every
# merge wait forever on phantom-required checks. reserved-path-review was
# likewise RETIRED 2026-07-17 (CTO directive; workflow + script deleted), so the
# peer-approval floor (REVIEWER_SET + REQUIRED_APPROVALS, see below) plus the
# DETECTIVE audit-force-merge backstop are now the review gates. Kept as an
# (empty) list so the concat sites below are a harmless no-op and re-adding a
# uniform governance context is a one-line change.
GOVERNANCE_REQUIRED_CONTEXTS: list[str] = []

# --------------------------------------------------------------------------
# CRITICAL fail-closed contexts (RCA: core PR #1676 merged 2026-06-24 with
# `CI / Platform (Go) = failure` AND `CI / all-required = skipped`, turning
# main RED. Same class as #3181.)
# --------------------------------------------------------------------------
# The repo's `<workflow> / all-required` AGGREGATOR SENTINEL MUST be green
# before ANY merge. It is checked UNCONDITIONALLY and INDEPENDENTLY of whatever
# branch protection happens to enumerate in status_check_contexts — the #1676
# gap was precisely that when BP did NOT list it (or listed a different suffix),
# a skipped/failed aggregator fell through the required-set check and was then
# swept up as a "non-required red" and FORCE-MERGED past.
#
# Rules (all fail-closed — unverifiable == BLOCK):
#   * `<workflow> / all-required` : the aggregator sentinel. `skipped` is FATAL
#                              — a skipped all-required means the required jobs
#                              it `needs:` did NOT actually run, so nothing
#                              gated the merge. failure/error/missing also
#                              BLOCK; only `success` passes. Because the
#                              sentinel `needs:` every required job (enforced by
#                              lint-required-no-paths + all-required-check), its
#                              green transitively proves each sub-job (e.g.
#                              `Platform (Go)`) ran and passed, so #1676 is fully
#                              covered by requiring THIS one context green.
# The context names are matched by their base prefix (the part before the
# trailing " (pull_request*)" event suffix) so the guard catches the context
# regardless of which event variant Gitea emitted it under.
#
# ZERO-DRIFT / PER-REPO (root-cause fix): the critical set is NOT a hardcoded
# constant — it is DERIVED PER-REPO from that repo's own checked-in SSOT
# (`.gitea/required-contexts.txt` → enforced_file_contexts) by
# `critical_context_prefixes()`. The former hardcoded default
# ("CI / Platform (Go),CI / all-required") was molecule-core-specific and, when
# this shared script ran for other repos (e.g. molecule-controlplane, which
# emits LOWERCASE `ci / all-required` and has NO `Platform (Go)` job), demanded
# contexts that were legitimately ABSENT on that repo — phantom-blocking EVERY
# PR ("CRITICAL required context(s) not green"). A context that legitimately
# does not exist on a repo is not a missing required check.
# `CRITICAL_REQUIRED_CONTEXT_PREFIXES` remains an OPTIONAL explicit override (a
# repo may still pin extra critical contexts by name); left empty (the default)
# the set is SSOT-derived.
CRITICAL_REQUIRED_CONTEXT_PREFIXES = [
    name.strip()
    for name in _env(
        "CRITICAL_REQUIRED_CONTEXT_PREFIXES",
        default="",
    ).split(",")
    if name.strip()
]

REQUIRED_CONTEXTS_RAW = _env(
    "REQUIRED_CONTEXTS",
    default="CI / all-required (pull_request)",
)
# --------------------------------------------------------------------------
# Enforced-contexts SSOT file (internal#3181 — close the BP↔allowlist gap)
# --------------------------------------------------------------------------
# `.gitea/required-contexts.txt` is the DOCUMENTED authoritative set of
# "required to merge" contexts. Historically it was only consumed by
# lint_no_coe_on_required.py, and the merge gate derived its required set
# ONLY from branch-protection `status_check_contexts`. So a context listed
# in the file but ABSENT from BP was NOT merge-blocking — it was classified
# as a non-required advisory red and the former force-merge path bypassed it. That is
# exactly how PR#3181 merged with `E2E Staging SaaS (full lifecycle) /
# E2E Staging Concierge Creates Workspace` RED (in the file, not in BP).
#
# Fix: the merge gate now ALSO enforces every ENFORCED entry of this file
# (fail-closed, event-suffix-insensitive). The owner keeps BP edits
# owner-side; this does NOT require expanding BP status_check_contexts.
#
# `# pending-#NNNN` exclusion: a context that is documented-required but
# currently RED for a known, tracked reason can be parked under a section
# marker line of the form `# pending-#NNNN (not yet enforced)`. EVERY entry
# at or below the FIRST such marker is EXCLUDED from enforcement until the
# operator promotes it (move it above the marker). This is the sequencing
# escape hatch: it lets us ship the gate WITHOUT freezing merges on a check
# that an in-flight runtime PR is still fixing. See SEQUENCING in the file.
ENFORCED_CONTEXTS_FILE = _env(
    "ENFORCED_CONTEXTS_FILE",
    default=".gitea/required-contexts.txt",
)
# A line of this shape begins the "documented but NOT-yet-enforced" tail.
# Everything from the first match to EOF is parsed as documentation only.
_PENDING_MARKER_RE = re.compile(r"^\s*#\s*pending-#\d+\b", re.IGNORECASE)
# Strip the Gitea event suffix so the bare file form (`workflow / job`)
# matches a live status context (`workflow / job (pull_request)` etc).
# Mirrors lint_no_coe_on_required.strip_event so both sides agree.
_EVENT_SUFFIX_RE = re.compile(
    r"\s*\((?:pull_request|push|pull_request_target)\)\s*$"
)

# The queue's OWN commit-status context. The `gitea-merge-queue` workflow's
# `queue` job posts `gitea-merge-queue / queue` on the PR head, and that status
# is `pending` for the whole duration of THIS run. A wildcard branch-protection
# set ("*") therefore matches the queue's own in-flight status and — because it
# is pending, never success, during self-evaluation — makes the queue decide
# `wait` on the very PR whose approval fired the run. That is a self-referential
# deadlock (core#4420): a PR cannot merge on the event of its OWN approval; it only
# merges when some UNRELATED trigger evaluates it (that run's pending self-status
# lands on the other PR's head). required_contexts_green() drops this context
# from the user-space GLOB evaluation; reassert_queue_runner_status() reconciles
# the same gate-runner context immediately before Gitea's protected merge write.
#
# This is NOT the advisory-skip the module forbids elsewhere (skipping a real
# test that carries a red/pending status): the queue job is the gate-RUNNER, not
# a gate, and requiring its own in-flight status to be green is a logical
# impossibility, not a soft-fail. Only wildcard patterns are affected; a LITERAL
# requirement naming this context is left untouched. Event-suffix-insensitive
# (fires on pull_request_review, so the live suffix is not in _EVENT_SUFFIX_RE).
SELF_STATUS_CONTEXT = "gitea-merge-queue / queue"
_SELF_STATUS_RE = re.compile(
    r"\A" + re.escape(SELF_STATUS_CONTEXT) + r"(?:\s*\([^)]*\))?\s*\Z"
)
# Gitea's commit-status write response can precede visibility in the combined
# status view used by wildcard branch protection. Keep both waits short and
# bounded: failure to observe the write is a hard stop, never permission to
# bypass the protected endpoint.
SELF_STATUS_CONFIRM_ATTEMPTS = 5
SELF_STATUS_CONFIRM_DELAY_SECONDS = 0.25
MERGE_STATUS_RETRY_ATTEMPTS = 3
MERGE_STATUS_RETRY_DELAY_SECONDS = 0.5

# Required contexts for push (main/staging) runs. The push CI uses the same
# aggregator names with " (push)" suffix. Checking these explicitly instead of
# the combined state avoids false-pause when non-blocking jobs (e.g. Platform
# Go with continue-on-error: true due to mc#774) have failed — their failures
# pollute the combined state but do not block merges.
PUSH_REQUIRED_CONTEXTS_RAW = _env(
    "PUSH_REQUIRED_CONTEXTS",
    default="CI / all-required (push)",
)

# Recognised official-reviewer set. A merge requires this many DISTINCT genuine
# approvals (not stale/dismissed, on the current head sha) from accounts in
# this set.
#
# CTO 2026-07-14: the peer roster is the union of the review, management, and
# owner teams — reviewers, managers, and owners may all satisfy the floor.
# The prior default {agent-reviewer,agent-researcher,agent-reviewer-cr2} was
# a login list of three accounts that no automation drives; they produced 0
# genuine approvals across 29 merges, so the floor was unsatisfiable and EVERY
# merge fell through to a manual owner/admin path (the queue was dead). This
# default is the current membership of teams code-reviewers/qa/security/
# managers/Owners. It is still a LOGIN list (not team-resolved) so the queue
# needs no org-team read scope to run; the trade-off is that it drifts as team
# membership changes — tracked as a follow-up to make this team-keyed (SSOT),
# which requires confirming the merge-actor token's org:read scope first.
REVIEWER_SET = {
    name.strip()
    for name in _env(
        "REVIEWER_SET",
        default=(
            "molecule-code-reviewer,core-qa,core-security,"          # reviewers (code-reviewers/qa/security)
            "core-lead,cp-lead,dev-lead,app-lead,sdk-lead,"          # management
            "infra-lead,release-manager,pm,agent-pm,"               # management
            "hongming,cui,hongming-personal,hongming-pc2,"          # owners
            "claude-ceo-assistant,hongming-ceo-delegated"           # owner/delegate
        ),
    ).split(",")
    if name.strip()
}
# CTO 2026-07-14: the genuine-peer floor is ONE approval. The previous default
# of 2 was drift and, combined with the dead REVIEWER_SET above and the actual
# review practice (<=1 general review per PR, 0/14 recent merges had 2 distinct
# peer approvals), made the queue unable to merge anything.
# Branch protection's value still wins when it is a valid int >= this floor
# (see the fail-closed clamp in branch-protection parsing).
REQUIRED_APPROVALS_DEFAULT = int(_env("REQUIRED_APPROVALS", default="1") or "1")

# --------------------------------------------------------------------------
# Runtime-bump auto-merge exemption (RFC internal#131 PR-A, dispatch 9c2e9c88)
# --------------------------------------------------------------------------
# The bump-bot publishes single-line .runtime-version bumps when a runtime-v
# release lands. These PRs are TRIVIAL by construction (one-file, one-line,
# identical mechanical pattern), and the runtime-smoke CI on the PR is
# designed to be the actual proof — the new runtime has been provisioned +
# run a workspace on it. We trust a curated allowlist of "boring" runtimes
# (currently empty by default; CEO-Asst populates per runtime policy) to
# auto-merge such bumps WITHOUT 2-genuine review — but ONLY if every guard
# and condition below holds. This exists to break the rebase-churn loop
# for runtime-v releases (otherwise every bump queues behind normal review
# cadence and lands a day late).
#
# FAIL-CLOSED: any guard/condition that is unverifiable → NOT exempt. The
# PR routes through the normal 2-genuine path instead. We never auto-merge
# on uncertainty.
RUNTIME_BUMP_BOT_USER = _env("RUNTIME_BUMP_BOT_USER", default="bump-bot")
# Allowlist of runtime names that are eligible for the exemption. Populated
# by the CEO-Asst via env var (comma-separated). Default empty = no runtime
# is eligible; the function is a no-op until the CEO-Asst opts a runtime in.
# RUNTIME_BUMP_EXEMPT_TEMPLATES values must match the runtime identifier in
# .runtime-version (e.g. "claude-code", "hermes", "kimi-cli"), NOT the
# repository name.
RUNTIME_BUMP_EXEMPT_TEMPLATES = {
    name.strip()
    for name in _env("RUNTIME_BUMP_EXEMPT_TEMPLATES", default="").split(",")
    if name.strip()
}
# Subset of branch-protection REQUIRED contexts that prove the PR was
# actually verified by a real runtime-provision/compat smoke (i.e. the CI
# job provisions a workspace on the new runtime and runs a basic task —
# NOT a static build/lint/unit-test job). The PR is GATE-ADEQUATE only if
# at least ONE of these (case-insensitive substring match) appears in the
# required_contexts set the queue loaded from branch protection. Adding
# a new runtime-smoke job to BP without adding the context name here will
# silently disable the exemption for that job — the fail-closed design
# deliberately errs on the side of falling back to 2-genuine.
RUNTIME_PROVISION_SMOKE_CONTEXTS = {
    name.strip().lower()
    for name in _env(
        "RUNTIME_PROVISION_SMOKE_CONTEXTS",
        default=(
            "runtime-provision-smoke,"
            "runtime-compat-smoke,"
            "e2e-runtime-provision"
        ),
    ).split(",")
    if name.strip()
}
# Labels that mark a runtime-bump as breaking. EXCLUDE-BREAKING (condition
# 3) treats any of these as "not eligible", regardless of semver. The
# bump-bot is not expected to add these labels — the set is a safety net
# for the case where a human adds one mid-flight (e.g. "the API changed,
# callers need a rewire" mid-bump).
BREAKING_LABELS = {
    name.strip()
    for name in _env(
        "BREAKING_LABELS",
        default="breaking,breaking-change,requires-consumer-wiring",
    ).split(",")
    if name.strip()
}

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""

# --------------------------------------------------------------------------
# Conductor snapshot (operator-config#158)
# --------------------------------------------------------------------------
# When the conductor tick writes a state snapshot before running the passes,
# both scripts see the SAME observed state instead of re-fetching independently
# and potentially disagreeing within the same tick.
# --------------------------------------------------------------------------


def load_conductor_snapshot() -> dict | None:
    """Load the conductor snapshot if present and fresh.

    The snapshot is written by the conductor wrapper
    (bin/molecule-core-cron-bot.sh conductor) to a path exported as
    CONDUCTOR_SNAPSHOT_FILE. It contains open PRs + per-head combined
    statuses + reviews captured in a single state-read.

    Returns the parsed snapshot dict, or None if absent, unreadable,
    or older than the freshness threshold (10 minutes — twice the */5
    conductor cadence, so a single skipped tick does not invalidate it).
    """
    path = os.environ.get("CONDUCTOR_SNAPSHOT_FILE", "")
    if not path:
        return None
    try:
        with open(path, "r", encoding="utf-8") as f:
            snapshot = json.load(f)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"::notice::conductor snapshot unreadable ({exc}); self-fetching")
        return None

    if not isinstance(snapshot, dict):
        return None

    # Freshness is fail-closed: an UNVERIFIABLE age must NOT be trusted as
    # current. A snapshot with no `ts` (absent/empty) or an unparseable `ts`
    # could be arbitrarily old (e.g. written by a wedged conductor), so we
    # CANNOT prove it is within the 10-minute window — discard it and let the
    # caller self-fetch the live state via the API (the None-return path every
    # consumer already handles on a cache miss). The previous behaviour skipped
    # the age check on an empty `ts` and swallowed a strptime failure as
    # "treat as fresh (conservative)" — that was ANTI-conservative: it trusted
    # an undated/old snapshot as current.
    ts_str = snapshot.get("ts", "")
    if not ts_str:
        print(
            "::notice::conductor snapshot has no ts (freshness unverifiable); "
            "self-fetching"
        )
        return None
    try:
        from datetime import datetime, timezone

        ts = datetime.strptime(ts_str, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=timezone.utc
        )
    except ValueError:
        print(
            f"::notice::conductor snapshot ts unparseable ({ts_str!r}; freshness "
            "unverifiable); self-fetching"
        )
        return None
    age_sec = (datetime.now(timezone.utc) - ts).total_seconds()
    if age_sec > 600:  # 10 minutes
        print(
            f"::notice::conductor snapshot stale ({int(age_sec)}s); "
            "self-fetching"
        )
        return None

    return snapshot


class ApiError(RuntimeError):
    pass


class MergePermissionError(ApiError):
    """The normal merge endpoint refused the request (403/404/405).

    This can be permission or branch-protection policy. The queue never
    bypasses it; it best-effort HOLDS the PR and scans the next candidate.
    """


class BranchUpdateConflictError(ApiError):
    """Updating the PR branch with the base hit a merge-conflict (HTTP 409).

    A true merge-conflict is NOT transient: the branch cannot be auto-updated
    until a human/agent rebases it. The queue should HOLD this PR (apply
    HOLD_LABEL) and advance to the next candidate, exactly like the permission
    path — otherwise the conflicted PR sits at the queue head and is retried
    every tick forever, head-of-line-blocking every ready PR behind it.

    NOTE: distinct from mergeable=None, which is Gitea STILL COMPUTING conflict
    state — that case is handled as a transient WAIT (no hold). This error is
    only raised on an explicit 409 returned by the /update endpoint."""


class BranchProtectionUnavailable(ApiError):
    """Branch protection (the authoritative required-context source) could not
    be enumerated. The queue must HOLD rather than merge with an unverified
    required-context set (fail-closed, no fail-open)."""


class EnforcedContextsUnavailable(ApiError):
    """The SSOT enforced-contexts file (`.gitea/required-contexts.txt`) could
    not be READ (missing or unreadable). The queue must HOLD rather than merge
    with the file-sourced SSOT enforcement SILENTLY DISABLED — returning an
    empty enforced set on a read failure is a fail-OPEN that defeats the whole
    point of #3181 (a deleted/unreadable SSOT would let the gate fall back to
    BP + governance only, exactly the force-merge-over-red class #3207 closes).

    DISTINCT from a successfully-read but legitimately-EMPTY file (only comments,
    or every entry parked below a `# pending-#NNNN` marker): that returns [] and
    enforces BP + governance only, which is a valid, intended state — NOT this
    error. Only an OSError opening/reading the file raises this."""


class PushRequiredContextsUnavailable(ApiError):
    """The push-required context set (env `PUSH_REQUIRED_CONTEXTS`) parsed to
    EMPTY. The queue must HOLD rather than treat the main-green backstop as
    satisfied (internal#3210 HIGH).

    The main-green gate checks `required_contexts_green(main_latest, <set>)`;
    with an empty set that call returns `(True, [])` — a VACUOUS pass for ANY
    main state, INCLUDING all-red. The backstop that makes the direct-merge
    path safe (it PAUSES the queue when main's required CI goes red after a
    semantic main-break) would silently disappear, and the queue would keep
    merging onto a red main.

    An empty parse is a MISCONFIGURATION (env unset/blank/all-comma), not a
    transient Gitea blip, so — like EnforcedContextsUnavailable — it propagates
    to main()'s ApiError handler (rc 1: no merge + operators paged) rather than
    being held quietly with rc 0."""


@dataclasses.dataclass(frozen=True)
class MergeDecision:
    ready: bool
    action: str
    reason: str


@dataclasses.dataclass(frozen=True)
class BranchProtection:
    """The subset of branch protection the queue depends on."""

    required_contexts: list[str]
    required_approvals: int
    block_on_rejected_reviews: bool
    block_on_outdated_branch: bool = False
    # True when this record was NOT read from the authoritative
    # `/branch_protections` endpoint but SYNTHESIZED from the env-configured
    # fallback because that endpoint was FORBIDDEN (HTTP 403) to the queue's
    # merge actor. In this mode the queue does NOT trust its own client-side
    # required-context/approval set as the authority — it relies on Gitea's
    # server-side branch-protection enforcement AT MERGE TIME (the protected
    # `/pulls/{n}/merge` endpoint re-checks BP itself and returns 405 for an
    # under-approved / red / stale-head PR, which merge_pull maps to
    # MergePermissionError -> HOLD). See get_branch_protection() for why a 403
    # is a DETERMINISTIC permission state that must NOT fail-closed forever.
    fallback: bool = False


def _require_runtime_env() -> None:
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "WATCH_BRANCH", "QUEUE_LABEL"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)
    if UPDATE_STYLE not in {"merge", "rebase"}:
        sys.stderr.write("::error::UPDATE_STYLE must be merge or rebase\n")
        sys.exit(2)


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
        # CF WAF in front of git.moleculesai.app 1010-bans the default
        # Python-urllib UA; send a non-urllib UA so this reaches Gitea
        # (transport-only — auth/method/semantics unchanged).
        "User-Agent": "molecule-ci-gate/1.0 (+gitea-api)",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        status = exc.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {path} -> HTTP {status}: {snippet}")
    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as exc:
        if expect_json:
            raise ApiError(f"{method} {path} -> HTTP {status} non-JSON: {exc}") from exc
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


def api_paginated(
    method: str,
    path: str,
    *,
    query: dict[str, str] | None = None,
    page_size: int = 50,
) -> list[dict]:
    """Fetch all pages of a paginated Gitea list endpoint.

    Gitea paginates with `page` (1-indexed) and `limit`. We loop until a
    page returns fewer than `page_size` items, indicating the end.
    """
    results: list[dict] = []
    page = 1
    while True:
        page_query = dict(query or {})
        page_query["page"] = str(page)
        page_query["limit"] = str(page_size)
        _, body = api(method, path, query=page_query)
        if not isinstance(body, list):
            raise ApiError(f"{path} paginated response not list")
        results.extend(body)
        if len(body) < page_size:
            break
        page += 1
    return results


def required_contexts(raw: str) -> list[str]:
    return [part.strip() for part in raw.split(",") if part.strip()]


def push_required_contexts() -> list[str]:
    """Required contexts for push (branch) CI runs. See PUSH_REQUIRED_CONTEXTS_RAW.

    Fail-closed (internal#3210 HIGH): if `PUSH_REQUIRED_CONTEXTS` is empty,
    whitespace-only, or all-comma, the parse is [] and the main-green backstop
    (`required_contexts_green(main_latest, [])`) would PASS VACUOUSLY for any
    main state, including all-red — letting the queue merge onto a red main.
    Raise PushRequiredContextsUnavailable instead so the gate HOLDS rather than
    silently disabling its own main-green guard. Mirrors the
    EnforcedContextsUnavailable fail-closed convention; never returns []."""
    contexts = required_contexts(PUSH_REQUIRED_CONTEXTS_RAW)
    if not contexts:
        raise PushRequiredContextsUnavailable(
            "PUSH_REQUIRED_CONTEXTS parsed to an empty set "
            f"(raw={PUSH_REQUIRED_CONTEXTS_RAW!r}); the main-green backstop "
            "would pass vacuously for ANY main state (including all-red). "
            "Merge gate HOLDS the tick (fail-closed) rather than disabling the "
            "main-green guard. Set at least the default 'CI / all-required "
            "(push)'."
        )
    return contexts


def status_state(status: dict) -> str:
    return str(status.get("status") or status.get("state") or "").lower()


def status_numeric_id(status: dict) -> int:
    """Return a sortable Gitea commit-status id, or -1 when unavailable."""
    value = status.get("id")
    if isinstance(value, bool):
        return -1
    try:
        return int(value)
    except (TypeError, ValueError):
        return -1


def latest_statuses_by_context(statuses: list[dict]) -> dict[str, dict]:
    # Gitea /statuses endpoint returns entries in ascending id order (oldest
    # first). We need the LAST occurrence of each context, so iterate in
    # reverse to prefer newer entries.
    latest: dict[str, dict] = {}
    for status in reversed(statuses):
        context = status.get("context")
        if isinstance(context, str):
            latest[context] = status  # overwrite: reverse order → newest wins
    return latest


class GiteaGlobCompileError(ValueError):
    """A branch-protection glob cannot be compiled with Gitea semantics."""


_GITEA_GLOB_SPECIAL = frozenset("*?\\[]{}")


def _is_glob(pattern: str) -> bool:
    """Mirror Gitea modules/glob.IsSpecialByte.

    Branch-protection entries are always compiled as Gitea globs. This helper
    is only used to keep the diagnostic distinction between a missing plain
    literal and a special pattern that matched nothing.
    """
    return any(c in _GITEA_GLOB_SPECIAL for c in pattern)


def _compile_gitea_glob(pattern: str) -> re.Pattern[str]:
    """Compile the status-context glob exactly along Gitea's modules/glob
    grammar (with no separators, as services/pull/commit_status.go invokes it).

    Supported server syntax is broader than Python ``fnmatch``: ``*``, ``**``,
    ``?``, character classes and ``[!x]`` negation, brace alternatives, and
    backslash escapes. Invalid classes/escapes fail closed at the caller.
    """
    pos = 0

    # Gitea hands glob escapes to Go's regexp compiler. Python's regexp
    # escape language is not the same: for example, Python accepts ``\u0061``
    # where Go rejects it, Python may parse ``\1`` as a backreference, and
    # Python's Unicode ``\w`` is wider than Go/RE2's ASCII class. Translate a
    # deliberately conservative common subset and reject every other
    # alphanumeric escape. An unsupported policy must block merging rather
    # than be interpreted differently from branch protection.
    regexp_classes = {
        "d": "[0-9]",
        "D": "[^0-9]",
        "s": r"[\t\n\f\r ]",
        "S": r"[^\t\n\f\r ]",
        "w": "[0-9A-Za-z_]",
        "W": "[^0-9A-Za-z_]",
    }
    class_fragments = {
        "d": "0-9",
        "s": r"\t\n\f\r ",
        "w": "0-9A-Za-z_",
    }
    control_escapes = frozenset("aftnrv")

    def compile_escape(*, in_character_class: bool) -> str:
        nonlocal pos
        if pos >= len(pattern):
            raise GiteaGlobCompileError("no character to escape")

        escaped = pattern[pos]
        pos += 1
        if escaped in control_escapes:
            return "\\" + escaped
        if in_character_class and escaped in class_fragments:
            return class_fragments[escaped]
        if not in_character_class and escaped in regexp_classes:
            return regexp_classes[escaped]

        # Go/RE2 accepts an escaped ASCII punctuation character as that
        # literal character. Re-escape it for Python instead of passing the
        # raw sequence through (whose meaning may differ).
        if (
            escaped.isascii()
            and 0x20 <= ord(escaped) <= 0x7E
            and not escaped.isalnum()
        ):
            return re.escape(escaped)

        raise GiteaGlobCompileError(f"unsupported regexp escape \\{escaped}")

    def compile_chars() -> str:
        nonlocal pos
        result = ""
        if pos < len(pattern) and pattern[pos] == "!":
            pos += 1
            result += "^"
        while pos < len(pattern):
            c = pattern[pos]
            pos += 1
            if c == "]":
                return "[" + result + "]"
            if c == "[":
                # Go/RE2 accepts POSIX named classes such as ``[[:alpha:]]``;
                # Python's regexp parser gives the same bytes nested-set
                # semantics. This conservative port does not translate that
                # syntax, so reject it instead of widening/narrowing the
                # branch-protection policy. A literal '[' remains available
                # as ``\[`` below.
                raise GiteaGlobCompileError(
                    "nested/POSIX character classes are unsupported"
                )
            if c == "\\":
                if pos >= len(pattern):
                    raise GiteaGlobCompileError("unterminated character class escape")
                result += compile_escape(in_character_class=True)
            else:
                result += c
        raise GiteaGlobCompileError("unterminated character class")

    def compile_part(subpattern: bool) -> str:
        nonlocal pos
        result = ""
        while pos < len(pattern):
            c = pattern[pos]
            pos += 1
            if subpattern and c == "}":
                return "(?:" + result + ")"
            if c == "*":
                if pos < len(pattern) and pattern[pos] == "*":
                    pos += 1
                # Gitea calls glob.Compile(ctx) without separators, so single
                # and double star both match any sequence of characters.
                result += ".*"
            elif c == "?":
                result += "."
            elif c == "[":
                result += compile_chars()
            elif c == "{":
                result += compile_part(True)
            elif c == "," and subpattern:
                result += "|"
            elif c == "\\":
                result += compile_escape(in_character_class=False)
            else:
                result += re.escape(c)
        # This intentionally mirrors Gitea's compiler: an opening brace that
        # reaches EOF returns the compiled subexpression rather than raising.
        return result

    try:
        return re.compile(r"\A" + compile_part(False) + r"\Z", re.ASCII)
    except re.error as exc:
        raise GiteaGlobCompileError(str(exc)) from exc


def required_contexts_green(
    latest_statuses: dict[str, dict],
    contexts: list[str],
) -> tuple[bool, list[str]]:
    # Gitea branch protection uses GLOB patterns in status_check_contexts —
    # most importantly the bare "*" ("every POSTED commit status must be
    # success"), but also narrower forms like "CI /*". Reading a glob
    # literally (`latest_statuses.get("*")` -> None -> "missing") made this
    # return (False, ["*=missing"]) for EVERY PR, including a fully green one —
    # so the queue decided `wait` on all of them and step 4b (the
    # `.gitea/required-contexts.txt` presence check) was never reached. The
    # queue thus merged nothing, and every merge on main was performed by a
    # human (core#4363).
    #
    # Each entry is matched with Gitea's real semantics:
    #   * a GLOB pattern is a SUPERSET check — it matches every posted context
    #     via the same grammar as modules/glob and requires all matches to be
    #     `success`.
    #     A glob that matches ZERO posted contexts is fail-closed: we cannot
    #     prove any required gate ran, so it WAITs (`<pattern>=no-match`). This
    #     closes the pre-#4363-followup vacuous-green hole where "*" over an
    #     empty status set returned green (safety then rested entirely on the
    #     step-0 critical backstop + step-4b presence check; now this layer
    #     itself refuses to bless an empty board).
    #   * a LITERAL entry (no wildcard) is a PRESENCE check — that exact named
    #     context must be posted AND `success`.
    # Results are de-duped so a context flagged by more than one pattern is
    # reported once. (core#4363 + follow-up: glob-generalized, fail-closed on
    # empty match.)
    missing_or_bad: list[str] = []
    seen: set[str] = set()

    def _flag(ctx: str, state: str) -> None:
        if ctx in seen:
            return
        seen.add(ctx)
        missing_or_bad.append(f"{ctx}={state or 'missing'}")

    for pattern in contexts:
        try:
            matcher = _compile_gitea_glob(pattern)
        except GiteaGlobCompileError:
            # Gitea drops an uncompileable required pattern from its matcher;
            # the queue is stricter and makes that policy defect explicit.
            _flag(pattern, "invalid-glob")
            continue
        matches = [c for c in latest_statuses if matcher.fullmatch(c)]
        if _is_glob(pattern):
            # Drop the queue's OWN in-flight status from wildcard matches ONLY
            # while it is `pending` — it stays `pending` throughout this very run,
            # so requiring it green under a glob like "*" is a self-referential
            # deadlock (core#4420, see SELF_STATUS_CONTEXT). The strip is gated on
            # the pending state (fail-closed): a self-status that has actually
            # FAILED/errored is a real red and MUST still block the merge — it
            # self-heals on the next clean tick when a fresh run re-posts a
            # pending (then success) status. A LITERAL requirement is left
            # untouched: a narrow glob ("CI /*") never matches it anyway, and if a
            # policy deliberately names it exactly, honor that.
            matches = [
                c
                for c in matches
                if not (
                    _SELF_STATUS_RE.fullmatch(c)
                    and status_state(latest_statuses.get(c) or {}) == "pending"
                )
            ]
        if not matches:
            # Fail-closed: a required pattern that matches nothing cannot be
            # shown green. Preserve the old `missing` wording for plain exact
            # names; special Gitea patterns report `no-match`.
            _flag(pattern, "no-match" if _is_glob(pattern) else "missing")
            continue
        for ctx in matches:
            state = status_state(latest_statuses.get(ctx) or {})
            if state != "success":
                _flag(ctx, state)

    return not missing_or_bad, missing_or_bad


def _strip_event(ctx: str) -> str:
    """Event-suffix-stripped form of a context name.

    `.gitea/required-contexts.txt` stores the bare `workflow / job` form;
    live status contexts carry a ` (pull_request)` / ` (push)` /
    ` (pull_request_target)` suffix. Strip it so file entries match live
    statuses regardless of trigger. Mirrors lint_no_coe_on_required.strip_event
    so both consumers agree on the canonical key.
    """
    return _EVENT_SUFFIX_RE.sub("", ctx).strip()


def load_enforced_file_contexts(path: str) -> list[str]:
    """Parse `.gitea/required-contexts.txt` → the ENFORCED (event-stripped)
    context names the merge gate must treat as merge-blocking.

    Rules:
      - blank lines and `#` comment lines are ignored;
      - inline `# ...` trailing comments are stripped (matches the lint);
      - a line matching `# pending-#NNNN ...` begins the NOT-YET-ENFORCED
        tail: that line AND everything after it (to EOF) is parsed as
        documentation only, NEVER enforced. This is the #3159 sequencing
        escape hatch — park a documented-but-currently-red context below
        the marker so shipping the gate does not freeze merges, then
        promote it (move it above the marker) once it goes green.

    Fail-closed contract (RC 13618): if the file is MISSING or UNREADABLE,
    RAISE EnforcedContextsUnavailable. The merge gate must HOLD rather than
    silently fall back to BP + governance only — returning [] on a read
    failure was a fail-OPEN: a deleted/unreadable SSOT would disable the
    file-sourced enforcement WITHOUT any merge being blocked. The lint
    (lint_no_coe_on_required) only guards the PR-CI path on the proposing
    branch; it does NOT protect the queue at merge time against a file that
    is absent/unreadable in the queue's own checkout, so the gate cannot
    delegate its fail-closed duty to the lint. A successfully-read file that
    is legitimately empty (comments only, or all entries below a pending
    marker) still returns [] — that is a valid "enforce BP + governance only"
    state, not an error.
    """
    enforced: list[str] = []
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.readlines()
    except (OSError, UnicodeError) as exc:
        # OSError = missing / permission / IO failure; UnicodeError (incl.
        # UnicodeDecodeError, a ValueError NOT an OSError) = a corrupt/binary
        # SSOT file. BOTH mean "cannot read the gate's source of truth" and
        # must fail closed CLEANLY as an unavailable-SSOT incident — not slip
        # past as an unhandled traceback that main()'s ApiError handler would
        # not classify. (Audit RC 13618 follow-up.)
        raise EnforcedContextsUnavailable(
            f"enforced-contexts SSOT {path} unreadable/undecodable ({exc!r}); "
            "merge gate HOLDS the tick (fail-closed) rather than silently "
            "enforcing BP + governance only"
        ) from exc
    in_pending_tail = False
    for raw in lines:
        if _PENDING_MARKER_RE.match(raw):
            # First pending marker → everything below is documentation.
            in_pending_tail = True
            continue
        if in_pending_tail:
            continue
        body = raw.split("#", 1)[0].strip()
        if not body:
            continue
        stripped = _strip_event(body)
        if stripped and stripped not in enforced:
            enforced.append(stripped)
    return enforced


def enforced_file_contexts_green(
    latest_statuses: dict[str, dict],
    enforced_stripped: list[str],
) -> tuple[bool, list[str]]:
    """Event-suffix-INSENSITIVE green check for the file-sourced enforced set.

    BP-required contexts are matched exactly (they carry the suffix BP
    records). File entries are bare, so here we compare event-stripped
    forms: an enforced entry is green only if SOME live status whose
    event-stripped context equals it is `success`. Missing, pending, or
    failure → not green (fail-closed, same semantics as
    required_contexts_green)."""
    # Best status per event-stripped context key (success wins; otherwise
    # any non-success is retained so a red is never masked by a missing).
    best: dict[str, str] = {}
    for ctx, status in latest_statuses.items():
        if not isinstance(ctx, str):
            continue
        key = _strip_event(ctx)
        state = status_state(status or {})
        prev = best.get(key)
        if prev == "success":
            continue
        if prev is None or state == "success":
            best[key] = state
    missing_or_bad: list[str] = []
    for ctx in enforced_stripped:
        state = best.get(ctx)
        if state != "success":
            missing_or_bad.append(f"{ctx}={state or 'missing'}")
    return not missing_or_bad, missing_or_bad


def parse_branch_protection(body: Any) -> BranchProtection:
    """Extract the queue-relevant fields from a branch_protections payload.

    Fail-closed: raises BranchProtectionUnavailable when status checks are
    expected but the required-context list cannot be enumerated. We never fall
    back to a hand-maintained env list as the authoritative required set —
    doing so risks merging when a real required context is red/missing.
    """
    if not isinstance(body, dict):
        raise BranchProtectionUnavailable("branch protection response not an object")
    enable = bool(body.get("enable_status_check"))
    contexts_raw = body.get("status_check_contexts")
    if not enable:
        # Status checks not enforced by BP at all. With no required contexts
        # the queue would gate on approvals only — acceptable, but make it
        # explicit and let the caller decide.
        contexts: list[str] = []
    else:
        if not isinstance(contexts_raw, list):
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is not a list"
            )
        contexts = [c for c in contexts_raw if isinstance(c, str) and c.strip()]
        if not contexts:
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is empty"
            )
    # FAIL-CLOSED approval floor (internal#3210 FIX-1).
    #
    # `required_approvals` is the genuine-approval bar consumed by
    # evaluate_merge_readiness step 3. A degraded BP value here would silently
    # weaken or zero that gate, so we clamp it UP to the default floor and never
    # accept a value that lowers/skips it:
    #
    #   - bool: `isinstance(True, int)` is True in Python, so an upstream bool
    #     True would coerce to int 1 and HALVE a 2-genuine bar to 1. Reject bool
    #     explicitly → fall back to the default floor.
    #   - 0 / negative: an admin lowering the bar, a Gitea migration/default
    #     reset, or a blanked/restored BP record can yield 0 (or a negative that
    #     passes a naive `< N` check). These must clamp UP to the default, NEVER
    #     down/skip.
    #   - valid positive int >= default: honored as-is (a stricter BP wins).
    #
    # max(int(approvals), REQUIRED_APPROVALS_DEFAULT) gives exactly this: the
    # derived bar is never below the SSOT default floor.
    approvals = body.get("required_approvals")
    if isinstance(approvals, bool) or not isinstance(approvals, int):
        required_approvals = REQUIRED_APPROVALS_DEFAULT
    else:
        required_approvals = max(approvals, REQUIRED_APPROVALS_DEFAULT)
    return BranchProtection(
        required_contexts=contexts,
        required_approvals=required_approvals,
        block_on_rejected_reviews=bool(body.get("block_on_rejected_reviews")),
        block_on_outdated_branch=bool(body.get("block_on_outdated_branch")),
    )


# Match the `-> HTTP <code>` fragment that api() embeds in every ApiError it
# raises (`f"{method} {path} -> HTTP {status}: {snippet}"`). Used to tell a
# DETERMINISTIC permission denial (403) apart from a transient/unknown failure.
_HTTP_STATUS_RE = re.compile(r"-> HTTP (\d{3})\b")


def _apierror_http_status(exc: ApiError) -> int | None:
    """Best-effort extract of the HTTP status embedded in an ApiError message,
    or None when the error carries no `-> HTTP <code>` marker (e.g. a non-JSON
    body error or a locally-raised ApiError)."""
    m = _HTTP_STATUS_RE.search(str(exc))
    return int(m.group(1)) if m else None


def _fallback_branch_protection(reason: str) -> BranchProtection:
    """Synthesize a SAFE branch-protection record for the merge-time-authoritative
    fallback path (see get_branch_protection).

    The env-configured defaults are used as the queue's CLIENT-side gate:
      * required_contexts  -> REQUIRED_CONTEXTS (default the aggregator
        `CI / all-required (pull_request)`). This is a SUBSET of the real
        wildcard BP set, so it can never be LESS strict at merge time than
        Gitea's own server-side check — Gitea still enforces the full `*` set
        and rejects (405) any PR with any other posted context red.
      * required_approvals -> REQUIRED_APPROVALS_DEFAULT, the env approval
        FLOOR (never 0). This matters because a repo whose BP records
        required_approvals=0 gets NO server-side approval enforcement from
        Gitea at merge time, so this client floor is the ONLY approval gate in
        fallback mode — it must never drop below the SSOT floor.
      * block_on_outdated_branch -> False: take the #2358 direct-merge path; a
        stale head Gitea still 405s server-side and merge_pull HOLDs it.

    fallback=True flags every consumer that the authority is Gitea's merge-time
    check, not this record.
    """
    print(
        "::warning::branch protection unreadable to the queue merge actor "
        f"({reason}); entering GITEA-AUTHORITATIVE FALLBACK: the protected "
        "merge endpoint re-checks branch protection server-side and rejects "
        "(405) any under-approved / red / stale-head PR. Client-side gate uses "
        f"REQUIRED_CONTEXTS={REQUIRED_CONTEXTS_RAW!r} and the approval floor "
        f"REQUIRED_APPROVALS={REQUIRED_APPROVALS_DEFAULT}. This grants the queue "
        "NO new privilege — it merges strictly less than Gitea itself allows."
    )
    return BranchProtection(
        required_contexts=required_contexts(REQUIRED_CONTEXTS_RAW),
        required_approvals=REQUIRED_APPROVALS_DEFAULT,
        block_on_rejected_reviews=True,
        block_on_outdated_branch=False,
        fallback=True,
    )


def get_branch_protection(branch: str) -> BranchProtection:
    """Fetch branch protection for `branch`.

    The `/branch_protections/{branch}` READ endpoint is ADMIN-only in Gitea
    ("user should be an owner or a collaborator with admin write"). The queue's
    non-bypass merge actor is deliberately a WRITE collaborator (least
    privilege — write is all it needs to MERGE), so this read can legitimately
    return 403 while the actor still has full authority to merge. Fail-closing
    on that 403 would red-stamp `gitea-merge-queue / queue` on EVERY PR under a
    `['*']` BP and jam the whole repo (the exact regression this fixes: the CP
    merge actor was reset admin->write and the queue died).

    A 403 is a DETERMINISTIC permission state — re-reading will never succeed,
    so holding forever is wrong. Instead FALL BACK to Gitea's own
    merge-time enforcement: build a safe env-derived BranchProtection
    (fallback=True) and let the protected merge endpoint be the authoritative
    gate. It remains IMPOSSIBLE for the queue to merge anything Gitea would
    reject: the queue never uses the admin force-override, so Gitea re-checks
    required approvals + required contexts server-side at write time and 405s an
    unready PR, which merge_pull converts to a HOLD.

    A NON-403 failure (transient 5xx, network, malformed body) is NOT
    deterministic and could self-resolve, so it stays FAIL-CLOSED
    (BranchProtectionUnavailable -> HOLD the tick, retry next tick) rather than
    dropping to the fallback on a possible Gitea blip.
    """
    try:
        _, body = api("GET", f"/repos/{OWNER}/{NAME}/branch_protections/{branch}")
    except ApiError as exc:
        status = _apierror_http_status(exc)
        if status == 403:
            return _fallback_branch_protection(
                f"HTTP 403 reading branch_protections/{branch} "
                "(merge actor lacks admin read; write is sufficient to merge)"
            )
        raise BranchProtectionUnavailable(
            f"could not fetch branch protection for {branch}: {exc}"
        ) from exc
    return parse_branch_protection(body)


def genuine_approvals(
    reviews: list[dict],
    *,
    headsha: str,
    reviewer_set: set[str],
) -> tuple[set[str], list[str]]:
    """Thin wrapper over the SSOT predicate in _approval_validator.py.

    All logic — the per-review commit_id / state / official / dismissed /
    stale contract — lives in _approval_validator.classify_reviews. This
    wrapper exists only to keep the call site (and external readers of
    the symbol) stable. Do NOT add any per-review logic here; if you need
    to change the predicate, edit _approval_validator.py.

    See _approval_validator.py for the full fail-closed contract
    (SEV-1 internal#812). The previous inline implementation had a
    `if isinstance(commit_id, str) and commit_id and headsha:` guard that
    silently accepted reviews with no commit_id; that fail-open surface is
    now closed at the SSOT.
    """
    return _classify_reviews_ssot(
        reviews, headsha=headsha, reviewer_set=reviewer_set
    )

def get_pull_reviews(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/reviews")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} reviews response not list")
    return body


def label_names(issue: dict) -> set[str]:
    return {
        label["name"]
        for label in issue.get("labels", [])
        if isinstance(label, dict) and isinstance(label.get("name"), str)
    }


# --------------------------------------------------------------------------
# Runtime-bump exemption (RFC internal#131 PR-A; spec 9c2e9c88)
# --------------------------------------------------------------------------
# Helpers + the is_runtime_bump_exempt() predicate. Wired into
# evaluate_merge_readiness via the runtime_bump_exempt flag (when True, the
# required-approvals check is skipped — the 2-genuine convention is
# intentionally bypassed for these PRs). The runtime-smoke CI context is
# STILL required (it's a required_contexts check, not an approvals check,
# so the exemption does not weaken the CI gate).
# --------------------------------------------------------------------------

# File touched by a qualifying runtime-bump. The bump-bot writes the new
# version as the ONLY changed line in this file; the line is also the ONLY
# removed line (the old version). Any other file touched, or multiple
# lines, fails the single-line guard and the PR is treated as non-bump.
RUNTIME_VERSION_FILE = ".runtime-version"


def _runtime_version_added_removed_lines(patch: str) -> tuple[int, int, str | None, str | None]:
    """Parse a unified-diff patch string and return the (added, removed) line
    counts AND the values of the single added/removed non-header lines.

    A runtime-bump patch has exactly ONE added line (the new version) and
    ONE removed line (the old version). If either count differs, the PR is
    not a clean single-line bump and is rejected by the guard.

    Header lines (`+++`, `---`) are excluded. Empty lines (a `+` or `-`
    with no content) are treated as a valid line — but a runtime-bump
    would not have one (the version is always a non-empty string).
    """
    added = 0
    removed = 0
    added_value: str | None = None
    removed_value: str | None = None
    for line in patch.splitlines():
        if line.startswith("+++") or line.startswith("---"):
            continue
        if line.startswith("+"):
            added += 1
            # Strip the leading `+` and any trailing newline. Preserve
            # the value verbatim so ==SSOT can compare it to the runtime-v
            # release tag without ambiguity (no trim, no strip).
            added_value = line[1:]
        elif line.startswith("-"):
            removed += 1
            removed_value = line[1:]
    return added, removed, added_value, removed_value


def is_runtime_bump_exempt(
    *,
    pr: dict,
    pr_files: list[dict],
    required_contexts: list[str],
    latest_runtime_v_tag: str | None,
    rc_active: bool,
    posted_contexts: list[str] | None = None,
) -> tuple[bool, str]:
    """Decide whether a PR is a qualifying runtime-bump eligible for the
    auto-merge exemption. Returns (exempt, reason).

    FAIL-CLOSED: every guard/condition that is unverifiable → NOT exempt.
    The PR routes through the normal 2-genuine path. We never auto-merge
    on uncertainty.

    GUARDS (all must hold):
      1. author == bump-bot (RUNTIME_BUMP_BOT_USER)
      2. diff touches EXACTLY .runtime-version AND has exactly 1 added +
         1 removed non-header line in that file
      3. no active release-candidate in progress (rc_active == False)
      4. all required CI contexts green — ENFORCED in evaluate_merge_readiness,
         not here; the exemption skips the approvals check, not the CI gate

    CONDITIONS (all must hold):
      1. ==SSOT: the added value == the latest runtime-v release tag
         (NOT merely "valid semver"; the value must match a real release
         the runtime repo has published)
      2a. GATE-ADEQUACY: the loaded required_contexts set includes a real
         runtime-provision/compat smoke context (a job that provisions +
         runs a workspace on the new runtime — NOT static build/lint)
      2b. GATE-ADEQUACY: the runtime name (parsed from the .runtime-version
         header) is on RUNTIME_BUMP_EXEMPT_TEMPLATES
      3. EXCLUDE-BREAKING: the new version is NOT semver-MAJOR (major>0)
         AND the PR has no breaking label (BREAKING_LABELS)
    """
    # GUARD 1: author must be bump-bot. Fail-closed: a missing/unknown
    # user is NOT bump-bot.
    author = (pr.get("user") or {}).get("login", "")
    if not isinstance(author, str) or author != RUNTIME_BUMP_BOT_USER:
        return False, f"author={author!r} != RUNTIME_BUMP_BOT_USER={RUNTIME_BUMP_BOT_USER!r}"

    # GUARD 2: diff must be EXACTLY .runtime-version, with exactly one
    # added line and one removed line (the old → new version swap).
    runtime_file = None
    other_files = 0
    for entry in pr_files:
        if not isinstance(entry, dict):
            # FAIL-CLOSED: an unparseable file entry is a structural
            # anomaly; we can't prove the diff is single-file. Default
            # to non-exempt.
            return False, "pr_files contains non-dict entry (unverifiable)"
        filename = entry.get("filename", "")
        if filename == RUNTIME_VERSION_FILE:
            if runtime_file is not None:
                # The same file appears twice in the diff listing
                # (shouldn't happen for a real PR but be defensive).
                return False, f"{RUNTIME_VERSION_FILE} appears multiple times in diff"
            runtime_file = entry
        else:
            other_files += 1
    if runtime_file is None:
        return False, f"diff does not touch {RUNTIME_VERSION_FILE}"
    if other_files > 0:
        return False, f"diff touches {other_files} other file(s) besides {RUNTIME_VERSION_FILE}"

    patch = runtime_file.get("patch", "")
    if not isinstance(patch, str) or not patch:
        return False, f"{RUNTIME_VERSION_FILE} diff has no patch text (unverifiable)"

    added, removed, added_value, removed_value = _runtime_version_added_removed_lines(patch)
    if added != 1 or removed != 1:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} diff has {added} added + {removed} removed "
            f"non-header lines (must be exactly 1 each)",
        )
    if not added_value or not removed_value:
        return False, f"{RUNTIME_VERSION_FILE} diff added/removed value is empty"

    # Parse the .runtime-version line into runtime name + version. The
    # format is `<runtime-name>@<version>` (e.g. `claude-code@v1.2.3`).
    # Both pieces are needed: the runtime name for the allowlist, the
    # version for the ==SSOT check and the semver-major check. If the
    # format is not parseable, fail-closed: we cannot determine which
    # runtime the bump is for, so the exemption cannot be safely
    # granted.
    if "@" not in added_value:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} value {added_value!r} is not in "
            f"'<runtime>@<version>' format (cannot determine runtime name "
            f"for allowlist + ==SSOT checks)",
        )
    runtime_name, _, version_part = added_value.partition("@")
    if not runtime_name or not version_part:
        return (
            False,
            f"{RUNTIME_VERSION_FILE} value {added_value!r} has empty "
            f"runtime or version after partition",
        )

    # GUARD 3: no active RC in progress. The caller computes this (it
    # needs to inspect PR list for an active `release-candidate` PR);
    # we trust the boolean, but a missing/None is treated as
    # unverifiable → fail-closed.
    if rc_active is True:
        return False, "active release-candidate PR in progress"

    # CONDITION 1: ==SSOT. The VERSION part of the .runtime-version
    # line must equal the latest runtime-v release tag (NOT the full
    # `<runtime>@<version>` string — the tag is just the version,
    # e.g. `v1.2.3`). None means the caller couldn't determine the
    # latest tag (network error, missing repo, etc.) → fail-closed.
    if not isinstance(latest_runtime_v_tag, str) or not latest_runtime_v_tag:
        return False, "latest runtime-v release tag is unverifiable (==SSOT)"
    if version_part != latest_runtime_v_tag:
        return (
            False,
            f"==SSOT mismatch: .runtime-version version={version_part!r} "
            f"!= latest runtime-v tag={latest_runtime_v_tag!r}",
        )

    # CONDITION 2a: GATE-ADEQUACY (CI side). At least one runtime-provision/
    # compat smoke context (a job that provisions + runs a workspace on the
    # new runtime — NOT static build/lint) must be REQUIRED, so that "all
    # required green" (GUARD 4) actually proves the bump safe.
    #
    # Under a wildcard BP set ("*"), the BP literals name no contexts, so the
    # adequacy check must consult the contexts ACTUALLY posted on the PR head:
    # under "*" every posted context is required-to-be-green, so a smoke
    # context that ran (is posted) is genuinely a required gate. A smoke job
    # that never ran is simply absent from the posted set → adequacy fails →
    # fall back to the 2-genuine path (fail-closed). Without this, ["*"] can
    # NEVER satisfy adequacy (`'runtime-provision-smoke' in '*'` is False), so
    # the exemption is dead and every bump PR waits forever for human approvals
    # (core#4363 made this reachable; before it the queue never got here).
    if "*" in required_contexts:
        adequacy_contexts = list(posted_contexts or [])
        adequacy_src = "posted contexts (wildcard BP)"
    else:
        adequacy_contexts = list(required_contexts)
        adequacy_src = "required_contexts"
    if not adequacy_contexts:
        return (
            False,
            f"no CI gate to verify adequacy against ({adequacy_src} is empty)",
        )
    adequacy_lower = [c.lower() for c in adequacy_contexts]
    has_runtime_smoke = any(
        any(smoke in ctx for smoke in RUNTIME_PROVISION_SMOKE_CONTEXTS)
        for ctx in adequacy_lower
    )
    if not has_runtime_smoke:
        return (
            False,
            f"{adequacy_src} has no runtime-provision/compat smoke "
            f"(looked for any of: {sorted(RUNTIME_PROVISION_SMOKE_CONTEXTS)})",
        )

    # CONDITION 2b: GATE-ADEQUACY (template side). The runtime name
    # (parsed above from the .runtime-version header) must be on the
    # allowlist.
    if runtime_name not in RUNTIME_BUMP_EXEMPT_TEMPLATES:
        return (
            False,
            f"runtime {runtime_name!r} not on RUNTIME_BUMP_EXEMPT_TEMPLATES "
            f"allowlist (currently: {sorted(RUNTIME_BUMP_EXEMPT_TEMPLATES)})",
        )

    # CONDITION 3: EXCLUDE-BREAKING. Semver-MAJOR (the major part of
    # the version is > 0) → not eligible. A leading 'v' is tolerated
    # (`v1.2.3` → major=1). Unparseable versions → fail-closed.
    clean_version = version_part.lstrip("v")
    try:
        major_str, _, _ = clean_version.split(".", 2)
        major_int = int(major_str)
    except (ValueError, IndexError):
        return (
            False,
            f"version {version_part!r} is not parseable as semver "
            f"(major.minor.patch)",
        )
    if major_int > 0:
        return (
            False,
            f"semver-MAJOR bump ({version_part}): major={major_int} > 0 → not eligible",
        )

    # CONDITION 3 (cont): breaking label check. The bump-bot is not
    # expected to set these, but a human might have added one
    # mid-flight. Empty pr_labels → trivially passes.
    pr_labels = label_names(pr)
    breaking_present = pr_labels & BREAKING_LABELS
    if breaking_present:
        return (
            False,
            f"PR has breaking label(s): {sorted(breaking_present)}",
        )

    return True, (
        f"runtime-bump exempt: bump-bot single-line .runtime-version "
        f"{removed_value!r}→{added_value!r}; runtime={runtime_name!r}; "
        f"==SSOT; smoke context present; allowlist hit; semver-minor"
    )


def _parse_bump_pr_title(title: str) -> tuple[str, str] | None:
    """Parse a bump-bot PR title into (runtime_name, version).

    Returns None if the title doesn't match a recognized bump-bot
    format. The caller treats a None return as "singleton bucket, no
    dedup possible" (fail-safe — a non-matching title is never
    spuriously grouped with anything else).

    Recognized formats (per the 9c2e9c88 spec):
      - "runtime:<runtime>-<version>"  (no leading 'v' on version)
      - "<runtime> bump to <version>"  (version may or may not have leading 'v')
      - "runtime: <runtime> bump to <version>"  (hybrid: prefix + verb form)

    Examples:
      "runtime:claude-code-v1.2.3"          -> ("claude-code", "v1.2.3")
      "claude-code bump to v1.2.3"         -> ("claude-code", "v1.2.3")
      "runtime: claude-code bump to v1.2.3"-> ("claude-code", "v1.2.3")
      "kimi-cli bump to v0.5.0"            -> ("kimi-cli", "v0.5.0")
      "fix typo in docs"                    -> None
    """
    if not isinstance(title, str) or not title:
        return None
    # The "runtime:" prefix is optional — strip it if present so we can
    # use a single regex for the two body forms.
    body = title[len("runtime:"):].lstrip() if title.startswith("runtime:") else title
    # Format 1: "<runtime>-<version>" — runtime name is the substring
    # before the last "-v<MAJOR>.<MINOR>.<PATCH>" suffix.
    m = re.search(r"-v\d+\.\d+\.\d+\s*$", body)
    if m:
        runtime_name = body[: m.start()].strip()
        version = body[m.start() + 1 :].strip()
        if runtime_name and version:
            return (runtime_name, version)
    # Format 2: "<runtime> bump to <version>"
    m = re.match(
        r"^(?P<runtime>\S+)\s+bump to\s+(?P<version>v?\d+\.\d+\.\d+)\s*$",
        body,
    )
    if m:
        runtime_name = m.group("runtime").strip()
        version = m.group("version").strip()
        # Normalize version to leading "v" so the dedup key matches across
        # formats (e.g. "1.2.3" from format 1 == "v1.2.3" from format 2).
        if not version.startswith("v"):
            version = "v" + version
        if runtime_name and version:
            return (runtime_name, version)
    return None


def dup_close_superseded_bump_prs(
    prs: list[dict],
    *,
    close_fn: "callable",
    comment_fn: "callable",
    dry_run: bool = False,
) -> list[int]:
    """Close all but the newest open auto-bump PR per (runtime_name, version)
    tuple. Returns the list of PR numbers that were closed.

    Rationale: when the bump-bot republishes a bump (e.g. CI was red on the
    first push, so the bot repushed with the same version), or when a new
    version lands while the old one is still open, multiple bump PRs can
    race for the same (runtime, version) slot. The exemption would auto-merge
    the OLDEST one first (FIFO) — but the OLDEST one has stale CI, and
    merging it would skip the new smoke run. We close the older ones (and
    post a comment) so the auto-merge path only ever sees the freshest.

    "Newest" = latest `updated_at` then highest `number` (both stable Gitea
    fields). PRs not authored by the bump-bot, or PRs whose diff is not a
    clean single-line .runtime-version bump, are ignored (not eligible for
    closure via this path — they may be hand-edits).

    `close_fn(pr_number, *, dry_run)` and `comment_fn(pr_number, body, *, dry_run)`
    are injected so the function is unit-testable without a live Gitea.
    """
    # Bucket PRs by (runtime, version). Skip PRs that aren't
    # single-line .runtime-version bumps authored by bump-bot.
    #
    # Title-based parsing: bump-bot PR titles follow one of two stable
    # formats per the 9c2e9c88 spec:
    #   - "runtime:<runtime>-<version>"  (e.g. "runtime:claude-code-v1.2.3")
    #   - "<runtime> bump to <version>"  (e.g. "claude-code bump to v1.2.3")
    # We parse the title to extract (runtime, version) explicitly so the
    # bucket key matches the documented dedup semantic ("newest per
    # (runtime, version)"). A title that doesn't match either format is
    # treated as its own single-PR bucket — the per-PR close guard
    # (`if len(group) <= 1: continue`) makes that a no-op. This is
    # fail-safe: a non-standard title is never spuriously closed.
    buckets: dict[tuple[str, str], list[dict]] = {}
    for pr in prs:
        if not isinstance(pr, dict):
            continue
        author = (pr.get("user") or {}).get("login", "")
        if author != RUNTIME_BUMP_BOT_USER:
            continue
        number = int(pr.get("number", 0))
        if not number:
            continue
        runtime_v = _parse_bump_pr_title(pr.get("title", ""))
        # Group by (runtime, version); non-matching titles get a unique
        # sentinel key so they end up in a singleton bucket (no-op).
        bucket_key = runtime_v if runtime_v is not None else (str(number), "")
        buckets.setdefault(bucket_key, []).append(pr)

    closed: list[int] = []
    for _key, group in buckets.items():
        if len(group) <= 1:
            continue
        # Sort newest-first by (updated_at desc, number desc).
        group_sorted = sorted(
            group,
            key=lambda p: (
                p.get("updated_at", ""),
                int(p.get("number", 0)),
            ),
            reverse=True,
        )
        keep = group_sorted[0]
        for stale in group_sorted[1:]:
            stale_number = int(stale.get("number", 0))
            keep_number = int(keep.get("number", 0))
            body = (
                f"Closing as superseded by #{keep_number} "
                f"(newer bump-bot PR for the same runtime-version slot; "
                f"the auto-merge exemption auto-merged the freshest to "
                f"avoid merging stale CI)."
            )
            try:
                comment_fn(stale_number, body, dry_run=dry_run)
            except ApiError:
                # Comment is best-effort; the close is what matters.
                pass
            try:
                close_fn(stale_number, dry_run=dry_run)
            except ApiError as exc:
                # Don't crash the tick on a single close failure; log
                # and continue. The other dupes in the same group
                # still need closing.
                sys.stderr.write(
                    f"::warning::dup-close failed for PR #{stale_number}: {exc}\n"
                )
                continue
            closed.append(stale_number)
    return closed


def choose_next_queued_issue(
    issues: list[dict],
    *,
    queue_label: str,
    hold_label: str = "",
) -> dict | None:
    candidates = []
    for issue in issues:
        labels = label_names(issue)
        if queue_label not in labels:
            continue
        if hold_label and hold_label in labels:
            continue
        if "pull_request" not in issue:
            continue
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates[0] if candidates else None


def _issue_is_draft(issue: dict) -> bool:
    """True if the issue/PR is a draft.

    The /issues listing exposes draft state under the `pull_request` sub-object
    (`{"draft": true}`); some Gitea versions also surface a top-level `draft`.
    Either is honoured. Drafts are never auto-considered for merging.
    """
    pr = issue.get("pull_request")
    if isinstance(pr, dict) and pr.get("draft") is True:
        return True
    return issue.get("draft") is True


def choose_candidate_issues(
    issues: list[dict],
    *,
    queue_label: str,
    opt_out_labels: set[str],
    auto_discover: bool,
) -> list[dict]:
    """All open PRs eligible for a merge attempt this tick, oldest-first.

    This is the auto-discovery selector. It does NOT change the merge bar — it
    only changes WHICH PRs are considered:

      - auto_discover=True (default): every open same-repo PR is a candidate,
        EXCEPT those carrying an opt-out label or marked draft. The QUEUE_LABEL
        is optional metadata, not a gate, so a ready PR reaches the queue with no
        human/agent labeling (the write:issue gap is removed).
      - auto_discover=False: legacy opt-IN — only PRs carrying queue_label are
        candidates (still skipping opt-out labels and drafts).

    Opt-out is the safety escape hatch: any opt_out_labels member present skips
    the PR entirely (never considered, never merged). Ordering is oldest-first
    (created_at, then number) to preserve the serialized FIFO ordering.

    Returns the FULL ordered list (not just the head) so process_once can SCAN
    THROUGH non-ready candidates instead of locking on the oldest. A non-ready
    auto-discovered PR (e.g. one with REQUEST_CHANGES or mergeable=false, which
    can never become ready without human action) must NOT head-of-line block the
    newer ready PRs behind it — the readiness check happens per-candidate in
    process_once, and a `wait` candidate is skipped to the next one.
    """
    candidates = []
    for issue in issues:
        if "pull_request" not in issue:
            continue
        labels = label_names(issue)
        if opt_out_labels & labels:
            continue  # opt-out: human kept this PR out of autonomous merging
        if _issue_is_draft(issue):
            continue  # drafts are never auto-merged
        if not auto_discover and queue_label not in labels:
            continue  # legacy opt-IN: require the queue label
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates


def choose_next_candidate_issue(
    issues: list[dict],
    *,
    queue_label: str,
    opt_out_labels: set[str],
    auto_discover: bool,
) -> dict | None:
    """The oldest eligible candidate, or None. Thin head-of-list wrapper around
    choose_candidate_issues; retained for callers/tests that only want the head.
    process_once uses the full list (choose_candidate_issues) so it can scan past
    non-ready PRs rather than HOL-block on the oldest."""
    candidates = choose_candidate_issues(
        issues,
        queue_label=queue_label,
        opt_out_labels=opt_out_labels,
        auto_discover=auto_discover,
    )
    return candidates[0] if candidates else None


def pr_contains_base_sha(commits: list[dict], base_sha: str) -> bool:
    for commit in commits:
        sha = commit.get("sha") or commit.get("id")
        if sha == base_sha:
            return True
    return False


def pr_has_current_base(pr: dict, commits: list[dict], main_sha: str) -> bool:
    if pr.get("merge_base") == main_sha:
        return True
    return pr_contains_base_sha(commits, main_sha)


def _context_base_name(context: str) -> str:
    """Strip the trailing ` (pull_request*)` / ` (push)` event suffix from a
    Gitea status context, returning the stable base name.

    `"CI / Platform (Go) (pull_request)"` -> `"CI / Platform (Go)"`.
    Note: the inner `(Go)` is preserved — only the FINAL parenthesized event
    token is removed (it always names a Gitea webhook event).
    """
    stripped = context.strip()
    if stripped.endswith(")"):
        open_idx = stripped.rfind(" (")
        if open_idx != -1:
            inner = stripped[open_idx + 2 : -1]
            if inner.startswith(("pull_request", "push")):
                return stripped[:open_idx].strip()
    return stripped


def _is_aggregator_sentinel(base_context: str) -> bool:
    """True for a `<workflow> / all-required` aggregator sentinel (job key ==
    `all-required`).

    The job key (the segment after the final ` / `) is compared
    case-INSENSITIVELY so a repo whose workflow is named `ci` (molecule-
    controlplane) is matched identically to one named `CI` (molecule-core).
    The workflow-name segment is preserved verbatim in the returned prefix, so
    the emitted-context match downstream stays exact.
    """
    parts = base_context.rsplit(" / ", 1)
    if len(parts) != 2:
        return False
    workflow, job = parts
    return bool(workflow.strip()) and job.strip().lower() == "all-required"


def critical_context_prefixes(
    enforced_file_contexts: list[str] | None,
) -> list[str]:
    """The per-repo CRITICAL fail-closed context set (zero-drift).

    Priority:
      1. An explicit `CRITICAL_REQUIRED_CONTEXT_PREFIXES` env override — a repo
         may still pin critical contexts by name. Used verbatim when non-empty.
      2. Otherwise DERIVED from this repo's own checked-in SSOT
         (`.gitea/required-contexts.txt` → ``enforced_file_contexts``): every
         ENFORCED `<workflow> / all-required` aggregator sentinel.

    This replaces the former hardcoded, molecule-core-specific default
    (`CI / Platform (Go), CI / all-required`) which phantom-blocked every PR on
    any repo whose emitted context names differ (molecule-controlplane emits
    LOWERCASE `ci / all-required` and has no `Platform (Go)` job). The
    aggregator sentinel is (by lint-required-no-paths + all-required-check)
    guaranteed non-path-filtered and `needs:` every required job, so requiring
    IT green — sourced from the SSOT, correctly-cased per repo — is the correct
    fail-closed backstop for ANY repo.

    Returns [] only when neither source yields a context (a genuine
    misconfiguration — no override AND an SSOT with no all-required sentinel);
    the caller (critical_contexts_block) fail-closes on empty.
    """
    if CRITICAL_REQUIRED_CONTEXT_PREFIXES:
        return list(CRITICAL_REQUIRED_CONTEXT_PREFIXES)
    derived: list[str] = []
    for ctx in enforced_file_contexts or []:
        base = _context_base_name(ctx)
        if _is_aggregator_sentinel(base) and base not in derived:
            derived.append(base)
    return derived


def critical_contexts_block(
    latest: dict[str, dict], critical_prefixes: list[str]
) -> list[str]:
    """FAIL-CLOSED guard for the per-repo CRITICAL context set.

    ``critical_prefixes`` is the repo-correct critical set from
    :func:`critical_context_prefixes` (SSOT-derived aggregator sentinel(s), or an
    explicit env override). Returns a list of human-readable block reasons; an
    EMPTY list means every critical context is provably green. A NON-empty list
    means the merge MUST be refused (evaluate_merge_readiness calls this before
    any merge decision).

    Fail-closed semantics (RCA core#1676):
      * A critical context that is failure/error/skipped/missing → BLOCK.
        `skipped` and `missing` are FATAL: the gate did not actually run, so
        we cannot prove it green. (#1676's `all-required = skipped` is the
        canonical case — a skipped aggregator sentinel means nothing gated
        the required jobs.)
      * Only `success` clears a critical context.

    Each critical PREFIX must be matched by at least one observed context whose
    base name equals it AND that context must be `success`. If the prefix is
    not present at all in the statuses, that is ALSO a block (missing == not
    proven green).
    """
    # Fail-closed empty-guard (core#4363 follow-up). This step-0 backstop is
    # the last line of defense against a vacuous merge. If the per-repo critical
    # set is empty (no env override AND the repo's SSOT carries no
    # `<workflow> / all-required` aggregator sentinel), the loop below would
    # iterate nothing and return [] — a fail-OPEN that lets a PR with no posted
    # CI sail through step 0. `push_required_contexts()` raises on its empty
    # parse for exactly this reason; the critical set must be just as
    # unforgiving. BLOCK rather than pass vacuously.
    if not critical_prefixes:
        return [
            "critical context set is empty — no CRITICAL_REQUIRED_CONTEXT_"
            "PREFIXES override AND the repo's .gitea/required-contexts.txt SSOT "
            "carries no '<workflow> / all-required' aggregator sentinel to derive "
            "from. The step-0 critical backstop would pass vacuously for ANY PR "
            "(incl. one with no posted CI). Merge gate HOLDS (fail-closed)."
        ]

    # Map base-name -> set of observed states for that base name.
    observed: dict[str, list[str]] = {}
    for context, status in latest.items():
        if not isinstance(context, str):
            continue
        base = _context_base_name(context)
        observed.setdefault(base, []).append(status_state(status))

    reasons: list[str] = []
    for prefix in critical_prefixes:
        states = observed.get(prefix)
        if not states:
            reasons.append(f"{prefix}=missing (critical context not reported — cannot prove green)")
            continue
        # Every occurrence of the critical context must be success. A single
        # failure/error/skipped occurrence blocks (newest-wins was already
        # applied by latest_statuses_by_context, but a critical context that
        # appears under multiple event suffixes must ALL be green).
        bad = [s or "missing" for s in states if s != "success"]
        if bad:
            reasons.append(f"{prefix}={','.join(sorted(set(bad)))} (critical context not green)")
    return reasons


def evaluate_merge_readiness(
    *,
    main_status: dict,
    pr_status: dict,
    required_contexts: list[str],
    required_approvals: int,
    approvers: set[str],
    request_changes: list[str],
    pr_has_current_base: bool,
    mergeable: bool | None,
    pr_labels: set[str] | None = None,
    runtime_bump_exempt: bool = False,
    enforced_file_contexts: list[str] | None = None,
    block_on_outdated_branch: bool = False,
) -> MergeDecision:
    # 0) CRITICAL fail-closed guard (RCA core#1676). Before ANY other gate, the
    #    PR head's per-repo critical context — the repo's own
    #    `<workflow> / all-required` aggregator sentinel, SSOT-derived from
    #    enforced_file_contexts (see critical_context_prefixes) — MUST be green.
    #    This is checked UNCONDITIONALLY, independent of branch protection's
    #    status_check_contexts. #1676 merged red because the aggregator fell out
    #    of the BP-required set and the former force path bypassed it. There is
    #    no exemption from this gate — runtime-bump exemption only bypasses the
    #    APPROVALS bar, never a red required CI status.
    critical_prefixes = critical_context_prefixes(enforced_file_contexts)
    pr_latest_critical = latest_statuses_by_context(pr_status.get("statuses") or [])
    critical_block = critical_contexts_block(pr_latest_critical, critical_prefixes)
    if critical_block:
        return MergeDecision(
            False, "wait",
            "CRITICAL required context(s) not green (fail-closed): "
            + "; ".join(critical_block),
        )

    # 1) Main's push-required contexts must be green. Combined state can be
    #    "failure" due to non-blocking jobs (continue-on-error: true) that do
    #    not gate merges, so check the explicit required set, not combined.
    #
    #    This main-green gate is ALSO the serialized backstop that makes the
    #    direct-merge (no update) path safe (issue #2358): after a direct merge
    #    of a behind-main PR, main re-runs its push CI; if a semantic main-break
    #    slips through (PR green standalone but broken when combined with newer
    #    main), main's required contexts go red and this gate PAUSES the queue —
    #    no further merge piles onto an unverified/red main until it is green.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        return MergeDecision(False, "pause", "main required contexts not green: " + ", ".join(main_bad))

    # 2) No open official REQUEST_CHANGES on the current head.
    if request_changes:
        return MergeDecision(
            False, "wait",
            "open REQUEST_CHANGES on current head from: " + ", ".join(sorted(request_changes)),
        )

    # 3) Enough distinct genuine official approvals on the current head.
    #    The runtime-bump exemption (RFC internal#131) bypasses this check
    #    for single-line .runtime-version bumps authored by bump-bot when
    #    the runtime-smoke CI is present in the required_contexts. The
    #    exemption does NOT bypass any CI gate (step 4 still requires
    #    every required context to be green) and does NOT bypass main's
    #    push-required gate (step 1). It ONLY bypasses the human-approvals
    #    bar, which a bot cannot satisfy by construction. Required
    #    approvals is forced to 0 for the check below when the exemption
    #    holds.
    #
    #    DEFENSIVE FLOOR (internal#3210 FIX-1, defence-in-depth). On the
    #    NON-exempt path the bar can never legitimately be below 1 genuine
    #    approval — a single upstream `required_approvals == 0` (degraded BP,
    #    a caller that bypassed parse_branch_protection, a future refactor)
    #    must NOT be able to zero the genuine-approval gate. parse_branch_
    #    protection already clamps the derived value up to the default floor;
    #    this is the second, independent guard at the consumption site so the
    #    two would both have to fail to open the gate. The runtime-bump
    #    exemption path is UNTOUCHED — it legitimately zeroes the HUMAN bar
    #    (a bot cannot self-approve), and the floor must not break it.
    if runtime_bump_exempt:
        effective_required_approvals = 0
    else:
        effective_required_approvals = max(required_approvals, 1)
    if len(approvers) < effective_required_approvals:
        return MergeDecision(
            False, "wait",
            f"insufficient genuine approvals on current head: have "
            f"{len(approvers)} ({', '.join(sorted(approvers)) or 'none'}), "
            f"need {effective_required_approvals}",
        )

    # 4) Every REQUIRED status context must be green (the branch-protection
    #    status_check_contexts set; the old uniform SOP governance checks were
    #    removed 2026-07-14). Under the production wildcard BP set ("*"),
    #    "required" means EVERY POSTED context — so there is NO "non-required
    #    red" exemption here: a red E2E Chat / Staging SaaS / ci-arm64-advisory /
    #    continue-on-error job all block, exactly like any other posted red
    #    (core#4363). Do NOT re-add an advisory-skip branch to
    #    required_contexts_green — that recreates the PR#3181 fail-open class.
    #    A genuinely advisory
    #    check must carry NO commit status (or be excluded from BP), not be
    #    skipped here.
    latest = latest_statuses_by_context(pr_status.get("statuses") or [])
    ok, missing_or_bad = required_contexts_green(latest, required_contexts)
    if not ok:
        return MergeDecision(False, "wait", "required contexts not green: " + ", ".join(missing_or_bad))

    # 4b) Every ENFORCED entry of `.gitea/required-contexts.txt` must be green
    #     (internal#3181). This is the SSOT-as-ENFORCED fix: the file is the
    #     documented "required to merge" set, but historically the gate read
    #     ONLY branch protection, so a file entry absent from BP was treated
    #     as a non-required advisory red and the former force path bypassed it —
    #     exactly how PR#3181 merged with `E2E Staging SaaS (full lifecycle) /
    #     E2E Staging Concierge Creates Workspace` RED.
    #
    #     Matching is event-suffix-INSENSITIVE (file entries are bare; live
    #     statuses carry a `(pull_request)` etc. suffix). Entries parked under
    #     a `# pending-#NNNN` marker in the file are NOT in this list (the
    #     loader excludes the tail) and are therefore NOT enforced — the
    #     sequencing escape hatch that keeps a known-red, tracked check from
    #     freezing the whole queue. Fail-closed: a red/missing enforced context
    #     returns `wait`. This runs AFTER step 4 so a BP-required red is reported first.
    # `enforced_file_contexts` here is ALWAYS a real (possibly empty) list:
    # a read FAILURE was converted to an EnforcedContextsUnavailable hold at
    # the call site BEFORE this function runs (RC 13618), so reaching here
    # with [] / None means "the SSOT was read and has nothing extra to
    # enforce", NOT "couldn't read it". Do not reintroduce a None→[] error
    # sentinel through this path.
    enforced = enforced_file_contexts or []
    if enforced:
        efok, efbad = enforced_file_contexts_green(latest, enforced)
        if not efok:
            return MergeDecision(
                False, "wait",
                "enforced required-contexts.txt entries not green: "
                + ", ".join(efbad),
            )

    # 5) Honor Gitea's live up-to-date branch-protection policy. Core currently
    #    sets block_on_outdated_branch=true, so a behind-main head must be
    #    updated and then receive fresh CI/review before any merge attempt.
    #    Ignoring this field sends an otherwise-ready PR to an inevitable HTTP
    #    405 and can wedge the queue when its token cannot write hold labels.
    if block_on_outdated_branch and not pr_has_current_base:
        return MergeDecision(
            False,
            "update",
            "branch protection requires an up-to-date head; updating with current main",
        )

    # 5b) DIRECT-MERGE when conflict-free only when branch protection permits
    #     behind-main heads (issue #2358 — throughput fix).
    #    If Gitea reports the PR conflict-free (mergeable is True), MERGE IT
    #    DIRECTLY even if its head does not yet contain current main. Branch
    #    protection may permit that path. We do not call /update in that policy
    #    mode because it triggers Gitea dismiss_stale_approvals and forces a
    #    full re-review every tick — the rebase-churn bottleneck that collapsed
    #    throughput to ~0/hr.
    #
    #    The merge bar is UNCHANGED: we only reach here with main green +
    #    >= required genuine approvals on the current head + no open
    #    REQUEST_CHANGES + every BP-required context green. The trade-off is
    #    that the PR's CI ran on a possibly-behind base, so a SEMANTIC main-break
    #    is caught by POST-merge main CI (step 1's pause backstop) rather than
    #    pre-merge.
    #
    if mergeable is True:
        return MergeDecision(True, "merge", "ready")

    # 6) NOT (yet) mergeable. TRI-STATE, fail-closed — never merge on an unknown.
    #    We MUST distinguish "still computing" (None/missing) from a "definitive
    #    conflict" (False); collapsing them would route a behind-main but
    #    STILL-COMPUTING PR into the /update path, whose dismiss_stale_approvals
    #    is the rebase-churn this change eliminates.
    #
    #    mergeable is None  → Gitea has NOT finished computing conflict state.
    #    WAIT: do nothing this tick — never /update (would dismiss genuine
    #    approvals during the compute window → churn), never merge. Re-check next
    #    tick once Gitea reports a decisive True/False.
    if mergeable is None:
        return MergeDecision(
            False, "wait",
            "PR mergeability is still being computed (mergeable=None) — waiting",
        )

    # mergeable is False → DEFINITIVE not-mergeable. If the head also does not
    #    contain current main, try the /update path to refresh the branch (this
    #    may resolve a behind-main non-conflict; a real conflict returns HTTP 409
    #    and process_once HOLDs the PR per #2352). If the head already contains
    #    current main yet Gitea still reports not-mergeable, there is nothing the
    #    queue can do (genuine conflict against current main) — WAIT.
    if not pr_has_current_base:
        return MergeDecision(False, "update", "PR not mergeable and head does not contain current main")
    return MergeDecision(False, "wait", "PR is not mergeable (conflicts)")


def get_branch_head(branch: str) -> str:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/branches/{branch}")
    commit = body.get("commit") if isinstance(body, dict) else None
    sha = commit.get("id") if isinstance(commit, dict) else None
    if not isinstance(sha, str) or len(sha) < 7:
        raise ApiError(f"branch {branch} response missing commit id")
    return sha


def _snapshot_status_for_sha(sha: str) -> dict | None:
    """Return a Gitea-shaped combined-status dict from the conductor snapshot
    if the SHA matches an open PR head, else None."""
    snapshot = load_conductor_snapshot()
    if snapshot is None:
        return None
    for pr in (snapshot.get("prs") or []):
        if pr.get("head_sha") == sha:
            statuses = pr.get("statuses") or []
            return {
                "state": pr.get("combined_state", "unknown"),
                "statuses": [
                    {"context": s.get("context"), "status": s.get("status")}
                    for s in statuses
                    if isinstance(s, dict)
                ],
            }
    return None


def get_combined_status(sha: str, *, prefer_live: bool = False) -> dict:
    """Combined status + all individual statuses for `sha`.

    Uses the conductor snapshot when available (same tick, same observed
    state as the merge-queue pass), otherwise self-fetches via API.

    `prefer_live=True` BYPASSES the snapshot entirely and always self-fetches
    the LIVE state from the Gitea status API. The cheap enumeration/scan pass
    (the decision) reads the snapshot for throughput, but the FINAL pre-merge
    re-check (internal#3210 — process_once, immediately before merge_pull)
    needs ground truth: a required/enforced/critical context that flipped to
    RED *within* the snapshot's freshness window — AFTER the snapshot was
    captured but before this tick acts — is still GREEN in the snapshot, so a
    snapshot read here would re-confirm a stale green and merge a now-red PR.
    A live read closes that within-window staleness gap to ~0.
    """
    if not prefer_live:
        snapshot_status = _snapshot_status_for_sha(sha)
        if snapshot_status is not None:
            return snapshot_status

    _, combined = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(combined, dict):
        raise ApiError(f"status for {sha} response not object")
    combined_statuses: list[dict] = combined.get("statuses") or []
    all_statuses = api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/commits/{sha}/statuses",
    )
    # Build latest per context: process combined first because it is Gitea's
    # authoritative latest view, then fill its capped/missing contexts from the
    # complete history.  Do not trust the paginated history's response order:
    # concurrent status writes can move page boundaries and surface an older
    # pending row before its newer success row.  Status ids are monotonic, so
    # sorting by id makes enrichment deterministic across page-order races.
    latest: dict[str, dict] = {}
    for status in sorted(combined_statuses, key=status_numeric_id, reverse=True):
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    for status in sorted(all_statuses, key=status_numeric_id, reverse=True):
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    combined["statuses"] = list(latest.values())
    return combined


def _snapshot_pr_to_issue(pr_entry: dict) -> dict:
    """Normalise a conductor-snapshot PR entry into the shape the queue
    expects from /issues (number, title, labels, pull_request sub-dict)."""
    return {
        "number": pr_entry.get("number"),
        "title": pr_entry.get("title"),
        "labels": [{"name": n} for n in (pr_entry.get("labels") or [])],
        "pull_request": {"draft": False},
        "created_at": "",
    }


def list_queued_issues() -> list[dict]:
    snapshot = load_conductor_snapshot()
    if snapshot is not None:
        prs = snapshot.get("prs") or []
        return [
            _snapshot_pr_to_issue(p)
            for p in prs
            if QUEUE_LABEL in (p.get("labels") or [])
        ]
    return api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
            "label": QUEUE_LABEL,
        },
    )


def list_candidate_issues(*, auto_discover: bool) -> list[dict]:
    """Open PR issues eligible for consideration this tick.

    With auto_discover=True (default) this enumerates ALL open PRs (no label
    filter) so the queue is self-sustaining — a ready PR is considered without
    any human/agent first adding QUEUE_LABEL. With auto_discover=False it falls
    back to the legacy label-filtered listing (opt-IN). Opt-out filtering and
    draft-skipping happen in choose_next_candidate_issue, not here.
    """
    snapshot = load_conductor_snapshot()
    if snapshot is not None:
        prs = snapshot.get("prs") or []
        return [_snapshot_pr_to_issue(p) for p in prs]
    if not auto_discover:
        return list_queued_issues()
    return api_paginated(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
        },
    )


def get_pull(pr_number: int) -> dict:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}")
    if not isinstance(body, dict):
        raise ApiError(f"PR #{pr_number} response not object")
    return body


def get_pull_commits(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/commits")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} commits response not list")
    return body


def post_comment(pr_number: int, body: str, *, dry_run: bool) -> None:
    print(f"::notice::comment PR #{pr_number}: {body.splitlines()[0][:160]}")
    if dry_run:
        return
    api("POST", f"/repos/{OWNER}/{NAME}/issues/{pr_number}/comments", body={"body": body})


def post_branch_update_comment_best_effort(
    pr_number: int, body: str, *, dry_run: bool
) -> None:
    """Report a successful branch refresh without making it a queue gate.

    AUTO_SYNC_TOKEN intentionally has repository-write but not issue-write
    scope. Updating a branch is the state transition; the comment is only
    observability. A 403 here must therefore stay visible while allowing the
    successful queue tick to finish green.
    """
    try:
        post_comment(pr_number, body, dry_run=dry_run)
    except (ApiError, urllib.error.URLError, TimeoutError) as exc:
        sys.stderr.write(
            f"::warning::could not post branch-update comment to PR "
            f"#{pr_number}: {exc}; branch update remains successful\n"
        )


def update_pull(pr_number: int, *, dry_run: bool) -> None:
    print(f"::notice::updating PR #{pr_number} with base branch via style={UPDATE_STYLE}")
    if dry_run:
        return
    try:
        api(
            "POST",
            f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/update",
            query={"style": UPDATE_STYLE},
            expect_json=False,
        )
    except ApiError as exc:
        # Gitea returns HTTP 409 when the base cannot be merged into the PR
        # branch because of a real conflict. The queue cannot auto-resolve a
        # conflict, so re-raise as BranchUpdateConflictError; process_once HOLDs
        # the PR and advances (HOL guard) instead of retrying it forever.
        # Match the HTTP STATUS token ("-> HTTP 409") specifically, not a bare
        # "409" substring — the PR number or path can itself contain "409"
        # (e.g. /pulls/1409/update) and must not be misread as a conflict.
        if "-> HTTP 409" in str(exc):
            raise BranchUpdateConflictError(str(exc)) from exc
        raise  # re-raise other ApiErrors unchanged


def add_label_by_name(pr_number: int, label_name: str, *, dry_run: bool) -> None:
    """Apply an existing repo label (by name) to a PR/issue.

    Used to HOLD a wedged PR so the queue advances. Resolves the label id from
    the repo label set; if the label does not exist, raises ApiError (the
    caller decides whether that is fatal).
    """
    print(f"::notice::applying label '{label_name}' to PR #{pr_number}")
    if dry_run:
        return
    _, labels = api("GET", f"/repos/{OWNER}/{NAME}/labels", query={"limit": "100"})
    label_id = None
    if isinstance(labels, list):
        for label in labels:
            if isinstance(label, dict) and label.get("name") == label_name:
                label_id = label.get("id")
                break
    if label_id is None:
        raise ApiError(f"label '{label_name}' not found in repo {OWNER}/{NAME}")
    api(
        "POST",
        f"/repos/{OWNER}/{NAME}/issues/{pr_number}/labels",
        body={"labels": [label_id]},
    )


def close_pull(pr_number: int, *, dry_run: bool) -> None:
    """Close a PR (issues endpoint; PRs are issues in Gitea's data model).

    Used by dup_close_superseded_bump_prs() to retire older bump-bot
    PRs that have been superseded by a fresh bump. Network/HTTP
    errors propagate as ApiError so the caller can decide whether
    to log-and-continue or fail the tick.

    The body is intentionally empty (Gitea does not require a close
    reason; the explanatory comment is posted separately).
    """
    if dry_run:
        print(f"::notice::(dry-run) would close PR #{pr_number}")
        return
    api("PATCH", f"/repos/{OWNER}/{NAME}/issues/{pr_number}", body={"state": "closed"})


def hold_pr(pr_number: int, hold_note: str, *, dry_run: bool) -> None:
    """Apply HOLD_LABEL to a wedged PR so the queue advances past it.

    choose_next_queued_issue skips HOLD_LABEL-bearing PRs, so this is the HOL
    guard when issue writes are available. The comment is observability-only,
    but the label is the durable opt-out state. If the label cannot be written,
    raise after attempting the diagnostic comment so main() makes this queue
    run red. A merge refusal must never be reported as a successful queue tick
    when no durable hold was recorded (internal#1082).
    """
    label_exc: ApiError | None = None
    try:
        add_label_by_name(pr_number, HOLD_LABEL, dry_run=dry_run)
    except ApiError as exc:
        label_exc = exc
        sys.stderr.write(
            f"::error::could not apply HOLD_LABEL to PR #{pr_number}: {exc}\n"
        )
        hold_note += (
            f"\n\n(NOTE: could not apply the hold label automatically: "
            f"{exc}. Please add `{HOLD_LABEL}` manually.)"
        )
    try:
        post_comment(pr_number, hold_note, dry_run=dry_run)
    except ApiError as comment_exc:
        sys.stderr.write(
            f"::error::could not post hold comment to PR #{pr_number}: "
            f"{comment_exc}\n"
        )
    if label_exc is not None:
        raise ApiError(
            f"could not durably hold PR #{pr_number} with `{HOLD_LABEL}` "
            f"after a permanent queue refusal: {label_exc}"
        ) from label_exc


def reassert_queue_runner_status(head_sha: str, *, dry_run: bool) -> None:
    """Replace only non-success queue-runner statuses before protected merge.

    Under wildcard branch protection, Gitea itself still matches the Actions
    job's in-flight ``gitea-merge-queue / queue`` status even though the
    user-space readiness gate correctly excludes that gate-runner context.
    Once every real merge gate and the final live regression check have passed,
    publish success for the exact runner context so Gitea's ordinary protected
    endpoint observes the same decision.  Product/test contexts are never
    written here.  A failed or malformed status write raises and prevents the
    merge (core#4420; observed in queue run 519372).
    """
    live_status = get_combined_status(head_sha, prefer_live=True)
    latest = latest_statuses_by_context(live_status.get("statuses") or [])
    for context in sorted(latest):
        current = latest[context]
        if not _SELF_STATUS_RE.fullmatch(context):
            continue
        if status_state(current) == "success":
            continue

        print(
            f"::notice::reasserting queue-runner status '{context}' as success "
            "before the ordinary protected merge write"
        )
        if dry_run:
            continue

        body: dict[str, Any] = {
            "state": "success",
            "context": context,
            "description": "Merge queue gates passed; protected merge write in progress",
        }
        target_url = current.get("target_url")
        safe_target_re = re.compile(
            rf"\A/{re.escape(OWNER)}/{re.escape(NAME)}/actions/runs/"
            r"[0-9]+/jobs/[0-9]+\Z"
        )
        if isinstance(target_url, str) and safe_target_re.fullmatch(target_url):
            body["target_url"] = target_url
        _, confirmation = api(
            "POST",
            f"/repos/{OWNER}/{NAME}/statuses/{head_sha}",
            body=body,
        )
        if (
            not isinstance(confirmation, dict)
            or confirmation.get("context") != context
            or status_state(confirmation) != "success"
        ):
            raise ApiError(
                "queue-runner status write did not confirm success for "
                f"{context!r} on {head_sha}"
            )

        # The POST response only proves that Gitea accepted the new row. The
        # protected merge path reads the separately materialized combined
        # commit-status view, which can lag briefly. Confirm through that same
        # live read surface before attempting the ordinary merge endpoint.
        for attempt in range(1, SELF_STATUS_CONFIRM_ATTEMPTS + 1):
            visible = get_combined_status(head_sha, prefer_live=True)
            visible_latest = latest_statuses_by_context(
                visible.get("statuses") or []
            )
            if status_state(visible_latest.get(context, {})) == "success":
                break
            if attempt < SELF_STATUS_CONFIRM_ATTEMPTS:
                time.sleep(SELF_STATUS_CONFIRM_DELAY_SECONDS)
        else:
            raise ApiError(
                "queue-runner status write was accepted but not visible as "
                f"success for {context!r} on {head_sha} after "
                f"{SELF_STATUS_CONFIRM_ATTEMPTS} live reads"
            )


def merge_pull(
    pr_number: int,
    *,
    dry_run: bool,
    head_commit_id: str,
) -> None:
    payload: dict[str, Any] = {
        "do": "merge",
        "merge_title_field": f"Merge PR #{pr_number} via Gitea merge queue",
        "merge_message_field": (
            "Serialized merge by gitea-merge-queue after current-main, "
            "genuine approvals, and required CI checks were green."
        ),
        "head_commit_id": head_commit_id,
    }
    print(f"::notice::merging PR #{pr_number} through the normal protected endpoint")
    if dry_run:
        return
    # The final live product/required-context re-check is performed by the
    # caller immediately before this function. Reconcile only the queue's own
    # gate-runner status with that decision; failure here propagates and no
    # merge request is sent. This is a normal status write, never an admin or
    # branch-protection override (core#4420).
    merge_path = f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/merge"
    for attempt in range(1, MERGE_STATUS_RETRY_ATTEMPTS + 1):
        reassert_queue_runner_status(head_commit_id, dry_run=False)
        try:
            api("POST", merge_path, body=payload, expect_json=False)
            return
        except ApiError as exc:
            msg = str(exc)
            status_visibility_race = (
                re.search(r"-> HTTP 405(?:\D|$)", msg) is not None
                and "Not all required status checks successful" in msg
            )
            if status_visibility_race and attempt < MERGE_STATUS_RETRY_ATTEMPTS:
                print(
                    "::warning::protected merge has not observed the confirmed "
                    "queue-runner success yet; retrying the same ordinary "
                    f"endpoint ({attempt}/{MERGE_STATUS_RETRY_ATTEMPTS})"
                )
                time.sleep(MERGE_STATUS_RETRY_DELAY_SECONDS)
                continue
            # Re-raise permission-like errors so process_once can HOLD this PR.
            # 403 = no push access, 404 = repo/pr not found, 405 = not allowed.
            if re.search(r"-> HTTP (?:403|404|405)(?:\D|$)", msg):
                raise MergePermissionError(msg) from exc
            raise  # re-raise other ApiErrors unchanged


def live_premerge_status_regressions(
    head_sha: str,
    *,
    required_contexts: list[str],
    enforced_file_contexts: list[str],
) -> list[str]:
    """internal#3210 — final pre-merge re-check against LIVE status.

    The merge decision (`evaluate_merge_readiness`) may run its status gates
    against the conductor SNAPSHOT, which is trusted while it is within its
    freshness window. But a required/enforced/critical context can flip to RED
    *inside* that window — AFTER the snapshot was captured — and still read
    GREEN from the snapshot. `process_once` only re-checks that MAIN has not
    moved before the merge POST; it does NOT re-verify the candidate's own
    statuses live. This re-fetches the candidate head's combined status with
    the snapshot BYPASSED (`prefer_live=True`) and re-runs the SAME status
    gates the decision used:

      * critical_contexts_block (CRITICAL_REQUIRED_CONTEXT_PREFIXES),
      * required_contexts_green(live_latest, required_contexts),
      * enforced_file_contexts_green(live_latest, enforced) — only if the
        enforced set is non-empty (matches evaluate_merge_readiness step 4b).

    Returns a list of human-readable regression reasons (now red/missing where
    the decision saw green). An EMPTY list means LIVE state still satisfies
    every status gate and the merge may proceed. Any non-empty list means the
    candidate must be SKIPPED (treated as `wait`, never merged) this tick.

    This does NOT re-do the approvals / REQUEST_CHANGES / mergeable gates —
    those are not snapshot-sourced status flips and process_once already holds
    them via the decision. It re-runs ONLY the status-context gates, which are
    exactly the ones the snapshot can stale.
    """
    live_status = get_combined_status(head_sha, prefer_live=True)
    live_latest = latest_statuses_by_context(live_status.get("statuses") or [])

    regressions: list[str] = []
    # Same order evaluate_merge_readiness checks: critical first (force cannot
    # bypass), then BP+governance required, then the enforced-file SSOT set.
    critical_block = critical_contexts_block(
        live_latest, critical_context_prefixes(enforced_file_contexts)
    )
    if critical_block:
        regressions.extend(critical_block)
    ok, bad = required_contexts_green(live_latest, required_contexts)
    if not ok:
        regressions.extend(bad)
    if enforced_file_contexts:
        efok, efbad = enforced_file_contexts_green(live_latest, enforced_file_contexts)
        if not efok:
            regressions.extend(efbad)
    return regressions


def process_once(*, dry_run: bool = False) -> int:
    # Required status contexts come from BRANCH PROTECTION, not a hand-kept env
    # list. Fail-closed: if BP cannot be enumerated, FAIL the whole tick rather
    # than merge against an unverified required set or publish a false green.
    try:
        bp = get_branch_protection(WATCH_BRANCH)
    except BranchProtectionUnavailable as exc:
        sys.stderr.write(
            f"::error::queue failed closed: branch protection for {WATCH_BRANCH} "
            f"unavailable; no merge attempted: {exc}\n"
        )
        # The hold is safe only if it is also honest. Returning success here
        # made an approval-triggered consumer publish a green queue context even
        # though the conductor could not read policy and made no merge attempt
        # (internal#1084). A transient API failure may retry on the next event or
        # manual dispatch, but this run must remain non-success.
        return 1
    # Uniform gate: governance checks are ALWAYS required, even if branch
    # protection does not enumerate them. Deduplicate against BP list.
    contexts = list(dict.fromkeys(bp.required_contexts + GOVERNANCE_REQUIRED_CONTEXTS))
    required_approvals = bp.required_approvals
    # SSOT-as-ENFORCED (internal#3181): the documented required set in
    # `.gitea/required-contexts.txt` is ALSO merge-blocking, fail-closed and
    # event-suffix-insensitive, regardless of whether each entry is in BP.
    # Entries below a `# pending-#NNNN` marker are excluded (sequencing).
    # FAIL-CLOSED (RC 13618): if the SSOT file is missing/unreadable here,
    # load_enforced_file_contexts raises EnforcedContextsUnavailable (an
    # ApiError) BEFORE the candidate loop. We deliberately do NOT catch it:
    # it propagates to main()'s ApiError handler → rc 1 (no merge + operators
    # paged). A vanished merge-gate SSOT is an incident, not a transient to be
    # held silently. BranchProtectionUnavailable is equally non-success: even a
    # transient Gitea blip cannot produce an honest green queue context.
    enforced_file_contexts = load_enforced_file_contexts(ENFORCED_CONTEXTS_FILE)
    print(
        f"::notice::queue policy from branch protection: "
        f"required_approvals={required_approvals} "
        f"block_on_outdated_branch={bp.block_on_outdated_branch} "
        f"required_contexts={contexts or '[none]'} "
        f"enforced_file_contexts={enforced_file_contexts or '[none]'}"
    )

    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    # Check push-required contexts explicitly instead of combined state.
    # See evaluate_merge_readiness for rationale.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        print(f"::notice::queue paused: {WATCH_BRANCH}@{main_sha[:8]} required contexts not green: {', '.join(main_bad)}")
        return 0

    candidates = choose_candidate_issues(
        list_candidate_issues(auto_discover=AUTO_DISCOVER),
        queue_label=QUEUE_LABEL,
        opt_out_labels=OPT_OUT_LABELS,
        auto_discover=AUTO_DISCOVER,
    )
    if not candidates:
        print(
            "::notice::no merge candidates "
            f"(auto_discover={'on' if AUTO_DISCOVER else 'off'})"
        )
        return 0

    # Runtime-bump dup-close (RFC internal#131 PR-A). If multiple bump-bot
    # PRs are open for the same (runtime, version) slot (e.g. the bot
    # repushed after a red CI run), the OLDEST has stale CI. The exemption
    # would auto-merge the first one the FIFO scan reaches — but we want
    # the FRESHEST to win (its smoke run is the relevant one). Close all
    # but the freshest per group so the auto-merge path only ever sees
    # the fresh bump. Best-effort: a single close failure logs and
    # continues (does NOT abort the tick).
    try:
        closed_dups = dup_close_superseded_bump_prs(
            candidates,
            close_fn=close_pull,
            comment_fn=post_comment,
            dry_run=dry_run,
        )
        if closed_dups:
            print(
                f"::notice::dup-close retired {len(closed_dups)} superseded "
                f"bump PR(s): {closed_dups}"
            )
    except ApiError as exc:
        # Defensive: dup_close_superseded_bump_prs catches per-close
        # ApiError internally; an outer raise means something more
        # fundamental is wrong. Log and continue — dup-close is an
        # optimization, not a gate.
        sys.stderr.write(
            f"::warning::dup-close encountered unexpected error: {exc}\n"
        )

    # HOL fix: SCAN THROUGH the FIFO candidate list until a PR we can ACT on is
    # found, instead of locking on the oldest and waiting. A non-ready candidate
    # (decision.action == "wait": REQUEST_CHANGES, mergeable!=True, insufficient
    # genuine approvals, or red required CI) is SKIPPED — it must NOT head-of-line
    # block the newer ready PRs behind it. The merge bar is unchanged: a skipped
    # PR is never merged, and the first ACTIONABLE candidate (an "update" that
    # advances a stale branch, or a fully-ready "merge") terminates the scan.
    #
    # `update` is treated as actionable, not skippable: a PR whose head merely
    # lacks current main is in a legitimate in-progress state (updating it +
    # rerunning CI moves it toward ready), unlike a PR that can never become
    # ready without a human (RC / conflict), which is a `wait` and gets skipped.
    for issue in candidates:
        decision, ctx = _evaluate_candidate(
            issue,
            main_sha=main_sha,
            main_status=main_status,
            required_contexts=contexts,
            required_approvals=required_approvals,
            dry_run=dry_run,
            enforced_file_contexts=enforced_file_contexts,
            block_on_outdated_branch=bp.block_on_outdated_branch,
        )
        if decision is None:
            continue  # not merge-eligible (not-open / opted-out / fork / wrong base)
        pr_number = ctx["pr_number"]
        print(f"::notice::PR #{pr_number} decision={decision.action}: {decision.reason}")
        if decision.action == "wait":
            # Non-ready: skip to the next candidate (no HOL block, no merge).
            continue
        if decision.action == "update":
            try:
                update_pull(pr_number, dry_run=dry_run)
            except BranchUpdateConflictError as exc:
                # The branch cannot be updated with main because of a real
                # conflict (HTTP 409 from /update). This is the #2352 HOL guard:
                # a conflict will not self-resolve without a human/agent rebase,
                # so re-attempting the update every tick would head-of-line block
                # every ready PR behind it. HOLD this PR (apply HOLD_LABEL, which
                # is an opt-out label so later ticks skip it) and CONTINUE the
                # scan so a newer ready PR can still merge this tick. Fail-closed:
                # a held PR is skipped, never merged.
                sys.stderr.write(
                    f"::error::branch-update conflict for PR #{pr_number}: {exc}\n"
                )
                hold_note = (
                    "merge-queue: could not update this branch with "
                    f"`{WATCH_BRANCH}` — the update returned a merge conflict "
                    f"(HTTP 409) that the queue cannot auto-resolve ({exc}). "
                    f"Applied `{HOLD_LABEL}` to unblock the queue (HOL guard). "
                    f"Fix: rebase/merge `{WATCH_BRANCH}` into this branch and "
                    f"resolve the conflicts, then remove `{HOLD_LABEL}` to requeue."
                )
                hold_pr(pr_number, hold_note, dry_run=dry_run)
                continue  # held — keep scanning for a mergeable candidate
            post_branch_update_comment_best_effort(
                pr_number,
                (
                    f"merge-queue: updated this branch with `{WATCH_BRANCH}` at "
                    f"`{main_sha[:12]}`. Waiting for CI on the refreshed head."
                ),
                dry_run=dry_run,
            )
            return 0
        if decision.ready:
            latest_main_sha = get_branch_head(WATCH_BRANCH)
            if latest_main_sha != main_sha:
                print(
                    f"::notice::main moved {main_sha[:8]} -> {latest_main_sha[:8]}; "
                    "deferring to next tick"
                )
                return 0
            # FINAL pre-merge re-check against LIVE status (internal#3210).
            # The decision above may have run its status gates against the
            # conductor snapshot; a required/enforced/critical context can
            # flip to RED within the snapshot's freshness window AFTER it was
            # captured and still read GREEN there. Re-fetch this candidate's
            # head statuses live (snapshot BYPASSED) and re-run the SAME
            # status gates. On ANY regression, SKIP this PR (treat as wait —
            # never merge a snapshot-green-but-now-red head) and keep scanning
            # so a genuinely-ready PR behind it can still merge this tick. One
            # extra GET, only for the single candidate about to merge.
            head_sha = ctx.get("head_sha")
            if not isinstance(head_sha, str) or not head_sha:
                print(
                    f"::notice::PR #{pr_number} SKIPPED: exact head SHA is "
                    "missing; refusing an unpinned merge request"
                )
                continue
            regressions = live_premerge_status_regressions(
                head_sha,
                required_contexts=contexts,
                enforced_file_contexts=enforced_file_contexts,
            )
            if regressions:
                print(
                    f"::notice::PR #{pr_number} SKIPPED: live pre-merge "
                    f"re-check found {', '.join(regressions)} "
                    "(snapshot was stale)"
                )
                continue  # skip — keep scanning for a still-ready candidate
            try:
                merge_pull(
                    pr_number,
                    dry_run=dry_run,
                    head_commit_id=head_sha,
                )
            except MergePermissionError as exc:
                # Normal merge refusal (HTTP 403/404/405). Never bypass it.
                # HOLD this PR and CONTINUE scanning only when the durable
                # HOLD_LABEL write succeeds. If the scoped token lacks
                # write:issue, hold_pr raises so main() returns non-success;
                # a refused merge with no durable hold must not look green.
                sys.stderr.write(f"::error::merge permission error for PR #{pr_number}: {exc}\n")
                hold_note = (
                    "merge-queue: the normal protected merge was refused "
                    f"({exc}). No force/admin bypass was attempted. Attempted "
                    f"to apply `{HOLD_LABEL}` so the queue can advance. Re-check "
                    "branch freshness, required checks, approvals, and token "
                    f"permissions; then remove `{HOLD_LABEL}` to requeue."
                )
                hold_pr(pr_number, hold_note, dry_run=dry_run)
                continue  # held — keep scanning for a mergeable candidate
            return 0
    return 0


def get_pull_diff_files(pr_number: int) -> list[dict]:
    """Fetch the list of files changed in a PR with their unified-diff patch.

    Gitea endpoint: GET /repos/{owner}/{name}/pulls/{pr}/files. Returns a
    list of `{filename, status, additions, deletions, changes, patch}` dicts.
    The `patch` field is the unified diff (may be None/empty for binary
    files or files beyond the Gitea diff-size cap).

    Used by is_runtime_bump_exempt() to inspect the .runtime-version diff
    (the only file a qualifying bump touches). Network/HTTP errors are
    caught and converted to ApiError by the api() helper; the caller
    treats an empty list as "no diff info available" (fail-closed: the
    exemption check will see an empty pr_files and reject the PR).

    Fail-safe: in addition to ApiError, any other exception (a malformed
    URL, a urllib transport error, a JSON decode failure, etc.) is
    swallowed and an empty list returned. is_runtime_bump_exempt() will
    then reject the PR (no .runtime-version found), so the PR takes the
    normal 2-genuine path. The empty list is the fail-closed sentinel.
    """
    try:
        _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/files")
    except (ApiError, ValueError, TypeError) as exc:
        # ValueError: malformed URL (urllib raised this in test monkeypatches
        # that don't set a real host). TypeError: rare but defensive. ApiError:
        # the documented HTTP failure mode. All three mean "diff not
        # fetchable" → fail-closed empty list.
        sys.stderr.write(
            f"::warning::get_pull_diff_files failed for PR #{pr_number}: {exc}\n"
        )
        return []
    if not isinstance(body, list):
        return []
    return body


def latest_runtime_v_tag_for(pr: dict) -> str | None:
    """Query the runtime repo for the latest runtime-v release tag matching
    the .runtime-version bump on `pr`. Returns the tag string, or None if
    it can't be determined (network error, no matching repo, no tags, etc.).

    FAIL-CLOSED contract: returning None is enough to fail the ==SSOT
    condition in is_runtime_bump_exempt — the PR will not be exempted.

    The lookup strategy:
      1. Read the runtime name from the pr's head branch name
         (bump-bot uses branch names like `runtime-bump/claude-code/v1.2.3`
         per the 9c2e9c88 spec; if the branch doesn't match, return None).
      2. Query the runtime repo's tags list
         (GET /repos/molecule-ai/molecule-ai-runtime-{name}/tags).
      3. Return the most recent tag that is exactly a `vMAJOR.MINOR.PATCH`
         version (so a v0.x.y dev tag doesn't accidentally satisfy).

    To keep the implementation minimal and testable, the runtime repo name
    is derived from a convention: the runtime name (extracted from the
    branch) maps to `molecule-ai/molecule-ai-runtime-<name>`. If the
    convention doesn't hold for a given runtime, the operator can set
    RUNTIME_BUMP_REPO_TEMPLATE env var to override (e.g.
    `org/repo-{name}-runtime`).
    """
    repo_template = _env(
        "RUNTIME_BUMP_REPO_TEMPLATE",
        default="molecule-ai/molecule-ai-runtime-{name}",
    )
    # Extract the runtime name from the branch. The branch format is
    # `runtime-bump/<runtime-name>/v<version>` per the 9c2e9c88 spec.
    head = pr.get("head") or {}
    branch = head.get("ref", "") if isinstance(head, dict) else ""
    if not isinstance(branch, str) or not branch.startswith("runtime-bump/"):
        return None
    parts = branch.split("/")
    if len(parts) != 3:
        return None
    runtime_name = parts[1]
    if not runtime_name:
        return None
    runtime_repo = repo_template.format(name=runtime_name)
    try:
        owner, name = runtime_repo.split("/", 1)
    except ValueError:
        return None
    try:
        _, tags = api("GET", f"/repos/{owner}/{name}/tags", query={"limit": "20"})
    except ApiError:
        return None
    if not isinstance(tags, list):
        return None
    # Pick the most recent tag that looks like a clean semver `vX.Y.Z`
    # (filter out pre-release / build-metadata / non-version tags).
    semver_re = re.compile(r"^v\d+\.\d+\.\d+$")
    for tag in tags:
        if not isinstance(tag, dict):
            continue
        tag_name = tag.get("name", "")
        if isinstance(tag_name, str) and semver_re.match(tag_name):
            return tag_name
    return None


def _is_active_release_candidate(prs: list[dict], *, pr_number: int) -> bool:
    """True if any open PR (other than `pr_number`) carries a label
    indicating a release-candidate is in progress.

    The label name is taken from RELEASE_CANDIDATE_LABELS env var
    (comma-separated, default "release-candidate,rc"). An open PR
    carrying ANY of these labels blocks the runtime-bump exemption —
    the bump would race with the RC and potentially land before the
    RC smoke completes.
    """
    rc_labels = {
        name.strip()
        for name in _env(
            "RELEASE_CANDIDATE_LABELS",
            default="release-candidate,rc",
        ).split(",")
        if name.strip()
    }
    for other in prs:
        if not isinstance(other, dict):
            continue
        try:
            other_number = int(other.get("number", 0))
        except (TypeError, ValueError):
            continue
        if other_number == pr_number:
            continue  # don't count the PR being evaluated
        if other.get("state") != "open":
            continue
        other_labels = label_names(other)
        if other_labels & rc_labels:
            return True
    return False


def _early_skip_reason(
    pr: dict,
    *,
    watch_branch: str,
) -> tuple[str | None, str | None]:
    """Return an observable skip reason for PRs that are not merge-eligible.

    Returns (reason, pr_comment). When reason is non-None the PR should be
    skipped by the queue. pr_comment is the body to post on the PR; None means
    the skip is silent w.r.t. PR comments (the reason is still emitted as a
    workflow notice by the caller). This keeps the early-skip decision pure and
    unit-testable while preserving the observable-vs-silent distinction.
    """
    if pr.get("state") != "open":
        return "not open", None
    if OPT_OUT_LABELS & label_names(pr):
        return "opt-out label", None
    if pr.get("draft") is True:
        return "draft", None
    if pr.get("base", {}).get("ref") != watch_branch:
        # Silent w.r.t. PR comments: stacked PRs whose base is not main are
        # re-evaluated every conductor tick; posting a comment each time floods
        # the PR. The caller still emits a workflow notice so the skip is
        # observable in logs.
        return f"base is not `{watch_branch}`", None
    if pr.get("head", {}).get("repo_id") != pr.get("base", {}).get("repo_id"):
        return "fork PR", "merge-queue: skipped; fork PRs are not supported by the serialized queue."
    return None, None


def _evaluate_candidate(
    issue: dict,
    *,
    main_sha: str,
    main_status: dict,
    required_contexts: list[str],
    required_approvals: int,
    dry_run: bool,
    enforced_file_contexts: list[str] | None = None,
    block_on_outdated_branch: bool = False,
) -> tuple[MergeDecision | None, dict]:
    """Evaluate a single auto-discovered candidate against the full merge bar.

    Returns (decision, ctx) where ctx carries {"pr_number"}. A None decision
    means the PR is not merge-eligible at all (not open / opted-out / draft /
    fork / wrong base) and the caller should skip to the next candidate; for
    fork / wrong-base the explanatory comment is posted here before returning.

    The merge bar is UNCHANGED from the single-PR path — this only factors the
    per-PR evaluation out so process_once can scan multiple candidates. A failed
    status fetch still raises (fail-closed): it propagates to the caller so the
    PR is never treated as green.
    """
    pr_number = int(issue["number"])
    ctx = {"pr_number": pr_number}
    pr = get_pull(pr_number)
    reason, pr_comment = _early_skip_reason(pr, watch_branch=WATCH_BRANCH)
    if reason is not None:
        # Observable in workflow logs (LOUD relative to silent disappearance),
        # but avoid PR-comment noise for the high-volume skip classes.
        print(f"::notice::PR #{pr_number} {reason}; skipping")
        if pr_comment is not None:
            post_comment(pr_number, pr_comment, dry_run=dry_run)
        return None, ctx

    head_sha = pr.get("head", {}).get("sha")
    if not isinstance(head_sha, str) or len(head_sha) < 7:
        raise ApiError(f"PR #{pr_number} missing head sha")
    # Surface the candidate head SHA so process_once can re-fetch its LIVE
    # combined status immediately before the merge POST (internal#3210
    # pre-merge re-check). The decision below may run against the conductor
    # snapshot; the final gate must re-verify against ground truth.
    ctx["head_sha"] = head_sha
    commits = get_pull_commits(pr_number)
    current_base = pr_has_current_base(pr, commits, main_sha)
    # Fail-closed: a failed status fetch raises here and propagates (the PR is
    # never treated as green).
    pr_status = get_combined_status(head_sha)
    pr_labels = label_names(pr)
    # FAIL-CLOSED, TRI-STATE: Gitea returns mergeable=None (or omits the field)
    # while it is still COMPUTING conflict state, mergeable=False for a definitive
    # conflict, and mergeable=True only when it has proven the PR conflict-free.
    # We preserve all THREE states (do NOT collapse None/missing into False):
    #   - True            → direct-merge eligible (step 5).
    #   - None / missing  → still computing → WAIT (never merge, never update,
    #                       never dismiss approvals); re-check next tick.
    #   - False           → definitive conflict → the update/hold path (step 6).
    # Collapsing None→False would route a behind-main but STILL-COMPUTING PR into
    # the /update path, which triggers dismiss_stale_approvals — the exact
    # rebase-churn this change eliminates. Normalize only to the literal True /
    # False / None set (some Gitea versions omit the key entirely → None).
    raw_mergeable = pr.get("mergeable")
    mergeable: bool | None = raw_mergeable if isinstance(raw_mergeable, bool) else None

    reviews = get_pull_reviews(pr_number)
    approvers, request_changes = genuine_approvals(
        reviews, headsha=head_sha, reviewer_set=REVIEWER_SET
    )

    # Runtime-bump exemption (RFC internal#131). Compute the exempt flag here
    # so it flows into evaluate_merge_readiness. The check itself lives in
    # is_runtime_bump_exempt() and is fail-closed — any unverifiable guard
    # or condition returns (False, reason), in which case the PR takes the
    # normal 2-genuine path. The PR is exempt ONLY if every guard + every
    # condition holds, AND an active release-candidate is NOT in progress.
    #
    # Fail-safe wrap: each helper (get_pull_diff_files,
    # latest_runtime_v_tag_for, _is_active_release_candidate) issues its own
    # API calls. A network blip or a partial outage must not abort the
    # whole tick; on error we default to NOT exempt (the PR takes the
    # normal 2-genuine path). This is consistent with the fail-closed
    # design: when in doubt, require 2-genuine.
    runtime_bump_exempt = False
    exempt_reason = "exemption check not run"
    try:
        pr_files = get_pull_diff_files(pr_number)
        latest_tag = latest_runtime_v_tag_for(pr)
        try:
            rc_prs = list_candidate_issues(auto_discover=False)
        except (ApiError, ValueError, TypeError) as exc:
            # list_candidate_issues can also hit the network via
            # api_paginated; if it errors here, treat as "no active RC"
            # (the conservative read — the PR may be exempted if every
            # other condition holds, but only when the call succeeds
            # and returns a clean list). Logging only — fail-closed
            # elsewhere (empty pr_files / None latest_tag → not exempt).
            sys.stderr.write(
                f"::warning::list_candidate_issues for RC check failed on "
                f"PR #{pr_number}: {exc}\n"
            )
            rc_prs = []
        rc_active = _is_active_release_candidate(prs=rc_prs, pr_number=pr_number)
        # Under a wildcard BP set the CI-side gate-adequacy check reads the
        # contexts ACTUALLY posted on the head (base names, event-suffix
        # stripped), since "*" itself names none. Harmless for a literal BP
        # set (adequacy then reads required_contexts and ignores this).
        posted_ctx_base_names = [
            _context_base_name(c)
            for c in latest_statuses_by_context(pr_status.get("statuses") or [])
        ]
        runtime_bump_exempt, exempt_reason = is_runtime_bump_exempt(
            pr=pr,
            pr_files=pr_files,
            required_contexts=required_contexts,
            latest_runtime_v_tag=latest_tag,
            rc_active=rc_active,
            posted_contexts=posted_ctx_base_names,
        )
    except (ApiError, ValueError, TypeError) as exc:
        # API error fetching diff / tag / RC status → fail-closed:
        # treat as not exempt, continue with normal 2-genuine path.
        runtime_bump_exempt = False
        exempt_reason = f"exemption check errored (fail-closed): {exc}"
    if runtime_bump_exempt:
        # Observable so an audit can confirm WHY the approvals gate was
        # bypassed. The reason is also returned via the MergeDecision
        # field if a merge decision is taken.
        print(f"::notice::PR #{pr_number} runtime-bump exempt: {exempt_reason}")
    else:
        # Only log "not exempt" at debug level (we don't want to spam
        # the workflow log for the common case). The decision.reason
        # returned by evaluate_merge_readiness will carry the real
        # "insufficient genuine approvals" message on the normal path.
        pass

    decision = evaluate_merge_readiness(
        main_status=main_status,
        pr_status=pr_status,
        required_contexts=required_contexts,
        required_approvals=required_approvals,
        approvers=approvers,
        request_changes=request_changes,
        pr_has_current_base=current_base,
        mergeable=mergeable,
        pr_labels=pr_labels,
        runtime_bump_exempt=runtime_bump_exempt,
        enforced_file_contexts=enforced_file_contexts,
        block_on_outdated_branch=block_on_outdated_branch,
    )
    return decision, ctx


@dataclasses.dataclass(frozen=True)
class ReadinessEntry:
    """One candidate's readiness state."""

    pr_number: int
    decision: MergeDecision | None
    reason: str


def enumerate_readiness(*, dry_run: bool = False) -> list[ReadinessEntry]:
    """Evaluate ALL candidates and return their readiness states.

    Fail-closed: if branch protection cannot be fetched, raise
    BranchProtectionUnavailable; if the enforced-contexts SSOT file is
    missing/unreadable, load_enforced_file_contexts raises
    EnforcedContextsUnavailable. Both propagate to main()'s ApiError
    handler (rc 1) — this read-only report must surface a broken gate, not
    print a summary derived from a silently-disabled SSOT. Unlike
    process_once, this does NOT stop at the first actionable candidate;
    it evaluates every eligible PR and returns the full list so a
    post-batch summary can be printed.
    """
    bp = get_branch_protection(WATCH_BRANCH)
    # Uniform gate: governance checks are ALWAYS required, even if branch
    # protection does not enumerate them. Deduplicate against BP list.
    contexts = list(dict.fromkeys(bp.required_contexts + GOVERNANCE_REQUIRED_CONTEXTS))
    required_approvals = bp.required_approvals
    enforced_file_contexts = load_enforced_file_contexts(ENFORCED_CONTEXTS_FILE)

    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())

    candidates = choose_candidate_issues(
        list_candidate_issues(auto_discover=AUTO_DISCOVER),
        queue_label=QUEUE_LABEL,
        opt_out_labels=OPT_OUT_LABELS,
        auto_discover=AUTO_DISCOVER,
    )

    entries: list[ReadinessEntry] = []
    for issue in candidates:
        pr_number = int(issue["number"])
        try:
            decision, ctx = _evaluate_candidate(
                issue,
                main_sha=main_sha,
                main_status=main_status,
                required_contexts=contexts,
                required_approvals=required_approvals,
                dry_run=dry_run,
                enforced_file_contexts=enforced_file_contexts,
                block_on_outdated_branch=bp.block_on_outdated_branch,
            )
        except ApiError as exc:
            # Fail-closed per candidate: an unreadable PR is recorded as
            # unverifiable, not skipped silently.
            entries.append(
                ReadinessEntry(
                    pr_number=pr_number,
                    decision=None,
                    reason=f"unverifiable (API error: {exc})",
                )
            )
            continue
        if decision is None:
            entries.append(
                ReadinessEntry(
                    pr_number=pr_number,
                    decision=None,
                    reason="not merge-eligible (opt-out/draft/fork/wrong-base)",
                )
            )
            continue
        entries.append(
            ReadinessEntry(
                pr_number=pr_number,
                decision=decision,
                reason=decision.reason,
            )
        )
    return entries


def print_post_batch_summary(entries: list[ReadinessEntry]) -> None:
    """Print a structured summary of all candidates' readiness.

    Emits ::notice:: lines for machine parsing and a human-readable
    block for operator visibility.
    """
    ready = [e for e in entries if e.decision and e.decision.ready]
    waiting = [e for e in entries if e.decision and not e.decision.ready]
    ineligible = [e for e in entries if e.decision is None]

    print("::group::merge-queue readiness summary")
    print(f"total_candidates={len(entries)}")
    print(f"ready={len(ready)}")
    print(f"waiting={len(waiting)}")
    print(f"ineligible/unverifiable={len(ineligible)}")
    print("")
    for e in entries:
        state = "ready" if e.decision and e.decision.ready else (
            "waiting" if e.decision else "ineligible"
        )
        action = e.decision.action if e.decision else "n/a"
        print(f"PR #{e.pr_number}: state={state} action={action} reason={e.reason}")
    print("::endgroup::")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument(
        "--enumerate",
        action="store_true",
        help="Evaluate all candidates and print a readiness summary without merging.",
    )
    args = parser.parse_args()
    _require_runtime_env()
    try:
        if args.enumerate:
            entries = enumerate_readiness(dry_run=args.dry_run)
            print_post_batch_summary(entries)
            return 0
        return process_once(dry_run=args.dry_run)
    except ApiError as exc:
        # FAIL-CLOSED: API errors are not "transient success" — they mean
        # the queue could not evaluate merge state. Returning 0 hides
        # persistent infra issues (auth drift, endpoint outages) from
        # operators. Return 1 so the cron job surfaces red and paging fires.
        sys.stderr.write(f"::error::queue API error: {exc}\n")
        return 1
    except urllib.error.URLError as exc:
        sys.stderr.write(f"::error::queue network error: {exc}\n")
        return 1
    except TimeoutError as exc:
        sys.stderr.write(f"::error::queue timeout: {exc}\n")
        return 1


if __name__ == "__main__":
    sys.exit(main())
