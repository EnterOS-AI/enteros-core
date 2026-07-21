"""The ephemeral-CP happy-path gate must be a REAL gate — and stay one.

Two mechanisms decide whether a check can stop a merge on molecule-core, and they
cover different holes:

  branch protection (status_check_contexts = ['*'])
      Every POSTED context must be `success`. Catches a RED check.
      BLIND to a context that never posts at all.

  .gitea/required-contexts.txt (enforced by gitea-merge-queue.py)
      Every entry ABOVE the first `# pending-#NNNN` marker must be PRESENT and
      `success` on the PR head. Catches a MISSING check.

`E2E Ephemeral CP Happy Path` was added to required-contexts.txt by #4271, whose
commit message read "make it REQUIRED" — but the line was appended to the END of
the file, landing it under `# pending-#3159`, in the documented-but-NOT-enforced
half. So for a day the gate was red-blocking (via BP) but not presence-checked:
narrow its `on:` filter, disable the workflow, rename the job, or let its run be
cancelled while still queued, and it posts nothing, BP sees a clean board, and the
merge proceeds. That is a phantom gate, and it is the exact class this lane was
built to delete.

These tests pin the properties that make it real. Each has a REACHABLE fail arm —
they were each negative-controlled by performing the mutation and watching them go
red (see the docstrings):

  1. the context is merge-queue ENFORCED (above the first pending marker);
  2. the producing workflow ALWAYS FIRES (no `paths:` filter — a paths-filtered
     workflow can never be a required context, it just never posts);
  3. the gating job carries NO `continue-on-error` (proven in task #113 to be a
     no-op mask under BP ['*'] anyway, but it would still lie to a reader);
  4. no arm of the job can reach a GREEN exit without having run the gate —
     specifically the fork arm and the empty-creds path must FAIL, not skip.

(4) is the one that matters most and is the easiest to regress: every "skip" that
exits 0 on an enforced context is a silent, permanent hole.
"""

import re
import unittest

import yaml
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
WORKFLOW = REPO / ".gitea/workflows/e2e-ephemeral-happy-path.yml"
REQUIRED = REPO / ".gitea/required-contexts.txt"

CONTEXT = "E2E Ephemeral CP Happy Path / E2E Ephemeral CP Happy Path"

# The merge queue's own marker regex — kept byte-identical to
# gitea-merge-queue.py::_PENDING_MARKER_RE so this test cannot drift from the
# thing it is asserting about.
_PENDING_MARKER_RE = re.compile(r"^\s*#\s*pending-#\d+\b", re.IGNORECASE)


def code_only(text: str) -> str:
    """Strip YAML comments.

    Without this, every assertion here is satisfiable — or falsifiable — by PROSE.
    The first draft of test_gating_job_has_no_continue_on_error reded against the
    correct workflow, because the job's comment block contains the sentence
    "`continue-on-error: true` is GONE". A guard that reads comments is grading the
    documentation, not the config.
    """
    out = []
    for line in text.splitlines():
        stripped = line.lstrip()
        if stripped.startswith("#"):
            continue
        out.append(line)
    return "\n".join(out)


def enforced_contexts() -> list[str]:
    """Exactly what gitea-merge-queue.py enforces: entries above the FIRST marker."""
    out = []
    for line in REQUIRED.read_text().splitlines():
        if _PENDING_MARKER_RE.match(line):
            break
        s = line.strip()
        if s and not s.startswith("#"):
            out.append(s)
    return out


class TestEphemeralGateIsEnforced(unittest.TestCase):
    def test_context_is_merge_queue_enforced_not_merely_documented(self):
        """Negative control: move the line below `# pending-#86` -> this reds.

        Being present in required-contexts.txt is NOT enough. Everything at or
        below the first pending marker is documented-but-unenforced, which is
        where this line spent its first day.
        """
        self.assertIn(
            CONTEXT,
            enforced_contexts(),
            f"{CONTEXT!r} is not in the ENFORCED block of .gitea/required-contexts.txt.\n"
            "If it is in the file but below a `# pending-#NNNN` marker, it is "
            "DOCUMENTED, not enforced: the merge queue will not require it to be "
            "present, so a run that never posts (narrowed `on:`, disabled workflow, "
            "renamed job, cancelled-while-queued) lets the merge through with the "
            "gate silently absent. Move it back above the first marker.",
        )

    def test_gate_always_fires(self):
        """Negative control: add `paths:` under `on:` -> this reds.

        A paths-filtered workflow cannot be a required context: on an unrelated
        diff it never fires, so the context never posts and the PR wedges (or, if
        it is not presence-checked, silently passes). Scoping belongs INSIDE the
        always-running job, via the inverted detect-changes profile.
        """
        text = code_only(WORKFLOW.read_text())
        on_block = text.split('"on":', 1)[1].split("\nconcurrency:", 1)[0]
        self.assertNotIn(
            "paths:",
            on_block,
            "e2e-ephemeral-happy-path.yml has a `paths:` filter under `on:`. Its "
            "context is merge-queue REQUIRED, so a diff outside those paths would "
            "never post it. Scope inside the job (detect-changes `e2e-ephemeral` "
            "profile), which no-ops but STILL POSTS.",
        )

    def test_gating_job_has_no_continue_on_error(self):
        """Negative control: add `continue-on-error: true` to happy-path -> this reds.

        Parsed, not string-matched: `continue-on-error: ${{ true }}` is just as much a
        mask, and a substring check for the literal `true` misses it.
        """
        wf = yaml.safe_load(WORKFLOW.read_text())
        coe = wf["jobs"]["happy-path"].get("continue-on-error", False)
        self.assertIn(
            coe,
            (False, None),
            f"The happy-path job sets continue-on-error={coe!r}. Task #113 proved the "
            "mask does not even suppress the commit status under BP ['*'] — it buys "
            "nothing and lies to the reader about whether this gate can fail.",
        )

    def test_no_arm_can_exit_green_without_running_the_gate(self):
        """The property, not the shape.

        A Gitea job whose every step SKIPS still SUCCEEDS. So on a REQUIRED
        context, "the gate did not run" and "the gate passed" are the same commit
        status unless something at RUNTIME says otherwise.

        The first version of this test string-matched step conditions, and an
        adversarial review broke it in one line: set the proof step to `if: false`
        and the guard still reported `5 OK` while the job posted SUCCESS having
        spun up nothing. Shape-matching cannot express "some arm ran" — there is
        always another way to skip every arm.

        So the workflow now stamps GATE_ARM at runtime and a final `if: always()`
        step FAILS when no arm stamped it. This test asserts that machinery exists
        and is wired to every arm. Negative controls, all performed:
          - proof step `if: false`            -> sentinel step fails the job (runtime)
          - delete the sentinel step          -> this test reds
          - drop the stamp from either arm    -> this test reds
        """
        wf = yaml.safe_load(WORKFLOW.read_text())
        steps = wf["jobs"]["happy-path"]["steps"]

        def run_of(s):
            return s.get("run") or ""

        # (a) The two GREEN-capable arms must each stamp a distinct GATE_ARM.
        proof = [s for s in steps if "ephemeral_cp_happy_path.sh" in run_of(s)]
        self.assertEqual(len(proof), 1, "expected exactly one proof step")
        self.assertIn(
            "GATE_ARM=proof",
            run_of(proof[0]),
            "The proof step does not stamp GATE_ARM=proof, so nothing at runtime can "
            "tell whether the gate actually ran. `if: false` on this step would post a "
            "GREEN required context with zero coverage.",
        )

        # The proof step's `if:` must be the REAL run-arm condition. A constant
        # (`if: false`) is caught at runtime by the sentinel, but catching it here
        # too means you learn at test time instead of burning a CI run.
        proof_if = str(proof[0].get("if", ""))
        self.assertIn(
            "needs.detect.outputs.happy",
            proof_if,
            f"The proof step's condition is {proof_if!r} — it does not depend on the "
            "detect output, so it is not the run-arm condition. A constant-false `if:` "
            "here means the gate never runs.",
        )

        noop = [s for s in steps if "GATE_ARM=noop" in run_of(s)]
        self.assertEqual(
            len(noop), 1,
            "The docs-only no-op arm does not stamp GATE_ARM=noop. Without it the "
            "sentinel cannot distinguish 'honestly no-op'd' from 'every arm skipped'.",
        )

        # (b) The catch-all must exist, run unconditionally, and be able to FAIL.
        sentinel = [
            s for s in steps
            if "GATE_ARM" in run_of(s) and "exit 1" in run_of(s) and "::error::" in run_of(s)
        ]
        self.assertTrue(
            sentinel,
            "No catch-all sentinel step. A job whose every step skips SUCCEEDS — on a "
            "required context that is a silent, permanent hole. Add an `if: always()` "
            "step that fails when GATE_ARM is unset.",
        )
        cond = str(sentinel[0].get("if", ""))
        self.assertIn(
            "always()",
            cond,
            "The sentinel is not `if: always()`, so it can be skipped by the very "
            "condition bug it exists to catch.",
        )

        # (b2) ORDERING IS LOAD-BEARING: the sentinel must come AFTER every arm
        #     that can stamp GATE_ARM. Steps run top-to-bottom; a sentinel that
        #     precedes the no-op arm fails BEFORE the arm can stamp — which made
        #     every docs-only PR red on this REQUIRED context (first hit:
        #     PR #4413, 2026-07-17). Negative control: reordering the sentinel
        #     above the no-op arm makes this assertion fail.
        sentinel_idx = steps.index(sentinel[0])
        noop_idx = steps.index(noop[0])
        self.assertGreater(
            sentinel_idx, noop_idx,
            "The GATE_ARM sentinel runs BEFORE the docs-only no-op arm, so a "
            "docs-only PR fails the sentinel before the arm can stamp GATE_ARM=noop. "
            "Move the sentinel below every stamping arm.",
        )
        proof_idx = steps.index(proof[0])
        self.assertGreater(
            sentinel_idx, proof_idx,
            "The GATE_ARM sentinel runs BEFORE the proof arm — it can never observe "
            "the proof stamp.",
        )

        # (c) The fork arm must FAIL, not print-and-pass. Check the last effective
        #     command, not a substring: `echo "would exit 1"` must not satisfy this.
        fork = [
            s for s in steps
            if "head.repo.fork == true" in str(s.get("if", ""))
        ]
        self.assertTrue(fork, "No fork-PR arm — a fork gets no creds, so it needs one.")
        for s in fork:
            lines = [l.strip() for l in run_of(s).splitlines() if l.strip()]
            self.assertTrue(
                lines and lines[-1] == "exit 1",
                "The fork arm does not END in a bare `exit 1`. Its context is REQUIRED, "
                "so anything that exits 0 is a green check on a PR the gate never ran.",
            )

        # (d) NO step may be skip-guarded on a secret being non-empty — for ANY var.
        #     The original hole was `env.AUTO_SYNC_TOKEN != ''`; forbidding only that
        #     literal string just moves the hole to the next variable.
        for s in steps:
            cond = str(s.get("if", ""))
            self.assertNotRegex(
                cond,
                r"env\.[A-Z_]*(TOKEN|KEY|SECRET)[A-Z_]*\s*!=\s*''",
                f"Step {s.get('name')!r} is skip-guarded on a secret being non-empty. "
                "That turns 'the credential is missing' into 'the required check is "
                "GREEN'. Fail closed in the creds step instead — never skip.",
            )

    def test_peervis_and_concierge_creates_are_gating_not_advisory(self):
        """peer_visibility + concierge_creates_workspace must be MERGE-BLOCKING.

        #4543 retired the push staging E2E lanes for these two journeys (the literal
        MCP `list_peers` auth contract, and concierge-creates-a-workspace) and moved
        them onto THIS per-PR gate — but it left them in the global advisory soak
        (E2E_EPHEMERAL_EXTRA_ADVISORY=1), so a regression to either merged GREEN. That
        is a coverage hole: the journeys RAN per-PR but could not FAIL the merge.

        The runner (tests/e2e/ephemeral_cp_happy_path.sh::gate_extra_scenarios) gates
        on a failed scenario that is named in E2E_EPHEMERAL_EXTRA_GATING regardless of
        the soak. So the invariant is: both keys appear in the workflow's
        E2E_EPHEMERAL_EXTRA_GATING list AND in the E2E_EPHEMERAL_EXTRA_SCENARIOS list
        it runs. This is the whole point of the #4543 fix.

        Negative control (performed): remove either key from E2E_EPHEMERAL_EXTRA_GATING
        (leave it advisory-only, as #4543 shipped it) -> this reds. Drop the whole
        E2E_EPHEMERAL_EXTRA_GATING line -> this reds.

        Parsed from the YAML env, not string-matched in comments (code_only would not
        help here — the value we assert on is real config, and the prose around it
        deliberately names the same keys).
        """
        wf = yaml.safe_load(WORKFLOW.read_text())
        # Find the step that runs the gate (its env carries the extra-scenario config).
        gate_step = None
        for s in wf["jobs"]["happy-path"]["steps"]:
            if "ephemeral_cp_happy_path.sh" in (s.get("run") or ""):
                gate_step = s
                break
        self.assertIsNotNone(gate_step, "no step runs ephemeral_cp_happy_path.sh")
        env = gate_step.get("env", {})

        def _keys(val: str) -> set[str]:
            return {t for t in re.split(r"[,\s]+", str(val or "")) if t}

        gating = _keys(env.get("E2E_EPHEMERAL_EXTRA_GATING", ""))
        scenarios = _keys(env.get("E2E_EPHEMERAL_EXTRA_SCENARIOS", ""))

        for key in ("peer_visibility", "concierge_creates_workspace"):
            self.assertIn(
                key,
                gating,
                f"{key!r} is not in E2E_EPHEMERAL_EXTRA_GATING, so its failure is "
                "suppressed by the global advisory soak (E2E_EPHEMERAL_EXTRA_ADVISORY=1) "
                "and a regression merges GREEN. #4543 moved this journey onto the "
                "ephemeral gate; it must GATE, not merely run. Add it to "
                "E2E_EPHEMERAL_EXTRA_GATING.",
            )
            self.assertIn(
                key,
                scenarios,
                f"{key!r} is gating-listed but not in E2E_EPHEMERAL_EXTRA_SCENARIOS, so "
                "it never runs — a gate on a scenario that does not execute is vacuous.",
            )

    def test_controlplane_baseline_is_pinned(self):
        """Negative control: replace the fetch with `git clone` of main -> this reds.

        The baseline CP image is built from a FOREIGN repo. Tracking its moving
        `main` made any controlplane commit able to red this — now enforced —
        context on every open core PR at once. Enforced + unpinned is not a red,
        it is a frozen merge queue (task #116).
        """
        text = code_only(WORKFLOW.read_text())
        self.assertRegex(
            text,
            r"CP_EPHEMERAL_REF:\s*[0-9a-f]{40}\b",
            "CP_EPHEMERAL_REF is not pinned to a full 40-char controlplane SHA.",
        )
        self.assertNotRegex(
            text,
            r"git clone[^\n]*molecule-controlplane",
            "The controlplane is `git clone`d rather than fetched at CP_EPHEMERAL_REF. "
            "A shallow clone of `main` drags main's tip down the wire regardless of "
            "what is checked out afterwards — the pin must be what is FETCHED.",
        )


if __name__ == "__main__":
    unittest.main()
