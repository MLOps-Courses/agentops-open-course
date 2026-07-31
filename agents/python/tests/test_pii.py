"""Unit tests for the Presidio PII-redaction guardrail (Ch. 4.5).

These exercise the pure ``redact_pii`` helper and the wired ``before_model_callback``, so they
run fully offline (no model, no key). The first call loads the small spaCy model once; subsequent
calls are cached.
"""

import json
from typing import cast

import pytest
from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext
from google.genai import types

from agent import pii, tools
from tests.domain import REFERENCE_DOMAIN

# The callback ignores the context (it only rewrites request parts), so a cast None is enough.
_NO_CONTEXT = cast("CallbackContext", None)
_NO_TOOL_CONTEXT = cast("ToolContext", None)
_CHECKOUT = REFERENCE_DOMAIN.services.checkout
_CHECKOUT_CASCADE_INCIDENT = REFERENCE_DOMAIN.incidents.checkout_cascade
_CHECKOUT_INCIDENT = REFERENCE_DOMAIN.incidents.checkout_latency
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down
_SERVICE_DOWN_RUNBOOK = REFERENCE_DOMAIN.runbooks.service_down


def test_redacts_email_and_phone() -> None:
    redacted = pii.redact_pii("Ping the on-call at jane.doe@acme.com or 555-123-4567.")
    assert "jane.doe@acme.com" not in redacted
    assert "555-123-4567" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted


def test_redacts_ip_address() -> None:
    redacted = pii.redact_pii(f"The failing host is 10.0.0.5 in the {_CHECKOUT} cluster.")
    assert "10.0.0.5" not in redacted
    assert "<IP_ADDRESS>" in redacted


def test_boundary_policy_retains_broad_personal_data_coverage() -> None:
    redacted = pii.redact_pii("The device MAC address is 00:11:22:33:44:55.")
    assert "00:11:22:33:44:55" not in redacted
    assert "<MAC_ADDRESS>" in redacted


def test_redacts_personal_data_without_corrupting_domain_identifiers() -> None:
    redacted = pii.redact_pii(f"Email jane.doe@acme.com from Paris about {_INVENTORY_INCIDENT}.")
    assert "jane.doe@acme.com" not in redacted
    assert "Paris" not in redacted
    assert _INVENTORY_INCIDENT in redacted


@pytest.mark.parametrize(
    "address",
    [
        f"jane@{_INVENTORY_INCIDENT.lower()}.com",
        "jane@v2.4.0.com",
        "jane@sev1.com",
        "jane@p95.com",
    ],
)
def test_safe_operational_tokens_inside_email_domains_still_redact(address: str) -> None:
    redacted = pii.redact_pii(f"Email {address}")
    assert address not in redacted
    assert "<EMAIL_ADDRESS>" in redacted


@pytest.mark.parametrize(
    "url",
    [
        "http://grafana.internal/d/abc",
        "http://agentops-mcp.agentops.svc.cluster.local:8000/mcp",
        f"https://{_INVENTORY_INCIDENT.lower()}.com/releases/v2.4.0",
    ],
)
def test_urls_survive_the_boundary_intact(url: str) -> None:
    """A URL is operational evidence here, not personal data — and must never be half-rewritten.

    The old policy listed ``URL`` as a boundary entity, and the recognizer matched only the
    leading part of an internal hostname: ``http://grafana.internal/d/abc`` reached the model
    as ``<URL>ernal/d/abc``. Partial rewriting is the worst outcome, because the result still
    looks like a hostname. The previous test for this used a public ``.com`` domain, which
    matched cleanly and hid the bug.
    """
    text = f"Visit {url} for the dashboard"
    assert pii.redact_pii(text) == text
    # The persisted policy never redacted URLs, so both boundaries now agree.
    assert pii.redact_persisted_text(text) == text


def test_credentials_inside_a_url_are_still_removed() -> None:
    """Dropping the URL entity must not weaken the credential tripwires that run first."""
    redacted = pii.redact_pii("curl https://api.example.com/v1?OPENAI_API_KEY=sk-secret-value")
    assert "sk-secret-value" not in redacted


def test_keeps_service_and_runbook_identifiers() -> None:
    text = f"Search the {_INVENTORY} logs and open {_SERVICE_DOWN_RUNBOOK} for {_CHECKOUT_INCIDENT}."
    assert pii.redact_pii(text) == text


@pytest.mark.parametrize("prefix", ["release", "version"])
def test_keeps_versions_when_the_entity_model_groups_the_prefix(prefix: str) -> None:
    text = f"{prefix} v2026.07.07-2"
    assert pii.redact_pii(text) == text


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        (f"John {_INVENTORY_INCIDENT}", f"<PERSON> {_INVENTORY_INCIDENT}"),
        (f"{_INVENTORY_INCIDENT} John", f"{_INVENTORY_INCIDENT} <PERSON>"),
    ],
)
def test_redacts_person_next_to_protected_incident_identifier(text: str, expected: str) -> None:
    assert pii.redact_pii(text) == expected


@pytest.mark.parametrize(
    "identifier",
    [_CHECKOUT_INCIDENT, _INVENTORY_INCIDENT, "SEV1", "SEV2", "SEV3"],
)
def test_keeps_exact_incident_and_severity_identifiers(identifier: str) -> None:
    assert pii.redact_pii(identifier) == identifier
    assert pii.redact_persisted_text(identifier) == identifier


def test_real_tool_payload_keeps_operational_evidence_while_redacting_pii() -> None:
    payload = {**tools.list_incidents(), "operator_email": "jane.doe@acme.com"}
    redacted = pii._redact_value(payload)  # noqa: SLF001 - tool-output boundary
    rendered = json.dumps(redacted)
    for evidence in (
        _CHECKOUT_CASCADE_INCIDENT,
        "SEV2",
        "p95",
        "p99",
        "v2026.07.07-2",
        "2026-07-08T06:30:00Z",
    ):
        assert evidence in rendered
    assert "jane.doe@acme.com" not in rendered
    assert "<EMAIL_ADDRESS>" in rendered


def test_leaves_pii_free_text_unchanged() -> None:
    clean = f"List the open incidents for the {_CHECKOUT} service."
    assert pii.redact_pii(clean) == clean


def test_persisted_text_redacts_pii_and_credentials_but_keeps_domain_ids() -> None:
    redacted = pii.redact_persisted_text(
        f"{_INVENTORY_INCIDENT} approved by jane.doe@acme.com with Bearer abcdefghijklmnop and sk-abcdefghijklmnop"
    )
    assert _INVENTORY_INCIDENT in redacted
    assert "jane.doe@acme.com" not in redacted
    assert "abcdefghijklmnop" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted
    assert redacted.count("<SECRET>") == 2


@pytest.mark.parametrize(
    ("secret", "visible_fragment"),
    [
        ("Bearer abcdefghijklmnop", "abcdefghijklmnop"),
        ("sk-abcdefghijklmnop", "abcdefghijklmnop"),
        ('password="correct horse battery staple"', "correct horse"),
        ("access_token='long lived credential'", "long lived"),
        ("OPENAI_API_KEY=plain-secret-value", "plain-secret"),
        ("GOOGLE_API_KEY=another-secret-value", "another-secret"),
        ('MY_PASSWORD="correct horse battery staple"', "correct horse"),
    ],
)
def test_boundary_policy_redacts_credentials(secret: str, visible_fragment: str) -> None:
    redacted = pii.redact_pii(f"Do not expose {secret} to the model.")
    assert visible_fragment not in redacted
    assert "<SECRET>" in redacted


def test_empty_text_is_untouched() -> None:
    assert pii.redact_pii("   ") == "   "


def test_callback_redacts_request_parts_in_place() -> None:
    # The wired before_model_callback must mask PII in every text part *before* the request leaves
    # the process — mutating in place and returning None so the (now-redacted) request proceeds.
    request = LlmRequest(
        contents=[
            types.Content(
                role="user",
                parts=[types.Part(text=f"Email jane.doe@acme.com about {_INVENTORY_INCIDENT}.")],
            )
        ],
    )
    result = pii.redact_request_pii(_NO_CONTEXT, request)
    assert result is None  # returning None lets the redacted request proceed to the model
    parts = request.contents[0].parts
    assert parts is not None
    redacted = parts[0].text
    assert redacted is not None
    assert "jane.doe@acme.com" not in redacted
    assert "<EMAIL_ADDRESS>" in redacted
    assert _INVENTORY_INCIDENT in redacted


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
                parts=[
                    types.Part.from_function_call(
                        name="lookup",
                        args={"incident_id": _INVENTORY_INCIDENT, "owner": "jane.doe@acme.com"},
                    )
                ],
            )
        ]
    )
    pii.redact_request_pii(_NO_CONTEXT, request)
    parts = request.contents[0].parts
    assert parts is not None
    function_call = parts[0].function_call
    assert function_call is not None
    assert "jane.doe@acme.com" not in str(function_call.args)
    assert function_call.args is not None
    assert function_call.args["incident_id"] == _INVENTORY_INCIDENT


def test_callback_redacts_credentials_in_text_and_structured_values() -> None:
    request = LlmRequest(
        contents=[
            types.Content(
                role="user",
                parts=[
                    types.Part(text='password="correct horse battery staple"'),
                    types.Part.from_function_response(
                        name="lookup",
                        response={"authorization": "Bearer abcdefghijklmnop"},
                    ),
                ],
            )
        ]
    )
    pii.redact_request_pii(_NO_CONTEXT, request)
    rendered = str(request.contents[0].parts)
    assert "correct horse" not in rendered
    assert "abcdefghijklmnop" not in rendered
    assert rendered.count("<SECRET>") == 2


def test_recursive_redaction_preserves_tuples_and_non_strings() -> None:
    redacted = pii._redact_value(("jane.doe@acme.com", 7))  # noqa: SLF001 - recursion boundary
    assert isinstance(redacted, tuple)
    assert redacted[1] == 7
    assert "jane.doe@acme.com" not in redacted[0]


def test_persisted_redaction_recurses_through_session_model_and_tool_shapes() -> None:
    value = {
        "session": {
            "messages": [
                {"role": "user", "parts": ("Email jane.doe@acme.com", 7)},
                {
                    "role": "tool",
                    "response": {
                        "authorization": "Bearer abcdefghijklmnop",
                        "incident_id": REFERENCE_DOMAIN.incidents.inventory_down,
                    },
                },
            ]
        }
    }

    redacted = pii.redact_persisted_value(value)
    rendered = json.dumps(redacted)
    assert "jane.doe@acme.com" not in rendered
    assert "abcdefghijklmnop" not in rendered
    assert "<EMAIL_ADDRESS>" in rendered
    assert "<SECRET>" in rendered
    assert REFERENCE_DOMAIN.incidents.inventory_down in rendered
    assert isinstance(redacted["session"]["messages"][0]["parts"], tuple)
    assert redacted["session"]["messages"][0]["parts"][1] == 7


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
    response = {"id": _INVENTORY_INCIDENT, "owner": "jane.doe@acme.com", "hosts": ["10.0.0.5"]}
    redacted = pii.redact_tool_output_pii(tool, {}, _NO_TOOL_CONTEXT, response)
    assert redacted is not None
    assert "jane.doe@acme.com" not in str(redacted)
    assert "10.0.0.5" not in str(redacted)
    assert redacted["id"] == _INVENTORY_INCIDENT


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
