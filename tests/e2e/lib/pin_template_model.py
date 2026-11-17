#!/usr/bin/env python3
"""Pin the default model of workspace templates bundled into an e2e tenant image.

usage: pin_template_model.py <workspace-configs-templates-dir> <model> <template>...

The ephemeral happy path builds THIS PR's tenant image with the templates
manifest.json pins (scripts/clone-manifest.sh). A workspace created from a
template without an explicit model takes the template's default
(handlers/workspace.go Create: top-level `model:`, else the first indented
`model:` inside `runtime_config:`). The concierge-created member in
concierge_creates_workspace is such a workspace, so without this step every e2e
run would spend on the base templates' default, the gemini-4-carbon platform
default (owner 2026-11-17: the cheapest model for any e2e testing, Carbon for the
default), and a ci-llm or vendor run could not serve it at all.

This rewrites, in the cloned copies only, exactly the lines Create reads (same
scanner semantics, including that an unindented line - a blank one too - ends the
runtime_config block) to the lane's e2e model. It fails if a named template is
missing or carries no default-model line, so a template reshaped upstream cannot
silently fall back to its own default.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

_MODEL_LINE = re.compile(r"^(?P<lead>\s*model:\s*)(?P<value>.*?)(?P<trail>\s*(#.*)?)$")


def pin(config_text: str, model: str) -> tuple[str, int]:
    """Return (new_text, lines_rewritten)."""
    out = []
    rewritten = 0
    in_runtime_config = False
    nested_done = False
    for raw in config_text.split("\n"):
        trimmed = raw.lstrip(" \t")
        indented = len(raw) > len(trimmed)
        if not indented:
            in_runtime_config = trimmed.startswith("runtime_config:")
        stripped = raw.strip()
        is_model = stripped.startswith("model:")
        top_level = is_model and not indented
        nested = is_model and indented and in_runtime_config and not nested_done
        if top_level or nested:
            m = _MODEL_LINE.match(raw)
            raw = f"{m.group('lead')}{model}{m.group('trail')}"
            rewritten += 1
            if nested:
                nested_done = True
        out.append(raw)
    return "\n".join(out), rewritten


def main(argv: list[str]) -> int:
    if len(argv) < 4:
        print(__doc__.strip().splitlines()[2], file=sys.stderr)
        return 2
    root, model, names = Path(argv[1]), argv[2].strip(), argv[3:]
    if not model:
        print("pin_template_model: empty model", file=sys.stderr)
        return 2
    for name in names:
        cfg = root / name / "config.yaml"
        if not cfg.is_file():
            print(f"pin_template_model: {cfg} not found", file=sys.stderr)
            return 1
        text, n = pin(cfg.read_text(encoding="utf-8"), model)
        if n == 0:
            print(f"pin_template_model: {cfg} has no default-model line", file=sys.stderr)
            return 1
        cfg.write_bytes(text.encode("utf-8"))
        print(f"pinned {name} default model -> {model} ({n} line(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
