#!/usr/bin/env python3
"""Fail when an ON CONFLICT inference clause targets a PARTIAL unique index
without repeating that index's predicate.

WHY THIS EXISTS
---------------
Postgres only infers a partial unique index when the ON CONFLICT clause repeats
its WHERE predicate. Omit it and NO constraint matches, so the statement is
rejected outright at runtime:

    there is no unique or exclusion constraint matching the ON CONFLICT
    specification

That is a *runtime* error on a statement that compiles, passes vet, and passes
sqlmock -- which never runs a planner and happily returns a canned OK. If the
caller logs-and-continues, as sweepers typically do, the insert silently never
happens and the table stays empty forever.

core#4977: `queueDriftEntry` shipped without the predicate. Every drift enqueue
failed for the life of the feature, the queue sat permanently empty, and nothing
ever converged. An empty queue reads identically as "no drift" and as "every
insert failed", so it survived repeated inspection and reached a customer.

molecule-core#4314 hit the same class in a2a_queue and fixed it there. This lint
encodes knowledge the codebase already had, so the next one fails at PR time.

WHAT IT CHECKS
--------------
For every `INSERT INTO <table> ... ON CONFLICT (<cols>) ...` in Go source:
  * find UNIQUE indexes on <table> whose column set equals <cols>
  * if the ONLY matching index(es) are PARTIAL, the clause MUST carry a WHERE
  * a matching total (non-partial) unique index makes the bare form correct

Deliberately NOT flagged: `ON CONFLICT ON CONSTRAINT <name>` (explicit, no
inference) and bare `ON CONFLICT DO ...` with no target (no inference).

Exit 0 clean, 1 on violations, 2 on usage error.
"""
from __future__ import annotations

import argparse
import pathlib
import re
import sys

# CREATE UNIQUE INDEX [IF NOT EXISTS] name ON table (cols) [WHERE pred]
_INDEX_RE = re.compile(
    r"CREATE\s+UNIQUE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"
    r"(?P<name>[\w\.\"]+)\s+ON\s+(?:public\.)?(?P<table>[\w\"]+)\s*"
    r"(?:USING\s+\w+\s*)?\((?P<cols>[^)]*)\)"
    r"(?P<tail>[^;]*)",
    re.IGNORECASE | re.DOTALL,
)

# INSERT INTO table ... ON CONFLICT (cols) [WHERE ...]
_INSERT_RE = re.compile(
    r"INSERT\s+INTO\s+(?:public\.)?(?P<table>[\w\"]+)(?P<body>.*?)"
    r"ON\s+CONFLICT\s*\((?P<cols>[^)]*)\)(?P<after>.{0,200})",
    re.IGNORECASE | re.DOTALL,
)

_LINE_COMMENT_RE = re.compile(r"//[^\n]*")


def strip_line_comments_outside_raw_strings(text: str) -> str:
    """Blank out Go `//` comments, preserving offsets so line numbers stay true.

    Without this, a doc comment that merely DESCRIBES an insert -- e.g. the
    sweeper's "4. On drift, INSERT INTO plugin_update_queue (ON CONFLICT DO
    NOTHING ...)" -- is scanned as if it were code and reports the same defect a
    second time, anchored to a comment line the reader cannot act on.

    SQL in this codebase lives in backtick raw strings, so backtick spans are
    preserved verbatim and only text outside them is stripped. That keeps a
    literal "//" inside SQL (rare, but possible in a URL) from being eaten.
    """
    parts = text.split("`")
    for i in range(0, len(parts), 2):  # even indices are OUTSIDE raw strings
        parts[i] = _LINE_COMMENT_RE.sub(lambda m: " " * len(m.group(0)), parts[i])
    return "`".join(parts)


def norm_cols(raw: str) -> tuple[str, ...]:
    """Normalise a column list for comparison: lowercase, unquoted, sorted."""
    out = []
    for part in raw.split(","):
        c = part.strip().strip('"').lower()
        c = c.split()[0] if c.split() else c  # drop opclass/ordering noise
        if c:
            out.append(c)
    return tuple(sorted(out))


def collect_unique_indexes(migrations: pathlib.Path):
    """table -> {cols: [(index_name, predicate_or_None), ...]}"""
    idx: dict[str, dict[tuple[str, ...], list[tuple[str, str | None]]]] = {}
    for path in sorted(migrations.glob("*.sql")):
        text = path.read_text(encoding="utf-8", errors="replace")
        for m in _INDEX_RE.finditer(text):
            table = m.group("table").strip('"').lower()
            cols = norm_cols(m.group("cols"))
            wm = re.search(
                r"\bWHERE\b(?P<pred>.+)", m.group("tail") or "", re.IGNORECASE | re.DOTALL
            )
            pred = " ".join(wm.group("pred").split()) if wm else None
            idx.setdefault(table, {}).setdefault(cols, []).append((m.group("name"), pred))
    return idx


def scan_go(root: pathlib.Path, indexes) -> list[str]:
    violations: list[str] = []
    for path in sorted(root.rglob("*.go")):
        if "/vendor/" in path.as_posix():
            continue
        text = strip_line_comments_outside_raw_strings(
            path.read_text(encoding="utf-8", errors="replace")
        )
        for m in _INSERT_RE.finditer(text):
            table = m.group("table").strip('"').lower()
            cols = norm_cols(m.group("cols"))
            matches = indexes.get(table, {}).get(cols)
            if not matches:
                continue  # unknown target; the DB will surface it, not this lint
            if any(pred is None for _, pred in matches):
                continue  # a total unique index makes the bare form inferable
            head = re.split(
                r"\bDO\b", m.group("after") or "", maxsplit=1, flags=re.IGNORECASE
            )[0]
            if re.search(r"\bWHERE\b", head, re.IGNORECASE):
                continue  # predicate present -- correct
            line = text[: m.start()].count("\n") + 1
            names = ", ".join(f"{n} WHERE {p}" for n, p in matches)
            violations.append(
                f"{path.as_posix()}:{line}: ON CONFLICT ({', '.join(cols)}) on "
                f"`{table}` infers a PARTIAL unique index but omits its predicate.\n"
                f"    matching partial index: {names}\n"
                f"    Postgres rejects this at RUNTIME: 'there is no unique or exclusion "
                f"constraint matching the ON CONFLICT specification'.\n"
                f"    Fix: ON CONFLICT ({', '.join(cols)}) WHERE {matches[0][1]} DO ..."
            )
    return violations


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--migrations", default="workspace-server/migrations")
    ap.add_argument("--go-root", default="workspace-server")
    args = ap.parse_args()

    migrations = pathlib.Path(args.migrations)
    go_root = pathlib.Path(args.go_root)
    if not migrations.is_dir() or not go_root.is_dir():
        print(f"::error::bad paths: {migrations} / {go_root}", file=sys.stderr)
        return 2

    indexes = collect_unique_indexes(migrations)
    partial_total = sum(
        1 for cols in indexes.values() for lst in cols.values() for _, p in lst if p is not None
    )
    # Anti-vacuity: this repo is known to contain partial unique indexes. Zero
    # means the parser broke, and a lint that inspects nothing must never pass.
    if partial_total == 0:
        print(
            "::error::onconflict-partial-index lint parsed ZERO partial unique indexes "
            "from the migrations. That is a broken parser, not a clean tree -- a lint "
            "that inspects nothing must never report success.",
            file=sys.stderr,
        )
        return 1

    violations = scan_go(go_root, indexes)
    if violations:
        for v in violations:
            print(f"::error::{v}", file=sys.stderr)
        print(
            f"\n{len(violations)} ON CONFLICT clause(s) target a partial unique index "
            f"without repeating its predicate.",
            file=sys.stderr,
        )
        return 1

    print(
        f"ON CONFLICT/partial-index lint OK "
        f"({partial_total} partial unique index(es) considered)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
