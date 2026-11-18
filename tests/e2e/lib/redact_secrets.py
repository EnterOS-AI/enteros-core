#!/usr/bin/env python3
"""Redact credential-looking values from stdin to stdout.

Used by staging E2E diagnostic emitters so run logs stay secret-safe.
Preserves HTTP status codes and non-secret error context.
"""

import argparse
import re
import sys


def redact(text: str) -> str:
    patterns = [
        # Authorization / Bearer header values (consume the whole value)
        (r"(?i)(Authorization\s*[:=]\s*)[^\n]*", r"\1<REDACTED>"),
        # Bare Bearer token (e.g. in bodies or query values)
        (r"(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+", r"\1<REDACTED>"),
        # Any JSON string key containing a credential marker. This covers
        # compound response fields such as platform_inbound_secret and keeps
        # suffixes such as secret_digest from escaping the exact-name rules.
        (
            r'(?i)("[^"\r\n]*(?:api[_-]?key|auth[_-]?token|token|secret|password|credential)[^"\r\n]*"\s*:\s*")(?:\\.|[^"\\])*"',
            r'\1<REDACTED>"',
        ),
        # Form/env/query assignments with compound sensitive keys. Ampersand
        # is a value boundary so useful sibling proof fields survive.
        (
            r"(?i)((?<![A-Za-z0-9_.-])[A-Za-z0-9_.-]*(?:api[_-]?key|auth[_-]?token|token|secret|password|credential)[A-Za-z0-9_.-]*\s*=\s*)[^&\s]+",
            r"\1<REDACTED>",
        ),
        # Known credential key names (JSON/YAML/env-style)
        (
            r"(?i)(\"?(?:ANTHROPIC_AUTH_TOKEN|ANTHROPIC_API_KEY|MINIMAX_API_KEY|OPENAI_API_KEY|CLIENT_SECRET|ACCESS_TOKEN|GITHUB_TOKEN|GITEA_TOKEN|MOLECULE_[A-Z_]*_(?:TOKEN|SECRET|KEY))\"?\s*[:=]\s*\"?)[^\"&\s,}\]]+",
            r"\1<REDACTED>",
        ),
        # Generic *_API_KEY / *_TOKEN / *_SECRET / *_AUTH_TOKEN / *_PASSWORD
        (
            r"(?i)(\"?[A-Z_]*(?:API_KEY|AUTH_TOKEN|TOKEN|SECRET|PASSWORD)\"?\s*[:=]\s*\"?)[^\"&\s,}\]]+",
            r"\1<REDACTED>",
        ),
        # URL query params that commonly carry credentials
        (
            r"(?i)([?&](?:token|api[_-]?key|auth|secret|client_secret|access_token|password)=)[^&\s\"\']+",
            r"\1<REDACTED>",
        ),
    ]
    for pat, repl in patterns:
        text = re.sub(pat, repl, text)
    return text


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--max-chars",
        type=int,
        help="truncate sanitized output to at most this many characters",
    )
    args = parser.parse_args()
    if args.max_chars is not None and args.max_chars < 0:
        parser.error("--max-chars must be non-negative")

    output = redact(sys.stdin.read())
    if args.max_chars is not None:
        output = output[: args.max_chars]
    sys.stdout.write(output)


if __name__ == "__main__":
    main()
