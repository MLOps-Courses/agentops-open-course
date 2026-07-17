"""Consume the AgentOps Agent MCP server as a client toolset (Chapter 3.3).

``McpToolset`` launches the MCP server (here over stdio) and adapts its tools into ADK tools an
agent can call — no code change to the agent beyond adding the toolset. This is the seam the
gateway later slots into (Ch. 5.2): point the toolset at the gateway instead of the raw server.
"""

from __future__ import annotations

import sys

from google.adk.tools.mcp_tool.mcp_session_manager import StdioConnectionParams, StreamableHTTPConnectionParams
from google.adk.tools.mcp_tool.mcp_toolset import McpToolset
from mcp import StdioServerParameters

from .config import settings


# --8<-- [start:ops-mcp-toolset]
def ops_mcp_toolset(url: str | None = None) -> McpToolset:
    """Return the toolset over local stdio or a gateway streamable-HTTP URL.

    Both transports carry the course's explicit deadlines (Chapter 4.5): a hung
    MCP server or gateway then fails a tool call fast instead of hanging a turn.
    """
    endpoint = url or settings.mcp_url
    if endpoint:
        # A secured gateway route (Ch. 5.5) authenticates the caller by bearer
        # token; the default local route needs no header.
        headers = {"Authorization": f"Bearer {settings.mcp_token.get_secret_value()}"} if settings.mcp_token else None
        return McpToolset(
            connection_params=StreamableHTTPConnectionParams(
                url=endpoint,
                headers=headers,
                timeout=settings.tool_timeout_s,
                sse_read_timeout=settings.tool_timeout_s,
            ),
        )
    return McpToolset(
        connection_params=StdioConnectionParams(
            # Use the current interpreter so it works inside the project's virtualenv.
            server_params=StdioServerParameters(command=sys.executable, args=["-m", "agent.mcp_server"]),
            timeout=settings.tool_timeout_s,
        ),
    )


# --8<-- [end:ops-mcp-toolset]
