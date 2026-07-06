"""Unit tests for lint_env_coupling_dismissal — the no-flaky / env-coupling lint.

Exercises the two pure scanners (scan_doc_text, scan_added_lines). The git
plumbing in main() is impure and covered by the workflow, not here.
"""
import importlib.util
import os

HERE = os.path.dirname(__file__)
SCRIPT = os.path.join(HERE, "..", ".gitea", "scripts", "lint_env_coupling_dismissal.py")
spec = importlib.util.spec_from_file_location("lint_env_coupling_dismissal", SCRIPT)
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


# --- CHECK 1: scan_doc_text -------------------------------------------------

def test_dismissal_without_rootcause_flagged():
    text = "The e2e went red again. It's just flaky, we re-ran it and it passed."
    assert mod.scan_doc_text(text) == ["flaky"]


def test_environmental_dismissal_flagged():
    text = "## COE\nThe smoke test failed. This was environmental — the box was busy."
    assert "environmental" in mod.scan_doc_text(text)


def test_dismissal_with_rootcause_is_clean():
    text = (
        "The e2e was flaky. Root cause: the gate polled the shared CP /health "
        "(false-ready, class C) instead of the org's ready signal. Fixed by "
        "polling the real signal."
    )
    assert mod.scan_doc_text(text) == []


def test_dismissal_with_class_marker_is_clean():
    text = "Looked environmental but is a class B race: a status write lands after paused."
    assert mod.scan_doc_text(text) == []


def test_allow_marker_suppresses():
    text = "Historic note: the old suite was flaky. lint-allow: env-coupling"
    assert mod.scan_doc_text(text) == []


def test_no_dismissal_is_clean():
    text = "Root cause: a missing MOLECULE_LLM_BASE_URL injection. Fixed."
    assert mod.scan_doc_text(text) == []


def test_environment_variable_prose_not_flagged():
    # Ordinary prose mentioning the environment must not trip the dismissal
    # regex (word-boundaried; only "environmental"/"env issue"-style hits).
    text = "We set the environment variable MOLECULE_LLM_BASE_URL in the test env."
    assert mod.scan_doc_text(text) == []


def test_multiple_distinct_dismissals_sorted_deduped():
    text = "Flaky and intermittent failure; also spurious failure. No cause given."
    out = mod.scan_doc_text(text)
    assert out == sorted(out)
    assert "flaky" in out


def test_not_reproducible_dismissal_flagged():
    text = "Closing: not reproducible on a re-run."
    assert mod.scan_doc_text(text)


# --- CHECK 2: scan_added_lines ---------------------------------------------

def test_fixed_go_sleep_in_e2e_flagged():
    added = ["\ttime.Sleep(5 * time.Second) // let it settle"]
    assert mod.scan_added_lines("test/e2e/provision_e2e_test.go", added)


def test_fixed_sleep_with_poll_marker_is_clean():
    added = [
        "\tfor { // poll the real ready signal",
        "\t\tif ready { break }",
        "\t\ttime.Sleep(200 * time.Millisecond) // backoff between polls",
    ]
    assert mod.scan_added_lines("test/e2e/provision_e2e_test.go", added) == []


def test_fixed_sleep_with_allow_marker_is_clean():
    added = ["\ttime.Sleep(30 * time.Second) // lint-allow: env-coupling documented safety net"]
    assert mod.scan_added_lines("test/e2e/x_e2e_test.go", added) == []


def test_shell_sleep_in_e2e_flagged():
    added = ["sleep 30  # wait for staging to redeploy"]
    assert mod.scan_added_lines("scripts/e2e-smoke.sh", added)


def test_settimeout_in_e2e_flagged():
    added = ["  await new Promise(r => setTimeout(r, 4000));"]
    assert mod.scan_added_lines("canvas/e2e/agent.e2e.ts", added)


def test_sleep_in_non_e2e_path_ignored():
    # The sleep check is scoped to e2e paths; a unit test's sleep is out of scope.
    added = ["\ttime.Sleep(time.Second)"]
    assert mod.scan_added_lines("internal/scheduler/scheduler_test.go", added) == []


def test_added_line_without_sleep_is_clean():
    added = ["\trequire.Eventually(t, ready, 30*time.Second, 200*time.Millisecond)"]
    assert mod.scan_added_lines("test/e2e/x_e2e_test.go", added) == []


def test_python_async_sleep_in_e2e_flagged():
    added = ["    await asyncio.sleep(3)  # give the agent time to boot"]
    assert mod.scan_added_lines("tests/e2e/test_agent_e2e.py", added)
