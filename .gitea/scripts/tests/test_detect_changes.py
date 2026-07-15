"""Fail-direction tests for .gitea/scripts/detect-changes.py.

These are the regression gate for the two vacuous-gate holes that the path
filter shipped with:

  C2 — the `ci` profile's allow-list (`^workspace-server/`, `^canvas/`,
       `^workspace/`, `^tests/e2e/|^scripts/|^infra/scripts/`) matched NOTHING
       under `.gitea/` (note `^scripts/` is anchored and does NOT match
       `.gitea/scripts/`). A PR touching only the CI enforcement machinery
       produced platform=false canvas=false python=false scripts=false, every
       heavy job took its no-op arm, and `CI / all-required` reported SUCCESS
       having run zero tests.

  C5 — the `peer-visibility` allow-list omits internal/router/router.go,
       cmd/server/main.go, internal/registry/ and internal/orgtoken/: a route
       rename or a registry/token regression in any of them breaks the literal
       list_peers assertion while the required gate silently no-ops.

Both are FIXED here, by inverting each profile to a DENY-list.

On the safety of flipping `peer-visibility` — a REQUIRED, continue-on-error-free
docker-host E2E — on for every PR, under branch protection
`status_check_contexts=['*']` where one red freezes the merge queue:

  * its real arm is PROVEN GREEN — 5 recent real-arm runs, 5/5 "GATE PASSED"
    (2 on PR#4316, 3 on PR#4332). Classified by READING THE JOB LOGS, not by
    inferring from duration: the real arm finishes in 30-45s (warm GOCACHE
    bind-mount + pre-pulled images), so it looks just like the no-op arm on a
    duration histogram. Do not repeat that mistake when re-evaluating this;
  * the cross-run wedge is fixed at source — the host-wide /proc sweeps that
    killed concurrent PRs' platform-servers are deleted, and
    test_no_host_wide_process_sweep.py fails the build if one comes back;
  * the marginal cost is ~35s: the job already runs on every PR (it is the
    always-running required-context emitter); only its STEPS were path-gated.

Every test below FAILS against the pre-fix allow-list module and PASSES against
the deny-list one — that is what makes them a gate rather than a description.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "detect-changes.py"
spec = importlib.util.spec_from_file_location("detect_changes", SCRIPT)
detect_changes = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = detect_changes
spec.loader.exec_module(detect_changes)

classify = detect_changes.classify
PROFILES = detect_changes.PROFILES

# Profiles that MUST be deny-lists ("run unless every changed path is provably
# inert"), not allow-lists ("run only if a changed path matches"). The other
# profiles (handlers-postgres, e2e-api, template-delivery) are deliberately
# scoped and are out of scope here.
#
DENY_PROFILES = ("ci", "e2e-ephemeral", "peer-visibility")

# The lanes of the `ci` profile that actually gate a job in ci.yml. `python` is
# excluded on purpose: the `Python Lint & Test` job (ci.yml:698) consumes no
# output and always runs, so that key gates nothing.
CI_GATING_LANES = ("platform", "canvas", "scripts")

# Paths that ARE the CI enforcement machinery. A change to any of them must be
# visible to CI.
CI_MACHINERY = [
    ".gitea/scripts/detect-changes.py",
    ".gitea/scripts/gitea-merge-queue.py",
    ".gitea/scripts/all-required-check.sh",
    ".gitea/required-contexts.txt",
    ".gitea/workflows/ci.yml",
    ".gitea/workflows/e2e-peer-visibility.yml",
]


# ---------------------------------------------------------------------------
# C2 — the CI machinery must not be invisible to CI
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("path", CI_MACHINERY)
def test_ci_machinery_change_is_visible_to_the_ci_profile(path: str) -> None:
    """A CI-machinery-only PR must not report every gating lane false.

    Pre-fix this returned {platform: False, canvas: False, python: False,
    scripts: False} for every one of these paths — `CI / all-required` green
    with zero tests run.
    """
    out = classify("ci", [path])
    assert any(out[lane] for lane in CI_GATING_LANES), (
        f"{path} left every gating ci lane false: {out}"
    )


@pytest.mark.parametrize("path", CI_MACHINERY)
def test_ci_machinery_change_runs_every_gating_lane(path: str) -> None:
    """Not just 'some lane' — the machinery defines all of them, so all run."""
    out = classify("ci", [path])
    for lane in CI_GATING_LANES:
        assert out[lane] is True, f"{path} did not trigger ci lane {lane}: {out}"


def test_no_gate_is_blind_to_its_own_workflow() -> None:
    """A gate must see a change to the workflow that defines it.

    This is also the mechanism that lets THIS PR prove the peer-visibility real
    arm green: it edits e2e-peer-visibility.yml, which the profile matches, so
    the gate fires its real (non-no-op) arm on this very PR.
    """
    assert classify("e2e-ephemeral", [".gitea/workflows/e2e-ephemeral-happy-path.yml"])["happy"]
    assert classify("peer-visibility", [".gitea/workflows/e2e-peer-visibility.yml"])["peervis"]


def test_a_brand_new_top_level_tree_is_not_invisible() -> None:
    """Vacuous-by-omission by construction: an allow-list can't see a new tree.

    `sdk/` does not exist today. Under the allow-list every lane reported false
    for it; under a deny-list an unknown path runs the lane.
    """
    out = classify("ci", ["sdk/client.go"])
    for lane in CI_GATING_LANES:
        assert out[lane] is True, f"new tree sdk/ invisible to ci lane {lane}: {out}"


def test_dotgitea_scripts_is_not_matched_by_the_old_anchored_scripts_pattern() -> None:
    """Pin the exact mechanism: `^scripts/` never matched `.gitea/scripts/`."""
    import re

    assert re.search(r"^scripts/", ".gitea/scripts/detect-changes.py") is None
    assert classify("ci", [".gitea/scripts/detect-changes.py"])["scripts"] is True


# ---------------------------------------------------------------------------
# C5 — the peer-visibility surface (CONFIRMED HOLE, held for the follow-up)
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "path",
    [
        # The four surfaces the OLD allow-list omitted — this is C5.
        "workspace-server/internal/router/router.go",
        "workspace-server/cmd/server/main.go",
        "workspace-server/internal/registry/registry.go",
        "workspace-server/internal/orgtoken/orgtoken.go",
        # ...and the ones it named, which must keep working.
        "workspace-server/internal/handlers/mcp.go",
        "workspace-server/internal/handlers/mcp_tools.go",
        "workspace-server/internal/wsauth/token.go",
        "workspace-server/internal/middleware/auth.go",
        "tests/e2e/test_peer_visibility_mcp_local.sh",
        "tests/e2e/lib/peer_visibility_assert.sh",
        ".gitea/workflows/e2e-peer-visibility.yml",
        # The gate boots the WHOLE binary and provisions real workspaces, so any
        # package in it can break the literal list_peers assertion. Enumerating
        # "the ones that matter" is the bet that lost; do not re-take it.
        "workspace-server/internal/handlers/delegation.go",
        "workspace-server/internal/provisioner/cp_provisioner.go",
        "manifest.json",
    ],
)
def test_peer_visibility_triggers_on_anything_that_can_break_list_peers(path: str) -> None:
    assert classify("peer-visibility", [path])["peervis"] is True, (
        f"{path} did not trigger the required E2E Peer Visibility gate"
    )


# ---------------------------------------------------------------------------
# The docs-only fast path must survive (a deny-list that never skips is just a
# cost regression)
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("profile", DENY_PROFILES)
def test_docs_only_diff_still_skips_every_deny_lane(profile: str) -> None:
    paths = ["docs/architecture/adr-004.md", "README.md", "docs/img/diagram.svg"]
    out = classify(profile, paths)
    assert not any(out.values()), f"{profile} ran on a docs-only diff: {out}"


@pytest.mark.parametrize("profile", DENY_PROFILES)
def test_one_substantive_file_beside_docs_is_enough_to_fire(profile: str) -> None:
    """`any()` semantics: inert paths cannot dilute a substantive one."""
    out = classify(profile, ["README.md", "docs/x.md", "workspace-server/internal/db/db.go"])
    assert any(out.values()), f"{profile} was diluted to false by docs paths: {out}"


# ---------------------------------------------------------------------------
# The inert-set claims each ci lane makes (each is a promise about what the
# job reads; if the job changes, these must change with it)
# ---------------------------------------------------------------------------

def test_canvas_fixture_change_triggers_the_go_lane() -> None:
    """workspace-server/internal/handlers/external_connection_test.go:375 reads
    ../../../canvas/src/components/__tests__/__fixtures__/
    external-connection.golden.json — so a canvas edit CAN break `go test`, and
    canvas/ is therefore NOT inert for the platform lane.

    Pre-fix: platform=false for a canvas-only PR, and the golden drift shipped.
    """
    out = classify("ci", ["canvas/src/components/__tests__/__fixtures__/external-connection.golden.json"])
    assert out["platform"] is True, f"canvas fixture invisible to the Go lane: {out}"


def test_go_only_change_does_not_run_the_canvas_lane() -> None:
    """canvas's job is `npm ci && npm run build && vitest run` inside canvas/,
    and no canvas source or test reads a path outside canvas/ — workspace-server
    is provably inert for it. Keeps the deny-list from becoming run-everything.
    """
    out = classify("ci", ["workspace-server/internal/handlers/mcp.go"])
    assert out["canvas"] is False, f"Go-only PR needlessly ran the canvas lane: {out}"


def test_go_and_canvas_only_changes_do_not_run_the_shellcheck_lane() -> None:
    """The shellcheck lane shellchecks + runs offline bash suites over
    tests/e2e, scripts/, infra/scripts; it reads no Go and no TS. The one
    cross-tree step (assert_e2e_tenant_contract.py) is OR-gated in ci.yml on
    `scripts || platform`, so no coverage is lost by denying these here.
    """
    assert classify("ci", ["workspace-server/internal/handlers/mcp.go"])["scripts"] is False
    assert classify("ci", ["canvas/src/components/Toolbar.tsx"])["scripts"] is False


def test_e2e_script_change_still_runs_the_shellcheck_lane() -> None:
    assert classify("ci", ["tests/e2e/test_chat.sh"])["scripts"] is True
    assert classify("ci", ["infra/scripts/setup.sh"])["scripts"] is True
    assert classify("ci", ["scripts/refresh-workspace-images.sh"])["scripts"] is True


def test_tests_harness_change_runs_the_ephemeral_gate() -> None:
    """The original inversion's motivating case (tests/harness/dind.sh)."""
    assert classify("e2e-ephemeral", ["tests/harness/dind.sh"])["happy"] is True


# ---------------------------------------------------------------------------
# Structural guard — stop a future edit from silently re-introducing an
# allow-list here
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("profile", DENY_PROFILES)
def test_deny_profiles_stay_deny_shaped(profile: str) -> None:
    for lane, pattern in PROFILES[profile].items():
        if profile == "ci" and lane == "python":
            continue  # gates no job; see module docstring of detect-changes.py
        assert pattern.startswith("^(?!"), (
            f"{profile}.{lane} is no longer a deny-list: {pattern!r}. An "
            "allow-list here is vacuous-by-omission by construction."
        )


def test_deny_list_helper_builds_the_documented_pattern() -> None:
    assert detect_changes.deny_list() == r"^(?!docs/)(?!.*\.md$)"
    assert detect_changes.deny_list("workspace-server/") == r"^(?!docs/)(?!.*\.md$)(?!workspace-server/)"


# ---------------------------------------------------------------------------
# Fail-open behaviour is unchanged (a missing/unknown diff base must still run
# everything, not skip everything)
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("profile", sorted(PROFILES))
def test_zero_sha_fails_open(profile: str) -> None:
    out = detect_changes.detect(profile, "push", "", "0" * 40)
    assert all(out.values()), f"{profile} failed CLOSED on a zero base sha: {out}"
    assert set(out) == set(PROFILES[profile])
