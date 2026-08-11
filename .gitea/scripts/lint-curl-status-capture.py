#!/usr/bin/env python3
"""Lint workflow bash for the two ways a curl-adjacent capture goes wrong.

RULE 1 — status-capture pollution (original rule, unchanged)

    HTTP_CODE=$(curl ... -w '%{http_code}' ... || echo "000")

`curl -w` writes the HTTP code to stdout BEFORE returning non-zero, so a
fallback inside the same command substitution appends a SECOND code and you get
"000000" or "409000" — values that fail every comparison while looking right.

RULE 2 — a capture that dies before its own guard (added 2026-08-11)

    body=$(cat /tmp/cleanup_resp 2>/dev/null | head -c 500)

The mirror image, and the one that actually took main down. curl's `-o` does
NOT open its output file when the transfer dies at DNS/connect/timeout, so the
file is simply absent. `cat` then exits 1. Gitea/act_runner invokes a `run:`
block as

    bash --noprofile --norc -e -o pipefail {0}

so `-e` AND `-o pipefail` are on at line 1 and a step cannot infer its flags
from its own `set` line. `pipefail` promotes `cat`'s failure to the PIPELINE's
status even though `head` succeeded, a bare `VAR=$(pipeline)` inherits it, and
`-e` kills the step — one line ABOVE the `if` that would have printed the
benign warning. `2>/dev/null` hides the message, not the status: the redirect
belongs to `cat`, the failure belongs to the pipeline.

Measured on this shape with the runner's own invocation (2026-08-11):

    flags "-e -o pipefail" -> rc=1, guard never runs
    flags "-o pipefail"    -> rc=0
    flags "-e"             -> rc=0

Both flags are load-bearing; neither alone reproduces it. That is why the rule
keys on the PIPELINE, not on `cat` alone — `body=$(cat f)` on its own is a
different (and louder) failure.

On 2026-08-11 a step whose own comment read "Best-effort. Failure here doesn't
fail the workflow" carried this shape and held main's combined status red for
70m13s (status id=88 failure 01:55:21Z -> id=123 success 03:05:34Z on
21ec47e78748). core#5166 fixed that site; this rule is the regression guard,
which did not exist and which the pre-fix file passed with exit 0.

WHY `|| true` IS NOT THE FIX
----------------------------
`|| true` converts "the command failed" into "the output was empty", which
downstream reads as "the endpoint answered with nothing" — a different bug, and
a quieter one. The fallback must yield a USABLE value that says what happened.
"""
from __future__ import annotations

import argparse
import glob
import os
import re
from pathlib import Path
from typing import NamedTuple

SELF = ".gitea/workflows/lint-curl-status-capture.yml"


def _same_path(a: str, b: str) -> bool:
    """Path equality that survives Windows separators and `./` prefixes.

    The old code compared raw strings, so on a developer box `glob` returned
    `.gitea/workflows\\lint-...yml` and the self-skip missed — the lint then
    flagged its OWN documentation comment. Same verdict on Linux, where the
    separators already matched; this only stops the tool lying on Windows.

    The backslash is folded EXPLICITLY rather than via `os.path.normpath`,
    which on POSIX treats `\\` as an ordinary filename character and would
    leave this function platform-dependent — passing on Windows and failing on
    the runner, which is exactly what the first version did.
    """
    def norm(p: str) -> str:
        return os.path.normcase(os.path.normpath(p.replace("\\", "/")))

    return norm(a) == norm(b)


class Finding(NamedTuple):
    path: str
    rule: str
    snippet: str
    hint: str


BAD_STATUS_CAPTURE = re.compile(
    r"""
    \$\(\s*
    curl\b
    [^)]*
    -w\s*['"]%\{http_code\}['"]
    [^)]*
    \|\|\s*
    (?:
        echo\s+['"]?000['"]?
        |
        printf\s+['"]000['"]
    )
    \s*\)
    """,
    re.DOTALL | re.VERBOSE,
)

# `VAR=$(` or `VAR="$(` — the quoted form is not decoration: it is how the two
# sites in promote-prod-tenant-pin.yml are written, and an enumeration that
# missed it reported "no remaining sites" while two sat in the tree.
CAPTURE_OPEN = re.compile(
    r"(?m)(?:^|[;&|(]|\bexport\b|\blocal\b)\s*"
    r"(?P<var>[A-Za-z_][A-Za-z0-9_]*)=(?P<q>\")?\$\("
)

# First element of the substitution: a reader consuming a FILE operand.
FILE_READER = re.compile(
    r"""\A\s*
    (?P<cmd>cat|head|tail)\b
    (?P<opts>(?:\s+-{1,2}[A-Za-z-]+(?:\s+\d+)?|\s+-\w?\d+)*)
    \s+
    (?P<file>(?:"[^"]+"|'[^']+'|[^\s|;)&]+))
    """,
    re.VERBOSE,
)

# Where curl is told to put its body — `-o FILE` / `--output FILE` ONLY.
#
# A `curl ... > FILE` redirect is deliberately NOT here, and the distinction is
# the whole rule: the SHELL opens a redirect target before curl even starts, so
# the file exists (empty) no matter how the transfer fails and the reader
# succeeds. `-o` is opened by CURL, lazily, once there is a response to write —
# so a DNS/connect/timeout failure leaves NO file. Only the second shape can
# make `cat` exit 1.
CURL_WRITES = re.compile(
    r"""curl\b[^\n]*?
        (?:
            -o\s+(?P<o>(?:"[^"]+"|'[^']+'|[^\s\\]+))
            |
            --output\s+(?P<oo>(?:"[^"]+"|'[^']+'|[^\s\\]+))
        )
    """,
    re.VERBOSE,
)


def _logical_shell(content: str) -> str:
    """Collapse bash line continuations so one curl command is one string."""
    return re.sub(r"\\\s*\n\s*", " ", content)


def _strip_full_line_comments(content: str) -> str:
    """Blank out whole-line `#` comments, preserving line count.

    Necessary, not cosmetic: core#5166 documents the defect by QUOTING the
    pre-fix line in a comment. A scanner that reads comments would flag the
    explanation of the bug as the bug — and the only way to make it green
    again would be to delete the explanation.

    Deliberately full-line only. Stripping from a mid-line `#` would eat
    `secretPath=%2Fshared%2F...#frag` and URL fragments inside real commands.
    """
    return "\n".join(
        "" if line.lstrip().startswith("#") else line
        for line in content.split("\n")
    )


def _unquote(token: str | None) -> str | None:
    if not token:
        return None
    if len(token) >= 2 and token[0] == token[-1] and token[0] in "\"'":
        return token[1:-1]
    return token


def _match_close(text: str, i: int) -> int:
    """Index of the `)` closing a `$(` whose body starts at `i`, or -1."""
    depth = 1
    while i < len(text):
        c = text[i]
        if c == "\\":
            i += 2
            continue
        if c == "'":
            j = text.find("'", i + 1)
            if j < 0:
                return -1
            i = j + 1
            continue
        if c == '"':
            i += 1
            while i < len(text):
                if text[i] == "\\":
                    i += 2
                    continue
                if text[i] == '"':
                    break
                i += 1
            i += 1
            continue
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    return -1


def _top_level_pipe(body: str) -> bool:
    """True if `body` contains a `|` outside quotes and outside nested `$(`."""
    depth = 0
    i = 0
    while i < len(body):
        c = body[i]
        if c == "\\":
            i += 2
            continue
        if c in "\"'":
            quote = c
            i += 1
            while i < len(body) and body[i] != quote:
                i += 2 if body[i] == "\\" else 1
            i += 1
            continue
        if c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
        elif c == "|" and depth == 0:
            if i + 1 < len(body) and body[i + 1] == "|":
                i += 2
                continue
            return True
        i += 1
    return False


def _curl_written_files(flat: str) -> set[str]:
    files = set()
    for m in CURL_WRITES.finditer(flat):
        for group in ("o", "oo"):
            name = _unquote(m.group(group))
            if name:
                files.add(name)
    return files


def scan_content(path: str, content: str) -> list[Finding]:
    flat = _logical_shell(_strip_full_line_comments(content))
    findings = [
        Finding(
            path=path,
            rule="status-capture-pollution",
            snippet=re.sub(r"\s+", " ", m.group(0)).strip()[:160],
            hint=(
                "'|| echo/printf 000' inside a $(curl ... -w '%{http_code}' ...) "
                "subshell. On non-2xx or connection failure curl's -w writes a "
                "status, then exits non-zero, then the fallback appends ANOTHER "
                "status. Route -w into a file so the exit code cannot pollute "
                "stdout."
            ),
        )
        for m in BAD_STATUS_CAPTURE.finditer(flat)
    ]

    written = _curl_written_files(flat)
    for m in CAPTURE_OPEN.finditer(flat):
        start = m.end()
        close = _match_close(flat, start)
        if close < 0:
            continue
        body = flat[start:close]
        if "||" in body:
            continue  # has a fallback; that is the fix, not the defect
        if not _top_level_pipe(body):
            continue
        reader = FILE_READER.match(body)
        if not reader:
            continue
        target = _unquote(reader.group("file"))
        if target not in written:
            # Not a file curl may fail to create (mktemp'd files always exist,
            # and a `>` redirect written by the SHELL is created even when the
            # command inside it never runs). Reporting those would be noise,
            # and noise is how a lint gets ignored.
            continue
        findings.append(
            Finding(
                path=path,
                rule="capture-dies-before-its-guard",
                snippet=re.sub(r"\s+", " ", m.group(0) + body + ")").strip()[:160],
                hint=(
                    f"`{m.group('var')}` captures a PIPELINE whose first element "
                    f"reads {target}, which `curl -o` never creates when the "
                    "transfer dies at DNS/connect/timeout — and there is no "
                    "fallback. Under the runner's `-e -o pipefail`, pipefail "
                    "promotes the reader's exit 1 to the pipeline, the bare "
                    "assignment inherits it, and -e kills the step BEFORE the "
                    "guard that would have reported the failure."
                ),
            )
        )
    return findings


def scan_paths(paths: list[str]) -> tuple[list[Finding], int]:
    findings: list[Finding] = []
    checked = 0
    for path in paths:
        if _same_path(path, SELF):
            continue
        content = Path(path).read_text(encoding="utf-8")
        checked += 1
        findings.extend(scan_content(path, content))
    return findings, checked


def default_paths() -> list[str]:
    return sorted(glob.glob(".gitea/workflows/*.yml"))


FIX_TEMPLATE = """\
Fix template — status capture (rule 1):
  set +e
  curl ... -w '%{http_code}' >code.txt 2>/dev/null
  set -e
  HTTP_CODE=$(cat code.txt 2>/dev/null || echo "000")
  # `[ -n x ] || y`, never `[ -z x ] && y`: a trailing `&&` whose test is FALSE
  # makes the whole line exit non-zero, and under the runner's -e that kills the
  # step. This template used to end with `[ -z "$HTTP_CODE" ] && HTTP_CODE="000"`
  # — the exact shape core#5166 refused. The lint was teaching the bug it exists
  # to prevent.
  [ -n "$HTTP_CODE" ] || HTTP_CODE="000"

Fix template — body capture (rule 2):
  # Read the file DIRECTLY (no pipeline) and give the fallback a USABLE value.
  # NOT `|| true`: that turns "the command failed" into "the body was empty",
  # which reads downstream as "the endpoint answered with nothing".
  body=$(head -c 500 /tmp/resp 2>/dev/null || echo "<no response body — curl wrote no output file (connect/DNS/timeout)>")\
"""


def print_report(findings: list[Finding], checked: int) -> int:
    # ANTI-VACUITY. A scanner that examined nothing reports "OK" and is
    # indistinguishable from a clean tree. `checked: 0` is an ERROR, never a
    # pass — this platform's signature failure is a guard that matched nothing
    # and said so in green.
    print(f"checked: {checked}")
    if checked == 0:
        print(
            "::error::lint-curl-status-capture examined ZERO files. A lint that "
            "scans nothing cannot fail, and its green means nothing. Check the "
            "path glob."
        )
        return 1

    if not findings:
        print(f"OK No curl-capture defects detected across {checked} file(s)")
        return 0

    print(f"::error::Found {len(findings)} curl-capture defect(s):")
    for finding in findings:
        print(f"::error file={finding.path}::[{finding.rule}] {finding.hint}")
        print(f"   matched: {finding.snippet}...")
    print()
    print(FIX_TEMPLATE)
    return 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("paths", nargs="*", help="workflow files to scan")
    args = parser.parse_args(argv)

    paths = args.paths or default_paths()
    findings, checked = scan_paths(paths)
    return print_report(findings, checked)


if __name__ == "__main__":
    raise SystemExit(main())
