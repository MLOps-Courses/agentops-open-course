"""The AgentOps Open Course reference agent (Python track).

The `adk` CLI (`adk web` / `adk run` / `adk api_server`) discovers `root_agent`
by importing this package, which re-exports the `agent` module below.
"""

from . import agent

__all__ = ["agent"]
