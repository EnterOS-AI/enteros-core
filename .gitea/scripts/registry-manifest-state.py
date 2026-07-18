#!/usr/bin/env python3
"""Print an authenticated registry tag digest; exit 10 only when tag is absent."""

from __future__ import annotations

import base64
import hashlib
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

UA = "curl/8.4.0"
CANONICAL_REGISTRY = "registry.moleculesai.app"
MAX_MANIFEST_BYTES = 5 * 1024 * 1024
ACCEPT = ", ".join(
    [
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ]
)


class RejectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        raise urllib.error.HTTPError(req.full_url, code, "redirect refused", headers, fp)


NO_REDIRECT_OPENER = urllib.request.build_opener(RejectRedirect)
REPOSITORY_RE = re.compile(
    r"^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$"
)
TAG_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$")


def parse_image_base(image: str) -> tuple[str, str]:
    """Validate one untagged release repository on the canonical registry."""

    if "@" in image or ":" in image or "/" not in image:
        raise ValueError("release image must be an untagged host/repository name")
    registry, repository = image.split("/", 1)
    if registry != CANONICAL_REGISTRY:
        raise ValueError(
            f"registry ref must use canonical host {CANONICAL_REGISTRY}"
        )
    if not REPOSITORY_RE.fullmatch(repository):
        raise ValueError("registry repository contains an invalid name segment")
    return registry, repository


def parse_ref(ref: str) -> tuple[str, str, str]:
    if "@" in ref or "/" not in ref:
        raise ValueError("registry ref must be a host/repository:tag selector")
    registry, remainder = ref.split("/", 1)
    if ":" not in remainder:
        raise ValueError("registry ref must include an explicit tag")
    repository, tag = remainder.rsplit(":", 1)
    registry, repository = parse_image_base(f"{registry}/{repository}")
    if not TAG_RE.fullmatch(tag):
        raise ValueError("registry ref has an invalid or empty tag")
    return registry, repository, tag


def manifest_digest(ref: str, user: str, token: str) -> str | None:
    registry, repository, tag = parse_ref(ref)
    auth = base64.b64encode(f"{user}:{token}".encode()).decode()
    url = (
        f"https://{registry}/v2/{urllib.parse.quote(repository, safe='/')}/"
        f"manifests/{urllib.parse.quote(tag, safe='')}"
    )
    request = urllib.request.Request(
        url,
        headers={
            "Accept": ACCEPT,
            "User-Agent": UA,
        },
    )
    # Redirects are rejected; keeping registry auth unredirected also prevents
    # urllib from copying it if redirect handling is accidentally weakened.
    request.add_unredirected_header("Authorization", f"Basic {auth}")
    try:
        with NO_REDIRECT_OPENER.open(request, timeout=30) as response:
            body = response.read(MAX_MANIFEST_BYTES + 1)
            advertised = response.headers.get("Docker-Content-Digest", "").lower()
    except urllib.error.HTTPError as exc:
        code = exc.code
        exc.close()
        if code == 404:
            return None
        # Registry error bodies are untrusted and may be arbitrarily large or
        # contain echoed credential material. The status code is sufficient for
        # the fail-closed operator error; never read or print the body.
        raise RuntimeError(f"GET registry manifest -> HTTP {code}") from exc
    if len(body) > MAX_MANIFEST_BYTES:
        raise RuntimeError(
            f"registry manifest exceeded {MAX_MANIFEST_BYTES}-byte response bound"
        )
    digest = "sha256:" + hashlib.sha256(body).hexdigest()
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", advertised):
        raise RuntimeError("registry response omitted a valid Docker-Content-Digest")
    if advertised != digest:
        raise RuntimeError(
            f"registry digest header {advertised} does not match manifest bytes {digest}"
        )
    return digest


def main() -> int:
    if len(sys.argv) >= 3 and sys.argv[1] in {"--validate-base", "--validate-ref"}:
        validator = parse_image_base if sys.argv[1] == "--validate-base" else parse_ref
        try:
            for value in sys.argv[2:]:
                validator(value)
        except Exception as exc:  # noqa: BLE001 - operator-facing CLI boundary.
            print(f"::error::{exc}", file=sys.stderr)
            return 2
        return 0
    if len(sys.argv) != 2:
        print(
            f"usage: {sys.argv[0]} <registry-image:tag> | "
            "--validate-base <registry/repository> [...] | "
            "--validate-ref <registry/repository:tag> [...]",
            file=sys.stderr,
        )
        return 2
    user = os.environ.get("REG_USER", "").strip()
    token = os.environ.get("REG_TOKEN", "").strip()
    if not user or not token:
        print("::error::REG_USER and REG_TOKEN are required", file=sys.stderr)
        return 1
    try:
        digest = manifest_digest(sys.argv[1], user, token)
    except Exception as exc:  # noqa: BLE001 - operator-facing CLI boundary.
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    if digest is None:
        return 10
    print(digest)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
