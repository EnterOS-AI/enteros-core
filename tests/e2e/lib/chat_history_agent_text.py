#!/usr/bin/env python3
"""Extract the latest agent text from a /chat-history response."""

from __future__ import annotations

import json
import sys
from typing import Any


def _content_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, dict):
        parts: list[str] = []
        for key in ("content", "text", "result", "message"):
            text = _content_text(value.get(key))
            if text:
                parts.append(text)
        return "\n".join(parts)
    if isinstance(value, list):
        return "\n".join(text for item in value if (text := _content_text(item)))
    return ""


def main() -> int:
    try:
        doc = json.load(sys.stdin)
    except Exception:
        return 0

    messages = doc.get("messages") if isinstance(doc, dict) else doc
    if not isinstance(messages, list):
        return 0

    for message in reversed(messages):
        if not isinstance(message, dict):
            continue
        role = str(message.get("role") or "").lower()
        if role not in ("agent", "assistant"):
            continue
        text = _content_text(message.get("content")).strip()
        if text:
            print(text)
            return 0
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
