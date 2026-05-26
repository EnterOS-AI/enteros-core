import importlib.util
import sys
from pathlib import Path
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / "ci-required-drift.py"
spec = importlib.util.spec_from_file_location("ci_required_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)

# Module-level constants are loaded from env at import time; set them
# explicitly so unit tests can import without the full env contract.
drift.SENTINEL_JOB = "all-required"
drift.CI_WORKFLOW_PATH = ".gitea/workflows/ci.yml"
drift.AUDIT_WORKFLOW_PATH = ".gitea/workflows/audit-force-merge.yml"


# ---------------------------------------------------------------------------
# Helper fixtures
# ---------------------------------------------------------------------------

def _make_ci_doc(jobs: dict) -> dict:
    return {"jobs": jobs}


def _make_audit_doc(required_checks: list[str]) -> dict:
    return {
        "jobs": {
            "audit": {
                "steps": [
                    {"env": {"REQUIRED_CHECKS": "\n".join(required_checks)}}
                ]
            }
        }
    }


# ---------------------------------------------------------------------------
# sentinel_needs
# ---------------------------------------------------------------------------

def test_sentinel_needs_returns_empty_when_absent():
    doc = _make_ci_doc({"all-required": {"runs-on": "ubuntu-latest"}})
    assert drift.sentinel_needs(doc) == set()


def test_sentinel_needs_parses_list():
    doc = _make_ci_doc(
        {"all-required": {"needs": ["platform-build", "canvas-build"]}}
    )
    assert drift.sentinel_needs(doc) == {"platform-build", "canvas-build"}


def test_sentinel_needs_parses_string():
    doc = _make_ci_doc({"all-required": {"needs": "platform-build"}})
    assert drift.sentinel_needs(doc) == {"platform-build"}


# ---------------------------------------------------------------------------
# ci_job_names / ci_jobs_all
# ---------------------------------------------------------------------------

def test_ci_job_names_excludes_sentinel_and_event_gated():
    doc = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {"if": "github.event_name == 'pull_request'"},
            "main-push": {"if": "github.ref == 'refs/heads/main'"},
            "all-required": {},
        }
    )
    assert drift.ci_job_names(doc) == {"platform-build"}


def test_ci_jobs_all_includes_event_gated():
    doc = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {"if": "github.event_name == 'pull_request'"},
            "all-required": {},
        }
    )
    assert drift.ci_jobs_all(doc) == {"platform-build", "canvas-build"}


# ---------------------------------------------------------------------------
# detect_drift — F1 / F1b with mocked I/O
# ---------------------------------------------------------------------------

SAMPLE_PROTECTION = {
    "status_check_contexts": [
        "CI / all-required (pull_request)",
        "Secret scan / Scan diff for credential-shaped strings (pull_request)",
    ]
}


def test_detect_drift_no_needs_sentinel_skips_f1():
    """Post-#1766 contract: all-required has no needs: → F1 is a false positive."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {},
        }
    )
    audit = _make_audit_doc(
        [
            "CI / all-required (pull_request)",
            "Secret scan / Scan diff for credential-shaped strings (pull_request)",
        ]
    )

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, debug = drift.detect_drift("main")

    assert findings == []
    assert debug["sentinel_needs"] == []


def test_detect_drift_typo_in_needs_triggers_f1b():
    """F1b still catches typos when needs exists."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "all-required": {"needs": ["platfom-build"]},  # typo
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert any("F1b" in f for f in findings)
    assert any("platfom-build" in f for f in findings)


def test_detect_drift_missing_job_in_needs_triggers_f1():
    """F1 still fires when needs is non-empty and jobs are missing."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {"needs": ["platform-build"]},
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert any("F1 —" in f for f in findings)
    assert any("canvas-build" in f for f in findings)
    assert not any("F1b" in f for f in findings)


def test_detect_drift_no_f1_when_needs_empty_even_with_jobs():
    """Explicit regression guard: empty needs + existing jobs = no F1."""
    ci = _make_ci_doc(
        {
            "platform-build": {},
            "canvas-build": {},
            "all-required": {"needs": []},
        }
    )
    audit = _make_audit_doc(["CI / all-required (pull_request)"])

    with patch.object(drift, "load_yaml", side_effect=[ci, audit]):
        with patch.object(drift, "api", return_value=(200, SAMPLE_PROTECTION)):
            findings, _ = drift.detect_drift("main")

    assert not any("F1 —" in f for f in findings)
