"""Pin that the concierge e2e lane can actually BLOCK a merge.

WHY THIS EXISTS
---------------
A concierge shipped to production with NO identity: its /configs/config.yaml
declared ``prompt_files: [prompts/concierge.md]`` while that file was never
materialized, so the runtime loaded nothing for its role, the agent introduced
itself as its base runtime, and it never called a single management verb.

Nothing pre-merge could stop it. The concierge scenarios ran on EVERY PR — and
every one of them was ADVISORY, so a red concierge lane merged green. The only
check that exercises a real concierge (``staging-tenant-cd / e2e-smoke``) runs
POST-merge, i.e. four minutes too late.

These assertions are structural, not prose: they read the workflow env and fail
if a scenario is silently demoted back to the soak.

WHAT IS STILL NOT COVERED (deliberately asserted, so it cannot be forgotten)
---------------------------------------------------------------------------
``concierge_platform_agent`` is DB/handler state — "no boot". It never starts a
runtime, builds a system prompt, registers an MCP, or makes a model call. The
present+CALLABLE half (``TestPlatformAgentMgmtMCP_Staging``) needs a real
concierge boot and is deferred to ``E2E_EPHEMERAL_REAL_RUNTIME_MATRIX`` — which
is referenced ONLY in comments and was never implemented. test_real_runtime_
matrix_gap_is_declared() pins that the gap is documented at the point of use, so
the next person reading the gating list learns what it does NOT buy them.
"""

from __future__ import annotations

import pathlib
import re
import unittest

REPO = pathlib.Path(__file__).resolve().parents[3]
WORKFLOW = REPO / ".gitea" / "workflows" / "e2e-ephemeral-happy-path.yml"

# Scenarios that must be able to fail the required context. Each maps to a
# product invariant a customer would notice immediately if it broke.
MUST_GATE = {
    "peer_visibility": "the MCP list_peers auth contract",
    "concierge_creates_workspace": "the concierge can create a workspace",
    "concierge_platform_agent": "the platform agent installs with the right identity/kind",
}


def _env_value(key: str) -> str:
    text = WORKFLOW.read_text(encoding="utf-8")
    m = re.search(rf"^\s*{re.escape(key)}:\s*(.+?)\s*$", text, re.MULTILINE)
    if not m:
        raise AssertionError(f"{key} not found in {WORKFLOW.name}")
    return m.group(1).strip().strip('"').strip("'")


class ConciergeLaneIsMergeBlocking(unittest.TestCase):
    def test_workflow_exists(self):
        # Non-vacuity: every other assertion reads this file, so prove it is there.
        self.assertTrue(WORKFLOW.is_file(), f"missing {WORKFLOW}")

    def test_required_scenarios_are_in_the_gating_list(self):
        gating = {s.strip() for s in _env_value("E2E_EPHEMERAL_EXTRA_GATING").split(",") if s.strip()}
        for scenario, why in sorted(MUST_GATE.items()):
            with self.subTest(scenario=scenario):
                self.assertIn(
                    scenario,
                    gating,
                    f"{scenario} ({why}) is NOT in E2E_EPHEMERAL_EXTRA_GATING — it runs on "
                    f"every PR but its failure cannot block a merge. That is how a "
                    f"persona-less concierge reached production. Re-add it, or delete "
                    f"this entry from MUST_GATE with a written reason.",
                )

    def test_gating_scenarios_are_actually_run(self):
        # A scenario listed as gating but never executed is a vacuous gate: it can
        # never go red, so it looks enforced while covering nothing.
        scenarios = {s.strip() for s in _env_value("E2E_EPHEMERAL_EXTRA_SCENARIOS").split(",") if s.strip()}
        gating = {s.strip() for s in _env_value("E2E_EPHEMERAL_EXTRA_GATING").split(",") if s.strip()}
        orphaned = sorted(gating - scenarios)
        self.assertEqual(
            orphaned,
            [],
            f"gating scenario(s) {orphaned} are not in E2E_EPHEMERAL_EXTRA_SCENARIOS — "
            f"they never run, so they can never fail: a gate covering nothing.",
        )

    def test_real_runtime_matrix_gap_is_declared(self):
        # The boot+callable half is still post-merge only. Require that the gap is
        # spelled out where the gating list is set, so nobody mistakes this lane for
        # proof that a concierge actually boots and calls its management verbs.
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn(
            "E2E_EPHEMERAL_REAL_RUNTIME_MATRIX",
            text,
            "the real-runtime (boot + callable) gap must stay named in this workflow",
        )
        self.assertRegex(
            text,
            r"no boot|does not give|NOT give",
            "the gating list must state that concierge_platform_agent is a no-boot "
            "check, so its green is not read as 'a concierge really works'",
        )


if __name__ == "__main__":
    unittest.main()
