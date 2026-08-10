#!/usr/bin/env python3
"""lint-context-set-shrink — every context this PR SHOULD post, must post.

THE DEFECT CLASS
================
core#5153 added a duplicate `env:` key to `.gitea/workflows/local-provision-e2e.yml`.
Gitea's workflow parser rejects a duplicate mapping key, so it never SCHEDULED that
workflow: neither of its two jobs posted any commit status at all, including
`Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)`, which
`.gitea/required-contexts.txt` names. Every other workflow ran, so head 23e5960e6
read 43/43 `success` and was fully mergeable.

    The lane did not fail. It ceased to exist.

Branch protection on main is `status_check_contexts=['*']` — "every POSTED context
must be green". A wildcard constrains statuses that EXIST; it cannot require one that
never arrives. Five workflow lints posted `success` on that very file, because each
checks a PROPERTY of the YAML and none checks the only thing that matters: did CI
actually run?

This gate checks that, and only that. It is cause-blind on purpose. Duplicate key,
malformed `on:`, renamed job, a narrowed `paths:` filter, a narrowed `branches:`
filter, a trigger type removed — all of them end in one observable, and the gate
watches the observable instead of enumerating causes.

    CHECK A  every context DERIVED from the head tree for this PR's event and
             diff must appear in the posted set.
    CHECK B  every lane the BASE branch would have run for this same PR must
             still be derivable at head, or carry a waiver. (Check A cannot see a
             removal: delete the workflow — or narrow its `on:` — and the
             expectation disappears along with the lane, so the absence becomes
             invisible to a check that only compares expectation against reality.)

             Both sides of Check B are derived for the SAME event and the SAME
             changed-file list, so it carries none of the event-asymmetry or
             paths-filter noise that makes a raw base-vs-head status diff
             unusable. Comparing derived LANES rather than file names means
             renaming a workflow FILE is not a finding — the lanes are unchanged —
             while deleting one, narrowing its trigger so it stops running, or
             changing a workflow's or job's `name:` all are. The last of those is
             deliberate: the lane's identity IS that string, and the same rename
             silently invalidates any matching line in required-contexts.txt.

WHY DERIVE, RATHER THAN DIFF AGAINST ANOTHER COMMIT
===================================================
The intuitive shape is "compare the head's context set against the base's". It does
not survive contact with the data, and it fails in the direction that makes a gate
worthless — reddening healthy PRs. Measured on molecule-core:

* EVENT ASYMMETRY IS TOTAL. On main@ad9986c565 all 45 contexts carry `(push)` and
  ZERO carry `(pull_request)`; a PR head is the mirror image. "PR-event contexts on
  the base" is the empty set. Worse, a MERGED head accumulates events an OPEN PR
  cannot have — `audit-force-merge / audit (pull_request_target)` fires on
  `types: [closed]`, `gitea-merge-queue / queue (pull_request_review)` on
  `types: [submitted]` — so any merged commit used as a reference reports contexts
  the PR under test structurally cannot post.
* `paths:` FILTERS DEFEAT ANY CROSS-PR REFERENCE. Across 29 healthy merged PR heads
  the `(pull_request)` count ranges 32..51 over a 55-context universe, of which only
  31 appear on every head. A Go-only PR legitimately posts 19 fewer contexts than a
  workflow-touching one.

Neither problem exists for a derivation, because a derivation is computed FOR THIS
PR: this event, this base branch, this diff. And it is strictly stronger — it needs
no comparable earlier commit to exist, so it catches a workflow that vanishes on the
FIRST push of a PR, where a diff-based check has nothing to compare against.

IS THE DERIVATION FAITHFUL? MEASURED, NOT ASSUMED
=================================================
A derivation that over-predicts reds healthy PRs; one that under-predicts is
vacuous. So it was scored against ground truth before being trusted: for 24 real
merged PR heads, derive the expected set from the tree at that sha plus that PR's
real changed-file list, and compare against what Gitea actually posted.

    24/24 heads exact. over-predicted = 0. under-predicted = 0.
    Head sizes 32..48 contexts; PR diffs 1..14 files.

Two properties make that accuracy achievable rather than lucky, and both were
verified rather than hoped for:

* `needs:` SKIPPING DOES NOT SUPPRESS A CONTEXT. This one is OBSERVED: `CI /
  Platform (Go)` (job `platform-build`) gates behind `needs: changes` and still
  posts on a one-file manifest-only PR.
* JOB-LEVEL `if:` DOES NOT NEED EVALUATING — and this one is asserted from the
  MECHANISM, not from observation, which is a weaker claim and is written weakly on
  purpose. Gitea writes the status rows at SCHEDULE time, before any job runs:
  on 23e5960e6 `CI / all-required` and `CI / Canvas Deploy Status` carry contiguous
  `pending` rows at +0s, ids 5..11, and neither job had started. A row that exists
  before evaluation cannot depend on the result of evaluation.

  It is NOT confirmed by seeing a skipped job still post, because there is nothing
  to see: a scan of 400 runs found ZERO jobs with `conclusion=skipped`. Nor is it
  currently load-bearing — every in-scope job-level `if:` is either `always()` or
  belongs to a push-only workflow, so no `pull_request` lane depends on it today.
  It is stated here so that a future reader knows which of the two properties is
  measured and which is reasoned, rather than trusting both equally.

Together they keep the derivation out of the business of evaluating `${{ }}`
expressions, which is where a faithful emulation of the scheduler would become
hopeless.

The one derivation error observed during development was mine, not the model's: the
harness read workflow bytes as cp1252, which corrupted an em-dash in
`template-delivery-e2e`'s job name and broke `ci.yml`'s parse, manufacturing a
phantom missing context on every head. Encoding is load-bearing here — the derived
string must byte-match the string Gitea composed — so every read in this file pins
UTF-8 explicitly.

WHAT THE DERIVATION MODELS, AND WHAT IT DOES NOT
================================================
Models: `on:` normalised across the scalar/list/mapping forms and the PyYAML 1.1
`on` -> `True` boolean-key trap; `types:` (defaulting to opened/synchronize/reopened)
requiring a push-shaped type; `branches:`/`branches-ignore:` against the PR base;
`paths:`/`paths-ignore:` against the PR's real changed files, with `**` and `*`
distinguished; job display names (`name:` or the job key).

Does not model, because it does not have to: `if:` expressions, `needs:` graphs,
matrices, or runner availability — none of which affect whether a context is
POSTED. `needs:` is measured; `if:` rests on the schedule-time mechanism above;
matrices and runner availability are neither, and are simply unused in scope here.

Deliberately outside scope: `pull_request_target` and `pull_request_review`
workflows. They fire on review and merge actions rather than on a push, so they are
not derivable from a diff, and the two of them are exactly the false positives that
sank the diff-based shape. A workflow triggered ONLY by those events is not covered
here — stated, not hidden.

SETTLEMENT: "NOT YET POSTED" vs "WILL NEVER POST"
=================================================
Mid-flight an expected context legitimately has not arrived; `x-total-count` GROWS
during settlement. Firing then is a false-failure machine.

The discriminator is measured. Gitea writes the `pending` row for every job of every
SCHEDULED workflow in one pass at push time, and a workflow it refuses to schedule
never posts even a pending. On the broken head 23e5960e6 all 43 contexts posted their
FIRST row inside a SIX-SECOND window (43/43 in the first 15s bucket). Completion is
slow and ragged — that same head took 1511s for its last job to finish — but PRESENCE
is decided in seconds.

That is why this gate compares presence only, never state. Presence settles in
seconds; state takes half an hour; and state is already covered by the `['*']`
wildcard. It also makes the gate immune to every way a state can lie: a CANCELLED job
posts `failure` on a superseded head, a `pending` on a job that already completed is
mid-settlement, and the status-reaper overwrites stranded pendings. All of those
change STATE. None changes PRESENCE.

So the gate polls until the posted set is identical across CSG_STABLE_READS
consecutive reads spaced CSG_POLL_SECONDS apart, with a wall-clock floor of
CSG_MIN_SETTLE_SECONDS. If it has not stabilised by CSG_MAX_SETTLE_SECONDS it reports
UNSETTLED and PASSES. While uncertain it never reds.

NOT VACUOUS
===========
A gate that reads nothing must not report success. It refuses in four places:

* zero contexts DERIVED, when the head has workflow files -> ERROR. That is the
  shape a broken derivation takes, and its failure direction is a silent GREEN;
* zero contexts POSTED on the head after settling -> ERROR;
* a workflow file at head that will not parse -> ERROR, because a file this gate
  cannot read is a file whose lanes it cannot vouch for (whether the file is
  SCHEDULABLE is lint-workflow-yaml's job, not this one's);
* a waiver naming this PR that matches nothing -> ERROR.

Every run prints `expected=N posted=M missing=K` so a coverage change shows up as a
number that moved rather than as silence.

THE ESCAPE HATCH
================
Declared in `.gitea/context-shrink-waivers.txt`, in the same PR:

    # <one or more comment lines giving the reason — mandatory>
    <pr-number>  <event-stripped context>

Explicit (in-tree, in the diff, under review), auditable (`git blame`), and
PR-SCOPED so it cannot silently protect a later PR. Not an env var: an env var set on
a runner leaves no trace in the artefact under review, which is the whole point of an
escape hatch.

EXIT CODES
    0  OK | UNSETTLED
    1  MISSING — a derived context never posted, or a base-branch lane is gone
    2  ERROR — vacuous derivation, unreadable workflow, or a failed read
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field

import yaml

# ---------------------------------------------------------------------------
# Context names
# ---------------------------------------------------------------------------
# Gitea suffixes a status context with the event that produced it. The bare
# `workflow / job` form is what `.gitea/required-contexts.txt` stores and it is the
# key this gate compares on. Mirrors gitea-merge-queue._EVENT_SUFFIX_RE and
# lint_no_coe_on_required.strip_event so all three agree on the canonical key.
#
# Anchored, and it strips ONLY the suffix. Over-anchoring is a live hazard here:
# `reserved-path-review` (a verdict) and `reserved-path-review / reserved-path-review
# (pull_request_target)` (the job ran) differ by nothing else, so a loose pattern
# collapses two distinct contexts onto one key, and a greedy one eats a job name that
# legitimately ends in parentheses — `Local Provision Lifecycle E2E (stub)` and
# `CI / Canvas (Next.js)` are both real, and both would be mangled.
_EVENT_SUFFIX_RE = re.compile(
    r"\s*\((pull_request|push|pull_request_target|pull_request_review)\)\s*$"
)

# The event this gate derives and compares. See the module docstring for why
# pull_request_target / pull_request_review are out of scope.
COMPARED_EVENT = "pull_request"

# A push to an open PR delivers `synchronize`; opening it delivers `opened`. A
# workflow whose `types:` excludes both cannot fire on the event this gate runs on.
PUSH_SHAPED_TYPES = frozenset({"opened", "synchronize", "reopened"})
DEFAULT_PR_TYPES = ("opened", "synchronize", "reopened")


def split_event(context: str) -> tuple[str, str]:
    """Split a live context into (event-stripped name, event); `""` when unsuffixed.

    The `.strip()` calls below are defensive only, and a mutant that removes them
    SURVIVES the test suite. That is an equivalent mutant, not a coverage gap —
    but the argument only holds if it covers EVERY caller, because the two
    implementations are genuinely distinct functions: they diverge on `'  A / a'`,
    `'\tA / a'` and `'A / a\xa0'`. The equivalence is a property of the INPUT
    DOMAIN, not of the code, so it has to be argued per entry point. There are two:

    1. LIVE STATUS CONTEXTS, via `posted_contexts`. `_EVENT_SUFFIX_RE` already
       consumes the whitespace between a context and its event suffix, and Gitea
       composes `f"{workflow} / {job}"` from values that were themselves stripped.
       Checked against 121 distinct real context strings across five heads: zero
       padded, zero that differ between the two implementations.
    2. WAIVER SUBJECTS, via `parse_waivers`. `_WAIVER_LINE_RE` captures the subject
       as `\\S.*?` with a trailing `\\s*$`, so it is already stripped at both ends
       before it ever reaches here — `'42\\t A / a \\t'` arrives as `'A / a'`.

    Recorded because "a mutant survived" and "a mutant cannot be distinguished" are
    different findings, only the first is a problem, and an equivalence argument
    that silently covers half its callers is a third thing again.
    """
    match = _EVENT_SUFFIX_RE.search(context)
    if match is None:
        return context.strip(), ""
    return context[: match.start()].strip(), match.group(1)


# ---------------------------------------------------------------------------
# Status reduction
# ---------------------------------------------------------------------------
def status_numeric_id(status: dict) -> int:
    """Sortable Gitea commit-status id, or -1 when unavailable.

    `True` is an `int` in Python and would sort ahead of every real id, so bools are
    rejected explicitly rather than left to `int()`.
    """
    value = status.get("id")
    if isinstance(value, bool):
        return -1
    try:
        return int(value)
    except (TypeError, ValueError):
        return -1


def latest_by_context(rows: list[dict]) -> dict[str, dict]:
    """Reduce raw status rows to the newest row per RAW context string.

    Gitea appends a new row per transition, so a context appears once per state
    change. Reduce by monotonic `id`, not by list order: the combined endpoint's
    ordering is not contractual, and `created_at` has second granularity, which ties
    for rows written in the same second.
    """
    latest: dict[str, dict] = {}
    for row in sorted(rows, key=status_numeric_id):
        context = row.get("context")
        if isinstance(context, str) and context.strip():
            latest[context] = row
    return latest


def posted_contexts(rows: list[dict]) -> set[str]:
    """Event-stripped `pull_request` context names present in `rows`. PRESENCE only."""
    names: set[str] = set()
    for context in latest_by_context(rows):
        name, event = split_event(context)
        if event == COMPARED_EVENT and name:
            names.add(name)
    return names


# ---------------------------------------------------------------------------
# Path matching
# ---------------------------------------------------------------------------
def _glob_to_regex(pattern: str) -> re.Pattern[str]:
    """Translate a workflow path filter to a regex, distinguishing `**` from `*`.

    `fnmatch` is wrong here in a way that only shows up on the patterns nobody
    tests: it maps `*` to `.*`, so `*.md` would match `docs/a.md` and a filter
    intended for top-level files would silently widen to the whole tree. `*` stops at
    a separator; `**` crosses them.
    """
    out = ["\\A"]
    index = 0
    while index < len(pattern):
        char = pattern[index]
        if char == "*":
            if pattern.startswith("**", index):
                out.append(".*")
                index += 2
                # `a/**/b` should also match `a/b`: swallow the separator that
                # follows a `**` so the middle segment may be empty.
                if pattern.startswith("/", index):
                    out.append("/?")
                    index += 1
                continue
            out.append("[^/]*")
        elif char == "?":
            out.append("[^/]")
        else:
            out.append(re.escape(char))
        index += 1
    out.append("\\Z")
    return re.compile("".join(out))


def path_matches(pattern: str, path: str) -> bool:
    return _glob_to_regex(pattern).match(path) is not None


def any_path_matches(patterns, paths) -> bool:
    compiled = [_glob_to_regex(p) for p in patterns]
    return any(rx.match(path) for rx in compiled for path in paths)


# ---------------------------------------------------------------------------
# Workflow parsing and derivation
# ---------------------------------------------------------------------------
class WorkflowUnreadable(RuntimeError):
    """A workflow file at head could not be parsed, so its lanes cannot be derived."""


def load_workflow(text: str, filename: str) -> dict:
    try:
        doc = yaml.safe_load(text)
    except yaml.YAMLError as exc:
        raise WorkflowUnreadable(f"{filename}: {str(exc).splitlines()[0]}") from exc
    return doc if isinstance(doc, dict) else {}


def workflow_triggers(doc: dict) -> dict:
    """The `on:` block, normalised across every form YAML allows it to take.

    `on` is a YAML 1.1 boolean, so PyYAML resolves the bare key to `True` rather than
    to the string `"on"`. Every workflow in this repo is therefore stored under the
    `True` key, and reading `doc["on"]` alone would derive NOTHING from ANY file —
    a total, silent, green-passing failure. Both spellings are read.
    """
    raw = doc.get("on")
    if raw is None:
        raw = doc.get(True)
    if isinstance(raw, str):
        return {raw: {}}
    if isinstance(raw, list):
        return {key: {} for key in raw if isinstance(key, str)}
    if isinstance(raw, dict):
        return {k: (v if isinstance(v, dict) else {}) for k, v in raw.items()}
    return {}


def fires_on_pull_request(doc: dict, *, base_branch: str, changed: list[str]) -> bool:
    """Would Gitea schedule this workflow for a push to a PR against `base_branch`?"""
    triggers = workflow_triggers(doc)
    if COMPARED_EVENT not in triggers:
        return False
    config = triggers[COMPARED_EVENT]

    types = config.get("types") or DEFAULT_PR_TYPES
    if isinstance(types, str):
        types = [types]
    if not PUSH_SHAPED_TYPES.intersection(types):
        return False

    branches = config.get("branches")
    if branches and not any(path_matches(b, base_branch) for b in branches):
        return False
    branches_ignore = config.get("branches-ignore")
    if branches_ignore and any(path_matches(b, base_branch) for b in branches_ignore):
        return False

    paths = config.get("paths")
    if paths and not any_path_matches(paths, changed):
        return False
    paths_ignore = config.get("paths-ignore")
    if paths_ignore:
        # The workflow is suppressed only when EVERY changed file is ignored. One
        # non-ignored file is enough to fire it.
        if all(any_path_matches(paths_ignore, [f]) for f in changed):
            return False
    return True


def workflow_contexts(doc: dict, filename: str) -> set[str]:
    """`<workflow name> / <job display name>` for every job in the workflow.

    Every job, with no attention paid to `if:` or `needs:`. Measured: Gitea posts a
    context for a job that skips, so filtering on either would UNDER-predict and open
    a hole exactly where the conditional lanes live.
    """
    name = doc.get("name")
    workflow_name = name.strip() if isinstance(name, str) and name.strip() else filename
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return set()
    contexts: set[str] = set()
    for key, job in jobs.items():
        if not isinstance(job, dict):
            continue
        display = job.get("name")
        display = display.strip() if isinstance(display, str) and display.strip() else key
        contexts.add(f"{workflow_name} / {display}")
    return contexts


def derive_expected(
    workflow_texts: dict[str, str],
    *,
    base_branch: str,
    changed: list[str],
    tolerate_unreadable: bool = False,
) -> tuple[dict[str, str], list[str]]:
    """Contexts a tree SHOULD post for the `pull_request` event.

    Returns `({lane: source workflow filename}, skipped)`.

    The lane->source mapping is not bookkeeping. It is what lets the caller ask the
    only question that separates "the base legitimately has fewer lanes" from "the
    base read came back partial": for each lane the head has and the base does not,
    WHICH FILE produced it, and did this PR touch that file?

    `tolerate_unreadable` is the one asymmetry between the two trees this gate
    reads, and it is deliberate. At HEAD an unreadable workflow is fatal: it is the
    PR's own tree, and a file the gate cannot parse is a file whose lanes it cannot
    vouch for, so skipping it would silently shrink the expectation into a vacuous
    pass. On the BASE branch it must NOT be fatal: main's state is not this PR's
    fault, and one unparseable file upstream would otherwise red every open PR in
    the repo until somebody fixed main — punishing every author for one person's
    mistake, which is how a gate gets deleted.
    """
    expected: dict[str, str] = {}
    skipped: list[str] = []
    for filename, text in sorted(workflow_texts.items()):
        try:
            doc = load_workflow(text, filename)
        except WorkflowUnreadable:
            if not tolerate_unreadable:
                raise
            skipped.append(filename)
            continue
        if not doc:
            continue
        if not fires_on_pull_request(doc, base_branch=base_branch, changed=changed):
            continue
        for lane in workflow_contexts(doc, filename):
            expected.setdefault(lane, filename)
    return expected, skipped


def workflow_files_in_diff(changed: list[str], workflows_dir: str) -> set[str]:
    """Bare filenames of workflow files this PR touches, for lane attribution."""
    prefix = workflows_dir.rstrip("/") + "/"
    return {
        path[len(prefix):]
        for path in changed
        if path.startswith(prefix) and "/" not in path[len(prefix):]
    }


# ---------------------------------------------------------------------------
# Waivers
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class Waiver:
    pr_number: int
    subject: str
    line_number: int
    reason: str


class WaiverFileError(ValueError):
    """The waiver file cannot be parsed. Fail closed — never ignore it."""


_WAIVER_LINE_RE = re.compile(r"^(?P<pr>\d+)\s+(?P<subject>\S.*?)\s*$")


def parse_waivers(text: str) -> list[Waiver]:
    """Parse `.gitea/context-shrink-waivers.txt`.

        # reason line (at least one, immediately above, mandatory)
        <pr-number><whitespace><event-stripped context>

    The mandatory reason is not decoration. An undocumented waiver is
    indistinguishable from a typo six months later, and this file exists precisely to
    be read by whoever is asking "why did that gate stop covering this?".
    """
    waivers: list[Waiver] = []
    reason_lines: list[str] = []
    for number, raw in enumerate(text.splitlines(), start=1):
        line = raw.strip()
        if not line:
            reason_lines = []
            continue
        if line.startswith("#"):
            reason_lines.append(line.lstrip("#").strip())
            continue
        match = _WAIVER_LINE_RE.match(line)
        if match is None:
            raise WaiverFileError(
                f"line {number}: expected `<pr-number> <subject>`, got {line!r}"
            )
        if not any(reason_lines):
            raise WaiverFileError(
                f"line {number}: waiver {line!r} has no reason. Put at least one `#` "
                "comment line immediately above it saying why this is allowed to "
                "disappear."
            )
        subject, event = split_event(match.group("subject"))
        if event:
            raise WaiverFileError(
                f"line {number}: the subject must be event-STRIPPED (drop the "
                f"`({event})` suffix): {match.group('subject')!r}"
            )
        waivers.append(
            Waiver(
                pr_number=int(match.group("pr")),
                subject=subject,
                line_number=number,
                reason=" ".join(r for r in reason_lines if r),
            )
        )
        reason_lines = []
    return waivers


# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------
OK = "OK"
MISSING = "MISSING"
UNSETTLED = "UNSETTLED"
ERROR = "ERROR"

_EXIT = {OK: 0, UNSETTLED: 0, MISSING: 1, ERROR: 2}


@dataclass
class Verdict:
    verdict: str
    expected_count: int = 0
    posted_count: int = 0
    missing: list[str] = field(default_factory=list)
    removed: list[str] = field(default_factory=list)
    waived: list[str] = field(default_factory=list)
    unexpected: list[str] = field(default_factory=list)
    detail: str = ""

    @property
    def exit_code(self) -> int:
        return _EXIT[self.verdict]

    def summary(self) -> str:
        return (
            f"context-shrink: verdict={self.verdict} "
            f"expected={self.expected_count} posted={self.posted_count} "
            f"missing={len(self.missing)} removed={len(self.removed)} "
            f"waived={len(self.waived)} unexpected={len(self.unexpected)}"
        )


def decide(
    *,
    expected: dict[str, str],
    posted: set[str],
    base_lanes: dict[str, str],
    waivers: list[Waiver],
    pr_number: int,
    settled: bool,
    head_has_workflows: bool,
    base_has_workflows: bool,
    pr_touched_workflow_files: set[str] = frozenset(),
    base_skipped_files: set[str] = frozenset(),
) -> Verdict:
    """Pure decision function. Every branch below is reachable from a test.

    Ordering is load-bearing, not arbitrary:
      1. a vacuous derivation on EITHER side, or an empty posted set, is ERROR
         before anything else, because every later comparison would be trivially
         satisfied by it;
      2. UNSETTLED outranks a missing context, because mid-flight absence is not
         evidence of absence;
      3. a vacuous waiver is ERROR even when the run would otherwise pass, so a
         waiver cannot rot into a no-op the way an unexercised rule does.
    """
    if head_has_workflows and not expected:
        return Verdict(
            ERROR,
            posted_count=len(posted),
            detail=(
                "derived ZERO expected contexts from a tree that HAS workflow files. "
                "A gate whose expectation is empty passes everything, so this is an "
                "error, not a pass. The usual cause is the `on:` key: YAML 1.1 "
                "resolves a bare `on` to the boolean True, and a reader that looks "
                "only for the string derives nothing from anything."
            ),
        )
    # Check B's own non-vacuity guard, and it exists because the omission was
    # real: without it, a base tree that read as empty made `base_lanes` empty,
    # `removed` empty, and the run report OK — a guard covering NOTHING, inside
    # the gate written to catch exactly that. It survived because the base read
    # fails late and `changed_files` usually errors first, which is precisely how
    # this shape stays alive. `read_workflow_tree_at` now separates "cannot read
    # the ref" (an error) from "this ref has no workflow directory", so reaching
    # here with an empty base side means Check B genuinely compared nothing.
    #
    # A repo whose base branch truly has no workflows — bootstrapping the very
    # first one — trips this. That is the deliberate trade: fail loud rather than
    # silently compare nothing, on a gate whose entire purpose is catching
    # comparisons that compare nothing.
    if head_has_workflows and (not base_has_workflows or not base_lanes):
        return Verdict(
            ERROR,
            expected_count=len(expected),
            posted_count=len(posted),
            detail=(
                "Check B compared NOTHING: the head has workflow files but the base "
                f"branch yielded {len(base_lanes)} lane(s) from "
                f"{'no' if not base_has_workflows else 'some'} workflow file(s). "
                "An empty base side cannot detect a removed lane, and it would "
                "report OK for every PR. Check BASE_REF and that the base branch "
                "is fetched."
            ),
        )

    # PARTIAL blindness, which the guard above cannot see. base_lanes = 22 against
    # expected = 45 passes every total-emptiness check while Check B is most of the
    # way blind, and nothing about the numbers looks wrong.
    #
    # The test is EXACT rather than a ratio, deliberately. A floor like
    # `len(base_lanes) >= 0.8 * len(expected)` reds a PR that legitimately adds a
    # lot of workflows, and a gate that reds honest PRs is worse than the gap it
    # closes. So instead: for every lane the head expects and the base does not
    # produce, ask WHICH FILE produced it. A PR may legitimately introduce a lane
    # only by touching that lane's workflow file — adding the file, adding a job,
    # or widening a trigger — and all three appear in the PR's own diff. A lane
    # sourced from a file this PR never touched cannot legitimately be new, so the
    # base read must have come back short.
    #
    # Unreachable today: `fetch-depth: 0` keeps origin/main current and a bad
    # BASE_REF dies in merge-base first. But the workflow's `git fetch … || true` is
    # the tolerate-silently shape that produced the incident this gate exists for,
    # and "unreachable today" is the same footing the parked stub started from.
    unexplained = sorted(
        lane
        for lane in set(expected) - set(base_lanes)
        if expected.get(lane) not in pr_touched_workflow_files
        and expected.get(lane) not in base_skipped_files
    )
    if unexplained:
        return Verdict(
            ERROR,
            expected_count=len(expected),
            posted_count=len(posted),
            detail=(
                f"Check B is PARTIALLY blind: the base branch produced "
                f"{len(base_lanes)} lane(s) against {len(expected)} expected, and "
                f"{len(unexplained)} of the difference comes from workflow file(s) "
                "this PR never touched — so those lanes cannot legitimately be new "
                "and the base read must have come back short. First few: "
                + ", ".join(
                    f"{lane!r} (from {expected.get(lane)})" for lane in unexplained[:3]
                )
                + ". The base side is read at the MERGE-BASE, so main moving on "
                "cannot cause this; it means the fork-point tree could not be read "
                "in full. Check that the base branch is fetched deeply enough for "
                "`git merge-base` and `git show <fork>:...` to resolve."
            ),
        )
    if not posted:
        return Verdict(
            ERROR,
            expected_count=len(expected),
            detail=(
                "read ZERO `(pull_request)` contexts on the head. That is this gate's "
                "own blind spot one level up: either the entire event failed to "
                "schedule, or the status read is broken. Both need a human."
            ),
        )
    if not settled:
        return Verdict(
            UNSETTLED,
            expected_count=len(expected),
            posted_count=len(posted),
            detail=(
                "the posted set was still growing when the poll budget ran out. "
                "Absence is not evidence of absence here, so this run does not fail. "
                "Re-running once the wave settles gives a real verdict."
            ),
        )

    mine = {w.subject for w in waivers if w.pr_number == pr_number}
    removed_lanes = set(base_lanes) - set(expected)
    raw_missing = set(expected) - posted
    candidates = raw_missing | removed_lanes
    waived = sorted(mine & candidates)
    missing = sorted(raw_missing - mine)
    removed = sorted(removed_lanes - mine)
    unexpected = sorted(posted - set(expected))

    unused = sorted(mine - candidates)
    if unused:
        return Verdict(
            ERROR,
            expected_count=len(expected),
            posted_count=len(posted),
            missing=missing,
            removed=removed,
            waived=waived,
            unexpected=unexpected,
            detail=(
                "waiver(s) for this PR match nothing: "
                + ", ".join(repr(s) for s in unused)
                + ". A waiver that waives nothing is a guard covering nothing. "
                "Delete the line, or fix the string — it is compared verbatim "
                "against the exact event-stripped context name."
            ),
        )

    if missing or removed:
        return Verdict(
            MISSING,
            expected_count=len(expected),
            posted_count=len(posted),
            missing=missing,
            removed=removed,
            waived=waived,
            unexpected=unexpected,
            detail=(
                "nothing else in CI can see this: branch protection `['*']` "
                "constrains statuses that exist, and a workflow that never scheduled "
                "posts none."
            ),
        )
    return Verdict(
        OK,
        expected_count=len(expected),
        posted_count=len(posted),
        waived=waived,
        unexpected=unexpected,
    )


# ---------------------------------------------------------------------------
# Gitea I/O
# ---------------------------------------------------------------------------
class ApiError(RuntimeError):
    pass


class Gitea:
    def __init__(self, base_url: str, token: str, repo: str, *, timeout: int = 30):
        self.base = base_url.rstrip("/")
        self.token = token
        self.repo = repo
        self.timeout = timeout

    def _get(self, path: str, query: dict[str, str] | None = None):
        url = self.base + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        request = urllib.request.Request(
            url, headers={"Authorization": f"token {self.token}"}
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            raise ApiError(f"GET {path} -> HTTP {exc.code}") from exc
        except (urllib.error.URLError, TimeoutError, ValueError) as exc:
            raise ApiError(f"GET {path} -> {exc}") from exc

    def combined_statuses(self, sha: str, *, page_size: int = 50) -> list[dict]:
        """Every row of the COMBINED status for `sha`, paginated to exhaustion.

        `/commits/{sha}/status` — the combined endpoint — and NOT `/statuses/{sha}`,
        which is lossy on this deployment: it serves duplicates and reproducibly
        omits rows, so gating on it means gating on a number that is sometimes wrong
        in the direction that passes.

        Gitea's default page size is 30 and it IGNORES `per_page`; `limit` is the
        honoured parameter, so ask for it explicitly and stop on a short page. An
        empty page past the end returns zero rows with `state: pending`, which is NOT
        a real pending — this reader never touches the envelope's `state`, only
        `statuses`, so that trap cannot bite here.
        """
        rows: list[dict] = []
        seen: set[int] = set()
        page = 1
        while page <= 100:
            body = self._get(
                f"/repos/{self.repo}/commits/{sha}/status",
                {"page": str(page), "limit": str(page_size)},
            )
            if not isinstance(body, dict):
                raise ApiError(f"combined status for {sha} is not an object")
            chunk = body.get("statuses") or []
            rows.extend(r for r in chunk if status_numeric_id(r) not in seen)
            seen.update(status_numeric_id(r) for r in chunk)
            if len(chunk) < page_size:
                return rows
            page += 1
        raise ApiError(f"combined-status pagination did not terminate for {sha}")


# ---------------------------------------------------------------------------
# git
# ---------------------------------------------------------------------------
def git(*args: str) -> str:
    """Run git, decoding as UTF-8 explicitly.

    The encoding is not incidental. A workflow display name containing a non-ASCII
    character — `template-delivery-e2e`'s job name has an em-dash, and
    `design-token-drift`'s has a left-right arrow — must be derived byte-identical to
    the string Gitea composed, or the gate reports a phantom missing context and a
    phantom unexpected one for the same lane. Letting Python pick the platform
    default (cp1252 on this host) does exactly that.
    """
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="strict",
        check=False,
    )
    if result.returncode != 0:
        raise ApiError(
            f"git {' '.join(args)} -> exit {result.returncode}: "
            f"{(result.stderr or '').strip()[:400]}"
        )
    return result.stdout


def merge_base(base_ref: str, head_sha: str) -> str:
    """The commit where this PR forked from its base branch.

    EVERY base-side read in this gate resolves through here, and that is the whole
    point. An earlier version diffed `changed` against the merge-base but read the
    base WORKFLOW TREE from the base branch's TIP, so the two sides differed by the
    PR's diff PLUS everything main had done since the fork — and Check B attributed
    all of it to the PR. A workflow retired on main while a PR sat open then reded
    that PR, unwaivably, for someone else's commit. Measured: main retired a
    workflow in 15 distinct commits since 2026-05-01, four of them in one week.

    Comparing against the fork point makes Check B's premise true by construction,
    and states its intent better: "lanes that existed where this PR forked and are
    gone at its head" is exactly the set attributable to the PR.
    """
    return git("merge-base", base_ref, head_sha).strip()


def base_tree_at_fork(
    base_ref: str, head_sha: str, directory: str
) -> tuple[str, dict[str, str]]:
    """`(fork point, workflow tree there)` — Check B's base side, in one place.

    This exists as its own function so the choice of ref is reachable from a unit
    test. It is the single line that made the difference between a gate and a
    queue-blocker: reading the tree at `base_ref` (the branch TIP) instead of at the
    fork point attributes everything main did since the fork to the PR, and a
    workflow retired upstream then reds an innocent PR unwaivably. Buried inside
    `main()` alongside the network calls, that swap survived mutation testing
    because nothing could reach it.
    """
    fork_point = merge_base(base_ref, head_sha)
    return fork_point, read_workflow_tree_at(fork_point, directory)


def changed_files(fork_point: str, head_sha: str) -> list[str]:
    """Files this PR changes, measured from the fork point.

    Not from the base branch tip: main advances while a PR is open, and diffing
    against a moving tip attributes other people's merges to this PR, which would
    fire `paths:`-filtered workflows in the derivation that Gitea never scheduled.
    """
    out = git("diff", "--name-only", fork_point, head_sha)
    return [line.strip() for line in out.splitlines() if line.strip()]


def read_workflow_tree_at(ref: str, directory: str) -> dict[str, str]:
    """`{filename: text}` for the workflow files as they exist at `ref`.

    Read through git rather than from the working tree: the base branch is not
    checked out, and `git show` is the only thing that sees it without mutating
    anything the rest of the job depends on.

    The ref is resolved FIRST, and a failure there propagates. Without that,
    `ls-tree` failing for a ref that does not exist and `ls-tree` failing because
    the directory is absent are the same empty dict, and an unresolvable BASE_REF
    would silently reduce Check B to comparing nothing while still reporting OK.
    Resolving separately makes "I could not read the base branch" an error and
    leaves `{}` meaning only "this ref genuinely has no workflow directory".
    """
    git("rev-parse", "--verify", f"{ref}^{{commit}}")
    try:
        listing = git("ls-tree", "--name-only", f"{ref}:{directory}")
    except ApiError:
        return {}
    texts: dict[str, str] = {}
    for line in listing.splitlines():
        name = line.strip()
        if name.endswith((".yml", ".yaml")):
            texts[name] = git("show", f"{ref}:{directory}/{name}")
    return texts


def read_workflow_tree(directory: str) -> dict[str, str]:
    """`{filename: text}` for the workflow files checked out at head."""
    texts: dict[str, str] = {}
    if not os.path.isdir(directory):
        return texts
    for entry in sorted(os.listdir(directory)):
        if not entry.endswith((".yml", ".yaml")):
            continue
        with open(os.path.join(directory, entry), "r", encoding="utf-8") as handle:
            texts[entry] = handle.read()
    return texts


# ---------------------------------------------------------------------------
# Settlement
# ---------------------------------------------------------------------------
def settle_head(
    fetch,
    sha: str,
    *,
    stable_reads: int,
    poll_seconds: float,
    min_seconds: float,
    max_seconds: float,
    sleep=time.sleep,
    clock=time.monotonic,
) -> tuple[set[str], bool, list[int]]:
    """Poll until the posted set stops changing.

    Returns `(contexts, settled, size_history)`. The history is printed so a reader
    can watch the wave in the log instead of taking "settled" on trust.

    `min_seconds` is a wall-clock floor deliberately independent of stability: the
    first two reads of a head that has scheduled NOTHING yet are both empty and
    therefore trivially "stable". Requiring time to have passed as well is what stops
    an all-empty read from being mistaken for a settled one.
    """
    started = clock()
    history: list[int] = []
    previous: set[str] | None = None
    stable = 0
    contexts: set[str] = set()
    while True:
        contexts = posted_contexts(fetch(sha))
        history.append(len(contexts))
        stable = stable + 1 if contexts == previous else 1
        previous = contexts
        elapsed = clock() - started
        if stable >= stable_reads and elapsed >= min_seconds and contexts:
            return contexts, True, history
        if elapsed >= max_seconds:
            return contexts, False, history
        sleep(poll_seconds)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
def _env(name: str, default: str = "") -> str:
    return (os.environ.get(name) or default).strip()


def _int_env(name: str, default: int) -> int:
    raw = _env(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        raise SystemExit(f"{name}={raw!r} is not an integer") from None


def _read_text(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return handle.read()
    except FileNotFoundError:
        return ""


def _report(result: Verdict) -> None:
    print("")
    print(result.summary())
    if result.detail:
        print(f"  {result.detail}")
    for context in result.missing:
        print(f"  DERIVED BUT NEVER POSTED : {context}")
    for name in result.removed:
        print(f"  LANE REMOVED BY THIS PR  : {name}")
    for subject in result.waived:
        print(f"  waived                   : {subject}")
    for context in result.unexpected:
        print(f"  posted but not derived   : {context}")
    if result.verdict == MISSING:
        print("")
        print(
            "::error::A lane this PR should have run did not run. Something stopped "
            "CI scheduling it: a YAML parse error (a duplicate mapping key is the "
            "one that did this for real in core#5153), a narrowed `on:`, `types:`, "
            "`branches:` or `paths:` filter, a renamed job, or a deleted workflow "
            "file. If the removal is intended, declare it in "
            ".gitea/context-shrink-waivers.txt with this PR's number and a reason."
        )


def main(argv: list[str] | None = None) -> int:
    del argv
    repo = _env("REPO")
    token = _env("GITEA_TOKEN")
    host = _env("GITEA_HOST", "git.moleculesai.app")
    api_url = _env("GITEA_API_URL", f"https://{host}/api/v1")
    head_sha = _env("HEAD_SHA")
    base_ref = _env("BASE_REF", "origin/main")
    base_branch = _env("BASE_BRANCH", "main")
    waivers_path = _env("WAIVERS_FILE", ".gitea/context-shrink-waivers.txt")
    # NOTE: WORKFLOWS_DIR is used BOTH as a filesystem path (the checked-out head
    # tree) and as a git pathspec after a `<ref>:` prefix (the base tree). The two
    # agree for a plain repo-relative path with forward slashes, which is the only
    # form this repo uses and the only form documented here. An absolute path, or a
    # Windows-style separator, would satisfy one reader and not the other.
    workflows_dir = _env("WORKFLOWS_DIR", ".gitea/workflows")
    try:
        pr_number = int(_env("PR_NUMBER", "0"))
    except ValueError:
        pr_number = 0

    for name, value in (("REPO", repo), ("GITEA_TOKEN", token), ("HEAD_SHA", head_sha)):
        if not value:
            print(f"::error::{name} is empty — cannot run. Failing closed.")
            return _EXIT[ERROR]
    if not pr_number:
        print("::error::PR_NUMBER is empty or zero — waivers could not be scoped.")
        return _EXIT[ERROR]

    try:
        waivers = parse_waivers(_read_text(waivers_path))
    except WaiverFileError as exc:
        print(f"::error::{waivers_path}: {exc}")
        return _EXIT[ERROR]

    api = Gitea(api_url, token, repo)
    try:
        fork_point, base_texts = base_tree_at_fork(base_ref, head_sha, workflows_dir)
        changed = changed_files(fork_point, head_sha)
        head_texts = read_workflow_tree(workflows_dir)
        expected, _ = derive_expected(
            head_texts, base_branch=base_branch, changed=changed
        )
        touched = workflow_files_in_diff(changed, workflows_dir)
        print(
            f"derivation: {len(head_texts)} workflow file(s) at head, "
            f"{len(changed)} changed path(s) vs {base_ref}, "
            f"base branch {base_branch!r} -> {len(expected)} expected context(s)"
        )
        # Check B. Derive the SAME thing from the base branch — same event, same
        # changed-file list — so the two sides are comparable by construction, and
        # the difference is exactly "lanes this PR removed".
        base_expected, base_skipped = derive_expected(
            base_texts,
            base_branch=base_branch,
            changed=changed,
            tolerate_unreadable=True,
        )
        print(
            f"base lanes: {len(base_texts)} workflow file(s) at fork point "
            f"{fork_point[:10]} -> "
            f"{len(base_expected)} lane(s) for this same diff, "
            f"{len(set(base_expected) - set(expected))} no longer derivable at head"
            + (f" ({len(base_skipped)} base file(s) unreadable, skipped)"
               if base_skipped else "")
        )
        posted, settled, history = settle_head(
            lambda sha: api.combined_statuses(sha),
            head_sha,
            stable_reads=_int_env("CSG_STABLE_READS", 3),
            poll_seconds=_int_env("CSG_POLL_SECONDS", 10),
            min_seconds=_int_env("CSG_MIN_SETTLE_SECONDS", 45),
            max_seconds=_int_env("CSG_MAX_SETTLE_SECONDS", 300),
        )
        print(f"head {head_sha[:10]}: settle poll sizes {history} settled={settled}")
    except WorkflowUnreadable as exc:
        print(f"::error::a workflow file at head will not parse: {exc}")
        print(
            "context-shrink: verdict=ERROR expected=0 posted=0 missing=0 "
            "(cannot derive from an unreadable workflow)"
        )
        return _EXIT[ERROR]
    except ApiError as exc:
        print(f"::error::{exc}")
        print("context-shrink: verdict=ERROR expected=0 posted=0 missing=0 (read failed)")
        return _EXIT[ERROR]

    result = decide(
        expected=expected,
        posted=posted,
        base_lanes=base_expected,
        base_has_workflows=bool(base_texts),
        pr_touched_workflow_files=touched,
        base_skipped_files=set(base_skipped),
        waivers=waivers,
        pr_number=pr_number,
        settled=settled,
        head_has_workflows=bool(head_texts),
    )
    _report(result)
    return result.exit_code


if __name__ == "__main__":
    sys.exit(main())
