"""Offline tests for the deterministic groundedness / citation-coverage logic."""

import pytest

from evals import groundedness_eval
from evals.groundedness_eval import claimed_entities, unsupported_claims
from tests.domain import REFERENCE_DOMAIN

_CASCADE_FAILURE_RUNBOOK = REFERENCE_DOMAIN.runbooks.cascade_failure
_CACHE = REFERENCE_DOMAIN.services.cache
_CHECKOUT_INCIDENT = REFERENCE_DOMAIN.incidents.checkout_latency
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down
_PAYMENTS = REFERENCE_DOMAIN.services.payments
_SEARCH = REFERENCE_DOMAIN.services.search


@pytest.fixture(autouse=True)
def ignore_retained_workflow_transcripts(monkeypatch) -> None:
    monkeypatch.delenv("AGENT_EVAL_OBSERVED_PATH", raising=False)


def test_claimed_entities_extracts_ids_services_and_runbooks() -> None:
    text = f"{_INVENTORY_INCIDENT} on {_PAYMENTS} is SEV1; see the {_CASCADE_FAILURE_RUNBOOK} runbook."
    assert claimed_entities(text) == {
        _INVENTORY_INCIDENT.lower(),
        "sev1",
        _PAYMENTS,
        _CASCADE_FAILURE_RUNBOOK,
    }


def test_service_terms_match_whole_tokens_only() -> None:
    # "auth" must not fire on "authored"; "cache" must not fire on "cached-out".
    assert claimed_entities("The change was authored last week") == set()


def test_ambiguous_search_verb_is_not_a_service_claim() -> None:
    assert claimed_entities("I can search the logs for more evidence.") == set()
    assert claimed_entities("I can cache the result for reuse.") == set()


def test_ambiguous_service_status_is_still_a_claim() -> None:
    assert claimed_entities("Search appears degraded.") == {_SEARCH}
    assert claimed_entities("Search has elevated errors.") == {_SEARCH}
    assert claimed_entities("Cache is operational.") == {_CACHE}


def test_ambiguous_service_is_still_checked_with_service_context() -> None:
    problems = unsupported_claims(
        [f"The {_SEARCH} service is degraded."],
        [f'{{"service": "{_INVENTORY}", "status": "healthy"}}'],
        ["What is degraded?"],
    )
    assert problems == [f"turn 1: answer claims {_SEARCH!r} with no supporting evidence"]


def test_ambiguous_service_accepts_canonical_nested_name_evidence() -> None:
    assert (
        unsupported_claims(
            ["The search service is degraded."],
            [f'{{"service": {{"name": "{_SEARCH}", "status": "degraded"}}}}'],
            ["What is degraded?"],
        )
        == []
    )


def test_grounded_answer_has_no_unsupported_claims() -> None:
    responses = [f"{_INVENTORY_INCIDENT} on {_PAYMENTS} is down."]
    evidence = [f'{{"id": "{_INVENTORY_INCIDENT}", "service": "{_PAYMENTS}", "status": "down"}}']
    questions = [f"What is happening with {_PAYMENTS}?"]
    assert unsupported_claims(responses, evidence, questions) == []


def test_entity_from_the_question_counts_as_grounded() -> None:
    # The user named the service; echoing it back is not a hallucination.
    responses = ["I could not find any incident for warehouse."]
    evidence = ["{}"]
    questions = ["What incidents affect warehouse?"]
    assert unsupported_claims(responses, evidence, questions) == []


def test_fabricated_incident_is_reported() -> None:
    responses = ["The root cause is INC-999, which I recommend resolving."]
    evidence = [f'{{"id": "{_INVENTORY_INCIDENT}"}}']
    questions = [f"Investigate {_INVENTORY_INCIDENT}."]
    problems = unsupported_claims(responses, evidence, questions)
    assert len(problems) == 1
    assert "inc-999" in problems[0]


def test_per_turn_grounding_is_independent() -> None:
    responses = [f"{_CHECKOUT_INCIDENT} is open.", f"{_INVENTORY_INCIDENT} is resolved."]
    evidence = [f'{{"id": "{_CHECKOUT_INCIDENT}"}}', "{}"]  # turn 2 never retrieved the second incident
    questions = ["First?", "Second?"]
    problems = unsupported_claims(responses, evidence, questions)
    assert problems == [f"turn 2: answer claims {_INVENTORY_INCIDENT.lower()!r} with no supporting evidence"]


def test_measure_retains_the_transcript_needed_to_audit_a_failure(monkeypatch) -> None:
    monkeypatch.setattr(
        groundedness_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "fabricated", "turns": [f"Investigate {_INVENTORY_INCIDENT}."]}}],
    )
    monkeypatch.setattr(
        groundedness_eval,
        "ask",
        lambda _turns, _eval_id: {
            "responses": ["INC-999 caused it."],
            "evidence": [f'{{"id": "{_INVENTORY_INCIDENT}"}}'],
            "provider_errors": [[]],
        },
    )
    observed = groundedness_eval.measure()["fabricated"]
    assert observed["questions"] == [f"Investigate {_INVENTORY_INCIDENT}."]
    assert observed["responses"] == ["INC-999 caused it."]
    assert observed["evidence"] == [f'{{"id": "{_INVENTORY_INCIDENT}"}}']
    assert observed["provider_errors"] == []
    assert observed["unsupported_claims"] == ["turn 1: answer claims 'inc-999' with no supporting evidence"]


def test_measure_retains_provider_failure_instead_of_reporting_vacuous_grounding(monkeypatch) -> None:
    monkeypatch.setattr(
        groundedness_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "degraded", "turns": [f"Investigate {_INVENTORY_INCIDENT}."]}}],
    )
    monkeypatch.setattr(
        groundedness_eval,
        "ask",
        lambda _turns, _eval_id: {
            "responses": ["The model provider is unavailable."],
            "evidence": [""],
            "provider_errors": [[{"code": "MODEL_UNAVAILABLE", "message": "Model request failed safely."}]],
        },
    )

    observed = groundedness_eval.measure()["degraded"]
    assert observed["provider_errors"] == ["turn 1: MODEL_UNAVAILABLE: Model request failed safely."]
    assert observed["unsupported_claims"] == []


def test_measure_reuses_the_exact_mlflow_transcript_when_configured(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_EVAL_OBSERVED_PATH", "evals/model-observed.json")
    monkeypatch.setenv("EVAL_MODEL_DIGEST", "sha256:canonical")
    monkeypatch.setattr(
        groundedness_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": [f"Investigate {_INVENTORY_INCIDENT}."]}}],
    )

    def load(path, *, expected_cases, model_digest):
        assert str(path) == "evals/model-observed.json"
        assert expected_cases == [{"inputs": {"eval_id": "lookup", "turns": [f"Investigate {_INVENTORY_INCIDENT}."]}}]
        assert model_digest == "sha256:canonical"
        return {
            "lookup": {
                "responses": [f"{_INVENTORY_INCIDENT} is open."],
                "evidence": [f'{{"id": "{_INVENTORY_INCIDENT}"}}'],
                "provider_errors": [[]],
            }
        }

    monkeypatch.setattr(groundedness_eval, "load_model_observations", load)
    monkeypatch.setattr(
        groundedness_eval,
        "ask",
        lambda *_args: pytest.fail("retained evidence must avoid a new model call"),
    )

    observed = groundedness_eval.measure()["lookup"]
    assert observed["responses"] == [f"{_INVENTORY_INCIDENT} is open."]
    assert observed["unsupported_claims"] == []


def test_main_module_exposes_measure_and_main() -> None:
    # measure()/main() are model-backed (weekly lane); assert they are importable callables.
    assert callable(groundedness_eval.measure)
    assert callable(groundedness_eval.main)
