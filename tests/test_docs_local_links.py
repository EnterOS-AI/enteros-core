"""Fail when a Markdown link points at a missing repository document.

External URLs are deliberately out of scope: this gate protects renames and
retirements inside the repository without making CI depend on the network.
"""

from __future__ import annotations

import re
from pathlib import Path
from urllib.parse import unquote


ROOT = Path(__file__).resolve().parents[1]
DOCS = ROOT / "docs"
MARKDOWN_LINK = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")
FRONTMATTER_SLUG = re.compile(r'^slug:\s*["\']?([^"\'\n]+)', re.MULTILINE)
EXTERNAL_SCHEME = re.compile(r"^(?:https?|mailto|tel|data):", re.IGNORECASE)


def _markdown_files() -> list[Path]:
    files = list(DOCS.rglob("*.md"))
    files.extend(ROOT / name for name in ("README.md", "README.zh-CN.md", "CONTRIBUTING.md"))
    return [path for path in files if path.exists()]


def _link_target(raw: str) -> str:
    raw = raw.strip()
    if raw.startswith("<") and ">" in raw:
        return raw[1 : raw.index(">")]
    # Markdown permits an optional title after whitespace. Repository paths in
    # this tree do not contain unescaped spaces.
    return raw.split(maxsplit=1)[0]


def _candidate_paths(source: Path, target: str) -> list[Path]:
    target = unquote(target.split("#", 1)[0].split("?", 1)[0]).strip()
    if not target:
        return []

    if target == "/docs":
        base = DOCS
    elif target.startswith("/docs/"):
        base = DOCS / target.removeprefix("/docs/")
    elif target.startswith("/"):
        # Application and API routes are not repository files.
        return []
    else:
        base = source.parent / target

    candidates = [base]
    if not base.suffix:
        candidates.extend((Path(f"{base}.md"), base / "index.md"))
    return candidates


def test_markdown_local_links_resolve() -> None:
    files = _markdown_files()
    slugs: set[str] = set()
    for path in files:
        if match := FRONTMATTER_SLUG.search(path.read_text(encoding="utf-8")):
            slugs.add(match.group(1).strip())

    broken: list[str] = []
    for source in files:
        for line_number, line in enumerate(source.read_text(encoding="utf-8").splitlines(), 1):
            for match in MARKDOWN_LINK.finditer(line):
                target = _link_target(match.group(1))
                if (
                    not target
                    or target.startswith("#")
                    or EXTERNAL_SCHEME.match(target)
                    or "$" in target
                    or "{" in target
                ):
                    continue

                candidates = _candidate_paths(source, target)
                if not candidates or any(path.exists() for path in candidates):
                    continue

                # Docusaurus-style blog links may use a frontmatter slug rather
                # than the source directory name.
                route_slug = target.split("#", 1)[0].rstrip("/").rsplit("/", 1)[-1]
                if route_slug in slugs:
                    continue

                relative = source.relative_to(ROOT)
                tried = ", ".join(str(path.relative_to(ROOT)) for path in candidates)
                broken.append(f"{relative}:{line_number}: {target} (tried {tried})")

    assert not broken, "Broken local Markdown links:\n" + "\n".join(broken)
