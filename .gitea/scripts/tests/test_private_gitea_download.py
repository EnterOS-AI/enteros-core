import hashlib
import importlib.util
import io
import sys
import urllib.error
import urllib.request
from pathlib import Path

import pytest


SCRIPT = Path(__file__).resolve().parents[1] / "private-gitea-download.py"
spec = importlib.util.spec_from_file_location("private_gitea_download", SCRIPT)
download = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = download
spec.loader.exec_module(download)


def test_download_is_bounded_authenticated_checksum_verified_and_atomic(
    tmp_path: Path, monkeypatch
) -> None:
    body = b"reviewed helper"
    captured = {}

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self, limit):
            captured["limit"] = limit
            return body

    def open_request(request, timeout):
        captured["request"] = request
        captured["timeout"] = timeout
        return Response()

    monkeypatch.setattr(download.NO_REDIRECT_OPENER, "open", open_request)
    output = tmp_path / "helper.py"
    download.download(
        "https://git.moleculesai.app/api/v1/repos/o/r/raw/helper.py?ref=" + "a" * 40,
        hashlib.sha256(body).hexdigest(),
        output,
        "secret-token",
    )

    assert output.read_bytes() == body
    assert output.stat().st_mode & 0o777 == 0o700
    assert captured["request"].get_header("Authorization") == "token secret-token"
    assert "Authorization" not in captured["request"].headers
    assert captured["request"].unredirected_hdrs["Authorization"] == "token secret-token"
    assert captured["request"].get_header("User-agent") == "curl/8.4.0"
    assert captured["timeout"] == 60
    assert captured["limit"] == 5 * 1024 * 1024 + 1


def test_redirect_handler_rejects_before_forwarding_authorization() -> None:
    request = urllib.request.Request(
        "https://git.moleculesai.app/private",
        headers={"Authorization": "token secret-token"},
    )
    with pytest.raises(urllib.error.HTTPError, match="redirect refused"):
        download.RejectRedirect().redirect_request(
            request,
            None,
            302,
            "Found",
            {},
            "https://attacker.invalid/steal",
        )


def test_http_error_body_is_not_read_and_is_closed(tmp_path: Path, monkeypatch) -> None:
    class ErrorBody(io.BytesIO):
        def __init__(self):
            super().__init__(b"sensitive-error-body")
            self.read_calls = 0

        def read(self, size: int = -1) -> bytes:
            self.read_calls += 1
            return super().read(size)

    body = ErrorBody()
    error = urllib.error.HTTPError(
        "https://git.moleculesai.app/private", 503, "down", {}, body
    )

    def fail(*_args, **_kwargs):
        raise error

    monkeypatch.setattr(download.NO_REDIRECT_OPENER, "open", fail)

    with pytest.raises(SystemExit):
        download.download(
            "https://git.moleculesai.app/private",
            "0" * 64,
            tmp_path / "helper.py",
            "token",
        )

    assert body.read_calls == 0
    assert body.closed


def test_rejects_wrong_origin_and_checksum_without_writing(tmp_path: Path) -> None:
    output = tmp_path / "helper.py"
    with pytest.raises(SystemExit):
        download.download("https://attacker.invalid/x", "0" * 64, output, "token")
    assert not output.exists()


@pytest.mark.parametrize(
    "url",
    [
        "https://user@git.moleculesai.app/private",
        "https://git.moleculesai.app:444/private",
        "https://git.moleculesai.app/private#unexpected-fragment",
    ],
)
def test_rejects_credentialed_nonstandard_or_fragmented_urls(
    tmp_path: Path, url: str
) -> None:
    with pytest.raises(SystemExit):
        download.download(url, "0" * 64, tmp_path / "helper.py", "token")
