"""Unit tests for the Presidio PII-redaction guardrail (Ch. 4.5).

These exercise the pure ``redact_pii`` helper and the wired ``before_model_callback``, so they
run fully offline (no model, no key). The first call loads the small spaCy model once; subsequent
calls are cached.
"""

from typing import cast

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.genai import types

from agent import pii

# The callback ignores the context (it only rewrites request parts), so a cast None is enough.
_NO_CONTEXT = cast("CallbackContext", None)


def test_redacts_email_and_phone() -> None:
    redacted = pii.redact_pii("Ping the on-call at jane.doe@acme.com or 555-123-4567.")
    assert "jane.doe@acme.com" not in redacted
    assert "555-123-4567" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted


def test_redacts_ip_address() -> None:
    redacted = pii.redact_pii("The failing host is 10.0.0.5 in the checkout cluster.")
    assert "10.0.0.5" not in redacted
    assert "<IP_ADDRESS>" in redacted


def test_leaves_pii_free_text_unchanged() -> None:
    clean = "List the open incidents for the checkout service."
    assert pii.redact_pii(clean) == clean


def test_empty_text_is_untouched() -> None:
    assert pii.redact_pii("   ") == "   "


def test_callback_redacts_request_parts_in_place() -> None:
    # The wired before_model_callback must mask PII in every text part *before* the request leaves
    # the process — mutating in place and returning None so the (now-redacted) request proceeds.
    request = LlmRequest(
        contents=[types.Content(role="user", parts=[types.Part(text="Email jane.doe@acme.com about INC-002.")])],
    )
    result = pii.redact_request_pii(_NO_CONTEXT, request)
    assert result is None  # returning None lets the redacted request proceed to the model
    parts = request.contents[0].parts
    assert parts is not None
    redacted = parts[0].text
    assert redacted is not None
    assert "jane.doe@acme.com" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted


def test_callback_tolerates_content_with_no_parts() -> None:
    # A Content with no parts (parts=None) must not raise — the callback simply skips it.
    request = LlmRequest(contents=[types.Content(role="user", parts=None)])
    assert pii.redact_request_pii(_NO_CONTEXT, request) is None
