#!/usr/bin/env python3
"""Bounded, checksum-verifying private Gitea download with argv-safe auth."""

from __future__ import annotations

import hashlib
import os
import pathlib
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from typing import NoReturn


class RejectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        raise urllib.error.HTTPError(req.full_url, code, "redirect refused", headers, fp)


NO_REDIRECT_OPENER = urllib.request.build_opener(RejectRedirect)


def fail(message: str) -> NoReturn:
    print(f"::error::{message}", file=sys.stderr)
    raise SystemExit(1)


def download(url: str, expected: str, output: pathlib.Path, token: str) -> None:
    if not token:
        fail("GITEA_DOWNLOAD_TOKEN is empty")
    parsed = urllib.parse.urlparse(url)
    try:
        port = parsed.port
    except ValueError:
        fail("private download URL has an invalid port")
    if (
        parsed.scheme != "https"
        or parsed.hostname != "git.moleculesai.app"
        or parsed.username is not None
        or parsed.password is not None
        or port not in (None, 443)
        or bool(parsed.fragment)
    ):
        fail(
            "private download URL must use the canonical "
            "https://git.moleculesai.app origin"
        )
    if not len(expected) == 64 or any(ch not in "0123456789abcdef" for ch in expected):
        fail("expected checksum must be lowercase SHA-256 hex")
    request = urllib.request.Request(
        url,
        headers={"User-Agent": "curl/8.4.0"},
    )
    # Defense in depth: redirects are rejected, and urllib also cannot copy the
    # credential from the normal header map if that handler ever regresses.
    request.add_unredirected_header("Authorization", f"token {token}")
    try:
        with NO_REDIRECT_OPENER.open(request, timeout=60) as response:
            body = response.read(5 * 1024 * 1024 + 1)
    except urllib.error.HTTPError as exc:
        exc.close()
        fail("private Gitea download failed: HTTPError")
    except Exception as exc:
        fail(f"private Gitea download failed: {type(exc).__name__}")
    if len(body) > 5 * 1024 * 1024:
        fail("private Gitea download exceeded 5 MiB bound")
    actual = hashlib.sha256(body).hexdigest()
    if actual != expected:
        fail(f"private Gitea download checksum mismatch: {actual} != {expected}")

    output.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{output.name}.", dir=output.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(body)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o700)
        os.replace(temporary, output)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def main() -> int:
    if len(sys.argv) != 4:
        print(f"usage: {sys.argv[0]} <https-url> <sha256> <output>", file=sys.stderr)
        return 2
    url, expected, output_arg = sys.argv[1:]
    download(
        url,
        expected,
        pathlib.Path(output_arg),
        os.environ.get("GITEA_DOWNLOAD_TOKEN", ""),
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
