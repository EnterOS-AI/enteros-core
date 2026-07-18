import importlib.util
import io
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path

import pytest


SCRIPT = Path(__file__).resolve().parents[1] / "infisical-read-secret.py"


def load_module():
    spec = importlib.util.spec_from_file_location("infisical_read_secret", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class Response:
    def __init__(self, body: bytes):
        self.body = body
        self.read_limits: list[int] = []

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self, limit: int) -> bytes:
        self.read_limits.append(limit)
        return self.body[:limit]


class Opener:
    def __init__(self, *responses):
        self.responses = list(responses)
        self.requests: list[tuple[urllib.request.Request, int]] = []

    def open(self, request: urllib.request.Request, timeout: int):
        self.requests.append((request, timeout))
        response = self.responses.pop(0)
        if isinstance(response, BaseException):
            raise response
        return response


def identity_env() -> dict[str, str]:
    return {
        "INFISICAL_CI_CLIENT_ID": "machine-client",
        "INFISICAL_CI_CLIENT_SECRET": "sensitive-client-secret",
        "INFISICAL_PROJECT_ID": "project-id",
    }


def test_reads_one_secret_with_bounded_no_redirect_requests_and_exact_ua() -> None:
    module = load_module()
    login = Response(json.dumps({"accessToken": "bearer-token"}).encode())
    secret = Response(json.dumps({"secret": {"secretValue": "registry-token"}}).encode())
    opener = Opener(login, secret)

    value = module.read_secret(
        "MOLECULE_REGISTRY_TOKEN",
        "/shared/dev-utils",
        "prod",
        "required",
        environ=identity_env(),
        opener=opener,
    )

    assert value == "registry-token"
    assert login.read_limits == [module.MAX_JSON_BYTES + 1]
    assert secret.read_limits == [module.MAX_JSON_BYTES + 1]
    assert [timeout for _request, timeout in opener.requests] == [30, 30]
    login_request, secret_request = [request for request, _timeout in opener.requests]
    assert login_request.get_header("User-agent") == "curl/8.4.0"
    assert secret_request.get_header("User-agent") == "curl/8.4.0"
    assert secret_request.get_header("Authorization") == "Bearer bearer-token"
    assert "Authorization" not in secret_request.headers
    assert secret_request.unredirected_hdrs["Authorization"] == "Bearer bearer-token"
    assert b"sensitive-client-secret" in login_request.data
    assert "sensitive-client-secret" not in repr(opener.requests)


def test_redirect_handler_rejects_before_forwarding_authorization() -> None:
    module = load_module()
    request = urllib.request.Request(
        "https://key.moleculesai.app/private",
        headers={"Authorization": "Bearer sensitive"},
    )
    with pytest.raises(urllib.error.HTTPError, match="redirect refused"):
        module.RejectRedirect().redirect_request(
            request, None, 302, "Found", {}, "https://attacker.invalid/steal"
        )


def test_oversized_or_malformed_json_fails_without_echoing_response_body() -> None:
    module = load_module()
    sensitive = b'sensitive-response-body:' + b"x" * module.MAX_JSON_BYTES
    opener = Opener(Response(sensitive))

    with pytest.raises(module.InfisicalError) as caught:
        module.read_secret(
            "KEY", "/shared/path", "prod", "required",
            environ=identity_env(), opener=opener,
        )

    assert "exceeded" in str(caught.value)
    assert "sensitive-response-body" not in str(caught.value)


def test_optional_404_is_distinct_and_http_error_body_is_not_read() -> None:
    module = load_module()
    login = Response(json.dumps({"accessToken": "bearer-token"}).encode())

    class ErrorBody(io.BytesIO):
        def __init__(self, body: bytes):
            super().__init__(body)
            self.read_calls = 0

        def read(self, size: int = -1) -> bytes:
            self.read_calls += 1
            return super().read(size)

    error_body = ErrorBody(b"sensitive-error-body")
    missing = urllib.error.HTTPError(
        "https://key.moleculesai.app/secret", 404, "missing", {}, error_body
    )
    opener = Opener(login, missing)

    with pytest.raises(module.SecretMissing):
        module.read_secret(
            "KEY", "/shared/path", "prod", "optional",
            environ=identity_env(), opener=opener,
        )
    assert error_body.read_calls == 0
    assert error_body.closed


def test_rejects_empty_or_multiline_secret_values() -> None:
    module = load_module()
    for value in ("", "line\nbreak", "nul\x00byte"):
        login = Response(json.dumps({"accessToken": "bearer-token"}).encode())
        secret = Response(json.dumps({"secret": {"secretValue": value}}).encode())
        with pytest.raises(module.InfisicalError):
            module.read_secret(
                "KEY", "/shared/path", "prod", "required",
                environ=identity_env(), opener=Opener(login, secret),
            )
