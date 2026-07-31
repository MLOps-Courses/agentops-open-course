"""Cross-cutting agent policy, attached once at the ADK ``App`` boundary (Chapter 4.5).

Every agent in this package is governed by the same rules: a per-session token budget,
bounded history, PII redaction on both sides of the model call, hardened tool output,
validated write arguments, and safe error surfaces. Attaching that policy to each ``Agent``
by hand made it a clerical duty — a tenth agent added by a learner silently shipped with no
redaction and no budget, and nothing failed.

ADK's answer to a cross-cutting concern is a ``BasePlugin`` registered on an ``App``: its
hooks fire for *every* agent the app runs, including sub-agents and workflow nodes. Adding
an agent therefore cannot lose the policy, because the policy was never attached per agent.

Two details are load-bearing and are preserved from the per-agent form:

* **Order.** The budget check runs first (a refused call needs no further work), then
  compaction bounds the history, then redaction runs on only the messages that survive.
* **First non-``None`` wins.** A callback returning a value short-circuits the rest, exactly
  as ADK's per-agent callback *lists* behave. ``record_token_usage`` returns ``None`` on
  purpose so the PII pass still sees every response.

``validate_actions`` is deliberately global here even though only two agents hold write
tools: it no-ops for every non-action tool, so making it universal costs nothing and means a
future agent that gains a write tool is guarded by construction rather than by review.
"""

from __future__ import annotations

from typing import Any

from google.adk import Workflow
from google.adk.agents import BaseAgent
from google.adk.agents.callback_context import CallbackContext
from google.adk.apps import App
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.plugins.base_plugin import BasePlugin
from google.adk.tools.base_tool import BaseTool
from google.adk.tools.tool_context import ToolContext

from .budget import enforce_token_budget, record_token_usage
from .compaction import compact_history
from .guardrails import handle_model_error, handle_tool_error, secure_tool_output, validate_actions
from .pii import redact_request_pii, redact_response_pii

# --8<-- [start:policy-plugin]
APP_NAME = "agentops-agent"


class AgentOpsPolicyPlugin(BasePlugin):
    """Apply the course's model, tool, and error policy to every agent in the app."""

    def __init__(self, name: str = "agentops_policy") -> None:
        super().__init__(name=name)

    async def before_model_callback(
        self, *, callback_context: CallbackContext, llm_request: LlmRequest
    ) -> LlmResponse | None:
        """Budget, then bound the history, then redact what survives."""
        for guard in (enforce_token_budget, compact_history, redact_request_pii):
            response = guard(callback_context, llm_request)
            if response is not None:
                return response
        return None

    async def after_model_callback(
        self, *, callback_context: CallbackContext, llm_response: LlmResponse
    ) -> LlmResponse | None:
        """Attribute this turn's tokens, then redact the response."""
        for guard in (record_token_usage, redact_response_pii):
            replacement = guard(callback_context, llm_response)
            if replacement is not None:
                return replacement
        return None

    async def before_tool_callback(
        self, *, tool: BaseTool, tool_args: dict[str, Any], tool_context: ToolContext
    ) -> dict[str, Any] | None:
        """Reject malformed arguments to a mutating action before it touches state."""
        return validate_actions(tool, tool_args, tool_context)

    async def after_tool_callback(
        self, *, tool: BaseTool, tool_args: dict[str, Any], tool_context: ToolContext, result: dict[str, Any]
    ) -> dict[str, Any] | None:
        """Harden untrusted tool output and redact PII before the model sees it."""
        return secure_tool_output(tool, tool_args, tool_context, result)

    async def on_model_error_callback(
        self, *, callback_context: CallbackContext, llm_request: LlmRequest, error: Exception
    ) -> LlmResponse | None:
        """Turn a provider failure into an actionable response instead of a stack trace."""
        return handle_model_error(callback_context, llm_request, error)

    async def on_tool_error_callback(
        self, *, tool: BaseTool, tool_args: dict[str, Any], tool_context: ToolContext, error: Exception
    ) -> dict[str, Any] | None:
        """Return a stable, non-sensitive error for a failed tool."""
        return handle_tool_error(tool, tool_args, tool_context, error)


def build_app(selected_root: BaseAgent | Workflow) -> App:
    """Attach the complete cross-cutting policy to one executable application."""
    return App(name=APP_NAME, root_agent=selected_root, plugins=[AgentOpsPolicyPlugin()])


# --8<-- [end:policy-plugin]
