"""Every workflow STEP that rolls the staging tenant fleet must wire the managed
rollout flags in — and must never splice the repo var into shell code.

WHY THIS IS PYTHON AND NOT A grep IN test-managed-flags.sh
----------------------------------------------------------
The bash lint could only ever be FILE-level, and that is not good enough here:
`staging-tenant-cd.yml` invokes the fleet script from TWO different steps (the
forward roll, and the rollback on a failed candidate). Delete the wiring from one
and a file-level grep still finds the other's, so it stays green while half the
call sites are silently unwired. Reviewers proved exactly that mutation slipping
through. Scoping the assertion to the STEP requires parsing the YAML, so it lives
here, where CI already runs pytest over .gitea/scripts/tests on any scripts/**
change.

WHAT IS BEING PROTECTED
-----------------------
redeploy-staging-fleet.sh STRIPS every key in MANAGED_FLAG_KEYS from the tenant
env it inherits by copy, and re-applies only what TENANT_FLAGS names. That is what
makes a rollout flag REVERSIBLE — tenant env is inherited by copy, so a hand-set
flag is otherwise sticky forever and a burn-in can never be un-flipped.

The corollary is sharp: a call site that does not pass TENANT_FLAGS does not get
"the old behaviour". It silently turns those flags OFF across the whole fleet. The
first cut of this PR shipped exactly that — the var was wired into ZERO callers, so
any unrelated merge to main would have ended an in-flight burn-in with no log line.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOWS = ROOT / ".gitea" / "workflows"

SCRIPT_PATH = "scripts/deploy/redeploy-staging-fleet.sh"
SCRIPT_BASENAME = "redeploy-staging-fleet.sh"

# ---------------------------------------------------------------------------
# CALL-SITE DISCOVERY — BY COMMAND POSITION, NOT BY A `bash <path>` REGEX.
#
# The old SCRIPT_RE was `(^|\s)bash\s+scripts/deploy/redeploy-staging-fleet\.sh`. A
# step became invisible to it — and therefore to EVERY assertion in this file,
# including the injection guard — the moment the invocation was written any other way:
# a quoted path, `./scripts/...`, `sh -c "..."`, an env-assignment prefix, or a
# `\`-continuation. Review demonstrated the dark-fleet end to end: quoting the path in
# the MANUAL redeploy workflow + deleting its env line stripped both managed flags with
# CI fully green, because the lint could no longer see the step.
#
# The fix is not a bigger regex — that just moves the hole. It is to model the actual
# question: IS THIS STEP EXECUTING THE SCRIPT? An execution puts the script in COMMAND
# position; a mere mention (a `shellcheck <script>` lint arg, a `paths:` filter, a
# comment) does not. So we find command-position invocations, AND — default-deny — we
# refuse to stay silent on any OTHER appearance of the basename we don't recognise: an
# unrecognised invocation form must TRIP the guard, not slip past it, exactly as the
# SSOT status guard does.
# ---------------------------------------------------------------------------

# Commands that legitimately take the script path as a NON-executing ARGUMENT. The
# script is named, not run. These are allow-listed by name so that anything NOT on the
# list which mentions the script trips the default-deny arm.
_NON_EXECUTING_CONSUMERS = (
    "shellcheck", "cat", "ls", "cp", "mv", "rm", "chmod", "chown", "git",
    "grep", "echo", "printf", "wc", "head", "tail", "sed", "awk", "stat",
    "shfmt", "diff", "test", "[",
)

# The script path in COMMAND position: first token of a simple command, after an
# optional interpreter (`bash`/`sh`), an optional `./`, and optional quotes. Matches
# `bash scripts/…`, `bash "scripts/…"`, `sh scripts/…`, `./scripts/…`, `scripts/…`.
_EXEC_RE = re.compile(
    r"""(?:(?:bash|sh)\s+)?          # optional interpreter
        ["']?\.?/?                    # optional ./ and quote
        """ + re.escape(SCRIPT_PATH) + r"""
    """,
    re.VERBOSE,
)


def _logical_lines(run: str):
    """Yield each logical line of a run body, comments stripped, `\\`-continuations
    joined (so a `shellcheck \\`-continued arg list is ONE line whose first command is
    `shellcheck`)."""
    joined = re.sub(r"\\\n", " ", run)
    for logical in joined.splitlines():
        no_comment = re.sub(r"(^|\s)#.*$", "", logical)
        if no_comment.strip():
            yield no_comment


def _simple_commands(logical: str):
    """Split a logical line into simple commands on `&&`, `||`, `;`, `|`."""
    for cmd in re.split(r"&&|\|\||[;|]", logical):
        cmd = cmd.strip()
        if cmd:
            yield cmd


# A pipeline that feeds ANYTHING into a shell: `... | bash`, `|sh`, `| bash -s`. When
# the script is what is being fed in (`cat SCRIPT | bash -s -- …`), the script IS
# executed — the leading `cat`/`head`/`sed` is NOT "naming it as an argument", it is
# streaming its bytes into an interpreter. Review drove the fleet dark exactly here:
# the `cat SCRIPT` segment took the non-executing-consumer free pass while the `| bash`
# segment did not contain the basename, so the step was invisible to BOTH arms.
#
# The interpreter is matched with an optional path and an optional `env` wrapper, so
# `| /bin/bash`, `| /usr/bin/env bash`, and `| env sh` all count — review demonstrated
# that a bare-`bash`-only recogniser was re-opened by exactly those spellings. `\b`
# after (bash|sh) keeps `| shellcheck` / `| shfmt` from tripping it.
_PIPE_TO_SHELL_RE = re.compile(r"\|\s*(?:\S*/)?(?:env\s+)?(?:bash|sh)\b")


def _first_word(cmd: str) -> str:
    """First token of a simple command, after stripping leading `VAR=value` env
    assignments (which precede the command they wrap)."""
    toks = cmd.split()
    i = 0
    while i < len(toks) and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", toks[i]):
        i += 1
    return toks[i] if i < len(toks) else ""


def _classify_run(run: str):
    """Return (executes, unknown_mentions) for one run body.

    executes         — the script is run in command position (these MUST be wired).
    unknown_mentions — the basename appears somewhere this parser did not classify as
                       either an execution or an allow-listed non-executing consumer.
                       Non-empty is a hard failure: an invocation form we don't
                       understand must not pass silently.
    """
    executes = False
    unknown = []
    for logical in _logical_lines(run):
        if SCRIPT_BASENAME not in logical:
            continue

        # WHOLE-LINE CHECK FIRST: is the script streamed into a shell? If so the line
        # is an execution no matter what the leading filter is, and the per-segment
        # allow-list must NOT get a chance to wave it through.
        #
        # Gated on the BASENAME, not the full path: under `working-directory:
        # scripts/deploy` (or after a `cd`) the line reads `cat redeploy-staging-fleet.sh
        # | bash …` with no `scripts/…` prefix, and review drove the fleet dark exactly
        # that way. The basename is already guaranteed present (loop guard above), so
        # this is just making the intent explicit.
        if _PIPE_TO_SHELL_RE.search(logical):
            executes = True
            continue

        for cmd in _simple_commands(logical):
            if SCRIPT_BASENAME not in cmd:
                continue
            fw = _first_word(cmd)
            fw_norm = fw.lstrip('"').lstrip("'").lstrip("./")
            is_exec = (
                (fw in ("bash", "sh") or fw_norm.startswith("scripts/"))
                and _EXEC_RE.search(cmd) is not None
            )
            if is_exec:
                executes = True
            elif fw in _NON_EXECUTING_CONSUMERS:
                pass  # the script is an argument being linted/copied/read, not run
            else:
                # A shape this parser does not recognise as either an execution or an
                # allow-listed mention. Default-deny: fail loudly rather than let a
                # possibly-unwired execution through.
                unknown.append(logical.strip())
    return executes, unknown


def _workflow_files():
    # Glob BOTH extensions, and composite actions — the old `*.yml`-only glob would
    # miss a `.yaml` workflow or a `.gitea/actions/**/action.y*ml` that rolled the
    # fleet. There are none today; the point is that adding one cannot silently escape.
    seen = set()
    for pat in ("*.yml", "*.yaml"):
        for wf in WORKFLOWS.glob(pat):
            seen.add(wf)
    actions = ROOT / ".gitea" / "actions"
    if actions.is_dir():
        for pat in ("action.yml", "action.yaml"):
            for a in actions.rglob(pat):
                seen.add(a)
    return sorted(seen)

# The one correct shape: the repo var arrives as an env VALUE, and the shell — not
# the templater — supplies the default. `${STAGING_TENANT_FLAGS-}` matters: an
# undefined repo var may arrive as an OMITTED env entry rather than an empty one,
# and the script fails closed on an UNSET TENANT_FLAGS. Without the `-` default, a
# repo that never created the var would hard-abort every staging CD roll.
PASSES_RE = re.compile(r'TENANT_FLAGS="\$\{STAGING_TENANT_FLAGS-\}"')
# The env VALUE must be the repo var itself. Asserting only that the KEY exists lets
# a typo'd or renamed var (`vars.STAGING_TENANT_FLAG`) — or a hardcoded "" — sail
# through: TENANT_FLAGS then arrives SET-AND-EMPTY, the script's fail-closed check
# stays silent by design, and every managed flag is stripped off the fleet with no
# log line. A rename is the single most likely real-world drift here.
EXPECTED_VALUE = "${{ vars.STAGING_TENANT_FLAGS }}"
# When the invocation does not carry TENANT_FLAGS= inline, the run: body must export
# it — a bare assignment does not reach the child process.
EXPORTS_RE = re.compile(r"^\s*export\s+TENANT_FLAGS\b", re.MULTILINE)
# Inline form: `TENANT_FLAGS="..." <exec>`, where <exec> is ANY of the execution
# spellings _EXEC_RE understands — bash/sh, `./`, quoted, bare. Hard-coding
# `(bash|sh)\s+scripts/…` here (as the first cut did) would FALSE-POSITIVE on a
# correctly-wired `./scripts/…` roll, and a guard that flags correct code gets
# deleted — taking the real protection with it. Built from the same exec pattern so
# discovery and wiring never diverge on what "an invocation" is.
INLINE_RE = re.compile(
    r'TENANT_FLAGS="\$\{STAGING_TENANT_FLAGS-\}"\s*(?:\\\s*\n\s*)?'
    + _EXEC_RE.pattern,
    re.VERBOSE,
)


def _iter_steps():
    """Every (file, job_name, step, run) across all workflows AND composite actions.
    Composite actions nest steps under `runs.steps`, not `jobs.*.steps`."""
    for wf in _workflow_files():
        doc = yaml.safe_load(wf.read_text(encoding="utf-8")) or {}
        for job_name, job in (doc.get("jobs") or {}).items():
            for step in job.get("steps") or []:
                yield wf, job_name, step, (step.get("run") or "")
        # Composite action: runs.steps
        runs = doc.get("runs") or {}
        for step in runs.get("steps") or []:
            yield wf, "runs", step, (step.get("run") or "")


def _steps_that_roll_the_fleet():
    for wf, job_name, step, run in _iter_steps():
        executes, _ = _classify_run(run)
        if executes:
            yield wf, job_name, step, run


def test_there_is_at_least_one_call_site():
    """Otherwise every assertion below is vacuously true."""
    assert list(_steps_that_roll_the_fleet()), (
        "no workflow step invokes redeploy-staging-fleet.sh — this guard would "
        "pass no matter how broken the wiring got"
    )


def test_no_unrecognised_invocation_form_escapes_the_parser():
    """DEFAULT-DENY on call-site discovery.

    The whole class of bug here is a real invocation the lint cannot SEE — review drove
    the fleet dark by quoting the path so the old `bash <path>` regex missed it, which
    silently un-scoped every assertion including the injection guard. So any appearance
    of the script basename in a run body that the parser classifies as NEITHER an
    execution NOR an allow-listed non-executing mention (a `shellcheck` arg, etc.) is a
    hard failure: the maintainer must either wire it (if it runs the script) or teach
    the allow-list (if it merely names it). Silence is not an option — that silence is
    exactly how the flags went dark with green CI.
    """
    offenders = []
    for wf, job_name, step, run in _iter_steps():
        _, unknown = _classify_run(run)
        for line in unknown:
            offenders.append(f"{wf.name} :: {job_name} :: {step.get('name')!r} -> {line}")
    assert not offenders, (
        "a run body names redeploy-staging-fleet.sh in a form the wiring lint does not "
        "recognise. If it EXECUTES the script it must be caught by the command-position "
        "parser (and then carry STAGING_TENANT_FLAGS); if it merely NAMES it, add the "
        "consuming command to _NON_EXECUTING_CONSUMERS. Do not leave it unclassified — "
        "an unseen call site is how the managed flags went dark with CI green.\n    "
        + "\n    ".join(offenders)
    )


def test_every_fleet_roll_step_wires_the_managed_flags():
    for wf, job_name, step, run in _steps_that_roll_the_fleet():
        where = f"{wf.name} :: job={job_name} :: step={step.get('name')!r}"
        env = step.get("env") or {}

        assert "STAGING_TENANT_FLAGS" in env, (
            f"{where} rolls the staging fleet but never declares STAGING_TENANT_FLAGS "
            f"in its env:. The script strips every managed rollout flag from the "
            f"inherited tenant env and re-applies only what TENANT_FLAGS names, so "
            f"this step would silently turn those flags OFF on every tenant — ending "
            f"an in-flight burn-in with no log line."
        )
        value = str(env["STAGING_TENANT_FLAGS"]).strip()
        assert value == EXPECTED_VALUE, (
            f"{where} wires STAGING_TENANT_FLAGS to {value!r}, expected "
            f"{EXPECTED_VALUE!r}. A typo'd/renamed repo var — or a hardcoded empty "
            f"string — makes TENANT_FLAGS arrive SET-AND-EMPTY. The script's "
            f"fail-closed check then stays silent BY DESIGN, and every managed flag "
            f"is stripped off the whole fleet on every roll, with no log line. "
            f"Checking only that the key exists does not catch this."
        )

        assert PASSES_RE.search(run), (
            f"{where} declares STAGING_TENANT_FLAGS but never passes it to the "
            f'script. Expected TENANT_FLAGS="${{STAGING_TENANT_FLAGS-}}" on the '
            f"invocation. The `-` default is load-bearing: an undefined repo var may "
            f"arrive UNSET, and the script fails closed on that."
        )

        # ...and it must actually REACH the child process. A standalone
        # `TENANT_FLAGS=...` assignment without an `export` does not: the script then
        # sees it unset and fails closed — loudly, but in the ROLLBACK path, i.e. at
        # the exact moment you are already undoing a bad deploy. Either form is fine;
        # having neither is not.
        assert INLINE_RE.search(run) or EXPORTS_RE.search(run), (
            f"{where} assigns TENANT_FLAGS but neither exports it nor puts it on the "
            f"invocation, so the script will not see it and will abort the roll."
        )


def test_the_repo_var_is_never_interpolated_into_shell_code():
    """`${{ vars.X }}` inside a `run:` body is spliced in as SHELL CODE.

    These jobs are `runs-on: docker-host` — privileged, with the docker socket. A
    repo-var value of `x"; touch /tmp/PWNED; echo "` would execute. The var belongs
    in `env:`, as a value. (It was an `export TENANT_FLAGS="${{ vars.… }}"` inside a
    run: body once; review demonstrated the injection.)
    """
    fleet_workflows = {wf.name for wf, _, _, _ in _steps_that_roll_the_fleet()}
    offenders = []
    for wf in sorted(WORKFLOWS.glob("*.yml")):
        if wf.name not in fleet_workflows:
            continue  # scoped to the workflows this change owns
        doc = yaml.safe_load(wf.read_text(encoding="utf-8")) or {}
        for job_name, job in (doc.get("jobs") or {}).items():
            for step in job.get("steps") or []:
                run = step.get("run") or ""
                for m in re.finditer(r"\$\{\{\s*vars\.(\w+)", run):
                    offenders.append(
                        f"{wf.name} :: {job_name} :: {step.get('name')!r} "
                        f"-> vars.{m.group(1)}"
                    )

    assert not offenders, (
        "a repo var is interpolated into a run: body — on a privileged runner that "
        "is a shell-injection sink, whatever the var is named. Pass it via env: and "
        f"read it as a shell variable instead. Offenders: {offenders}"
    )


@pytest.mark.parametrize("wf_name", ["staging-tenant-cd.yml"])
def test_the_rollback_path_is_covered_too(wf_name: str):
    """The rollback is the least-exercised path and the worst one to get wrong: it
    only runs when a candidate deploy already failed. If it strips the managed flags,
    a failed candidate ALSO (invisibly) ends an in-flight burn-in."""
    steps = [s for w, _, s, _ in _steps_that_roll_the_fleet() if w.name == wf_name]
    assert len(steps) >= 2, (
        f"{wf_name} should roll the fleet from BOTH the forward-roll step and the "
        f"rollback step; found {len(steps)}. If the rollback stopped calling the "
        f"script, delete this test deliberately — do not let it silently pass."
    )


def test_pipe_into_a_shell_is_an_execution_not_a_mention():
    """Regression pin for the round-3 escape.

    `cat <script> | bash -s -- …` RUNS the fleet, but the classifier used to split on
    `|` and judge each segment alone: `cat <script>` took the non-executing-consumer
    free pass, and `bash -s` did not contain the basename — so the step slipped past
    BOTH the exec check and default-deny, and review drove the fleet dark with green CI.
    Every filter whose stdout is piped into a shell must count as an execution.
    """
    for filt in ("cat", "head", "tail", "sed 's/a/b/'", "awk '{print}'", "grep ."):
        body = f"{filt} {SCRIPT_PATH} | bash -s -- --tag x"
        executes, unknown = _classify_run(body)
        assert executes, (
            f"`{body}` streams the fleet script into a shell — it RUNS it — but the "
            f"classifier did not mark it as an execution, so the wiring assertions never "
            f"apply to it and the managed flags can be stripped with no log line."
        )
    # The interpreter spelling must not matter: a pathed or env-wrapped shell runs the
    # script just the same. Review re-opened the hole with exactly these.
    for interp in ("/bin/bash -s", "/usr/bin/env bash -s", "env sh", "bash"):
        body = f"cat {SCRIPT_PATH} | {interp}"
        executes, _ = _classify_run(body)
        assert executes, (
            f"`{body}` pipes the script into a shell but was not seen as an execution — "
            f"a pathed/env-wrapped interpreter runs it identically to a bare `bash`."
        )
    # ...and the script may be named by BASENAME alone under `working-directory:`/`cd`,
    # with no `scripts/…` prefix on the line. That was a demonstrated dark path too.
    executes, _ = _classify_run(f"cat {SCRIPT_BASENAME} | bash -s -- --tag x")
    assert executes, (
        f"`cat {SCRIPT_BASENAME} | bash` (no path prefix — the step ran under "
        f"working-directory: scripts/deploy) pipes the script into a shell but was not "
        f"seen as an execution. Gating the pipe check on the full path re-opened this."
    )
    # ...but a pipe to a NON-shell is genuinely just reading, and must not be an exec.
    executes, unknown = _classify_run(f"cat {SCRIPT_PATH} | grep TENANT_FLAGS")
    assert not executes and not unknown, (
        "`cat <script> | grep …` reads the script, it does not run it — flagging it "
        "would cry wolf on a legitimate lint/inspection and get the guard deleted."
    )


# Every in-repo file that EXECUTES the fleet script, other than the workflows this
# guard already checks and the tests that use it as a fixture. A shell WRAPPER
# (`scripts/deploy/roll-wrapper.sh` that runs the fleet internally) is invisible to a
# workflow-level lint — the basename never appears in any run body — so a new one could
# roll the fleet unwired with the guard none the wiser. This is default-deny on
# CALLERS: the set of in-repo callers is pinned, and adding one forces a deliberate
# choice (wire it + list it here, or don't add it), instead of a silent latent hole.
_KNOWN_FLEET_CALLERS = {
    # (relative path) : why it is allowed to name/run the script without this lint
    #                   asserting TENANT_FLAGS on it
    "scripts/deploy/tests/test-managed-flags.sh": "the bash test harness; drives the script with its own fixtures",
    "scripts/deploy/redeploy-staging-fleet.sh": "the script itself",
    "scripts/deploy/probe-enteros-buildinfo.sh": (
        "MENTION-ONLY (Enter OS Phase 2, internal#1089): the advisory enteros.ai "
        "edge probe's comments point at the fleet script as the BLOCKING gate and "
        "mirror its BRAND_PREFIXES / json_git_sha helpers. It never executes the "
        "fleet script and rolls nothing, so TENANT_FLAGS does not apply."
    ),
}


def test_no_unlisted_in_repo_file_invokes_the_fleet_script():
    """The wrapper-indirection boundary, made enforceable.

    Workflows are covered by the command-position parser above. In-repo *scripts* are
    not — a wrapper that runs the fleet script internally never shows up in a run body.
    So pin the caller set: any scripts/ file that names redeploy-staging-fleet.sh must
    be on _KNOWN_FLEET_CALLERS, and a new one is a deliberate decision, not a silent
    gap. (Reviewer-suggested cheap closer for the latent wrapper case.)

    KNOWN, ACCEPTED SCOPE LIMITS (this is a grep, and grep has a floor):
      - It scans `scripts/**/*.sh` only. A caller under `.gitea/scripts/`, or a
        `.py`/other-language file that shells out to the fleet script, is not scanned.
      - It matches the LITERAL basename. A wrapper that assembles the name from pieces
        (`S=redeploy-staging-fleet; bash "scripts/deploy/$S.sh"`) is invisible to it.
    Both are DELIBERATE OBFUSCATION, not plausible accidental drift — you have to be
    trying to hide an execution from the linter. The real defence against a genuinely
    unwired caller is the script's own fail-closed-on-unset check plus code review; this
    test closes the one *accidental* form (a new plainly-named wrapper) cheaply. The
    out-of-repo caller is unreachable by any in-repo lint, full stop.
    """
    scripts_dir = ROOT / "scripts"
    offenders = []
    for path in scripts_dir.rglob("*.sh"):
        rel = path.relative_to(ROOT).as_posix()
        if rel in _KNOWN_FLEET_CALLERS:
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        # Any mention of the basename in a scripts/ file that is not a known caller.
        if SCRIPT_BASENAME in text:
            offenders.append(rel)
    assert not offenders, (
        "an in-repo script names redeploy-staging-fleet.sh but is not a known caller.\n"
        "    A wrapper that runs the fleet script is INVISIBLE to the workflow-level "
        "wiring lint — the basename never appears in a run body — so it could roll the "
        "fleet with the managed flags stripped and this guard would never see it.\n"
        "    If this file EXECUTES the fleet script, make it pass TENANT_FLAGS through "
        "(the same contract the workflows honour) and add it to _KNOWN_FLEET_CALLERS "
        "with a note. If it merely mentions it, add it with that note. Do not leave it "
        f"unlisted.\n    Offenders: {offenders}"
    )
