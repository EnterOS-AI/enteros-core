"""Tests for `.gitea/scripts/lint-curl-status-capture.py`.

Run:
    python3 -m pytest tests/test_lint_curl_status_capture.py -v
"""
from __future__ import annotations

import importlib.util
import re
from pathlib import Path


REPO = Path(__file__).resolve().parent.parent
SCRIPT_PATH = REPO / ".gitea" / "scripts" / "lint-curl-status-capture.py"


def _load_module():
    spec = importlib.util.spec_from_file_location("lint_curl_status_capture", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


# --------------------------------------------------------------------------
# Rule 1 — status-capture pollution (behaviour unchanged)
# --------------------------------------------------------------------------


def test_finds_quoted_echo_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w "%{http_code}" https://example.test || echo "000")
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "echo" in findings[0].snippet


def test_finds_unquoted_echo_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w '%{http_code}' https://example.test || echo 000)
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "echo" in findings[0].snippet


def test_finds_printf_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w '%{http_code}' https://example.test || printf '000')
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "printf" in findings[0].snippet


def test_ignores_tempfile_fallback_after_curl():
    lint = _load_module()
    # The fix shape, including the `[ -n ] ||` normalisation. Deliberately NOT
    # `[ -z "$HTTP_CODE" ] && HTTP_CODE="000"`: a trailing `&&` whose test is
    # false makes the whole line exit non-zero, which under the runner's -e
    # kills the step. A fixture that carries the bad shape teaches it.
    content = """
    set +e
    curl -sS -o /tmp/body -w '%{http_code}' https://example.test >/tmp/code
    rc=$?
    set -e
    HTTP_CODE=$(cat /tmp/code 2>/dev/null || echo "000")
    [ -n "$HTTP_CODE" ] || HTTP_CODE="000"
    """

    assert lint.scan_content("workflow.yml", content) == []


def test_collapses_bash_line_continuations():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body \\
      -w "%{http_code}" \\
      https://example.test \\
      || echo "000")
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1


# --------------------------------------------------------------------------
# Rule 2 — a capture that dies before its own guard
# --------------------------------------------------------------------------

# Verbatim from `.gitea/workflows/sweep-stale-e2e-orgs.yml` as it stood on main
# when it held the combined status red for 70m13s on 2026-08-11 (status id=88
# failure 01:55:21Z -> id=123 success 03:05:34Z, commit 21ec47e78748).
PRE_FIX_SWEEP = """
        run: |
          set +e
          curl -sS -o /tmp/cleanup_resp -w "%{http_code}" \\
            --max-time 60 \\
            -X POST "$MOLECULE_CP_URL/cp/admin/orphan-tunnels/cleanup" \\
            -H "Authorization: Bearer $ADMIN_TOKEN" >/tmp/cleanup_code
          set -e
          http_code=$(cat /tmp/cleanup_code 2>/dev/null || echo "000")
          body=$(cat /tmp/cleanup_resp 2>/dev/null | head -c 500)
          if [ "$http_code" = "200" ]; then
            echo "ok"
          fi
"""


def test_the_exact_text_that_took_main_down_is_a_finding():
    lint = _load_module()

    findings = lint.scan_content("sweep-stale-e2e-orgs.yml", PRE_FIX_SWEEP)

    rules = [f.rule for f in findings]
    assert "capture-dies-before-its-guard" in rules, (
        "the pre-fix text passed the lint — this is the regression that had no "
        f"guard at all. findings={findings}"
    )
    hit = [f for f in findings if f.rule == "capture-dies-before-its-guard"][0]
    assert "/tmp/cleanup_resp" in hit.hint


def test_the_5166_fix_shape_is_green():
    lint = _load_module()
    content = """
          curl -sS -o /tmp/cleanup_resp -w "%{http_code}" https://x.test >/tmp/cleanup_code
          http_code=$(cat /tmp/cleanup_code 2>/dev/null || echo "000")
          [ -n "$http_code" ] || http_code="000"
          body=$(head -c 500 /tmp/cleanup_resp 2>/dev/null || echo "<no response body>")
    """

    assert lint.scan_content("workflow.yml", content) == []


def test_a_fallback_inside_the_capture_clears_the_finding():
    """Mutation control: add ONLY the fallback and the finding must disappear."""
    lint = _load_module()
    fixed = PRE_FIX_SWEEP.replace(
        'body=$(cat /tmp/cleanup_resp 2>/dev/null | head -c 500)',
        'body=$(cat /tmp/cleanup_resp 2>/dev/null | head -c 500 || echo "<none>")',
    )
    assert fixed != PRE_FIX_SWEEP

    before = [f.rule for f in lint.scan_content("w.yml", PRE_FIX_SWEEP)]
    after = [f.rule for f in lint.scan_content("w.yml", fixed)]

    assert "capture-dies-before-its-guard" in before
    assert "capture-dies-before-its-guard" not in after


def test_a_mktemp_file_is_not_a_finding():
    """Negative control that varies exactly one thing: who creates the file.

    `mktemp` creates the file, so `cat` cannot fail on absence. Flagging it
    would be noise, and a noisy lint gets muted.
    """
    lint = _load_module()
    content = """
    STATUS_TMP=$(mktemp)
    curl -sS -w '%{http_code}' https://example.test > "$STATUS_TMP"
    n=$(cat "$STATUS_TMP" | wc -l)
    """

    assert [f for f in lint.scan_content("w.yml", content)
            if f.rule == "capture-dies-before-its-guard"] == []


def test_a_plain_read_without_a_pipeline_is_not_this_rule():
    """`VAR=$(cat f)` fails loudly on its own status; the pipeline is what hides
    it behind `head`'s exit 0 until pipefail promotes it."""
    lint = _load_module()
    content = """
    curl -sS -o /tmp/resp https://example.test
    body=$(cat /tmp/resp)
    """

    assert [f for f in lint.scan_content("w.yml", content)
            if f.rule == "capture-dies-before-its-guard"] == []


def test_the_quoted_capture_form_is_not_a_blind_spot():
    """`var="$(...)"` is how promote-prod-tenant-pin.yml writes its captures.

    An earlier enumeration keyed on `=$(` only and reported "no remaining
    sites" while two sat in the tree.
    """
    lint = _load_module()
    content = """
    curl -sS -o /tmp/resp https://example.test
    body="$(cat /tmp/resp 2>/dev/null | head -c 500)"
    """

    findings = [f for f in lint.scan_content("w.yml", content)
                if f.rule == "capture-dies-before-its-guard"]
    assert len(findings) == 1


def test_a_commented_out_example_is_not_a_finding():
    """core#5166 documents the defect by QUOTING the pre-fix line in a comment.

    A scanner that read comments would flag the explanation of the bug as the
    bug, and the only way back to green would be deleting the explanation.
    """
    lint = _load_module()
    content = """
          curl -sS -o /tmp/resp https://example.test
          # The previous line here was
          #     body=$(cat /tmp/resp 2>/dev/null | head -c 500)
          # which is a PIPELINE inside a bare assignment.
          body=$(head -c 500 /tmp/resp 2>/dev/null || echo "<none>")
    """

    assert lint.scan_content("workflow.yml", content) == []


# --------------------------------------------------------------------------
# Anti-vacuity and the report itself
# --------------------------------------------------------------------------


def test_zero_files_checked_is_an_error_not_a_pass(capsys):
    """A scanner that examined nothing must not be able to report success."""
    lint = _load_module()

    rc = lint.print_report([], 0)

    out = capsys.readouterr().out
    assert rc == 1
    assert "checked: 0" in out
    assert "::error::" in out


def test_a_clean_scan_reports_how_many_files_it_checked(capsys):
    lint = _load_module()

    rc = lint.print_report([], 42)

    out = capsys.readouterr().out
    assert rc == 0
    assert "checked: 42" in out


def test_the_fix_template_does_not_teach_the_trailing_and_shape():
    """The printed remedy must not carry the bug the tool exists to prevent.

    `[ -z "$X" ] && X="000"` as the LAST line of a step exits non-zero whenever
    the test is false — which is the success case. core#5166 refused that shape;
    the lint was still printing it.
    """
    lint = _load_module()

    # Only the EXECUTABLE lines. The template names the bad shape inside a
    # comment in order to explain why it is wrong; an assertion that could not
    # tell the two apart would force the explanation to be deleted — the same
    # comment-vs-code confusion the scanner itself has to get right.
    code_lines = [
        ln for ln in lint.FIX_TEMPLATE.split("\n")
        if ln.strip() and not ln.strip().startswith("#")
    ]
    offenders = [ln for ln in code_lines if re.search(r"&&\s*HTTP_CODE=", ln)]
    assert not offenders, offenders
    assert any('[ -n "$HTTP_CODE" ] || HTTP_CODE="000"' in ln for ln in code_lines)
    assert not [ln for ln in code_lines if "|| true" in ln]


def test_the_real_workflow_tree_is_scanned_and_clean():
    """End-to-end, on the actual tree, with the count asserted non-zero."""
    lint = _load_module()
    import os

    cwd = os.getcwd()
    os.chdir(REPO)
    try:
        paths = lint.default_paths()
        findings, checked = lint.scan_paths(paths)
    finally:
        os.chdir(cwd)

    assert checked >= 50, f"only {checked} workflow files scanned"
    assert findings == [], f"live findings: {findings}"


def test_self_skip_survives_windows_separators():
    """The self-skip compared raw strings and missed on a `\\`-separated path,
    so the lint flagged its own documentation comment on developer boxes."""
    lint = _load_module()

    assert lint._same_path(".gitea/workflows\\lint-curl-status-capture.yml", lint.SELF)
    assert lint._same_path("./" + lint.SELF, lint.SELF)
    assert not lint._same_path(".gitea/workflows/ci.yml", lint.SELF)
