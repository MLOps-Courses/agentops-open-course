"""Consume the Ops Copilot MCP server as a client toolset (Chapter 3.3).

``McpToolset`` launches the MCP server (here over stdio) and adapts its tools into ADK tools an
agent can call — no code change to the agent beyond adding the toolset. This is the seam the
gateway later slots into (Ch. 5.2): point the toolset at the gateway instead of the raw server.
"""

from __future__ import annotations

import sys

from google.adk.tools.mcp_tool.mcp_session_manager import StdioConnectionParams
from google.adk.tools.mcp_tool.mcp_toolset import McpToolset
from mcp import StdioServerParameters


def ops_mcp_toolset() -> McpToolset:
    """Return a toolset that launches and talks to the Ops Copilot MCP server over stdio."""
    return McpToolset(
        connection_params=StdioConnectionParams(
            # Use the current interpreter so it works inside the project's virtualenv.
            server_params=StdioServerParameters(command=sys.executable, args=["-m", "agent.mcp_server"]),
        ),
    )
