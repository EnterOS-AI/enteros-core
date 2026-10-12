"""Unit tests for lint_no_coe_on_required — fixture catch + clean."""
import importlib.util
import os
import textwrap

HERE = os.path.dirname(__file__)
SCRIPT = os.path.join(HERE, "..", ".gitea", "scripts", "lint_no_coe_on_required.py")
spec = importlib.util.spec_from_file_location("lint_no_coe_on_required", SCRIPT)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def _wf(tmp_path, name, body):
    d = tmp_path / ".gitea" / "workflows"
    d.mkdir(parents=True, exist_ok=True)
    (d / name).write_text(textwrap.dedent(body))


def _allow(tmp_path, contexts):
    (tmp_path / ".gitea").mkdir(parents=True, exist_ok=True)
    (tmp_path / ".gitea" / "required-contexts.txt").write_text("\n".join(contexts) + "\n")


def test_coe_on_required_job_flagged(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          all-required:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo gate
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    info = ctxs["CI / all-required"]
    assert info[2] is True  # continue-on-error detected


def test_coe_string_true_flagged(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          gate:
            runs-on: ubuntu-latest
            continue-on-error: "true"
            steps:
              - run: echo hi
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert ctxs["CI / gate"][2] is True


def test_required_job_without_coe_clean(tmp_path):
    _wf(tmp_path, "ci.yml", """\
        name: CI
        on: [pull_request]
        jobs:
          all-required:
            runs-on: ubuntu-latest
            steps:
              - run: echo gate
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert ctxs["CI / all-required"][2] is False


def test_named_job_context_uses_name_not_key(tmp_path):
    _wf(tmp_path, "e2e.yml", """\
        name: E2E API Smoke Test
        on: [pull_request]
        jobs:
          e2e-api:
            name: E2E API Smoke Test
            runs-on: ubuntu-latest
            steps:
              - run: echo hi
    """)
    ctxs = mod.job_contexts(str(tmp_path / ".gitea" / "workflows"))
    assert "E2E API Smoke Test / E2E API Smoke Test" in ctxs


def test_strip_event_suffix():
    assert mod.strip_event("CI / all-required (pull_request)") == "CI / all-required"
    assert mod.strip_event("ci / build (push)") == "ci / build"
    assert mod.strip_event("X / y") == "X / y"


def test_allowlist_load(tmp_path):
    _allow(tmp_path, ["# comment", "CI / all-required", "  ci / build (push)  "])
    got = mod.load_required_allowlist(str(tmp_path / ".gitea" / "required-contexts.txt"))
    assert got == {"CI / all-required", "ci / build"}


# ── H3: the required-set is every PR-posted context, not just the ~8 in
# .gitea/required-contexts.txt. BP is ["*"] — every posted status is
# merge-blocking. These tests pin the widening, and each is a NEGATIVE
# CONTROL: every one of them PASSES (i.e. fails to catch the mask) against
# the pre-H3 allowlist-only lint.
# ──────────────────────────────────────────────────────────────────────

def _run_main(monkeypatch, tmp_path, waivers=None):
    """Invoke main() against a tmp workflows dir + allowlist."""
    monkeypatch.setattr(mod, "WORKFLOWS_DIR", str(tmp_path / ".gitea" / "workflows"))
    monkeypatch.setattr(mod, "REQUIRED_FILE", str(tmp_path / ".gitea" / "required-contexts.txt"))
    # No token -> live_required_contexts() degrades to None, no BP call.
    monkeypatch.setattr(mod, "GITEA_TOKEN", "")
    monkeypatch.setattr(mod, "REPO", "")
    monkeypatch.setattr(mod, "MASK_WAIVERS", waivers if waivers is not None else {})
    return mod.main()


def test_masked_pr_job_absent_from_allowlist_now_FAILS(tmp_path, monkeypatch):
    """THE H3 REGRESSION TEST.

    A masked job on a pull_request workflow that is NOT listed in
    required-contexts.txt. The old lint scoped `required` to the allowlist,
    so it returned 0 — green — while the job could not go red at all.
    Under BP ["*"] that job's status IS merge-blocking. Must now fail.
    """
    _allow(tmp_path, ["CI / all-required"])  # deliberately does NOT list the job below
    _wf(tmp_path, "harness.yml", """\
        name: Harness Replays
        on: [pull_request]
        jobs:
          harness-replays:
            name: Harness Replays
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: ./run-all-replays.sh
    """)
    assert _run_main(monkeypatch, tmp_path) == 1


def test_masked_pr_detector_job_now_FAILS(tmp_path, monkeypatch):
    """A masked DETECTOR is the worst case: on failure its outputs are empty,
    every downstream step no-ops, and the gate reports a silent green."""
    _allow(tmp_path, ["CI / all-required"])
    _wf(tmp_path, "harness.yml", """\
        name: Harness Replays
        on: [pull_request]
        jobs:
          detect-changes:
            runs-on: docker-host
            continue-on-error: true
            outputs:
              run: ${{ steps.decide.outputs.run }}
            steps:
              - id: decide
                run: echo run=true >> "$GITHUB_OUTPUT"
    """)
    assert _run_main(monkeypatch, tmp_path) == 1


def test_unquoted_on_key_is_still_seen_as_pr_triggered(tmp_path, monkeypatch):
    """PyYAML (YAML 1.1) resolves an UNQUOTED `on:` key to the boolean True.

    Half this repo's workflows use bare `on:` and half use quoted `"on":`.
    A lint that only does doc.get("on") is silently blind to the bare-`on:`
    half — which would make THIS lint the very thing it polices. Negative
    control: delete the doc.get(True) fallback in workflow_events() and this
    test goes green-when-it-should-be-red.
    """
    _allow(tmp_path, [])
    _wf(tmp_path, "bare.yml", """\
        name: Bare On
        on:
          pull_request:
            paths: ['x/**']
        jobs:
          lint:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo hi
    """)
    import yaml as _y
    doc = _y.safe_load((tmp_path / ".gitea" / "workflows" / "bare.yml").read_text())
    assert "on" not in doc and True in doc, "precondition: PyYAML folds bare `on:` to True"
    assert mod.workflow_events(doc) == {"pull_request"}
    assert _run_main(monkeypatch, tmp_path) == 1


def test_quoted_on_key_is_seen_as_pr_triggered(tmp_path, monkeypatch):
    _allow(tmp_path, [])
    _wf(tmp_path, "quoted.yml", """\
        name: Quoted On
        "on":
          pull_request:
            branches: [main]
        jobs:
          job:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo hi
    """)
    assert _run_main(monkeypatch, tmp_path) == 1


def test_pull_request_target_also_posts_a_pr_context(tmp_path, monkeypatch):
    _allow(tmp_path, [])
    _wf(tmp_path, "prt.yml", """\
        name: PRT
        on:
          pull_request_target:
            types: [opened]
        jobs:
          gate:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo hi
    """)
    assert _run_main(monkeypatch, tmp_path) == 1


def test_push_only_masked_job_is_NOT_widened_in(tmp_path, monkeypatch):
    """Guard the PROPERTY, not the shape: the widening is 'posts a PR status',
    NOT 'has continue-on-error anywhere'. A push-only workflow posts no PR
    context, so a mask there is out of scope for this lint. Without this test
    an over-broad implementation (flag every coe job) would still pass every
    other test in this file."""
    _allow(tmp_path, [])
    _wf(tmp_path, "pushonly.yml", """\
        name: Push Only
        on:
          push:
            branches: [main]
        jobs:
          sweeper:
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo hi
    """)
    assert _run_main(monkeypatch, tmp_path) == 0


def test_pr_job_without_mask_is_clean(tmp_path, monkeypatch):
    _allow(tmp_path, [])
    _wf(tmp_path, "clean.yml", """\
        name: Clean
        on: [pull_request]
        jobs:
          gate:
            runs-on: ubuntu-latest
            steps:
              - run: echo hi
    """)
    assert _run_main(monkeypatch, tmp_path) == 0


def test_explicit_waiver_lets_a_mask_through(tmp_path, monkeypatch):
    _allow(tmp_path, [])
    _wf(tmp_path, "adv.yml", """\
        name: Advisory Lane
        on: [pull_request]
        jobs:
          lane:
            name: advisory
            runs-on: ubuntu-latest
            continue-on-error: true
            steps:
              - run: echo hi
    """)
    # Unwaived -> red. Waived -> green. Both arms asserted: a waiver that
    # never changes the outcome would not be a waiver.
    assert _run_main(monkeypatch, tmp_path) == 1
    assert _run_main(monkeypatch, tmp_path,
                     waivers={"Advisory Lane / advisory": "tracked in #123"}) == 0


def test_stale_waiver_warns_but_does_not_fail(tmp_path, monkeypatch, capsys):
    """A waiver for a mask that no longer exists must NOT hard-fail: the
    in-flight PRs removing those masks would otherwise dead-lock against this
    lint (whichever merged second would red main). It must still be reported."""
    _allow(tmp_path, [])
    _wf(tmp_path, "clean.yml", """\
        name: Clean
        on: [pull_request]
        jobs:
          gate:
            runs-on: ubuntu-latest
            steps:
              - run: echo hi
    """)
    rc = _run_main(monkeypatch, tmp_path, waivers={"Gone / vanished": "was #999"})
    assert rc == 0
    out = capsys.readouterr().out
    assert "stale mask waiver" in out and "Gone / vanished" in out


def test_unparseable_workflow_fails_closed(tmp_path, monkeypatch):
    """A workflow we cannot parse is a workflow whose jobs we cannot police.
    The old code `continue`d past YAMLError — so a malformed file made its
    masked jobs INVISIBLE and the lint printed OK. Same vacuous shape this
    lint exists to kill; it must fail closed."""
    _allow(tmp_path, [])
    _wf(tmp_path, "broken.yml", """\
        name: Broken
        on: [pull_request]
        jobs:
          gate:
            runs-on: ubuntu-latest
            steps:
              - run: echo "unterminated
             bad-indent: [oops
    """)
    errs = []
    mod.job_contexts(str(tmp_path / ".gitea" / "workflows"), parse_errors=errs)
    assert errs, "precondition: the fixture must actually be unparseable"
    assert _run_main(monkeypatch, tmp_path) == 1


def test_missing_allowlist_fails_closed(tmp_path, monkeypatch):
    (tmp_path / ".gitea" / "workflows").mkdir(parents=True, exist_ok=True)
    assert _run_main(monkeypatch, tmp_path) == 1
