"""The Ops Copilot tools, exposed as an MCP server (Chapter 3.3).

The Model Context Protocol (MCP) lets tools live in their own process and be consumed by *any*
MCP client — so this one server backs **both** agent tracks and, later, the gateway (Ch. 5.2).
It re-exposes the read-only tools over stdio; the guarded write actions stay in-process. Run it
with ``mise run mcp`` (``python -m agent.mcp_server``).
"""

from __future__ import annotations

from mcp.server.fastmcp import FastMCP

from . import memory, tools

mcp = FastMCP("ops-copilot")

# Re-expose the read-only tools over MCP. FastMCP derives each tool's schema from the same
# type hints and docstrings the ADK function tools already carry.
for _tool in (
    tools.list_incidents,
    tools.get_incident,
    tools.get_service_status,
    memory.get_runbook,
    memory.search_runbooks,
):
    mcp.add_tool(_tool)


def main() -> None:
    """Run the MCP server over stdio (for an MCP client or the gateway)."""
    mcp.run()


if __name__ == "__main__":
    main()
