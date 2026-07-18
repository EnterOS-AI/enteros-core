import hashlib
import importlib.util
import sys
import urllib.error
from pathlib import Path

import pytest


SCRIPT = Path(__file__).resolve().parents[1] / "registry-manifest-state.py"
spec = importlib.util.spec_from_file_location("registry_manifest_state", SCRIPT)
state = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = state
spec.loader.exec_module(state)


def test_release_image_base_policy_is_canonical_and_untagged() -> None:
    assert state.parse_image_base(
        "registry.moleculesai.app/molecule-ai/molecule-tenant"
    ) == ("registry.moleculesai.app", "molecule-ai/molecule-tenant")


@pytest.mark.parametrize(
    "image",
    [
        "attacker.invalid/molecule-ai/molecule-tenant",
        "registry.moleculesai.app/molecule-ai/molecule-tenant:latest",
        "registry.moleculesai.app/molecule-ai/molecule-tenant@sha256:" + "a" * 64,
        "registry.moleculesai.app/Molecule-AI/molecule-tenant",
        "registry.moleculesai.app/molecule-ai//molecule-tenant",
    ],
)
def test_release_image_base_policy_rejects_unsafe_names(image: str) -> None:
    with pytest.raises(ValueError):
        state.parse_image_base(image)


def test_reads_authenticated_manifest_with_exact_ua_and_digest(monkeypatch) -> None:
    body = b'{"schemaVersion":2}'
    captured = {}

    class Response:
        headers = {"Docker-Content-Digest": "sha256:" + hashlib.sha256(body).hexdigest()}

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self, limit):
            captured["read_limit"] = limit
            return body

    def fake_urlopen(request, timeout):
        captured["request"] = request
        captured["timeout"] = timeout
        return Response()

    monkeypatch.setattr(state.NO_REDIRECT_OPENER, "open", fake_urlopen)
    digest = state.manifest_digest(
        "registry.moleculesai.app/org/image:staging-deadbeef", "u", "t"
    )
    assert digest == Response.headers["Docker-Content-Digest"]
    assert captured["request"].get_header("User-agent") == "curl/8.4.0"
    assert captured["request"].get_header("Authorization").startswith("Basic ")
    assert "Authorization" not in captured["request"].headers
    assert captured["request"].unredirected_hdrs["Authorization"].startswith(
        "Basic "
    )
    assert captured["timeout"] == 30
    assert captured["read_limit"] == state.MAX_MANIFEST_BYTES + 1


def test_only_404_means_absent(monkeypatch) -> None:
    def fail(code):
        def fake(request, timeout):
            raise urllib.error.HTTPError(request.full_url, code, "x", {}, None)

        return fake

    monkeypatch.setattr(state.NO_REDIRECT_OPENER, "open", fail(404))
    assert state.manifest_digest(
        "registry.moleculesai.app/org/image:tag", "u", "t"
    ) is None
    monkeypatch.setattr(state.NO_REDIRECT_OPENER, "open", fail(401))
    with pytest.raises(RuntimeError, match="401"):
        state.manifest_digest("registry.moleculesai.app/org/image:tag", "u", "t")


def test_registry_http_errors_do_not_read_or_echo_untrusted_response_body(
    monkeypatch,
) -> None:
    class ErrorBody:
        def __init__(self):
            self.calls = []
            self.closed = False

        def read(self, *args):
            self.calls.append(args)
            return b"sensitive-registry-error-body"

        def close(self):
            self.closed = True

    body = ErrorBody()

    def fail(request, timeout):
        raise urllib.error.HTTPError(request.full_url, 503, "down", {}, body)

    monkeypatch.setattr(state.NO_REDIRECT_OPENER, "open", fail)
    with pytest.raises(RuntimeError) as caught:
        state.manifest_digest("registry.moleculesai.app/org/image:tag", "u", "t")

    assert body.calls == []
    assert body.closed
    assert "sensitive-registry-error-body" not in str(caught.value)


def test_rejects_malformed_ref_or_digest_header(monkeypatch) -> None:
    with pytest.raises(ValueError):
        state.parse_ref("image:tag")

    class Response:
        headers = {}

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self, _limit):
            return b"{}"

    monkeypatch.setattr(state.NO_REDIRECT_OPENER, "open", lambda *_args, **_kwargs: Response())
    with pytest.raises(RuntimeError, match="Docker-Content-Digest"):
        state.manifest_digest("registry.moleculesai.app/org/image:tag", "u", "t")


def test_rejects_noncanonical_registry_before_sending_auth() -> None:
    with pytest.raises(ValueError, match="canonical"):
        state.parse_ref("attacker.invalid/org/image:tag")


def test_rejects_oversized_manifest(monkeypatch) -> None:
    monkeypatch.setattr(state, "MAX_MANIFEST_BYTES", 8)

    class Response:
        headers = {"Docker-Content-Digest": "sha256:" + "0" * 64}

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self, limit):
            return b"x" * limit

    monkeypatch.setattr(
        state.NO_REDIRECT_OPENER, "open", lambda *_args, **_kwargs: Response()
    )
    with pytest.raises(RuntimeError, match="exceeded"):
        state.manifest_digest("registry.moleculesai.app/org/image:tag", "u", "t")


def test_registry_auth_redirect_is_rejected_before_forwarding() -> None:
    request = state.urllib.request.Request(
        "https://registry.moleculesai.app/v2/org/image/manifests/tag",
        headers={"Authorization": "Basic secret"},
    )
    with pytest.raises(urllib.error.HTTPError, match="redirect refused"):
        state.RejectRedirect().redirect_request(
            request, None, 307, "redirect", {}, "https://attacker.invalid/steal"
        )
