"""Unit tests for the MCP server and client wiring (Ch. 3.3)."""

import asyncio
import sqlite3
from contextlib import closing
from types import SimpleNamespace
from typing import cast

import httpx
import pytest
from mcp import Client
from mcp.server.transport_security import TransportSecurityMiddleware
from mcp.types import LATEST_PROTOCOL_VERSION
from starlette.requests import Request

from agent import data, mcp_server, tools
from agent.config import settings
from agent.mcp_client import MCP_READ_TOOL_NAMES, ops_mcp_toolset
from agent.mcp_server import mcp


def test_mcp_server_exposes_exactly_the_allowlisted_read_tools() -> None:
    """Server registration and client allowlist are one contract, asserted in both directions.

    Adding a tool to the server without adding it here means it is silently never offered to
    the model; removing one leaves a stale allow entry. Set equality catches both, and
    ``scripts/check-infra.sh`` asserts the three gateway configs carry the same six names.
    """
    registered = asyncio.run(mcp.list_tools())
    assert {tool.name for tool in registered} == set(MCP_READ_TOOL_NAMES)


def test_mcp_read_tools_advertise_read_only_hints() -> None:
    """Hints help clients present the tools honestly; they never replace the allowlist."""
    for tool in asyncio.run(mcp.list_tools()):
        assert tool.annotations == mcp_server.READ_ONLY, tool.name
    assert mcp_server.READ_ONLY.read_only_hint is True
    assert mcp_server.READ_ONLY.open_world_hint is False


def test_mcp_transport_security_uses_a_narrow_host_allowlist() -> None:
    settings = mcp_server.TRANSPORT_SECURITY
    assert settings.enable_dns_rebinding_protection is True
    assert {
        "localhost",
        "localhost:*",
        "127.0.0.1",
        "127.0.0.1:*",
        "host.docker.internal",
        "host.docker.internal:*",
        "agentgateway",
        "agentgateway:*",
        "agentgateway.agentops.svc.cluster.local",
        "agentgateway.agentops.svc.cluster.local:*",
        "agentops-mcp",
        "agentops-mcp:*",
        "agentops-mcp.agentops.svc.cluster.local",
        "agentops-mcp.agentops.svc.cluster.local:*",
    } <= set(settings.allowed_hosts)
    assert "*" not in settings.allowed_hosts


@pytest.mark.parametrize(
    "host",
    [
        "localhost",
        "localhost:8000",
        "127.0.0.1:8000",
        "host.docker.internal:8000",
        "agentgateway:3000",
        "agentgateway.agentops.svc.cluster.local:3000",
        "agentops-mcp:8000",
        "agentops-mcp.agentops.svc.cluster.local",
        "agentops-mcp.agentops.svc.cluster.local:8000",
    ],
)
def test_mcp_transport_security_accepts_expected_authorities(host) -> None:
    settings = mcp_server.TRANSPORT_SECURITY
    middleware = TransportSecurityMiddleware(settings)
    request = cast("Request", SimpleNamespace(headers={"host": host}))
    assert asyncio.run(middleware.validate_request(request)) is None


def test_mcp_transport_security_rejects_untrusted_host() -> None:
    settings = mcp_server.TRANSPORT_SECURITY
    middleware = TransportSecurityMiddleware(settings)
    request = cast("Request", SimpleNamespace(headers={"host": "attacker.example"}))
    response = asyncio.run(middleware.validate_request(request))
    assert response is not None
    assert response.status_code == 421


def test_mcp_allowed_hosts_can_be_narrowed_by_environment(monkeypatch) -> None:
    monkeypatch.setenv("MCP_ALLOWED_HOSTS", " agentops-mcp:8000,agentops-mcp ")
    assert mcp_server._allowed_hosts() == ["agentops-mcp:8000", "agentops-mcp"]  # noqa: SLF001


def test_mcp_allowed_hosts_rejects_an_empty_override(monkeypatch) -> None:
    monkeypatch.setenv("MCP_ALLOWED_HOSTS", " , ")
    with pytest.raises(ValueError, match="at least one host authority"):
        mcp_server._allowed_hosts()  # noqa: SLF001


def test_mcp_toolset_constructs() -> None:
    # McpToolset connects lazily, so building it does not require the server to be running.
    toolset = ops_mcp_toolset()
    assert toolset is not None
    asyncio.run(toolset.close())


def test_gateway_mcp_toolset_constructs() -> None:
    toolset = ops_mcp_toolset("http://localhost:3000/mcp")
    assert toolset is not None
    asyncio.run(toolset.close())


def test_streamable_http_initialize_and_tool_call_round_trip() -> None:
    """Exercise the 2025-era handshake that ADK and agentgateway clients still negotiate."""

    async def exercise() -> None:
        # JSON responses keep this protocol test finite under an in-process ASGI
        # transport; production clients may negotiate the equivalent SSE form.
        app = mcp.streamable_http_app(
            json_response=True,
            stateless_http=True,
            transport_security=mcp_server.TRANSPORT_SECURITY,
        )
        transport = httpx.ASGITransport(app=app)
        common_headers = {
            "accept": "application/json, text/event-stream",
            "content-type": "application/json",
        }
        async with app.router.lifespan_context(app), httpx.AsyncClient(transport=transport) as client:
            initialized = await client.post(
                "http://localhost/mcp",
                headers=common_headers,
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": "2025-06-18",
                        "capabilities": {},
                        "clientInfo": {"name": "offline-contract-test", "version": "1"},
                    },
                },
            )
            initialized.raise_for_status()
            assert initialized.json()["result"]["serverInfo"]["name"] == "agentops-agent"
            session_headers = {
                **common_headers,
                "mcp-protocol-version": initialized.json()["result"]["protocolVersion"],
            }
            ready = await client.post(
                "http://localhost/mcp",
                headers=session_headers,
                json={"jsonrpc": "2.0", "method": "notifications/initialized"},
            )
            assert ready.status_code == 202
            result = await client.post(
                "http://localhost/mcp",
                headers=session_headers,
                json={
                    "jsonrpc": "2.0",
                    "id": 2,
                    "method": "tools/call",
                    "params": {"name": MCP_READ_TOOL_NAMES[0], "arguments": {}},
                },
            )
            result.raise_for_status()
            payload = result.json()["result"]
            assert payload.get("isError") is not True
            assert any(item.get("text") for item in payload["content"])

    asyncio.run(exercise())


def test_modern_client_discovers_and_calls_read_tools_without_a_handshake() -> None:
    """MCP 2026-07-28 clients connect statelessly; the same server serves both protocol eras."""

    async def exercise() -> tuple[str, set[str], bool]:
        async with Client(mcp) as client:
            listed = await client.list_tools()
            called = await client.call_tool(MCP_READ_TOOL_NAMES[0], {})
            return client.protocol_version, {tool.name for tool in listed.tools}, bool(called.is_error)

    protocol_version, names, is_error = asyncio.run(exercise())
    assert protocol_version == LATEST_PROTOCOL_VERSION
    assert names == set(MCP_READ_TOOL_NAMES)
    assert is_error is False


def test_gateway_toolset_sends_bearer_token_when_configured(monkeypatch) -> None:
    from pydantic import SecretStr

    from agent.mcp_client import settings as client_settings

    monkeypatch.setattr(client_settings, "mcp_token", SecretStr("demo-jwt"))
    toolset = ops_mcp_toolset("http://localhost:3000/mcp")
    params = toolset._mcp_session_manager._connection_params  # noqa: SLF001 - asserts the auth header
    assert getattr(params, "headers", None) == {"Authorization": "Bearer demo-jwt"}
    asyncio.run(toolset.close())


def test_gateway_toolset_sends_no_header_without_a_token(monkeypatch) -> None:
    from agent.mcp_client import settings as client_settings

    monkeypatch.setattr(client_settings, "mcp_token", None)
    toolset = ops_mcp_toolset("http://localhost:3000/mcp")
    params = toolset._mcp_session_manager._connection_params  # noqa: SLF001
    assert getattr(params, "headers", "missing") is None
    asyncio.run(toolset.close())


def test_mcp_main_runs_stdio_transport(monkeypatch) -> None:
    called: list[str] = []
    monkeypatch.setenv("MCP_TRANSPORT", "stdio")
    monkeypatch.setattr(mcp_server.mcp, "run", called.append)
    mcp_server.main()
    assert called == ["stdio"]


@pytest.mark.parametrize(
    ("transport", "factory"),
    [("sse", "sse_app"), ("streamable-http", "streamable_http_app")],
)
def test_mcp_http_transports_have_a_bounded_sigterm_drain(transport, factory, monkeypatch) -> None:
    app = object()
    call: dict[str, object] = {}

    def fake_run(target, **kwargs) -> None:
        call.update({"app": target, **kwargs})

    options: dict[str, object] = {}

    def build(**kwargs) -> object:
        options.update(kwargs)
        return app

    monkeypatch.setenv("MCP_TRANSPORT", transport)
    monkeypatch.setattr(mcp_server.mcp, factory, build)
    monkeypatch.setattr(mcp_server.uvicorn, "run", fake_run)
    mcp_server.main()
    assert call == {
        "app": app,
        "host": "127.0.0.1",
        "port": 8000,
        "log_level": "info",
        "timeout_graceful_shutdown": 10,
    }
    assert options["transport_security"] is mcp_server.TRANSPORT_SECURITY
    assert options.get("stateless_http", transport == "streamable-http") is (transport == "streamable-http")


def test_mcp_main_rejects_unknown_transport(monkeypatch) -> None:
    monkeypatch.setenv("MCP_TRANSPORT", "websocket")
    with pytest.raises(ValueError, match="Unsupported MCP_TRANSPORT"):
        mcp_server.main()


def test_mcp_cli_treats_keyboard_interrupt_as_clean_shutdown(monkeypatch) -> None:
    def interrupt() -> None:
        raise KeyboardInterrupt

    monkeypatch.setattr(mcp_server, "main", interrupt)
    assert mcp_server.cli() is None


def test_mcp_health_routes_are_registered() -> None:
    paths = {route.path for route in mcp._custom_starlette_routes}  # noqa: SLF001
    assert {"/healthz", "/livez"} <= paths


def test_mcp_healthz_reports_ready() -> None:
    data.db_path()  # the A2A owner publishes state before the read-only MCP pod becomes ready
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 200


def test_mcp_probe_and_read_tools_do_not_prepare_writable_state(monkeypatch) -> None:
    destination = settings.state_dir / "incidents.db"
    assert not destination.exists()

    def reject_prepare() -> None:
        raise AssertionError("read-only MCP path attempted a runtime migration")

    monkeypatch.setattr(data, "prepare_runtime_database", reject_prepare)
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 503
    assert tools.list_incidents()["count"] > 0
    assert not destination.exists()


def test_mcp_healthz_fails_on_fresh_state_without_initializing_it(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(settings, "state_dir", tmp_path / "fresh-state")
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 503
    assert not (settings.state_dir / "incidents.db").exists()


def test_mcp_healthz_rejects_a_corrupt_runtime_database() -> None:
    settings.state_dir.mkdir(parents=True)
    destination = settings.state_dir / "incidents.db"
    destination.write_text("not a SQLite database", encoding="utf-8")
    before = destination.read_bytes()
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 503
    assert destination.read_bytes() == before


def test_mcp_healthz_rejects_unprepared_audit_schema_without_migrating_it() -> None:
    destination = data.db_path()
    with closing(sqlite3.connect(destination)) as connection:
        connection.execute("DROP INDEX uq_audit_log_idempotency")
        connection.commit()
    before = destination.read_bytes()
    before_mtime = destination.stat().st_mtime_ns

    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 503
    assert destination.read_bytes() == before
    assert destination.stat().st_mtime_ns == before_mtime


def test_mcp_livez_is_trivially_alive() -> None:
    response = asyncio.run(mcp_server.livez(cast("Request", None)))
    assert response.status_code == 200
