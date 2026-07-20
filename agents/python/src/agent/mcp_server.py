"""The AgentOps Agent tools, exposed as an MCP server (Chapter 3.3).

The Model Context Protocol (MCP) lets tools live in their own process and be consumed by *any*
MCP client — so this one server backs the agent and, later, the gateway (Ch. 5.2).
It re-exposes the read-only tools over stdio; the guarded write actions stay in-process. Run it
with ``mise run mcp`` (``python -m agent.mcp_server``).
"""

from __future__ import annotations

import os
from typing import Literal, cast

import uvicorn
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from starlette.requests import Request
from starlette.responses import JSONResponse

from . import memory, tools
from .config import settings
from .data import probe_runtime_database

_DEFAULT_ALLOWED_HOSTS = (
    "127.0.0.1",
    "127.0.0.1:*",
    "localhost",
    "localhost:*",
    "[::1]",
    "[::1]:*",
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
)
_ALLOWED_ORIGINS = ("http://127.0.0.1:*", "http://localhost:*", "http://[::1]:*")


def _allowed_hosts() -> list[str]:
    """Return the explicit DNS-rebinding allowlist from CSV or secure defaults."""
    raw = os.environ.get("MCP_ALLOWED_HOSTS")
    if raw is None:
        return list(_DEFAULT_ALLOWED_HOSTS)
    hosts = [host.strip() for host in raw.split(",") if host.strip()]
    if not hosts:
        raise ValueError("MCP_ALLOWED_HOSTS must contain at least one host authority")
    return hosts


mcp = FastMCP(
    "agentops-agent",
    host=os.environ.get("MCP_HOST", "127.0.0.1"),
    port=int(os.environ.get("MCP_PORT", "8000")),
    stateless_http=True,
    transport_security=TransportSecuritySettings(
        enable_dns_rebinding_protection=True,
        allowed_hosts=_allowed_hosts(),
        allowed_origins=list(_ALLOWED_ORIGINS),
    ),
)

# Re-expose the read-only tools over MCP. FastMCP derives each tool's schema from the same
# type hints and docstrings the ADK function tools already carry.
for _tool in (
    tools.list_incidents,
    tools.get_incident,
    tools.get_service_status,
    tools.search_service_logs,
    memory.get_runbook,
    memory.search_runbooks,
):
    mcp.add_tool(_tool)


# Kubernetes-facing health endpoints on the HTTP transports (Ch. 6). FastMCP
# serves custom routes without auth — suitable exactly for probes.
@mcp.custom_route("/healthz", methods=["GET"])
async def healthz(request: Request) -> JSONResponse:
    """Readiness: shared state is readable and remains writable for agent actions."""
    del request
    problems: list[str] = []
    try:
        probe_runtime_database()
    except Exception as error:  # readiness reports every failure class as unready
        problems.append(f"dataset unavailable: {type(error).__name__}")
    if not os.access(settings.state_dir, os.W_OK):
        problems.append(f"state directory is not writable: {settings.state_dir}")
    if problems:
        return JSONResponse({"status": "unready", "problems": problems}, status_code=503)
    return JSONResponse({"status": "ready"})


@mcp.custom_route("/livez", methods=["GET"])
async def livez(request: Request) -> JSONResponse:
    """Liveness: trivial by design — restarts only help a wedged process."""
    del request
    return JSONResponse({"status": "alive"})


# --8<-- [start:mcp-server-transport]
def _run_http(transport: Literal["sse", "streamable-http"]) -> None:
    """Serve MCP HTTP with the same bounded SIGTERM drain as A2A."""
    app = mcp.sse_app() if transport == "sse" else mcp.streamable_http_app()
    uvicorn.run(
        app,
        host=mcp.settings.host,
        port=mcp.settings.port,
        log_level=mcp.settings.log_level.lower(),
        timeout_graceful_shutdown=int(settings.drain_timeout_s),
    )


def main() -> None:
    """Run over stdio by default or bounded Uvicorn for HTTP transports."""
    transport = os.environ.get("MCP_TRANSPORT", "stdio")
    if transport not in {"stdio", "sse", "streamable-http"}:
        raise ValueError(f"Unsupported MCP_TRANSPORT: {transport!r}")
    if transport == "stdio":
        mcp.run("stdio")
        return
    _run_http(cast("Literal['sse', 'streamable-http']", transport))


# --8<-- [end:mcp-server-transport]


def cli() -> None:
    """Treat Ctrl-C as a normal foreground-server shutdown."""
    try:
        main()
    except KeyboardInterrupt:
        return


if __name__ == "__main__":
    cli()
