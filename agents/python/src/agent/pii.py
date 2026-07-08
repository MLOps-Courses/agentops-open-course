"""PII redaction guardrail — Microsoft Presidio (Chapter 4.5).

Presidio detects and redacts personally identifiable information (emails, phone numbers, IP
addresses, names, …) **fully locally** — no API, no account, no key (MIT licensed). This module
wires it as an ADK ``before_model_callback`` so PII in the conversation is masked *before* it
reaches the model provider — and therefore before it lands in traces, logs, or the provider's
servers. The engines are built lazily (``functools.cache``) on first use, so importing the agent
stays fast and unit tests that never redact never pay for spaCy.
"""

from __future__ import annotations

from functools import cache

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
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
    function, so it is trivially unit-testable ([4.2. Testing](../../../docs/4.%20Quality/4.2.%20Testing.md)).
    """
    if not text.strip():
        return text
    results = _analyzer().analyze(text=text, language="en")
    if not results:
        return text
    # presidio_analyzer and presidio_anonymizer each declare their own RecognizerResult class; they are
    # runtime-compatible (this is Presidio's canonical usage) but nominally distinct to the type checker.
    return _anonymizer().anonymize(text=text, analyzer_results=results).text  # ty: ignore[invalid-argument-type]


def redact_request_pii(callback_context: CallbackContext, llm_request: LlmRequest) -> None:
    """``before_model_callback``: mask PII in every text part before the request leaves the process.

    Returning ``None`` lets the (now-redacted) request proceed to the model. Mutating the request in
    place is the whole guarantee: the provider — and any OTLP trace of the call — only ever sees masked text.
    """
    del callback_context  # part of the ADK callback signature; unused here
    for content in llm_request.contents:
        for part in content.parts or []:
            if part.text:
                part.text = redact_pii(part.text)
    # Returning None (implicitly) lets the now-redacted request proceed to the model.
