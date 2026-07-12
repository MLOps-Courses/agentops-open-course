"""The AgentOps Open Course reference package.

Pure data and MCP modules import without initializing ADK. CLI discovery still
finds ``agent.agent.root_agent`` through the lazy package attribute below.
"""

from __future__ import annotations

from importlib import import_module
from types import ModuleType
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from . import agent as agent

__all__ = ["agent"]


def __getattr__(name: str) -> ModuleType:
    """Load the ADK agent only when a caller explicitly requests it."""
    if name != "agent":
        raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
    module = import_module(f"{__name__}.agent")
    globals()[name] = module
    return module


def __dir__() -> list[str]:
    """Advertise the lazy public attribute to discovery and interactive tools."""
    return sorted({*globals(), *__all__})
