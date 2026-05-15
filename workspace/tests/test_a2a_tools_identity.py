"""Tests for ``tool_get_runtime_identity`` and ``tool_update_agent_card``.

These two MCP tools close the T4-tier workspace owner-permission gaps
reported via the canvas:

  - the agent could not update its own ``agent_card`` (no MCP tool
    wrapped the existing ``POST /registry/update-card`` endpoint);
  - the agent could not identify which model it was running (the
    ``MODEL`` env var is injected by ``provisioner.workspace_provision``
    but nothing surfaced it back to the agent).

Ported from molecule-ai-workspace-runtime PR#17 (mirror-only repo;
canonical edit point per ``reference_runtime_repo_is_mirror_only``).
Adapted to core's conventions:

  * tool functions return ``str`` (JSON-encoded), matching every other
    tool in ``a2a_tools_*`` modules. Tests ``json.loads`` to inspect.
  * permission check ``memory.write`` runs inline in
    ``tool_update_agent_card`` (same pattern as
    ``a2a_tools_memory.tool_commit_memory``).
  * ``WORKSPACE_ID`` is read directly from ``os.environ`` — core does
    not have the runtime's validated-cache layer (``molecule_runtime.
    builtin_tools.validation``).
"""
from __future__ import annotations

import json

import pytest


# --- Drift gate: re-export aliases on a2a_tools ------------------------------

class TestBackCompatAliases:
    """Pin that ``a2a_tools.tool_*`` resolves to the same callable as
    ``a2a_tools_identity.tool_*``. Refactor wrapping (e.g. a doc-string
    wrapper that loses the function identity) silently breaks call
    sites that ``patch("a2a_tools.tool_update_agent_card", ...)`` —
    this gate makes that drift fail fast."""

    def test_tool_get_runtime_identity_alias(self):
        import a2a_tools
        import a2a_tools_identity
        assert a2a_tools.tool_get_runtime_identity is a2a_tools_identity.tool_get_runtime_identity

    def test_tool_update_agent_card_alias(self):
        import a2a_tools
        import a2a_tools_identity
        assert a2a_tools.tool_update_agent_card is a2a_tools_identity.tool_update_agent_card


# --- tool_get_runtime_identity ----------------------------------------------

class TestGetRuntimeIdentity:
    """The tool returns env-derived runtime identity. No HTTP call."""

    @pytest.mark.asyncio
    async def test_returns_all_known_env_fields(self, monkeypatch):
        from a2a_tools_identity import tool_get_runtime_identity

        monkeypatch.setenv("MODEL", "claude-opus-4-7")
        monkeypatch.setenv("MODEL_PROVIDER", "anthropic")
        monkeypatch.setenv("TIER", "T4")
        monkeypatch.setenv("WORKSPACE_ID", "ws-abc")
        monkeypatch.setenv("ADAPTER_MODULE", "adapter")
        monkeypatch.setenv("MOLECULE_MODEL", "claude-opus-4-7")
        monkeypatch.setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")

        out = await tool_get_runtime_identity()
        # MCP tools return JSON-encoded strings (matches the contract
        # every other tool_* in a2a_tools_* uses).
        assert isinstance(out, str)
        parsed = json.loads(out)

        assert parsed["model"] == "claude-opus-4-7"
        assert parsed["model_provider"] == "anthropic"
        assert parsed["tier"] == "T4"
        assert parsed["workspace_id"] == "ws-abc"
        assert parsed["runtime"] == "adapter"
        assert parsed["molecule_model"] == "claude-opus-4-7"
        assert parsed["anthropic_base_url"] == "https://api.anthropic.com"

    @pytest.mark.asyncio
    async def test_missing_env_returns_empty_strings(self, monkeypatch):
        """Tool MUST NOT raise when env vars are absent — every key is
        present but the value is the empty string. The agent then knows
        the slot exists but is unset."""
        from a2a_tools_identity import tool_get_runtime_identity

        for var in (
            "MODEL", "MODEL_PROVIDER", "TIER", "WORKSPACE_ID",
            "ADAPTER_MODULE", "MOLECULE_MODEL", "ANTHROPIC_BASE_URL",
        ):
            monkeypatch.delenv(var, raising=False)

        parsed = json.loads(await tool_get_runtime_identity())
        assert parsed["model"] == ""
        assert parsed["model_provider"] == ""
        assert parsed["tier"] == ""
        assert parsed["workspace_id"] == ""
        assert parsed["runtime"] == ""
        assert parsed["molecule_model"] == ""
        assert parsed["anthropic_base_url"] == ""

    @pytest.mark.asyncio
    async def test_no_http_call_made(self, monkeypatch):
        """``get_runtime_identity`` is env-only — must not open
        httpx.AsyncClient even if the call would otherwise succeed.
        Tripwire any client construction."""
        import httpx

        from a2a_tools_identity import tool_get_runtime_identity

        class _Tripwire:
            def __init__(self, *_a, **_kw):
                raise AssertionError(
                    "tool_get_runtime_identity must not open httpx.AsyncClient"
                )

        monkeypatch.setattr(httpx, "AsyncClient", _Tripwire)
        # Must not raise.
        await tool_get_runtime_identity()

    @pytest.mark.asyncio
    async def test_helper_dict_matches_string_payload(self, monkeypatch):
        """``_runtime_identity_payload`` is the dict-returning helper
        used by both the public tool and tests. Verify the public tool
        json.dumps the same dict — no field is dropped or renamed by
        the encoding step."""
        from a2a_tools_identity import (
            _runtime_identity_payload,
            tool_get_runtime_identity,
        )

        monkeypatch.setenv("MODEL", "claude-opus-4-7")
        monkeypatch.setenv("TIER", "T4")
        monkeypatch.setenv("WORKSPACE_ID", "ws-helper-check")

        helper = _runtime_identity_payload()
        tool_str = await tool_get_runtime_identity()
        assert json.loads(tool_str) == helper


# --- tool_update_agent_card -------------------------------------------------


class _MockResponse:
    def __init__(self, status_code: int, payload: dict):
        self.status_code = status_code
        self._payload = payload
        self.text = json.dumps(payload)

    def json(self):
        return self._payload


class _MockClient:
    """Drop-in for httpx.AsyncClient context manager.

    Records the URL + json body + headers the tool POSTed so the test
    can assert against them. Returns the canned _MockResponse passed
    in at construction time.
    """

    def __init__(self, *, response: _MockResponse, captured: dict):
        self._response = response
        self._captured = captured

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_args):
        return False

    async def post(self, url, *, json=None, headers=None, **_kw):  # noqa: A002
        self._captured["url"] = url
        self._captured["json"] = json
        self._captured["headers"] = headers
        return self._response


@pytest.fixture
def _grant_memory_write(monkeypatch):
    """Force the inline RBAC gate inside ``tool_update_agent_card`` to
    succeed. The gate calls
    ``a2a_tools_rbac.check_memory_write_permission`` which inspects
    ``$MOLECULE_ROLES`` / the role table; the patch sidesteps that
    machinery so tests can focus on the platform-call shape.
    """
    import a2a_tools_identity
    monkeypatch.setattr(
        a2a_tools_identity, "_check_memory_write_permission", lambda: True
    )


class TestUpdateAgentCard:
    @pytest.mark.asyncio
    async def test_posts_to_registry_update_card(
        self, monkeypatch, _grant_memory_write,
    ):
        """Hits POST {PLATFORM_URL}/registry/update-card with the
        workspace bearer and the {workspace_id, agent_card} body shape
        the platform handler expects (workspace-server
        ``internal/handlers/registry.go``)."""
        import a2a_tools_identity

        monkeypatch.setenv("WORKSPACE_ID", "ws-42")
        # Ensure PLATFORM_URL re-import sees a deterministic value —
        # a2a_client imports it at module load so we patch the symbol
        # on a2a_tools_identity directly (the module's own reference).
        monkeypatch.setattr(a2a_tools_identity, "PLATFORM_URL", "http://test.invalid")

        captured: dict = {}
        response = _MockResponse(200, {"status": "updated"})

        def _client_factory(*_a, **_kw):
            return _MockClient(response=response, captured=captured)

        monkeypatch.setattr(a2a_tools_identity.httpx, "AsyncClient", _client_factory)
        monkeypatch.setattr(
            a2a_tools_identity, "_auth_headers_for_heartbeat",
            lambda: {"Authorization": "Bearer ws-token-xyz"},
        )

        card = {"name": "agent-foo", "version": "0.1.0", "description": "demo"}
        result_str = await a2a_tools_identity.tool_update_agent_card(card)
        result = json.loads(result_str)

        # URL: PLATFORM_URL + /registry/update-card
        assert captured["url"] == "http://test.invalid/registry/update-card"

        # The platform handler expects {workspace_id, agent_card}; the
        # agent_card is the raw object the agent submitted.
        body = captured["json"]
        assert body["workspace_id"] == "ws-42"
        assert body["agent_card"] == card

        # Auth header from auth_headers_for_heartbeat is forwarded
        # verbatim — same path commit_memory uses.
        assert captured["headers"]["Authorization"] == "Bearer ws-token-xyz"

        assert result["success"] is True
        assert result["status"] == "updated"

    @pytest.mark.asyncio
    async def test_propagates_server_error(
        self, monkeypatch, _grant_memory_write,
    ):
        """Non-200 from platform surfaces as a structured error to the
        agent. The agent sees {success:false, status_code, error} and
        can decide whether to retry, fall back, or escalate."""
        import a2a_tools_identity

        monkeypatch.setenv("WORKSPACE_ID", "ws-42")
        monkeypatch.setattr(a2a_tools_identity, "PLATFORM_URL", "http://test.invalid")

        captured: dict = {}
        response = _MockResponse(400, {"error": "invalid card"})

        monkeypatch.setattr(
            a2a_tools_identity.httpx, "AsyncClient",
            lambda *a, **kw: _MockClient(response=response, captured=captured),
        )
        monkeypatch.setattr(
            a2a_tools_identity, "_auth_headers_for_heartbeat", lambda: {},
        )

        result = json.loads(
            await a2a_tools_identity.tool_update_agent_card({"name": "x"})
        )
        assert result["success"] is False
        assert result["status_code"] == 400
        assert "invalid card" in str(result["error"]).lower()

    @pytest.mark.asyncio
    async def test_rejects_non_dict_card(self, _grant_memory_write):
        """The MCP schema constrains transport callers to pass a dict;
        in-process callers (tests, sibling modules) can still pass any
        type. Reject non-dict defensively so the platform isn't asked
        to validate JSON-encoded strings or lists."""
        from a2a_tools_identity import tool_update_agent_card

        result = json.loads(await tool_update_agent_card("not-a-dict"))
        assert result["success"] is False
        assert "dict" in str(result["error"]).lower()

    @pytest.mark.asyncio
    async def test_workspace_id_missing_returns_error(
        self, monkeypatch, _grant_memory_write,
    ):
        """If WORKSPACE_ID is not set the tool refuses to issue the
        request — it would otherwise POST with an empty workspace_id
        and let the platform return a confusing 400."""
        from a2a_tools_identity import tool_update_agent_card

        monkeypatch.delenv("WORKSPACE_ID", raising=False)

        result = json.loads(await tool_update_agent_card({"name": "x"}))
        assert result["success"] is False
        assert "workspace_id" in str(result["error"]).lower()

    @pytest.mark.asyncio
    async def test_denies_when_memory_write_permission_missing(self, monkeypatch):
        """The agent's RBAC role must grant ``memory.write`` to update
        the card. Read-only roles get an RBAC error string back
        immediately, never touching the platform."""
        import a2a_tools_identity

        monkeypatch.setenv("WORKSPACE_ID", "ws-42")
        monkeypatch.setattr(
            a2a_tools_identity, "_check_memory_write_permission", lambda: False,
        )

        # Tripwire httpx — must not be called when RBAC denies.
        import httpx

        class _Tripwire:
            def __init__(self, *_a, **_kw):
                raise AssertionError("RBAC denial must short-circuit before httpx call")

        monkeypatch.setattr(httpx, "AsyncClient", _Tripwire)

        result = json.loads(
            await a2a_tools_identity.tool_update_agent_card({"name": "x"}),
        )
        assert result["success"] is False
        assert "memory.write" in str(result["error"]).lower()

    @pytest.mark.asyncio
    async def test_network_exception_returns_structured_error(
        self, monkeypatch, _grant_memory_write,
    ):
        """A network exception (DNS failure, connect timeout, etc) is
        wrapped into a structured error dict instead of bubbling up
        to the MCP transport layer."""
        import a2a_tools_identity

        monkeypatch.setenv("WORKSPACE_ID", "ws-42")
        monkeypatch.setattr(a2a_tools_identity, "PLATFORM_URL", "http://test.invalid")

        class _ExplodingClient:
            async def __aenter__(self):
                return self

            async def __aexit__(self, *_a):
                return False

            async def post(self, *_a, **_kw):
                raise RuntimeError("simulated DNS failure")

        monkeypatch.setattr(
            a2a_tools_identity.httpx, "AsyncClient",
            lambda *a, **kw: _ExplodingClient(),
        )

        result = json.loads(
            await a2a_tools_identity.tool_update_agent_card({"name": "x"})
        )
        assert result["success"] is False
        assert "network" in str(result["error"]).lower()


# --- Registry contract ------------------------------------------------------


class TestRegistryContract:
    """Pin the new tools' registration in platform_tools.registry. The
    structural tests in ``test_platform_tools.py`` already check
    registry↔MCP alignment; these are tighter assertions specific to
    the two new tools so a future contributor deleting one entry sees
    a focused failure."""

    def test_get_runtime_identity_in_registry(self):
        from platform_tools.registry import by_name
        spec = by_name("get_runtime_identity")
        assert spec.section == "a2a"
        # No input parameters — env-only call.
        assert spec.input_schema == {"type": "object", "properties": {}}
        # impl points at the actual tool function, not a shim.
        from a2a_tools_identity import tool_get_runtime_identity
        assert spec.impl is tool_get_runtime_identity

    def test_update_agent_card_in_registry(self):
        from platform_tools.registry import by_name
        spec = by_name("update_agent_card")
        assert spec.section == "a2a"
        assert "card" in spec.input_schema["properties"]
        assert spec.input_schema["required"] == ["card"]
        from a2a_tools_identity import tool_update_agent_card
        assert spec.impl is tool_update_agent_card
