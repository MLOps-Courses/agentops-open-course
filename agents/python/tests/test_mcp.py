"""Unit tests for the MCP server and client wiring (Ch. 3.3)."""

import asyncio

from agent.mcp_client import ops_mcp_toolset
from agent.mcp_server import mcp


def test_mcp_server_exposes_the_read_tools() -> None:
    registered = asyncio.run(mcp.list_tools())
    names = {tool.name for tool in registered}
    assert {"list_incidents", "get_incident", "get_service_status", "get_runbook", "search_runbooks"} <= names


def test_mcp_toolset_constructs() -> None:
    # McpToolset connects lazily, so building it does not require the server to be running.
    toolset = ops_mcp_toolset()
    assert toolset is not None
