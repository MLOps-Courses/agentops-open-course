"""PII redaction guardrail — Microsoft Presidio (Chapter 4.5).

Presidio detects and redacts personally identifiable information (emails, phone numbers, IP
addresses, names, …) **fully locally** — no API, no account, no key (MIT licensed). This module
wires callbacks at the outbound model, inbound model, and tool-output boundaries. The outbound
callback prevents detected PII from reaching the provider. Session ingestion happens earlier;
trace and log safety therefore also relies on telemetry content capture being disabled. The
engines are built lazily (``functools.cache``) on first use.
"""

from __future__ import annotations

from functools import cache
from typing import Any

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.tools.base_tool import BaseTool
from google.adk.tools.tool_context import ToolContext
from presidio_analyzer import AnalyzerEngine
from presidio_analyzer.nlp_engine import NlpEngineProvider
from presidio_anonymizer import AnonymizerEngine

# Use the small English spaCy model (pinned in pyproject.toml). The default Presidio engine would
# otherwise download the 400 MB ``en_core_web_lg`` at first run — this keeps the agent light and offline.
_NLP_CONFIG = {"nlp_engine_name": "spacy", "models": [{"lang_code": "en", "model_name": "en_core_web_sm"}]}


@cache
def _analyzer() -> AnalyzerEngine:
    """Build the Presidio analyzer once, backed by the small spaCy model."""
    nlp_engine = NlpEngineProvider(nlp_configuration=_NLP_CONFIG).create_engine()
    return AnalyzerEngine(nlp_engine=nlp_engine, supported_languages=["en"])


@cache
def _anonymizer() -> AnonymizerEngine:
    """Build the Presidio anonymizer once (replaces spans with ``<ENTITY_TYPE>`` placeholders)."""
    return AnonymizerEngine()


def redact_pii(text: str) -> str:
    """Return ``text`` with any detected PII replaced by ``<ENTITY_TYPE>`` placeholders.

    Fully local and deterministic; a string with no PII is returned unchanged. This is a pure
    function, so it is trivially unit-testable ([4.2. Testing](../../../../docs/4.%20Quality/4.2.%20Testing.md)).
    """
    if not text.strip():
        return text
    results = _analyzer().analyze(text=text, language="en")
    if not results:
        return text
    # presidio_analyzer and presidio_anonymizer each declare their own RecognizerResult class; they are
    # runtime-compatible (this is Presidio's canonical usage) but nominally distinct to the type checker.
    return _anonymizer().anonymize(text=text, analyzer_results=results).text  # ty: ignore[invalid-argument-type]


def _redact_value(value: Any) -> Any:
    """Recursively redact strings in tool arguments and structured results."""
    if isinstance(value, str):
        return redact_pii(value)
    if isinstance(value, dict):
        return {key: _redact_value(item) for key, item in value.items()}
    if isinstance(value, list):
        return [_redact_value(item) for item in value]
    if isinstance(value, tuple):
        return tuple(_redact_value(item) for item in value)
    return value


def redact_request_pii(callback_context: CallbackContext, llm_request: LlmRequest) -> None:
    """``before_model_callback``: mask PII in every text part before the request leaves the process.

    Returning ``None`` lets the now-redacted request proceed. Mutating the request in place guarantees
    that detected PII is masked at the provider boundary; telemetry content capture is controlled separately.
    """
    del callback_context  # part of the ADK callback signature; unused here
    for content in llm_request.contents:
        for part in content.parts or []:
            if part.text:
                part.text = redact_pii(part.text)
            if part.function_call and part.function_call.args:
                part.function_call.args = _redact_value(dict(part.function_call.args))
            if part.function_response and part.function_response.response:
                part.function_response.response = _redact_value(dict(part.function_response.response))
    # Returning None (implicitly) lets the now-redacted request proceed to the model.


def redact_response_pii(callback_context: CallbackContext, llm_response: LlmResponse) -> LlmResponse | None:
    """``after_model_callback``: redact text and tool arguments before caller/tool use."""
    del callback_context
    changed = False
    if llm_response.content:
        for part in llm_response.content.parts or []:
            if part.text:
                redacted = redact_pii(part.text)
                changed = changed or redacted != part.text
                part.text = redacted
            if part.function_call and part.function_call.args:
                redacted_args = _redact_value(dict(part.function_call.args))
                changed = changed or redacted_args != part.function_call.args
                part.function_call.args = redacted_args
    return llm_response if changed else None


def redact_tool_output_pii(
    tool: BaseTool,
    args: dict[str, Any],
    tool_context: ToolContext,
    tool_response: dict[str, Any],
) -> dict[str, Any] | None:
    """``after_tool_callback``: redact structured tool output before reuse by the model."""
    del tool, args, tool_context
    redacted = _redact_value(tool_response)
    return redacted if redacted != tool_response else None
