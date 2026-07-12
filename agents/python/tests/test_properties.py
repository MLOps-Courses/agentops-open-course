"""Property-based tests (Hypothesis) for security-critical input boundaries (Ch. 4.2).

Model-generated tool arguments are fuzzed input by nature. These properties
explore the input space (unicode, control characters, pathological lengths)
far beyond hand-picked examples. The Hypothesis profile is deterministic
(``derandomize=True``) so the offline gate stays reproducible across machines.
"""

import re

from hypothesis import given
from hypothesis import settings as hypothesis_settings
from hypothesis import strategies as st

from agent.guardrails import neutralize_injections
from agent.models import normalize_incident_id, normalize_slug
from agent.pii import redact_pii

# Deterministic profile: same examples on every machine and every run — the
# course's offline gate must never flake (see pyproject/pytest determinism).
# deadline=None: Presidio's first call loads the spaCy model (slow but deterministic).
hypothesis_settings.register_profile("course", derandomize=True, database=None, max_examples=200, deadline=None)
hypothesis_settings.load_profile("course")

_INCIDENT_ID = re.compile(r"^INC-\d+$")
_SLUG = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")


@given(st.text(max_size=200))
def test_incident_id_normalization_has_no_third_state(value: str) -> None:
    """Output either matches the strict pattern or the input is rejected — never anything else."""
    normalized = normalize_incident_id(value)
    assert normalized is None or _INCIDENT_ID.fullmatch(normalized)


@given(st.text(max_size=200))
def test_slug_normalization_has_no_third_state(value: str) -> None:
    normalized = normalize_slug(value)
    assert normalized is None or _SLUG.fullmatch(normalized)


@given(st.text(max_size=200))
def test_normalizers_are_idempotent(value: str) -> None:
    for normalize in (normalize_incident_id, normalize_slug):
        once = normalize(value)
        if once is not None:
            assert normalize(once) == once


@given(st.text(max_size=200))
def test_no_traversal_or_sql_metacharacters_survive(value: str) -> None:
    """Accepted identifiers reach the filesystem and SQL layers: keep them inert."""
    for normalize in (normalize_incident_id, normalize_slug):
        normalized = normalize(value)
        if normalized is not None:
            assert not any(dangerous in normalized for dangerous in ("/", "\\", "..", ";", "'", '"', " ", "\x00"))


# A pool of PII the anonymizer's recognizers detect deterministically (regex-backed
# entities: email, IP, phone). Name recognition is model-based and not asserted here.
_PII_VALUES = st.sampled_from(["jane.doe@acme.com", "10.20.30.40", "+1-202-555-0143"])


@given(prefix=st.text(alphabet=st.characters(codec="ascii"), max_size=40), pii=_PII_VALUES)
def test_redaction_removes_seeded_pii(prefix: str, pii: str) -> None:
    text = f"{prefix} contact {pii} for details"
    assert pii not in redact_pii(text)


@given(prefix=st.text(alphabet=st.characters(codec="ascii"), max_size=40), pii=_PII_VALUES)
def test_redaction_is_stable_across_calls(prefix: str, pii: str) -> None:
    text = f"{prefix} contact {pii} for details"
    assert redact_pii(text) == redact_pii(text)


@given(st.text(max_size=300))
def test_injection_neutralization_never_raises_and_reports_hits(value: str) -> None:
    neutralized, hits = neutralize_injections(value)
    assert hits >= 0
    if hits:
        assert "[neutralized-injection]" in neutralized
