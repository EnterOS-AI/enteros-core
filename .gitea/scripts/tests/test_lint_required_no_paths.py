"""Tests for lint-required-no-paths — BOTH arms of the
"a required check must not be able to go green without running" invariant.

  ARM A (blocking)     — declarative `on: paths:` on a required workflow.
  ARM B (report-only)  — the SAME filter hand-rolled in shell inside a
                         detect-changes job, whose boolean output gates the
                         required-context job's real work.

Every guard here is NEGATIVE-CONTROLLED: for each detector we assert it
FIRES on the broken input AND stays silent on the correct input. A guard
that has only ever been seen to pass is not a guard
(feedback_negative_control_every_test).

The final class is a LIVE mutation proof against the real repo: the four
lanes that carry the Arm-B shape today MUST be detected, and the lanes that
do not carry it MUST NOT be. If someone converts a lane to always-run, the
corresponding assertion here is what tells them to promote the lint.
"""
import importlib.util
import io
import sys
import textwrap
import urllib.error
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
SCRIPT = Path(__file__).resolve().parents[1] / "lint-required-no-paths.py"
spec = importlib.util.spec_from_file_location("lint_required_no_paths", SCRIPT)
lint = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = lint
spec.loader.exec_module(lint)


def _doc(src: str) -> dict:
    return yaml.safe_load(textwrap.dedent(src))


# ---------------------------------------------------------------------------
# The canonical broken shape, as it exists in the repo today: a detect-changes
# job computing a boolean from a diff, gating a required-context job.
# ---------------------------------------------------------------------------
BROKEN = """
    name: E2E Thing
    on:
      pull_request:
    jobs:
      detect-changes:
        outputs:
          api: ${{ steps.decide.outputs.api }}
        steps:
          - id: decide
            run: python3 .gitea/scripts/detect-changes.py --profile api
      e2e:
        name: E2E Thing
        needs: detect-changes
        steps:
          - name: No-op pass (paths filter excluded this commit)
            if: needs.detect-changes.outputs.api != 'true'
            run: |
              echo "No changes — gate satisfied without running tests."
              exit 0
          - if: needs.detect-changes.outputs.api == 'true'
            uses: actions/checkout@v6
          - name: Run the actual E2E
            if: needs.detect-changes.outputs.api == 'true'
            run: go test ./tests/e2e/...
"""

# The same lane, FIXED: the check always runs.
FIXED = """
    name: E2E Thing
    on:
      pull_request:
    jobs:
      e2e:
        name: E2E Thing
        steps:
          - uses: actions/checkout@v6
          - name: Run the actual E2E
            run: go test ./tests/e2e/...
"""


# ===========================================================================
# ARM B — detect_noop_gate
# ===========================================================================
class TestArmBFiresOnBrokenInput:
    def test_fires_on_the_canonical_broken_lane(self):
        findings = lint.detect_noop_gate(_doc(BROKEN), "e2e")
        assert findings, "detector MISSED the green-by-no-op shape"

    def test_reports_both_b1_and_b2_on_the_canonical_lane(self):
        blob = " ".join(lint.detect_noop_gate(_doc(BROKEN), "e2e"))
        assert "[B1 no-op arm]" in blob
        assert "[B2 all-steps-gated]" in blob

    def test_b1_alone_fires_when_an_ungated_preflight_step_exists(self):
        """The e2e-peer-visibility.yml shape.

        This lane has ONE ungated substantive step (a `bash -n` script-syntax
        preflight), so the all-steps-gated signature (B2) does NOT hold — yet
        the required context still greens without running the E2E. The first
        draft of this detector used B2 alone and MISSED this lane. B1 (the
        explicit no-op arm) is what catches it.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"].insert(
            0, {"name": "Validate driving scripts", "run": "bash -n tests/e2e/lib/assert.sh"}
        )
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B1 no-op arm]" in blob, "B1 must catch what B2 cannot"
        assert "[B2 all-steps-gated]" not in blob

    def test_b2_alone_fires_when_the_noop_echo_step_is_deleted(self):
        """Deleting the tell-tale echo step must NOT satisfy the lint.

        The defect is the all-steps-skippable shape, not the echo step. A lane
        that removes the "No-op pass" step but keeps the gate still greens
        without running.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"] = [
            s for s in doc["jobs"]["e2e"]["steps"] if "No-op pass" not in str(s.get("name"))
        ]
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B2 all-steps-gated]" in blob, "B2 must survive deletion of the echo step"
        assert "[B1 no-op arm]" not in blob

    def test_fires_on_git_diff_and_paths_filter_predicates_too(self):
        """The predicate source is behavioural, not a hardcoded script name."""
        for producer in (
            {"id": "decide", "run": "git diff --name-only origin/main | grep -q api"},
            {"id": "decide", "uses": "dorny/paths-filter@v3"},
        ):
            doc = _doc(BROKEN)
            doc["jobs"]["detect-changes"]["steps"] = [producer]
            assert lint.detect_noop_gate(doc, "e2e"), f"missed producer {producer}"


class TestArmBSilentOnCorrectInput:
    """NEGATIVE CONTROLS. A detector that cannot be seen to stay quiet on a
    correct workflow will red-block the repo the moment a lane is fixed."""

    def test_silent_on_the_fixed_always_run_lane(self):
        assert lint.detect_noop_gate(_doc(FIXED), "e2e") == []

    def test_silent_when_the_gate_is_not_diff_derived(self):
        """`if: needs.build.outputs.image != ''` is not a paths filter.

        Only a predicate computed from a repo DIFF degrades the gate the way
        `on: paths:` does. Gating on a build artifact is ordinary sequencing.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["detect-changes"]["steps"] = [
            {"id": "decide", "run": "echo api=true >> $GITHUB_OUTPUT"}
        ]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_silent_when_the_producer_exports_no_outputs(self):
        doc = _doc(BROKEN)
        del doc["jobs"]["detect-changes"]["outputs"]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_silent_when_the_job_does_not_need_the_producer(self):
        doc = _doc(BROKEN)
        del doc["jobs"]["e2e"]["needs"]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_a_real_step_behind_the_noop_arm_is_not_inert(self):
        """If the `!= 'true'` arm actually RUNS the check, it is not a no-op."""
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"][0]["run"] = "go test ./tests/e2e/..."
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B1 no-op arm]" not in blob


class TestInertStepClassification:
    @pytest.mark.parametrize(
        "body",
        [
            'echo "nothing to do"',
            'echo "a"\necho "b"\nexit 0',
            "set -euo pipefail\necho ok\n:",
            "# just a comment\ntrue",
        ],
    )
    def test_inert_bodies(self, body):
        assert lint._is_inert_step({"run": body}) is True

    @pytest.mark.parametrize(
        "body",
        [
            "go test ./...",
            'echo "starting"\nbash tests/e2e/run.sh',
            "docker compose up -d",
            'echo hi\ncurl -fsS https://example.test/health',
        ],
    )
    def test_substantive_bodies(self, body):
        assert lint._is_inert_step({"run": body}) is False

    def test_an_action_step_is_substantive(self):
        assert lint._is_inert_step({"uses": "actions/setup-go@v5"}) is False

    def test_setup_actions_are_not_the_substantive_work(self):
        assert lint._is_setup_step({"uses": "actions/checkout@v6"}) is True
        assert lint._is_setup_step({"uses": "docker/login-action@v3"}) is True
        assert lint._is_setup_step({"uses": "dorny/paths-filter@v3"}) is False


# ===========================================================================
# ARM A — detect_paths_filters (the original, blocking arm)
# ===========================================================================
class TestArmA:
    def _write(self, tmp_path: Path, src: str) -> Path:
        p = tmp_path / "wf.yml"
        p.write_text(textwrap.dedent(src))
        return p

    def test_fires_on_paths(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              pull_request:
                paths: ['**.go']
            jobs: {}
        """)
        assert lint.detect_paths_filters(p)

    def test_fires_on_paths_ignore(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              push:
                paths-ignore: ['docs/**']
            jobs: {}
        """)
        assert lint.detect_paths_filters(p)

    def test_silent_without_a_filter(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              pull_request:
                types: [opened]
            jobs: {}
        """)
        assert lint.detect_paths_filters(p) == []


# ===========================================================================
# ENFORCED-context enumeration — the bug that made this lint a no-op.
# ===========================================================================
class TestEnforcedContextEnumeration:
    def test_stops_at_the_first_pending_marker(self, tmp_path):
        f = tmp_path / "required-contexts.txt"
        f.write_text(textwrap.dedent("""
            # a comment
            CI / all-required
            E2E API Smoke Test / E2E API Smoke Test (pull_request)

            # pending-#2409 (not yet enforced) ---
            Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)
            # pending-#3159 (not yet enforced) ---
            E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot
        """))
        got = lint.load_enforced_contexts(str(f))
        assert got == ["CI / all-required", "E2E API Smoke Test / E2E API Smoke Test"]

    def test_event_suffix_is_stripped(self):
        assert lint.strip_event("CI / all-required (pull_request)") == "CI / all-required"
        assert lint.strip_event("CI / all-required") == "CI / all-required"

    def test_a_job_name_ending_in_parens_is_not_mistaken_for_an_event(self):
        ctx = "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)"
        assert lint.strip_event(ctx) == ctx

    def test_wildcard_is_recognised_as_the_meta_gate(self):
        """`["*"]` is what live BP actually holds. It must NOT be treated as an
        enumerable context — doing so is what made this lint resolve ZERO
        workflows and green out."""
        assert "*" in lint.WILDCARD_CONTEXTS
        assert lint.parse_context("*") is None

    def test_missing_ssot_file_fails_closed(self, tmp_path):
        with pytest.raises(SystemExit) as e:
            lint.load_enforced_contexts(str(tmp_path / "nope.txt"))
        assert e.value.code == 3


# ===========================================================================
# TRANSIENT-FAILURE HANDLING — "did not run" is not "failed".
#
# Regression cover for: a Cloudflare 502 on the branch_protections GET
# killed this lint before it opened a single workflow file, propagated as
# an uncaught ApiError, and exited 1 — and exit 1 in this lint's contract
# means "a required workflow carries a paths filter". The lint reported a
# compliance finding it had not made.
#
# Every guard here is negative-controlled: the retry must recover a
# transient failure AND must not swallow a genuine finding AND must not
# retry a terminal 4xx.
# ===========================================================================
class _FakeResp:
    def __init__(self, body: bytes, status: int = 200):
        self._b, self.status = body, status

    def read(self):
        return self._b

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


_BP_OK = b'{"branch_name":"main","status_check_contexts":["*"]}'
_CF_502 = (
    b'{"type":"https://developers.cloudflare.com/support/troubleshooting/'
    b'http-status-codes/cloudflare-5xx-errors/error-502/",'
    b'"title":"Bad gateway","status":502}'
)


def _scripted_opener(script):
    """Return (opener, attempts_list). `script` items: 'ok' | 'reset' |
    'timeout' | an int-ish HTTP status string."""
    attempts: list[str] = []

    def _open(req, *a, **kw):
        what = script[len(attempts)] if len(attempts) < len(script) else script[-1]
        attempts.append(what)
        if what == "ok":
            return _FakeResp(_BP_OK)
        if what == "reset":
            raise ConnectionResetError(10054, "forcibly closed by remote host")
        if what == "timeout":
            raise TimeoutError("timed out")
        code = int(what)
        body = _CF_502 if code >= 500 else b'{"message":"forbidden"}'
        raise urllib.error.HTTPError(
            req.full_url, code, "err", {}, io.BytesIO(body)
        )

    return _open, attempts


@pytest.fixture
def api_env(monkeypatch):
    """Full runtime env contract + instant, deterministic backoff.

    The script snapshots GITEA_HOST/REPO/... into module globals at IMPORT
    time, and the test module imports it once with an empty env — so the
    env vars alone are not enough; the derived globals must be set too.
    """
    monkeypatch.setenv("GITEA_TOKEN", "dummy")
    monkeypatch.setenv("GITEA_HOST", "git.moleculesai.app")
    monkeypatch.setenv("REPO", "molecule-ai/molecule-core")
    monkeypatch.setenv("BRANCH", "main")
    monkeypatch.setattr(lint, "GITEA_TOKEN", "dummy")
    monkeypatch.setattr(lint, "GITEA_HOST", "git.moleculesai.app")
    monkeypatch.setattr(lint, "REPO", "molecule-ai/molecule-core")
    monkeypatch.setattr(lint, "BRANCH", "main")
    monkeypatch.setattr(lint, "OWNER", "molecule-ai")
    monkeypatch.setattr(lint, "NAME", "molecule-core")
    monkeypatch.setattr(lint, "API", "https://git.moleculesai.app/api/v1")
    monkeypatch.setattr(lint, "API_MAX_ATTEMPTS", 4)
    monkeypatch.setattr(lint, "API_BACKOFF_BASE", 0.0)
    monkeypatch.setattr(lint, "API_BACKOFF_CAP", 0.0)
    monkeypatch.setattr(lint.time, "sleep", lambda _s: None)


class TestTransientRetry:
    def test_the_helper_retries_a_5xx_and_returns_the_eventual_success(
        self, api_env, monkeypatch
    ):
        opener, attempts = _scripted_opener(["502", "502", "ok"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        status, doc = lint.api("GET", "/repos/x/y/branch_protections/main")
        assert status == 200
        assert doc["status_check_contexts"] == ["*"]
        assert len(attempts) == 3, "must have retried twice before succeeding"

    @pytest.mark.parametrize("transport", ["reset", "timeout"])
    def test_the_helper_retries_transport_failures(
        self, api_env, monkeypatch, transport
    ):
        opener, attempts = _scripted_opener([transport, "ok"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        status, _ = lint.api("GET", "/x")
        assert status == 200
        assert len(attempts) == 2

    @pytest.mark.parametrize(
        "exc",
        [
            pytest.param(__import__("ssl").SSLError("decryption failed"), id="ssl"),
            pytest.param(OSError(5, "Input/output error"), id="oserror"),
            pytest.param(
                __import__("http.client", fromlist=["x"]).ResponseNotReady("Idle"),
                id="responsenotready",
            ),
            pytest.param(ConnectionResetError(10054, "reset"), id="reset"),
        ],
    )
    def test_a_READ_PHASE_failure_is_transient_too(
        self, api_env, monkeypatch, exc
    ):
        """urlopen SUCCEEDS and the failure happens later in resp.read().

        urllib only wraps CONNECT-phase errors in URLError, so an earlier
        revision of this fix — which enumerated leaf exception types —
        let a read-phase ssl.SSLError / raw OSError / ResponseNotReady
        escape as a bare traceback exiting 1, i.e. "a required workflow has
        a paths filter". Classify by BASE class so the read phase cannot
        fall through the net.
        """
        attempts = []

        class ExplodingRead:
            status = 200

            def read(self):
                raise exc

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        def _open(req, *a, **kw):
            attempts.append(1)
            return _FakeResp(_BP_OK) if len(attempts) > 2 else ExplodingRead()

        monkeypatch.setattr(lint.urllib.request, "urlopen", _open)
        status, doc = lint.api("GET", "/x")
        assert status == 200, "a read-phase failure must be RETRIED, not raised"
        assert len(attempts) == 3

    def test_exhaustion_raises_ApiUnreachable_not_a_bare_ApiError(
        self, api_env, monkeypatch
    ):
        """The TYPE is the contract: callers must be able to tell 'never got
        an answer' from 'was told no'."""
        opener, attempts = _scripted_opener(["502"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        with pytest.raises(lint.ApiUnreachable) as e:
            lint.api("GET", "/x")
        assert len(attempts) == 4
        assert e.value.attempts == 4
        assert e.value.last_status == 502
        assert "UNREACHABLE" in str(e.value)

    @pytest.mark.parametrize("code", ["400", "401", "403", "404", "409", "422"])
    def test_4xx_is_NEVER_retried(self, api_env, monkeypatch, code):
        """A 4xx is an authorisation/addressing FACT, not weather. Retrying
        it cannot change the answer and buries a real permissions defect
        under retry noise — e.g. the merge-queue actor's persistent 403 on
        this exact endpoint, which must stay loud and immediate."""
        opener, attempts = _scripted_opener([code])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        with pytest.raises(lint.ApiError) as e:
            lint.api("GET", "/x")
        assert len(attempts) == 1, f"HTTP {code} must cost exactly ONE attempt"
        assert not isinstance(e.value, lint.ApiUnreachable), (
            "a terminal 4xx must NOT be reported as 'unreachable'"
        )
        assert f"HTTP {code}" in str(e.value)

    def test_429_is_the_one_retried_4xx(self, api_env, monkeypatch):
        """429 is not a statement about authorisation; it means 'later'."""
        opener, attempts = _scripted_opener(["429", "ok"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        status, _ = lint.api("GET", "/x")
        assert status == 200 and len(attempts) == 2

    @pytest.mark.parametrize("code", [500, 502, 503, 504, 520, 521, 522, 524])
    def test_every_5xx_including_cloudflares_52x_is_transient(self, code):
        assert lint._is_transient_status(code)

    @pytest.mark.parametrize("code", [400, 401, 403, 404, 409, 422])
    def test_no_other_4xx_is_transient(self, code):
        assert not lint._is_transient_status(code)

    def test_HTTPError_IS_a_subclass_of_the_transient_tuple(self):
        """Documents the footgun rather than pretending it is not there.

        Since _TRANSIENT_EXC widened to base classes, HTTPError -> URLError
        -> OSError means this is literally True. Clause ORDER in api() is
        the only thing keeping 4xx terminal. If this assertion ever starts
        failing, the tuple narrowed and the ordering comment is stale; if
        it keeps passing, `test_4xx_is_NEVER_retried` is what stops a
        reorder from silently retrying 403s."""
        assert issubclass(urllib.error.HTTPError, lint._TRANSIENT_EXC)

    def test_the_main_net_does_not_swallow_the_exit_protocol(self):
        """`Exception`, not `BaseException`, is load-bearing.

        The env-contract and YAML-parse failures signal via sys.exit(2) /
        sys.exit(3), which raise SystemExit — a BaseException. Widening
        main()'s catch-all would silently collapse exit 2 and exit 3 into
        4, destroying two documented codes inside the contract this file
        sharpens."""
        assert not issubclass(SystemExit, Exception)
        assert issubclass(SystemExit, BaseException)

    @pytest.mark.parametrize("code", [2, 3])
    def test_exit_2_and_3_still_propagate_through_main(
        self, api_env, monkeypatch, capsys, code
    ):
        """The executable half of the assertion above."""
        opener, _ = _scripted_opener(["ok"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        monkeypatch.setattr(
            lint, "build_job_index", lambda *a, **k: sys.exit(code)
        )
        with pytest.raises(SystemExit) as e:
            lint.main()
        capsys.readouterr()
        assert e.value.code == code, (
            f"exit {code} was swallowed and remapped — main() must catch "
            f"Exception, never BaseException"
        )


class TestUnreachableIsNotNonCompliance:
    """The whole point: on retry exhaustion the lint must say the API was
    unreachable, and must NOT say a workflow is non-compliant."""

    def _run(self, monkeypatch, script, capsys):
        opener, attempts = _scripted_opener(script)
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        rc = lint.main()
        cap = capsys.readouterr()
        return rc, cap.out + cap.err, attempts

    def test_exhaustion_exits_5_with_an_unmistakable_did_not_run_banner(
        self, api_env, monkeypatch, capsys
    ):
        rc, log, attempts = self._run(monkeypatch, ["502"], capsys)
        assert rc == 5, "exhaustion must NOT be exit 1 (a paths-filter finding)"
        assert rc != 0, "and must NOT be green — a lint that cannot check fails"
        assert len(attempts) == 4
        for phrase in (
            "LINT DID NOT RUN",
            "GITEA API UNREACHABLE",
            "NOT a compliance finding",
            "ZERO workflow files were inspected",
            "DO NOT go looking for a paths-filter defect",
            "Exit 5 = INFRASTRUCTURE UNREACHABLE",
        ):
            assert phrase in log, f"missing from the log: {phrase!r}"

    def test_exhaustion_makes_no_compliance_claim_and_no_traceback(
        self, api_env, monkeypatch, capsys
    ):
        rc, log, _ = self._run(monkeypatch, ["502"], capsys)
        assert rc == 5
        for phrase in (
            "has a paths filter that would degrade",
            "ARM A —",
            "Traceback",
            "can post SUCCESS WITHOUT RUNNING",
        ):
            assert phrase not in log, f"compliance-shaped output leaked: {phrase!r}"

    def test_a_transient_502_that_recovers_passes_cleanly(
        self, api_env, monkeypatch, capsys
    ):
        rc, log, attempts = self._run(monkeypatch, ["502", "502", "ok"], capsys)
        assert rc == 0
        assert len(attempts) == 3
        assert "(transient) on attempt 1/4" in log
        assert "LINT DID NOT RUN" not in log
        assert "::notice::Linting" in log, "it must actually have linted"

    def test_persistent_403_stays_a_loud_auth_failure(
        self, api_env, monkeypatch, capsys
    ):
        """The retry must not mask a permissions defect: ONE attempt, exit 4,
        the AUTH FAILURE message — never 'unreachable'."""
        rc, log, attempts = self._run(monkeypatch, ["403"], capsys)
        assert rc == 4, "403 is fail-closed auth, not exit 5 unreachable"
        assert len(attempts) == 1, "403 must not be retried"
        assert "AUTH FAILURE" in log
        # The log states the OBSERVED fact and quotes the API, rather than
        # prescribing a grant that is already in place (see
        # test_the_403_summary_names_the_api_message_not_a_presumed_cause).
        assert "Gitea said:" in log and "forbidden" in log
        assert "was NOT retried" in log
        assert "retrying in" not in log
        assert "UNREACHABLE" not in log and "LINT DID NOT RUN" not in log

    def test_the_did_not_run_banner_also_lands_in_the_run_summary(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """A failed step's log is collapsed by default; the run summary is
        what an operator sees FIRST. 'Unmistakable' has to hold before
        anyone expands a log."""
        summary = tmp_path / "summary.md"
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
        rc, _, _ = self._run(monkeypatch, ["502"], capsys)
        assert rc == 5
        md = summary.read_text(encoding="utf-8")
        assert "THE LINT DID NOT RUN" in md
        assert "NOT a compliance finding" in md
        assert "ZERO workflow files were inspected" in md
        assert "the API never answered" in md

    def test_a_broken_step_summary_never_changes_the_verdict(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """Reporting must never be able to turn a verdict into something
        else — including into a traceback exiting 1."""
        monkeypatch.setenv(
            "GITHUB_STEP_SUMMARY", str(tmp_path / "no" / "such" / "dir" / "s.md")
        )
        rc, log, _ = self._run(monkeypatch, ["502"], capsys)
        assert rc == 5
        assert "LINT DID NOT RUN" in log
        assert "Traceback" not in log

    def test_a_clean_run_writes_no_did_not_run_summary(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """Negative control: the banner must not appear when the lint ran."""
        summary = tmp_path / "summary.md"
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
        rc, _, _ = self._run(monkeypatch, ["502", "ok"], capsys)
        assert rc == 0
        assert not summary.exists() or "DID NOT RUN" not in summary.read_text(
            encoding="utf-8"
        )

    @pytest.mark.parametrize(
        "exc_factory, expect_rc",
        [
            # Read-phase NETWORK failures: api() retries, exhausts, exit 5.
            # "the API never answered" is a TRUE claim here.
            (lambda: __import__("ssl").SSLError("decryption failed"), 5),
            (lambda: OSError(5, "Input/output error"), 5),
            (
                lambda: __import__(
                    "http.client", fromlist=["x"]
                ).ResponseNotReady("Idle"),
                5,
            ),
        ],
    )
    def test_read_phase_network_faults_exit_5_never_1(
        self, api_env, monkeypatch, capsys, exc_factory, expect_rc
    ):
        exc = exc_factory()

        class ExplodingRead:
            status = 200

            def read(self):
                raise exc

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        monkeypatch.setattr(
            lint.urllib.request, "urlopen", lambda *a, **k: ExplodingRead()
        )
        rc = lint.main()
        log = "".join(capsys.readouterr())
        assert rc == expect_rc, "must NOT be exit 1 (a paths-filter finding)"
        assert "LINT DID NOT RUN" in log
        assert "has a paths filter that would degrade" not in log

    def test_an_internal_bug_exits_4_and_never_claims_the_api_was_unreachable(
        self, api_env, monkeypatch, capsys
    ):
        """The catch-all must not fabricate a cause.

        A TypeError in our own YAML walk is NOT "the API was unreachable".
        Routing every escaping exception to exit 5 would blame Cloudflare
        for our bug — inventing a cause we did not observe, which is the
        same fault as inventing a finding we did not make. Exit 4 claims
        only what is known: verification did not complete.
        """
        opener, _ = _scripted_opener(["ok"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)

        def boom(*a, **kw):
            raise TypeError("bug in the lint's own job indexing")

        monkeypatch.setattr(lint, "build_job_index", boom)
        rc = lint.main()
        log = "".join(capsys.readouterr())
        assert rc == 4, "an internal bug must not exit 1 and must not exit 5"
        assert "LINT DID NOT COMPLETE" in log
        assert "unexpected internal error" in log
        assert "NOT a compliance finding" in log
        assert "bug in the lint itself" in log
        # It must NOT assert the API was unreachable — that never happened.
        assert "GITEA API UNREACHABLE" not in log
        assert "has a paths filter that would degrade" not in log

    def test_nothing_at_all_escapes_main_as_a_bare_traceback(
        self, api_env, monkeypatch, capsys
    ):
        """The property in one assertion: for every failure mode we can
        inject, main() returns a code and never propagates."""
        for inject in (
            KeyError("missing"),
            ValueError("bad"),
            RuntimeError("boom"),
            AttributeError("nope"),
        ):
            opener, _ = _scripted_opener(["ok"])
            monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
            monkeypatch.setattr(
                lint, "build_job_index", lambda *a, _e=inject, **k: (_ for _ in ()).throw(_e)
            )
            rc = lint.main()
            capsys.readouterr()
            assert rc == 4, f"{type(inject).__name__} escaped or mis-coded as {rc}"

    def test_the_403_auth_failure_also_lands_in_the_run_summary(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """The 403 branch needs the summary MORE than the others.

        A failed step's log is collapsed by default, and this is the one
        branch pointing at a live permissions defect rather than weather —
        the merge-queue actor's 403 on this same endpoint. A 403 buried in
        a folded log is how an authorisation defect gets mistaken for
        flake."""
        summary = tmp_path / "summary.md"
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
        rc, _, attempts = self._run(monkeypatch, ["403"], capsys)
        assert rc == 4
        assert len(attempts) == 1
        md = summary.read_text(encoding="utf-8")
        assert "AUTH FAILURE" in md
        assert "NOT a compliance finding" in md
        assert "repo-admin" in md
        assert "was NOT retried" in md, (
            "the summary must say re-running will not help — otherwise the "
            "operator treats an authorisation fact as a transient one"
        )
        assert "HTTP 403" in md

    def test_the_403_summary_degrades_cleanly_when_unset_or_unwritable(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """Same contract as the other branches: reporting must never be
        able to change the verdict."""
        monkeypatch.delenv("GITHUB_STEP_SUMMARY", raising=False)
        rc, log, _ = self._run(monkeypatch, ["403"], capsys)
        assert rc == 4 and "AUTH FAILURE" in log and "Traceback" not in log

        monkeypatch.setenv(
            "GITHUB_STEP_SUMMARY", str(tmp_path / "no" / "dir" / "s.md")
        )
        rc, log, _ = self._run(monkeypatch, ["403"], capsys)
        assert rc == 4 and "AUTH FAILURE" in log and "Traceback" not in log

    def test_the_403_summary_names_the_api_message_not_a_presumed_cause(
        self, api_env, monkeypatch, capsys, tmp_path
    ):
        """The remediation must not instruct something already done.

        The old text said "grant repo-admin to mc-drift-bot (team
        `drift-bot`, perm=admin)" — but that team IS perm=admin and the
        account is already `owner` here, so an operator finds the box
        ticked and blames the lint. And this was the one terminal branch
        that discarded Gitea's own message, which is exactly what
        distinguishes a permission 403 from a PAT-scope 403 from a
        Cloudflare WAF 403."""
        summary = tmp_path / "summary.md"
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
        opener, _ = _scripted_opener(["403"])
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        rc = lint.main()
        log = "".join(capsys.readouterr())
        md = summary.read_text(encoding="utf-8")
        assert rc == 4
        # Gitea's own body is surfaced, in BOTH the log and the summary.
        assert "forbidden" in md, "the API's own message must be quoted"
        assert "forbidden" in log
        # It points at diagnosis, naming all three kinds.
        for kind in ("permission", "PAT scope", "Cloudflare WAF"):
            assert kind in md, f"403 taxonomy missing {kind!r}"
        # And it does NOT issue the already-satisfied instruction.
        assert "already `owner`" in md
        assert "grant repo-admin to mc-drift-bot" not in md
        assert "grant repo-admin to mc-drift-bot" not in log

    def test_404_still_degrades_gracefully_without_retrying(
        self, api_env, monkeypatch, capsys
    ):
        rc, log, attempts = self._run(monkeypatch, ["404"], capsys)
        assert rc == 0
        assert len(attempts) == 1
        assert "returned HTTP 404" in log and "Falling back to the" in log
        assert "retrying in" not in log

    def test_an_unhandled_terminal_4xx_exits_4_not_1(
        self, api_env, monkeypatch, capsys
    ):
        """A 422/400 used to `raise` out of run() as a bare traceback →
        exit 1 → indistinguishable from an Arm-A finding."""
        rc, log, _ = self._run(monkeypatch, ["422"], capsys)
        assert rc == 4
        assert "LINT DID NOT COMPLETE" in log
        assert "NOT a compliance finding" in log
        assert "Traceback" not in log


class TestGenuineNonComplianceStillReds:
    """Negative control for the whole fix: the retry must not make the lint
    softer. A real paths-filter must still exit 1 — including when a
    transient 502 preceded the successful fetch."""

    OFFENDER = """
        name: Offending Lane
        on:
          pull_request:
            paths: ['**.go']
        jobs:
          build:
            name: Offending Lane
            runs-on: ubuntu-latest
            steps:
              - run: go build ./...
    """

    @pytest.fixture
    def offending_repo(self, tmp_path, monkeypatch):
        wfdir = tmp_path / "workflows"
        wfdir.mkdir()
        (wfdir / "offender.yml").write_text(
            textwrap.dedent(self.OFFENDER), encoding="utf-8"
        )
        ssot = tmp_path / "required.txt"
        ssot.write_text("Offending Lane / Offending Lane\n", encoding="utf-8")
        monkeypatch.setattr(lint, "WORKFLOWS_DIR", str(wfdir))
        monkeypatch.setattr(lint, "REQUIRED_CONTEXTS_FILE", str(ssot))

    @pytest.mark.parametrize(
        "script", [["ok"], ["502", "502", "ok"], ["reset", "ok"]]
    )
    def test_a_real_paths_filter_still_exits_1(
        self, api_env, offending_repo, monkeypatch, capsys, script
    ):
        opener, _ = _scripted_opener(script)
        monkeypatch.setattr(lint.urllib.request, "urlopen", opener)
        rc = lint.main()
        cap = capsys.readouterr()
        log = cap.out + cap.err
        assert rc == 1, "a genuine Arm-A finding must still red as exit 1"
        assert "ARM A" in log
        assert "has a paths filter that would degrade" in log
        assert "LINT DID NOT RUN" not in log


# ===========================================================================
# THE EXIT CONTRACT IS ENUMERATED, NOT SAMPLED.
#
# The first version of this control parametrised over three hand-written
# response scripts covering exits {0,4,5} and claimed to prove that no
# branch could be added without a run-summary — while exits 1, 2 and 3
# wrote nothing and it could not notice. It also could not simply be
# extended: its assertion was `"NOT a compliance finding" in md`, and exit
# 1 IS a compliance finding.
#
# So the class is now derived from `lint.EXIT_MEANING`, the machine-readable
# exit contract in the script, and the CONTENT assertion varies by the
# class recorded there. Adding an exit code to the registry without a
# driver here fails `test_every_exit_code_has_a_driver`; adding a branch
# that skips the summary fails `test_every_non_clean_exit_writes_a_summary`.
# ===========================================================================
class TestExitContractIsFullyCovered:
    OFFENDER = """
        name: Offending Lane
        on:
          pull_request:
            paths: ['**.go']
        jobs:
          build:
            name: Offending Lane
            runs-on: ubuntu-latest
            steps:
              - run: go build ./...
    """
    CLEAN = """
        name: Clean Lane
        on: pull_request
        jobs:
          build:
            name: Clean Lane
            runs-on: ubuntu-latest
            steps:
              - run: go build ./...
    """

    def _drive(self, code, lint_mod, monkeypatch, tmp_path):
        """Produce exit `code` for real. Returns the observed exit code."""
        wf = tmp_path / "workflows"
        wf.mkdir(exist_ok=True)
        ssot = tmp_path / "required.txt"

        def _lay(body, ctx):
            (wf / "lane.yml").write_text(
                textwrap.dedent(body), encoding="utf-8"
            )
            ssot.write_text(ctx + "\n", encoding="utf-8")
            monkeypatch.setattr(lint_mod, "WORKFLOWS_DIR", str(wf))
            monkeypatch.setattr(lint_mod, "REQUIRED_CONTEXTS_FILE", str(ssot))

        if code == 0:
            _lay(self.CLEAN, "Clean Lane / Clean Lane")
            script = ["ok"]
        elif code == 1:
            _lay(self.OFFENDER, "Offending Lane / Offending Lane")
            script = ["ok"]
        elif code == 2:
            _lay(self.CLEAN, "Clean Lane / Clean Lane")
            monkeypatch.delenv("GITEA_TOKEN", raising=False)
            script = ["ok"]
        elif code == 3:
            _lay(self.CLEAN, "Clean Lane / Clean Lane")
            monkeypatch.setattr(
                lint_mod, "REQUIRED_CONTEXTS_FILE", str(tmp_path / "gone.txt")
            )
            script = ["ok"]
        elif code == 4:
            _lay(self.CLEAN, "Clean Lane / Clean Lane")
            script = ["403"]
        elif code == 5:
            _lay(self.CLEAN, "Clean Lane / Clean Lane")
            script = ["502"]
        else:  # pragma: no cover - guarded by the driver-coverage test
            raise AssertionError(f"no driver for exit code {code}")

        opener, _ = _scripted_opener(script)
        monkeypatch.setattr(lint_mod.urllib.request, "urlopen", opener)
        try:
            return lint_mod.main()
        except SystemExit as e:  # exits 2 and 3 signal via SystemExit
            return e.code

    def test_every_exit_code_has_a_driver(self):
        """If someone adds a code to EXIT_MEANING, this fails until they
        add a driver above — which is what makes the next test complete."""
        assert set(lint.EXIT_MEANING) == {0, 1, 2, 3, 4, 5}

    @pytest.mark.parametrize("code", sorted(lint.EXIT_MEANING))
    def test_every_non_clean_exit_writes_a_summary(
        self, api_env, monkeypatch, capsys, tmp_path, code
    ):
        summary = tmp_path / "summary.md"
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
        got = self._drive(code, lint, monkeypatch, tmp_path)
        capsys.readouterr()
        assert got == code, f"driver for exit {code} produced {got}"
        md = summary.read_text(encoding="utf-8") if summary.exists() else ""
        klass = lint.EXIT_MEANING[code][0]
        if klass == "clean":
            assert "DID NOT" not in md and "FINDING" not in md, (
                "a clean run must not emit a scary banner"
            )
            return
        assert md.strip(), f"exit {code} ({klass}) wrote NO run-summary"
        if klass == "finding":
            assert "IS a compliance finding" in md
            assert "NOT a compliance finding" not in md
        else:
            assert "NOT a compliance finding" in md
            assert "IS a compliance finding" not in md

    @pytest.mark.parametrize("code", sorted(lint.EXIT_MEANING))
    def test_no_exit_path_depends_on_the_summary_being_writable(
        self, api_env, monkeypatch, capsys, tmp_path, code
    ):
        """Reporting must never change a verdict, on ANY branch."""
        monkeypatch.setenv(
            "GITHUB_STEP_SUMMARY", str(tmp_path / "no" / "dir" / "s.md")
        )
        got = self._drive(code, lint, monkeypatch, tmp_path)
        log = "".join(capsys.readouterr())
        assert got == code
        assert "Traceback" not in log


# ===========================================================================
# LIVE MUTATION PROOF against the real repo.
# ===========================================================================
class TestAgainstTheRealRepo:
    """These are the assertions that make the lint real.

    If a lane below is converted to always-run, its assertion here fails —
    that is the signal to strike it from the list and, once the list is
    empty, to set NOOP_GATE_ENFORCE=1 and make Arm B blocking.
    """

    # Every one of these is an ENFORCED (merge-blocking) context whose job can
    # post SUCCESS without running. Verified live 2026-07-14.
    STILL_BROKEN = {
        "e2e-api.yml": "e2e-api",
        "e2e-peer-visibility.yml": "peer-visibility",
        "handlers-postgres-integration.yml": "integration",
        "template-delivery-e2e.yml": "delivery",
    }
    # ENFORCED contexts that correctly always run. These are the negative
    # controls that prove the detector is not just shouting at everything.
    STILL_CLEAN = {
        "ci.yml": "all-required",
        "secret-scan.yml": "scan",
        "concierge-creates-workspace-hermetic.yml": "hermetic",
    }

    def _load(self, name: str) -> dict:
        p = REPO_ROOT / ".gitea" / "workflows" / name
        if not p.is_file():
            pytest.skip(f"{name} not present in this checkout")
        return yaml.safe_load(p.read_text())

    @pytest.mark.parametrize("wf,job", sorted(STILL_BROKEN.items()))
    def test_enforced_lanes_with_a_noop_arm_are_detected(self, wf, job):
        findings = lint.detect_noop_gate(self._load(wf), job)
        assert findings, (
            f"{wf}::{job} is an ENFORCED context that can green without "
            f"running, but the detector did not flag it."
        )

    @pytest.mark.parametrize("wf,job", sorted(STILL_CLEAN.items()))
    def test_enforced_lanes_that_always_run_are_not_flagged(self, wf, job):
        findings = lint.detect_noop_gate(self._load(wf), job)
        assert findings == [], f"false positive on {wf}::{job}: {findings}"

    def test_arm_a_is_clean_across_the_enforced_set(self):
        """Arm A is BLOCKING. If this ever fails, the repo is wedged — so it is
        also the check that proves making Arm A live here does not wedge it."""
        ssot = REPO_ROOT / ".gitea" / "required-contexts.txt"
        if not ssot.is_file():
            pytest.skip("required-contexts.txt not present")
        offenders = []
        for ctx in lint.load_enforced_contexts(str(ssot)):
            wf_name = ctx.split(" / ", 1)[0]
            for p in (REPO_ROOT / ".gitea" / "workflows").glob("*.y*ml"):
                doc = yaml.safe_load(p.read_text())
                if isinstance(doc, dict) and doc.get("name") == wf_name:
                    if lint.detect_paths_filters(p):
                        offenders.append(ctx)
        assert offenders == [], f"Arm A would BLOCK on: {offenders}"
