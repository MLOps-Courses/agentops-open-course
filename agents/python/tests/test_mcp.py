"""Unit tests for the MCP server and client wiring (Ch. 3.3)."""

import asyncio
from types import SimpleNamespace
from typing import cast

import pytest
from mcp.server.transport_security import TransportSecurityMiddleware
from starlette.requests import HTTPConnection, Request

from agent import mcp_server
from agent.config import settings
from agent.mcp_client import ops_mcp_toolset
from agent.mcp_server import mcp


def test_mcp_server_exposes_the_read_tools() -> None:
    registered = asyncio.run(mcp.list_tools())
    names = {tool.name for tool in registered}
    assert {
        "list_incidents",
        "get_incident",
        "get_service_status",
        "search_service_logs",
        "get_runbook",
        "search_runbooks",
    } <= names


def test_mcp_transport_security_uses_a_narrow_host_allowlist() -> None:
    settings = mcp.settings.transport_security
    assert settings is not None
    assert settings.enable_dns_rebinding_protection is True
    assert {
        "localhost",
        "localhost:*",
        "127.0.0.1",
        "127.0.0.1:*",
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
        "agentgateway:3000",
        "agentgateway.agentops.svc.cluster.local:3000",
        "agentops-mcp:8000",
        "agentops-mcp.agentops.svc.cluster.local",
        "agentops-mcp.agentops.svc.cluster.local:8000",
    ],
)
def test_mcp_transport_security_accepts_expected_authorities(host) -> None:
    settings = mcp.settings.transport_security
    assert settings is not None
    middleware = TransportSecurityMiddleware(settings)
    request = cast("HTTPConnection", SimpleNamespace(headers={"host": host}))
    assert asyncio.run(middleware.validate_request(request)) is None


def test_mcp_transport_security_rejects_untrusted_host() -> None:
    settings = mcp.settings.transport_security
    assert settings is not None
    middleware = TransportSecurityMiddleware(settings)
    request = cast("HTTPConnection", SimpleNamespace(headers={"host": "attacker.example"}))
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

    monkeypatch.setenv("MCP_TRANSPORT", transport)
    monkeypatch.setattr(mcp_server.mcp, factory, lambda: app)
    monkeypatch.setattr(mcp_server.uvicorn, "run", fake_run)
    mcp_server.main()
    assert call == {
        "app": app,
        "host": "127.0.0.1",
        "port": 8000,
        "log_level": "info",
        "timeout_graceful_shutdown": 10,
    }


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
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 200


def test_mcp_healthz_fails_without_the_seed_dataset(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(settings, "data_dir", tmp_path / "missing")
    monkeypatch.setattr(settings, "state_dir", tmp_path / "fresh-state")
    response = asyncio.run(mcp_server.healthz(cast("Request", None)))
    assert response.status_code == 503


def test_mcp_livez_is_trivially_alive() -> None:
    response = asyncio.run(mcp_server.livez(cast("Request", None)))
    assert response.status_code == 200
