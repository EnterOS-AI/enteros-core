"""Tests for lint_context_set_shrink.

Every fixture below was checked against the question "what WRONG implementation
would also pass this?", and several are deliberately asymmetric because the
obvious symmetric version answered "quite a few".
"""

import importlib.util
import sys
from pathlib import Path

import pytest

SCRIPT_PATH = Path(__file__).resolve().parent.parent / "lint_context_set_shrink.py"
_spec = importlib.util.spec_from_file_location("lcss", SCRIPT_PATH)
lcss = importlib.util.module_from_spec(_spec)
# Register BEFORE exec: `@dataclass` resolves annotations through
# `sys.modules[cls.__module__]`, which is None for a module that was created but
# never registered. Mirrors the pattern in test_gitea_merge_queue.py.
sys.modules[_spec.name] = lcss
_spec.loader.exec_module(lcss)


# ---------------------------------------------------------------------------
# Context-name splitting
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "raw, name, event",
    [
        ("CI / all-required (pull_request)", "CI / all-required", "pull_request"),
        ("CI / all-required (push)", "CI / all-required", "push"),
        (
            "audit-force-merge / audit (pull_request_target)",
            "audit-force-merge / audit",
            "pull_request_target",
        ),
        (
            "gitea-merge-queue / queue (pull_request_review)",
            "gitea-merge-queue / queue",
            "pull_request_review",
        ),
        # No suffix at all: `reserved-path-review` is posted as a bare verdict.
        ("reserved-path-review", "reserved-path-review", ""),
    ],
)
def test_split_event_strips_exactly_the_event_suffix(raw, name, event):
    assert lcss.split_event(raw) == (name, event)


@pytest.mark.parametrize(
    "raw",
    [
        # Real job names that END in parentheses. A greedy `\(.*\)$` matcher eats
        # these, silently renaming the lane and making the gate report a phantom
        # missing context for a job that posted perfectly well.
        "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)",
        "CI / Canvas (Next.js)",
        "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge (compile+skip)",
        "Lint workflow YAML (repository compatibility policy) / Lint workflow YAML",
    ],
)
def test_split_event_leaves_parenthesised_job_names_intact(raw):
    assert lcss.split_event(raw) == (raw, "")


def test_split_event_strips_only_the_outermost_event_suffix():
    """Both features in one string: a parenthesised job name AND an event."""
    raw = "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub) (pull_request)"
    assert lcss.split_event(raw) == (
        "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)",
        "pull_request",
    )


# ---------------------------------------------------------------------------
# Status reduction
# ---------------------------------------------------------------------------
def _row(id_, context, status="success"):
    return {"id": id_, "context": context, "status": status}


def test_latest_by_context_reduces_by_id_not_by_list_order():
    """Deliberately anti-sorted: the NEWEST row is FIRST in the list.

    An implementation that trusted list order (or `reversed()`) would pick the
    oldest row and still return a plausible-looking dict, so the fixture is ordered
    to make that choice visibly wrong.
    """
    rows = [
        _row(99, "CI / all-required (pull_request)", "success"),
        _row(12, "CI / all-required (pull_request)", "pending"),
        _row(50, "CI / all-required (pull_request)", "failure"),
    ]
    latest = lcss.latest_by_context(rows)
    assert latest["CI / all-required (pull_request)"]["status"] == "success"


def test_status_numeric_id_rejects_booleans():
    """`True == 1` in Python, so a bool id would sort as a real, very old id."""
    assert lcss.status_numeric_id({"id": True}) == -1
    assert lcss.status_numeric_id({"id": 7}) == 7
    assert lcss.status_numeric_id({"id": None}) == -1


def test_posted_contexts_keeps_only_pull_request_and_strips_the_suffix():
    """Asymmetric on purpose: each event class appears exactly once, and the
    pull_request entry is NOT the first row, so 'return everything' and 'return
    the first' both fail."""
    rows = [
        _row(1, "audit-force-merge / audit (pull_request_target)"),
        _row(2, "main-canary / canary (push)"),
        _row(3, "CI / all-required (pull_request)"),
        _row(4, "gitea-merge-queue / queue (pull_request_review)"),
        _row(5, "reserved-path-review"),
    ]
    assert lcss.posted_contexts(rows) == {"CI / all-required"}


def test_posted_contexts_ignores_state_entirely():
    """Presence, never state. A pending and a failure both COUNT as posted."""
    rows = [
        _row(1, "A / a (pull_request)", "pending"),
        _row(2, "B / b (pull_request)", "failure"),
    ]
    assert lcss.posted_contexts(rows) == {"A / a", "B / b"}


# ---------------------------------------------------------------------------
# Path globbing
# ---------------------------------------------------------------------------
@pytest.mark.parametrize(
    "pattern, path, expected",
    [
        # `*` must NOT cross a separator - this is the case fnmatch gets wrong.
        ("*.md", "README.md", True),
        ("*.md", "docs/design/rfc.md", False),
        # `**` must cross separators.
        (".gitea/workflows/**", ".gitea/workflows/ci.yml", True),
        (".gitea/workflows/**", ".gitea/workflows/nested/deep/ci.yml", True),
        (".gitea/workflows/**", ".gitea/scripts/ci.py", False),
        # `a/**/b` also matches `a/b` with the middle segment empty.
        ("canvas/**/page.tsx", "canvas/page.tsx", True),
        ("canvas/**/page.tsx", "canvas/app/dash/page.tsx", True),
        # `?` is a single non-separator character.
        ("v?/x", "v1/x", True),
        ("v?/x", "v/1/x", False),
        # Literal dots are not wildcards.
        ("a.b", "axb", False),
    ],
)
def test_path_matches(pattern, path, expected):
    assert lcss.path_matches(pattern, path) is expected


# ---------------------------------------------------------------------------
# Trigger parsing
# ---------------------------------------------------------------------------
def test_workflow_triggers_reads_the_yaml_bool_on_key():
    """THE trap. YAML 1.1 resolves a bare `on` to the boolean True, so PyYAML
    stores the trigger block under `True` and NOT under `"on"`.

    This is parsed from real YAML text rather than hand-built as a dict, because a
    hand-built `{"on": ...}` fixture would pass against an implementation that only
    ever looks up the string — which derives NOTHING from ANY real workflow, and
    fails green.
    """
    import yaml

    doc = yaml.safe_load("on:\n  pull_request:\n    types: [opened]\njobs: {}\n")
    assert True in doc and "on" not in doc, "fixture no longer exercises the trap"
    assert lcss.workflow_triggers(doc) == {"pull_request": {"types": ["opened"]}}


@pytest.mark.parametrize(
    "text, expected",
    [
        ("on: pull_request\n", {"pull_request": {}}),
        ("on: [push, pull_request]\n", {"push": {}, "pull_request": {}}),
        ("on:\n  push:\n    branches: [main]\n", {"push": {"branches": ["main"]}}),
        # A mapping whose value is null - `pull_request:` with nothing under it.
        ("on:\n  pull_request:\n", {"pull_request": {}}),
    ],
)
def test_workflow_triggers_normalises_every_form(text, expected):
    import yaml

    assert lcss.workflow_triggers(yaml.safe_load(text)) == expected


def _doc(text):
    import yaml

    return yaml.safe_load(text)


@pytest.mark.parametrize(
    "text, changed, fires",
    [
        # No pull_request trigger at all - push-only workflows like main-canary.
        ("on:\n  push:\n    branches: [main]\njobs: {}\n", ["a.go"], False),
        # Plain pull_request with no filters always fires.
        ("on:\n  pull_request:\njobs: {}\n", ["a.go"], True),
        # types: excluding every push-shaped type (audit-force-merge is [closed]).
        ("on:\n  pull_request:\n    types: [closed]\njobs: {}\n", ["a.go"], False),
        # types: including synchronize fires.
        ("on:\n  pull_request:\n    types: [closed, synchronize]\njobs: {}\n", ["a.go"], True),
        # branches: that does not include the base branch.
        ("on:\n  pull_request:\n    branches: [staging]\njobs: {}\n", ["a.go"], False),
        ("on:\n  pull_request:\n    branches: [main, staging]\njobs: {}\n", ["a.go"], True),
        # paths: matching / not matching the PR's real diff.
        (
            "on:\n  pull_request:\n    paths: ['.gitea/workflows/**']\njobs: {}\n",
            ["workspace-server/main.go"],
            False,
        ),
        (
            "on:\n  pull_request:\n    paths: ['.gitea/workflows/**']\njobs: {}\n",
            ["workspace-server/main.go", ".gitea/workflows/ci.yml"],
            True,
        ),
        # paths-ignore suppresses only when EVERY changed file is ignored.
        ("on:\n  pull_request:\n    paths-ignore: ['docs/**']\njobs: {}\n", ["docs/a.md"], False),
        (
            "on:\n  pull_request:\n    paths-ignore: ['docs/**']\njobs: {}\n",
            ["docs/a.md", "main.go"],
            True,
        ),
    ],
)
def test_fires_on_pull_request(text, changed, fires):
    assert lcss.fires_on_pull_request(_doc(text), base_branch="main", changed=changed) is fires


# ---------------------------------------------------------------------------
# Context derivation
# ---------------------------------------------------------------------------
def test_workflow_contexts_prefers_job_name_and_falls_back_to_the_key():
    """Asymmetric: one job HAS a `name:`, the other does not. A fixture where both
    had names could not tell `job.name` from `job.name or key`."""
    doc = _doc(
        "name: Local Provision Lifecycle E2E\n"
        "jobs:\n"
        "  lifecycle-stub:\n"
        "    name: Local Provision Lifecycle E2E (stub)\n"
        "  detect:\n"
        "    runs-on: ubuntu-latest\n"
    )
    assert lcss.workflow_contexts(doc, "local-provision-e2e.yml") == {
        "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)",
        "Local Provision Lifecycle E2E / detect",
    }


def test_workflow_contexts_falls_back_to_the_filename_when_unnamed():
    doc = _doc("jobs:\n  build:\n    runs-on: ubuntu-latest\n")
    assert lcss.workflow_contexts(doc, "nameless.yml") == {"nameless.yml / build"}


def test_workflow_contexts_includes_conditional_jobs():
    """Gitea posts a context for a job that SKIPS, so `if:` and `needs:` must not
    filter the derivation. Measured: `CI / Platform (Go)` gates behind
    `needs: detect-changes` and still posts on a one-file manifest-only PR."""
    doc = _doc(
        "name: CI\n"
        "jobs:\n"
        "  detect-changes:\n"
        "    name: Detect changes\n"
        "  platform:\n"
        "    name: Platform (Go)\n"
        "    needs: detect-changes\n"
        "    if: needs.detect-changes.outputs.go == 'true'\n"
    )
    assert lcss.workflow_contexts(doc, "ci.yml") == {
        "CI / Detect changes",
        "CI / Platform (Go)",
    }


def test_derive_expected_end_to_end():
    """Three workflows: one fires, one is push-only, one is filtered out by paths.
    All three differ, so a derivation that returned everything - or nothing - fails.
    """
    tree = {
        "fires.yml": "name: Fires\non:\n  pull_request:\njobs:\n  a:\n    name: A\n",
        "push-only.yml": "name: PushOnly\non:\n  push:\njobs:\n  b:\n    name: B\n",
        "filtered.yml": (
            "name: Filtered\non:\n  pull_request:\n    paths: ['canvas/**']\n"
            "jobs:\n  c:\n    name: C\n"
        ),
    }
    assert lcss.derive_expected(tree, base_branch="main", changed=["main.go"]) == {
        "Fires / A"
    }
    # Same tree, a diff that DOES touch canvas: the filtered lane joins the set.
    assert lcss.derive_expected(
        tree, base_branch="main", changed=["canvas/page.tsx"]
    ) == {"Fires / A", "Filtered / C"}


def test_duplicate_env_key_still_derives_the_lane():
    """The regression that motivated the whole gate (core#5153).

    `yaml.safe_load` ACCEPTS a duplicate mapping key and keeps the last one, while
    Gitea's parser REJECTS the document and schedules nothing. That asymmetry is
    exactly why this gate works on the duplicate-key case: the derivation still sees
    the lane and expects it, and the expectation goes unmet.

    If PyYAML ever started rejecting duplicate keys, this test would fail loudly
    rather than the gate quietly going blind.
    """
    text = (
        "name: Local Provision Lifecycle E2E\n"
        "on:\n"
        "  pull_request:\n"
        "    branches: [main, staging]\n"
        "jobs:\n"
        "  lifecycle-stub:\n"
        "    name: Local Provision Lifecycle E2E (stub)\n"
        "    steps:\n"
        "      - name: Fetch\n"
        "        env:\n"
        "          INFISICAL_CI_CLIENT_ID: x\n"
        "        env:\n"
        "          TRANSIENT_LEDGER: /tmp/ledger.txt\n"
    )
    expected = lcss.derive_expected(
        {"local-provision-e2e.yml": text}, base_branch="main", changed=["a.go"]
    )
    assert expected == {
        "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)"
    }


def test_unparseable_workflow_raises_rather_than_deriving_nothing():
    """A file the gate cannot read is a file whose lanes it cannot vouch for.
    Silently skipping it would shrink the expectation - a vacuous pass."""
    with pytest.raises(lcss.WorkflowUnreadable):
        lcss.derive_expected(
            {"broken.yml": "name: X\n  bad: [unclosed\n"},
            base_branch="main",
            changed=["a.go"],
        )


# ---------------------------------------------------------------------------
# Waivers
# ---------------------------------------------------------------------------
def test_parse_waivers_accepts_a_documented_entry():
    waivers = lcss.parse_waivers(
        "# retiring the staging concierge lane, see core#86\n"
        "5199  E2E Staging SaaS (full lifecycle) / E2E Staging SaaS\n"
    )
    assert len(waivers) == 1
    assert waivers[0].pr_number == 5199
    assert waivers[0].subject == "E2E Staging SaaS (full lifecycle) / E2E Staging SaaS"
    assert "core#86" in waivers[0].reason


def test_parse_waivers_rejects_an_undocumented_entry():
    with pytest.raises(lcss.WaiverFileError, match="no reason"):
        lcss.parse_waivers("5199  A / b\n")


def test_parse_waivers_rejects_a_reason_separated_by_a_blank_line():
    """A blank line breaks the association, so the comment is documentation for
    something else. Accepting it would let an entry inherit an unrelated reason."""
    with pytest.raises(lcss.WaiverFileError, match="no reason"):
        lcss.parse_waivers("# a reason for something else\n\n5199  A / b\n")


def test_parse_waivers_rejects_an_event_suffixed_context():
    with pytest.raises(lcss.WaiverFileError, match="event-STRIPPED"):
        lcss.parse_waivers("# reason\n5199  A / b (pull_request)\n")


def test_parse_waivers_rejects_a_line_without_a_pr_number():
    with pytest.raises(lcss.WaiverFileError, match="expected"):
        lcss.parse_waivers("# reason\nA / b\n")


def test_parse_waivers_ignores_a_comment_only_file():
    assert lcss.parse_waivers("# nothing here\n#\n") == []


# ---------------------------------------------------------------------------
# decide()
# ---------------------------------------------------------------------------
def _decide(**kw):
    args = dict(
        expected={"A / a", "B / b"},
        posted={"A / a", "B / b"},
        deleted_workflows=set(),
        waivers=[],
        pr_number=1,
        settled=True,
        head_has_workflows=True,
    )
    args.update(kw)
    return lcss.decide(**args)


def test_decide_ok_reports_the_counts():
    v = _decide()
    assert (v.verdict, v.exit_code) == (lcss.OK, 0)
    assert v.expected_count == 2 and v.posted_count == 2
    assert "expected=2 posted=2 missing=0" in v.summary()


def test_decide_flags_a_derived_context_that_never_posted():
    v = _decide(posted={"A / a"})
    assert (v.verdict, v.exit_code) == (lcss.MISSING, 1)
    assert v.missing == ["B / b"]
    assert "missing=1" in v.summary()


def test_decide_flags_a_deleted_workflow_file():
    """Check B. The expectation cannot see a deletion - delete the file and the
    derived context disappears with it - so deletion is checked separately."""
    v = _decide(deleted_workflows={"e2e-staging-saas.yml"})
    assert (v.verdict, v.exit_code) == (lcss.MISSING, 1)
    assert v.deleted == ["e2e-staging-saas.yml"]
    assert v.missing == []


def test_decide_empty_posted_is_an_error_not_a_pass():
    v = _decide(posted=set())
    assert (v.verdict, v.exit_code) == (lcss.ERROR, 2)
    assert "ZERO" in v.detail


def test_decide_empty_derivation_from_a_populated_tree_is_an_error():
    """The shape a broken `on:` reader takes. Its failure direction is a silent
    GREEN - every posted context trivially satisfies an empty expectation - so it
    gets an assertion rather than trust."""
    v = _decide(expected=set(), head_has_workflows=True)
    assert (v.verdict, v.exit_code) == (lcss.ERROR, 2)
    assert "ZERO expected" in v.detail


def test_decide_empty_derivation_is_fine_when_there_are_no_workflows():
    v = _decide(expected=set(), head_has_workflows=False)
    assert v.verdict == lcss.OK


def test_decide_unsettled_never_reds_even_with_a_missing_context():
    """Mid-flight absence is not evidence of absence. UNSETTLED must outrank a
    missing context, or the gate reds every PR it happens to read early."""
    v = _decide(posted={"A / a"}, settled=False)
    assert (v.verdict, v.exit_code) == (lcss.UNSETTLED, 0)
    assert v.missing == []


def test_decide_honours_a_waiver_for_this_pr():
    waiver = lcss.Waiver(pr_number=42, subject="B / b", line_number=1, reason="r")
    v = _decide(posted={"A / a"}, waivers=[waiver], pr_number=42)
    assert (v.verdict, v.exit_code) == (lcss.OK, 0)
    assert v.waived == ["B / b"] and v.missing == []


def test_decide_ignores_a_waiver_belonging_to_another_pr():
    """Same waiver, same missing context, DIFFERENT pr_number. The only variable
    that changes between this test and the one above is the scope."""
    waiver = lcss.Waiver(pr_number=42, subject="B / b", line_number=1, reason="r")
    v = _decide(posted={"A / a"}, waivers=[waiver], pr_number=99)
    assert (v.verdict, v.exit_code) == (lcss.MISSING, 1)
    assert v.missing == ["B / b"]


def test_decide_rejects_a_waiver_that_waives_nothing():
    waiver = lcss.Waiver(pr_number=42, subject="C / c", line_number=1, reason="r")
    v = _decide(waivers=[waiver], pr_number=42)
    assert (v.verdict, v.exit_code) == (lcss.ERROR, 2)
    assert "waives nothing" in v.detail


def test_decide_does_not_red_on_an_unexpected_extra_context():
    """A context that posted without being derived is reported, never fatal: the
    gate's job is lanes that vanished, not lanes that appeared."""
    v = _decide(posted={"A / a", "B / b", "C / c"})
    assert (v.verdict, v.exit_code) == (lcss.OK, 0)
    assert v.unexpected == ["C / c"]


# ---------------------------------------------------------------------------
# Settlement
# ---------------------------------------------------------------------------
class _Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


def _rows(names):
    return [_row(i, f"{n} (pull_request)") for i, n in enumerate(names, start=1)]


def test_settle_head_waits_for_the_set_to_stop_growing():
    """The wave: 2 contexts, then 5, then steady. A reader that took the first
    result would see 2 and report three phantom missing contexts."""
    waves = [["a", "b"], ["a", "b", "c", "d", "e"]] + [["a", "b", "c", "d", "e"]] * 6
    calls = iter(waves)
    clock = _Clock()
    contexts, settled, history = lcss.settle_head(
        lambda sha: _rows(next(calls)),
        "sha",
        stable_reads=3,
        poll_seconds=10,
        min_seconds=45,
        max_seconds=300,
        sleep=clock.sleep,
        clock=clock,
    )
    assert settled is True
    assert contexts == {"a", "b", "c", "d", "e"}
    assert history[0] == 2 and history[-1] == 5


def test_settle_head_does_not_call_an_all_empty_head_settled_immediately():
    """Two empty reads are trivially 'stable'. Only the wall-clock floor stops that
    from being mistaken for a settled, genuinely-empty head - which would then be
    compared against a full expectation and red everything."""
    clock = _Clock()
    contexts, settled, history = lcss.settle_head(
        lambda sha: [],
        "sha",
        stable_reads=2,
        poll_seconds=10,
        min_seconds=45,
        max_seconds=60,
        sleep=clock.sleep,
        clock=clock,
    )
    assert settled is False and contexts == set()
    assert clock.now >= 60, "gave up before the floor elapsed"


def test_settle_head_gives_up_and_reports_unsettled():
    """A set that never stops changing must time out, not spin forever."""
    counter = iter(range(1, 500))
    clock = _Clock()
    _, settled, history = lcss.settle_head(
        lambda sha: _rows([f"c{i}" for i in range(next(counter))]),
        "sha",
        stable_reads=3,
        poll_seconds=10,
        min_seconds=0,
        max_seconds=50,
        sleep=clock.sleep,
        clock=clock,
    )
    assert settled is False
    assert len(history) >= 5


# ---------------------------------------------------------------------------
# Pagination
# ---------------------------------------------------------------------------
class _FakeGitea(lcss.Gitea):
    def __init__(self, pages):
        super().__init__("http://x/api/v1", "t", "o/r")
        self.pages = pages
        self.queries = []

    def _get(self, path, query=None):
        self.queries.append(dict(query or {}))
        index = int((query or {}).get("page", "1")) - 1
        rows = self.pages[index] if index < len(self.pages) else []
        return {"state": "pending", "statuses": rows}


def test_combined_statuses_paginates_to_exhaustion():
    """Two full pages then a short one. Stopping at the first page would silently
    drop 50 contexts and manufacture a shrink."""
    page_size = 3
    pages = [
        [_row(1, "a (pull_request)"), _row(2, "b (pull_request)"), _row(3, "c (pull_request)")],
        [_row(4, "d (pull_request)"), _row(5, "e (pull_request)"), _row(6, "f (pull_request)")],
        [_row(7, "g (pull_request)")],
    ]
    api = _FakeGitea(pages)
    rows = api.combined_statuses("sha", page_size=page_size)
    assert len(rows) == 7
    assert all(q.get("limit") == "3" for q in api.queries), "must send `limit`"
    assert not any("per_page" in q for q in api.queries), "`per_page` is ignored by Gitea"


def test_combined_statuses_does_not_read_the_envelope_state():
    """An empty page past the end returns zero rows with `state: pending`. That is
    not a real pending, and this reader must never look at it."""
    api = _FakeGitea([[]])
    assert api.combined_statuses("sha", page_size=50) == []
