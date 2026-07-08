"""Unit tests for the Presidio PII-redaction guardrail (Ch. 4.5).

These exercise the pure ``redact_pii`` helper, so they run fully offline (no model, no key).
The first call loads the small spaCy model once; subsequent calls are cached.
"""

from agent import pii


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
