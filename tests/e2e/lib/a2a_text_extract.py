#!/usr/bin/env python3
"""Extract text from the A2A JSON shapes used by the staging harnesses."""

from __future__ import annotations

import json
import sys
from collections.abc import Iterable
from typing import Any


def _text_parts(parts: Any) -> Iterable[str]:
    if not isinstance(parts, list):
        return
    for part in parts:
        if not isinstance(part, dict):
            continue
        kind = part.get("kind") or part.get("type")
        text = part.get("text")
        if isinstance(text, str) and text and (kind in (None, "", "text")):
            yield text


def _extract(doc: Any) -> Iterable[str]:
    if not isinstance(doc, dict):
        return

    # Queue-status responses wrap the eventual A2A JSON-RPC response here.
    response_body = doc.get("response_body")
    if isinstance(response_body, dict):
        yield from _extract(response_body)

    result = doc.get("result")
    if not isinstance(result, dict):
        return

    # Standard A2A JSON-RPC result.parts.
    yield from _text_parts(result.get("parts"))

    # A2A Task status message parts.
    status = result.get("status")
    if isinstance(status, dict):
        message = status.get("message")
        if isinstance(message, dict):
            yield from _text_parts(message.get("parts"))

    # Alternative message.parts placement.
    message = result.get("message")
    if isinstance(message, dict):
        yield from _text_parts(message.get("parts"))

    # A2A task artifacts.
    artifacts = result.get("artifacts")
    if isinstance(artifacts, list):
        for artifact in artifacts:
            if isinstance(artifact, dict):
                yield from _text_parts(artifact.get("parts"))


def main() -> int:
    try:
        doc = json.load(sys.stdin)
    except Exception:
        return 0

    print("\n".join(_extract(doc)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
