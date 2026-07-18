"""Offline tests for the deterministic groundedness / citation-coverage logic."""

from evals import groundedness_eval
from evals.groundedness_eval import claimed_entities, unsupported_claims


def test_claimed_entities_extracts_ids_services_and_runbooks() -> None:
    text = "INC-002 on payments is SEV1; see the cascade-failure runbook."
    assert claimed_entities(text) == {"inc-002", "sev1", "payments", "cascade-failure"}


def test_service_terms_match_whole_tokens_only() -> None:
    # "auth" must not fire on "authored"; "cache" must not fire on "cached-out".
    assert claimed_entities("The change was authored last week") == set()


def test_grounded_answer_has_no_unsupported_claims() -> None:
    responses = ["INC-002 on payments is down."]
    evidence = ['{"id": "INC-002", "service": "payments", "status": "down"}']
    questions = ["What is happening with payments?"]
    assert unsupported_claims(responses, evidence, questions) == []


def test_entity_from_the_question_counts_as_grounded() -> None:
    # The user named the service; echoing it back is not a hallucination.
    responses = ["I could not find any incident for warehouse."]
    evidence = ["{}"]
    questions = ["What incidents affect warehouse?"]
    assert unsupported_claims(responses, evidence, questions) == []


def test_fabricated_incident_is_reported() -> None:
    responses = ["The root cause is INC-999, which I recommend resolving."]
    evidence = ['{"id": "INC-002"}']
    questions = ["Investigate INC-002."]
    problems = unsupported_claims(responses, evidence, questions)
    assert len(problems) == 1
    assert "inc-999" in problems[0]


def test_per_turn_grounding_is_independent() -> None:
    responses = ["INC-001 is open.", "INC-002 is resolved."]
    evidence = ['{"id": "INC-001"}', "{}"]  # turn 2 never retrieved INC-002
    questions = ["First?", "Second?"]
    problems = unsupported_claims(responses, evidence, questions)
    assert problems == ["turn 2: answer claims 'inc-002' with no supporting evidence"]


def test_main_module_exposes_measure_and_main() -> None:
    # measure()/main() are model-backed (weekly lane); assert they are importable callables.
    assert callable(groundedness_eval.measure)
    assert callable(groundedness_eval.main)
