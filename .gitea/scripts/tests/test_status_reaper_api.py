import importlib.util
import json
import pathlib
import urllib.error


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "status-reaper.py"


def load_reaper():
    spec = importlib.util.spec_from_file_location("status_reaper", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    mod.API = "https://git.example.test/api/v1"
    mod.GITEA_TOKEN = "test-token"
    mod.API_TIMEOUT_SEC = 1
    mod.API_RETRIES = 3
    mod.API_RETRY_SLEEP_SEC = 0
    return mod


class FakeResponse:
    status = 200

    def __init__(self, payload):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False

    def read(self):
        return json.dumps(self.payload).encode("utf-8")


def test_api_retries_transient_timeout(monkeypatch):
    mod = load_reaper()
    calls = {"n": 0}

    def fake_urlopen(req, timeout):
        calls["n"] += 1
        if calls["n"] == 1:
            raise TimeoutError("simulated slow Gitea API")
        return FakeResponse({"ok": True})

    monkeypatch.setattr(mod.urllib.request, "urlopen", fake_urlopen)

    status, body = mod.api("GET", "/repos/o/r/commits")

    assert status == 200
    assert body == {"ok": True}
    assert calls["n"] == 2


def test_api_raises_after_retry_budget(monkeypatch):
    mod = load_reaper()

    def fake_urlopen(req, timeout):
        raise urllib.error.URLError("connection reset")

    monkeypatch.setattr(mod.urllib.request, "urlopen", fake_urlopen)

    try:
        mod.api("GET", "/repos/o/r/commits")
    except mod.ApiError as exc:
        assert "failed after 3 attempts" in str(exc)
    else:
        raise AssertionError("expected ApiError")
