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
        """Negative control: add `continue-on-error: true` to happy-path -> this reds."""
        text = code_only(WORKFLOW.read_text())
        happy = text.split("\n  happy-path:", 1)[1]
        # Only the job's own keys, i.e. up to the first step.
        job_header = happy.split("\n    steps:", 1)[0]
        self.assertNotIn(
            "continue-on-error: true",
            job_header,
            "The happy-path job is masked with continue-on-error: true. Task #113 "
            "proved the mask does not even suppress the commit status under BP "
            "['*'] — so it buys nothing and lies to the reader about whether this "
            "gate can fail.",
        )

    def test_no_arm_can_exit_green_without_running_the_gate(self):
        """The property, not the shape: every non-running arm must FAIL, not skip.

        Negative control: revert the fork step to `::notice::` with no `exit 1`, or
        delete the empty-secret check in the creds step -> this reds.

        Both of these were real: the fork arm posted SUCCESS on an enforced context
        with a cheerful notice, and the creds step succeeded on an EMPTY Infisical
        value while every downstream step was gated on `env.AUTO_SYNC_TOKEN != ''`
        and therefore skipped. Either one turns the gate green having proved
        nothing — an Infisical rotation would have silently disarmed the lane.
        """
        text = code_only(WORKFLOW.read_text())

        # The fork arm must fail closed on a code diff.
        fork_steps = [
            b
            for b in text.split("\n      - name:")
            if "head.repo.fork == true" in b.split("run:", 1)[0]
        ]
        self.assertTrue(
            fork_steps,
            "No fork-PR arm found at all. A fork gets no e2e creds, so it must have "
            "an arm — and that arm must FAIL, not silently green.",
        )
        for step in fork_steps:
            self.assertIn(
                "exit 1",
                step,
                "The fork-PR arm exits 0. Its context is merge-queue REQUIRED, so "
                "that is a GREEN required check on a PR the gate never ran against. "
                "A gate that cannot observe its subject must not report success.",
            )

        # The creds step must fail closed on an EMPTY secret value, and no step may
        # re-introduce a skip-guard that turns empty creds into a silent green.
        self.assertNotIn(
            "env.AUTO_SYNC_TOKEN != ''",
            "\n".join(
                l for l in text.splitlines() if l.strip().startswith("if:")
            ),
            "A step is gated on `env.AUTO_SYNC_TOKEN != ''`. That is the skip-guard "
            "that made empty Infisical creds produce a GREEN required context with "
            "zero coverage. Fail closed in the creds step instead — never skip.",
        )
        self.assertRegex(
            text,
            r'\[ -n "\$AUTO_SYNC_TOKEN" \]',
            "The creds step does not assert AUTO_SYNC_TOKEN is non-empty. "
            "`curl -f` does not fire when Infisical returns an empty VALUE (renamed "
            "key, moved secretPath, blank secret), so the step would succeed with "
            "no credential.",
        )
        self.assertRegex(
            text,
            r'\[ -n "\$MINIMAX_API_KEY" \]',
            "The creds step does not assert MINIMAX_API_KEY is non-empty — the gate "
            "would spin up a CP it cannot drive an LLM turn against.",
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
