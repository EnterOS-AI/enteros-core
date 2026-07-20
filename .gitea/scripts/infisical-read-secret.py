#!/usr/bin/env python3
"""Read exactly one bounded CI secret from the Infisical SSOT."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Mapping, Sequence
from typing import Any, NoReturn


BASE_URL = "https://key.moleculesai.app"
USER_AGENT = "curl/8.4.0"
MAX_JSON_BYTES = 1024 * 1024
REQUEST_TIMEOUT_SECONDS = 30


class InfisicalError(RuntimeError):
    """An authenticated read could not be proven safe and complete."""


class SecretMissing(InfisicalError):
    """An optional secret does not exist."""


class RejectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001
        raise urllib.error.HTTPError(req.full_url, code, "redirect refused", headers, fp)


NO_REDIRECT_OPENER = urllib.request.build_opener(RejectRedirect)


def read_json(response: Any, label: str) -> Any:
    body = response.read(MAX_JSON_BYTES + 1)
    if len(body) > MAX_JSON_BYTES:
        raise InfisicalError(f"{label} response exceeded {MAX_JSON_BYTES}-byte bound")
    try:
        return json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError, TypeError) as exc:
        raise InfisicalError(f"{label} response was not valid JSON") from exc


def request_json(
    request: urllib.request.Request,
    label: str,
    *,
    opener: Any,
) -> Any:
    try:
        with opener.open(request, timeout=REQUEST_TIMEOUT_SECONDS) as response:
            return read_json(response, label)
    except InfisicalError:
        raise
    except urllib.error.HTTPError as exc:
        code = exc.code
        exc.close()
        raise InfisicalError(f"{label} failed: HTTP {code}") from exc
    except Exception as exc:
        raise InfisicalError(f"{label} failed: {type(exc).__name__}") from exc


def read_secret(
    key: str,
    secret_path: str,
    environment: str,
    mode: str,
    *,
    environ: Mapping[str, str] | None = None,
    opener: Any = NO_REDIRECT_OPENER,
) -> str:
    if mode not in {"required", "optional"}:
        raise InfisicalError("mode must be required or optional")
    values = os.environ if environ is None else environ
    client_id = values.get("INFISICAL_CI_CLIENT_ID", "")
    client_secret = values.get("INFISICAL_CI_CLIENT_SECRET", "")
    project_id = values.get("INFISICAL_PROJECT_ID", "")
    if not all((client_id, client_secret, project_id)):
        raise InfisicalError("Infisical CI identity environment is incomplete")

    login = urllib.request.Request(
        f"{BASE_URL}/api/v1/auth/universal-auth/login",
        data=json.dumps(
            {"clientId": client_id, "clientSecret": client_secret},
            separators=(",", ":"),
        ).encode(),
        headers={"User-Agent": USER_AGENT, "Content-Type": "application/json"},
        method="POST",
    )
    login_body = request_json(login, "Infisical authentication", opener=opener)
    token = login_body.get("accessToken") if isinstance(login_body, dict) else None
    if not isinstance(token, str) or not token:
        raise InfisicalError("Infisical authentication returned no access token")

    query = urllib.parse.urlencode(
        {
            "workspaceId": project_id,
            "environment": environment,
            "secretPath": secret_path,
        }
    )
    url = f"{BASE_URL}/api/v3/secrets/raw/{urllib.parse.quote(key, safe='')}?{query}"
    request = urllib.request.Request(
        url,
        headers={"User-Agent": USER_AGENT},
    )
    # Keep bearer auth out of Request.headers so urllib will not copy it to a
    # follow-up request even if the explicit no-redirect policy regresses.
    request.add_unredirected_header("Authorization", f"Bearer {token}")
    try:
        body = request_json(request, "Infisical secret read", opener=opener)
    except InfisicalError as exc:
        cause = exc.__cause__
        if mode == "optional" and isinstance(cause, urllib.error.HTTPError):
            if cause.code == 404:
                raise SecretMissing("optional Infisical secret is absent") from exc
        raise

    value = (body.get("secret") or {}).get("secretValue") if isinstance(body, dict) else None
    if not isinstance(value, str) or not value:
        raise InfisicalError("Infisical returned an empty secret value")
    if any(character in value for character in "\r\n\x00"):
        raise InfisicalError("Infisical secret value contains a line break or NUL")
    return value


def usage(program: str) -> NoReturn:
    print(
        f"usage: {program} <secret-key> <secret-path> <environment> [optional]",
        file=sys.stderr,
    )
    raise SystemExit(2)


def main(argv: Sequence[str] | None = None) -> int:
    args = list(sys.argv if argv is None else argv)
    if len(args) not in {4, 5} or (len(args) == 5 and args[4] != "optional"):
        usage(args[0] if args else "infisical-read-secret.py")
    mode = "optional" if len(args) == 5 else "required"
    try:
        value = read_secret(args[1], args[2], args[3], mode)
    except SecretMissing:
        return 10
    except InfisicalError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    sys.stdout.write(value)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
