"""Unit tests for the Presidio PII-redaction guardrail (Ch. 4.5).

These exercise the pure ``redact_pii`` helper and the wired ``before_model_callback``, so they
run fully offline (no model, no key). The first call loads the small spaCy model once; subsequent
calls are cached.
"""

from typing import cast

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext
from google.genai import types

from agent import pii, tools

# The callback ignores the context (it only rewrites request parts), so a cast None is enough.
_NO_CONTEXT = cast("CallbackContext", None)
_NO_TOOL_CONTEXT = cast("ToolContext", None)


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


def test_persisted_text_redacts_pii_and_credentials_but_keeps_domain_ids() -> None:
    redacted = pii.redact_persisted_text(
        "INC-002 approved by jane.doe@acme.com with Bearer abcdefghijklmnop and sk-abcdefghijklmnop"
    )
    assert "INC-002" in redacted
    assert "jane.doe@acme.com" not in redacted
    assert "abcdefghijklmnop" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted
    assert redacted.count("<SECRET>") == 2


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


def test_callback_redacts_structured_tool_results() -> None:
    request = LlmRequest(
        contents=[
            types.Content(
                role="tool",
                parts=[
                    types.Part.from_function_response(
                        name="lookup", response={"owner": "jane.doe@acme.com", "nested": ["10.0.0.5"]}
                    )
                ],
            )
        ]
    )
    pii.redact_request_pii(_NO_CONTEXT, request)
    parts = request.contents[0].parts
    assert parts is not None
    response = parts[0].function_response
    assert response is not None
    assert "jane.doe@acme.com" not in str(response.response)
    assert "10.0.0.5" not in str(response.response)


def test_callback_redacts_function_call_arguments() -> None:
    request = LlmRequest(
        contents=[
            types.Content(
                role="model",
                parts=[types.Part.from_function_call(name="lookup", args={"owner": "jane.doe@acme.com"})],
            )
        ]
    )
    pii.redact_request_pii(_NO_CONTEXT, request)
    parts = request.contents[0].parts
    assert parts is not None
    function_call = parts[0].function_call
    assert function_call is not None
    assert "jane.doe@acme.com" not in str(function_call.args)


def test_recursive_redaction_preserves_tuples_and_non_strings() -> None:
    redacted = pii._redact_value(("jane.doe@acme.com", 7))  # noqa: SLF001 - recursion boundary
    assert isinstance(redacted, tuple)
    assert redacted[1] == 7
    assert "jane.doe@acme.com" not in redacted[0]


def test_after_model_callback_redacts_final_output() -> None:
    response = LlmResponse(content=types.Content(role="model", parts=[types.Part(text="Email jane.doe@acme.com")]))
    redacted = pii.redact_response_pii(_NO_CONTEXT, response)
    assert redacted is response
    assert response.content is not None
    assert response.content.parts is not None
    assert "jane.doe@acme.com" not in (response.content.parts[0].text or "")


def test_after_model_callback_redacts_function_call_and_skips_clean_output() -> None:
    response = LlmResponse(
        content=types.Content(
            role="model",
            parts=[types.Part.from_function_call(name="lookup", args={"owner": "jane.doe@acme.com"})],
        )
    )
    assert pii.redact_response_pii(_NO_CONTEXT, response) is response
    assert response.content is not None
    assert response.content.parts is not None
    function_call = response.content.parts[0].function_call
    assert function_call is not None
    assert "jane.doe@acme.com" not in str(function_call.args)

    clean = LlmResponse(content=types.Content(role="model", parts=[types.Part(text="")]))
    assert pii.redact_response_pii(_NO_CONTEXT, clean) is None
    assert pii.redact_response_pii(_NO_CONTEXT, LlmResponse()) is None


def test_after_tool_callback_redacts_nested_output() -> None:
    tool = FunctionTool(func=tools.get_incident)
    response = {"owner": "jane.doe@acme.com", "hosts": ["10.0.0.5"]}
    redacted = pii.redact_tool_output_pii(tool, {}, _NO_TOOL_CONTEXT, response)
    assert redacted is not None
    assert "jane.doe@acme.com" not in str(redacted)
    assert "10.0.0.5" not in str(redacted)


def test_after_tool_callback_returns_none_for_clean_output() -> None:
    tool = FunctionTool(func=tools.get_incident)
    assert pii.redact_tool_output_pii(tool, {}, _NO_TOOL_CONTEXT, {"count": 7}) is None


def test_streamed_partial_chunks_are_redacted_individually() -> None:
    """Streaming does not bypass redaction: every partial chunk passes the callback (Ch. 3.6)."""
    from google.genai import types

    chunk = LlmResponse(
        partial=True,
        content=types.Content(role="model", parts=[types.Part(text="escalate to jane.doe@acme.com now")]),
    )
    redacted = pii.redact_response_pii(cast("CallbackContext", None), chunk)
    assert redacted is not None
    assert redacted.content is not None
    assert redacted.content.parts
    assert "jane.doe@acme.com" not in (redacted.content.parts[0].text or "")


def test_chunk_boundary_entities_are_best_effort_but_the_aggregate_is_caught() -> None:
    """Per-chunk redaction of a split entity is best-effort (detection depends on the
    fragment), while the final aggregated response is always redacted as a whole.
    Fragments already streamed to the client cannot be retracted — which is why
    AGENT_A2A_STREAMING defaults to false (Ch. 3.6)."""
    first, second = "contact jane.doe@", "acme.com for access"
    aggregate = pii.redact_pii(first + second)
    assert "jane.doe@acme.com" not in aggregate
    assert "<" in aggregate  # replaced with an <ENTITY_TYPE> placeholder
