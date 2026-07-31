"""Proof and ratchet for the reference agent's domain-portability seam."""

import ast
import re
from collections import Counter
from dataclasses import replace
from pathlib import Path
from typing import Any, TypedDict

import pytest
from google.genai import types

from agent import data, memory, pii, tools
from agent.config import settings
from agent.mcp_client import MCP_READ_TOOL_NAMES
from agent.models import Severity, TriageReport
from agent.server import agent_card
from evals import mlflow_eval
from evals.required_trajectory import required_tools_in_order

from .domain import PIVOT_DOMAIN, REFERENCE_DOMAIN, DomainVocabulary, adapt_dataset


class ToolCall(TypedDict):
    """Typed evaluator fixture at the external JSON boundary."""

    name: str
    args: dict[str, Any]


def _belongs_to_assertion(node: ast.AST, parents: dict[ast.AST, ast.AST]) -> bool:
    """Return whether a literal is part of a direct behavioral assertion."""
    current = parents.get(node)
    while current is not None:
        if isinstance(current, ast.Assert):
            return True
        current = parents.get(current)
    return False


def test_pivot_domain_uses_one_adapter_surface_for_reads_writes_and_reports() -> None:
    adapt_dataset(settings.data_dir, REFERENCE_DOMAIN, PIVOT_DOMAIN)

    incident_id = PIVOT_DOMAIN.incidents.inventory_down
    service = PIVOT_DOMAIN.services.inventory
    runbook = PIVOT_DOMAIN.runbooks.service_down
    incident = tools.get_incident(incident_id)["incident"]
    assert incident["service"] == service
    assert incident["runbook"] == runbook
    assert memory.get_runbook(runbook)["slug"] == runbook

    audit = data.restart_service_with_audit(
        service,
        actor="portability-test",
        approved_by="reviewer",
        rationale="prove the domain adapter",
        session_id="pivot-session",
        invocation_id="pivot-invocation",
    )
    assert audit is not None
    assert audit.target == service
    report = TriageReport(
        incident_id=incident_id,
        severity=Severity.SEV1,
        affected_services=[service, PIVOT_DOMAIN.services.checkout],
        hypothesis="The catalog service is unavailable.",
        evidence=["The pivot fixture returned its linked incident and runbook."],
        recommended_runbook=runbook,
    )
    assert report.incident_id == incident_id


def test_domain_adapter_rejects_source_and_target_vocabulary_collisions() -> None:
    """A dict-based adapter must fail before duplicate vocabulary can overwrite a mapping."""
    colliding_reference = replace(
        REFERENCE_DOMAIN,
        services=replace(
            REFERENCE_DOMAIN.services,
            payments=REFERENCE_DOMAIN.services.checkout,
        ),
    )
    colliding_pivot = replace(
        PIVOT_DOMAIN,
        services=replace(
            PIVOT_DOMAIN.services,
            payments=PIVOT_DOMAIN.services.checkout,
        ),
    )

    with pytest.raises(ValueError, match=r"source domain vocabulary contains duplicate values: checkout"):
        adapt_dataset(settings.data_dir, colliding_reference, PIVOT_DOMAIN)
    with pytest.raises(ValueError, match=r"target domain vocabulary contains duplicate values: orders"):
        adapt_dataset(settings.data_dir, REFERENCE_DOMAIN, colliding_pivot)


@pytest.mark.parametrize(
    ("domain", "pivot"),
    [(REFERENCE_DOMAIN, False), (PIVOT_DOMAIN, True)],
    ids=("reference-domain", "pivot-domain"),
)
def test_both_domains_preserve_protocol_policy_and_evaluation_contracts(
    domain: DomainVocabulary,
    *,
    pivot: bool,
) -> None:
    """Exercise the reusable boundary without maintaining a second application."""
    if pivot:
        adapt_dataset(settings.data_dir, REFERENCE_DOMAIN, domain)

    incident_id = domain.incidents.inventory_down
    service = domain.services.inventory
    runbook = domain.runbooks.service_down
    incident = tools.get_incident(incident_id)["incident"]
    dependency_incidents = (
        (domain.incidents.cache_memory, domain.incidents.database_cascade),
        (domain.incidents.database_cascade, domain.incidents.checkout_cascade),
        (domain.incidents.inventory_down, domain.incidents.checkout_latency),
    )
    observed_dependency_edges = tuple(
        tuple(tools.get_incident(dependency_incident)["incident"]["service"] for dependency_incident in edge)
        for edge in dependency_incidents
    )
    assert observed_dependency_edges == domain.dependency_edges

    # Protocol: the same typed record crosses the read surface while MCP and A2A
    # retain their stable, domain-independent contracts.
    assert incident == {
        **incident,
        "id": incident_id,
        "service": service,
        "runbook": runbook,
    }
    assert len(MCP_READ_TOOL_NAMES) == 6
    assert [interface.protocol_binding for interface in agent_card.supported_interfaces] == ["JSONRPC"]

    # Policy: operational identifiers remain useful while personal data is
    # removed, and the evaluation policy admits only the expected guarded write.
    redacted = pii.redact_pii(f"Email jane.doe@example.com about {incident_id} on {service}.")
    assert "jane.doe@example.com" not in redacted
    assert incident_id in redacted
    expected_tools: list[ToolCall] = [
        {"name": "get_incident", "args": {"incident_id": incident_id}},
        {"name": "get_runbook", "args": {"slug": runbook}},
        {"name": "restart_service", "args": {"name": service}},
    ]
    actual_tools: list[ToolCall] = [
        {"name": "get_incident", "args": {"incident_id": incident_id, "detail": True}},
        {"name": "get_service_status", "args": {"name": service}},
        {"name": "get_runbook", "args": {"slug": runbook}},
        {"name": "restart_service", "args": {"name": service}},
    ]
    expectations = {
        "expected_responses": [f"{incident_id} reports {service} down; use {runbook}."],
        "expected_tools": [expected_tools],
        "response_contracts": [
            {
                "required_terms": [incident_id.lower(), service, "down", runbook],
                "absent_entities": [],
                "negated_terms": [],
                "claims": [],
            }
        ],
    }
    outputs = {
        "responses": [f"{incident_id} reports {service} down; use {runbook}."],
        "tools": [actual_tools],
    }
    assert mlflow_eval.tool_trajectory(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.response_facts(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.tool_policy(outputs=outputs, expectations=expectations) is True

    actual_calls = [types.FunctionCall(name=call["name"], args=call["args"]) for call in actual_tools]
    required_calls = [types.FunctionCall(name=call["name"], args=call["args"]) for call in expected_tools]
    assert required_tools_in_order(actual_calls, required_calls)


def test_domain_literals_cannot_spread_outside_the_ratcheted_contract() -> None:
    """Report reference vocabulary growth without treating examples as generic setup."""
    python_root = Path(__file__).parents[1]
    excluded = {
        Path("src/agent/domain.py"),
        Path("tests/domain.py"),
        Path("tests/test_domain_portability.py"),
    }
    # Legacy behavior-specific assertions retain visible domain examples under
    # exact per-file budgets. Omitted files have a zero budget.
    # Generic service names such as "database", "inventory", and "search" also
    # describe implementation concepts, so count them only as complete string
    # literals. Distinctive incident IDs, runbook slugs, and "api-gateway" are
    # safe to count wherever they appear inside a string.
    maximum_occurrences = {
        Path("src/agent/actions.py"): 2,
        Path("src/agent/data.py"): 1,
        Path("src/agent/guardrails.py"): 1,
        Path("src/agent/longterm.py"): 4,
        Path("src/agent/memory.py"): 2,
        Path("src/agent/models.py"): 1,
        Path("src/agent/pii.py"): 1,
        Path("src/agent/server.py"): 1,
        Path("src/agent/tools.py"): 2,
        Path("tests/test_compaction.py"): 2,
        Path("tests/test_evalset.py"): 12,
        Path("tests/test_governance.py"): 2,
        Path("tests/test_memory.py"): 4,
        Path("tests/test_report.py"): 8,
        Path("tests/test_retrieval.py"): 5,
        Path("tests/test_security.py"): 9,
        Path("tests/test_server.py"): 6,
        Path("tests/test_smoke.py"): 1,
    }
    # These files have completed the setup migration. Domain terms remain
    # welcome inside direct assertions when they make the contract clearer,
    # but every fixture, decorator, and evaluator constant must use the typed
    # vocabulary seam. This catches generic service names inside larger strings,
    # which the legacy whole-file budget deliberately cannot do safely.
    setup_literal_free = {
        Path("evals/mlflow_eval.py"),
        Path("tests/test_actions.py"),
        Path("tests/test_data.py"),
        Path("tests/test_groundedness_eval.py"),
        Path("tests/test_longterm.py"),
        Path("tests/test_mlflow_eval.py"),
        Path("tests/test_pii.py"),
        Path("tests/test_required_trajectory.py"),
        Path("tests/test_tools.py"),
    }
    vocabulary_terms = (
        *REFERENCE_DOMAIN.incidents.values(),
        *REFERENCE_DOMAIN.services.values(),
        *REFERENCE_DOMAIN.runbooks.values(),
    )
    collisions = sorted(term for term, count in Counter(vocabulary_terms).items() if count > 1)
    assert not collisions, f"reference vocabulary contains duplicate values: {collisions}"
    distinctive_terms = (
        *REFERENCE_DOMAIN.incidents.values(),
        *REFERENCE_DOMAIN.runbooks.values(),
        REFERENCE_DOMAIN.services.gateway,
    )
    generic_service_terms = tuple(
        service for service in REFERENCE_DOMAIN.services.values() if service != REFERENCE_DOMAIN.services.gateway
    )
    generic_service_terms_casefold = frozenset(service.casefold() for service in generic_service_terms)
    violations: list[str] = []
    candidates = (
        *python_root.joinpath("src", "agent").rglob("*.py"),
        *python_root.joinpath("evals").rglob("*.py"),
        *python_root.joinpath("tests").glob("*.py"),
    )
    for path in candidates:
        relative = path.relative_to(python_root)
        if relative in excluded:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(relative))
        string_nodes = [
            (node, node.value)
            for node in ast.walk(tree)
            if isinstance(node, ast.Constant) and isinstance(node.value, str)
        ]
        if relative in setup_literal_free:
            parents = {child: parent for parent in ast.walk(tree) for child in ast.iter_child_nodes(parent)}
            docstrings = {
                node.body[0].value
                for node in ast.walk(tree)
                if isinstance(node, (ast.Module, ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef))
                and node.body
                and isinstance(node.body[0], ast.Expr)
                and isinstance(node.body[0].value, ast.Constant)
                and isinstance(node.body[0].value.value, str)
            }

            setup_constants = [
                value
                for node, value in string_nodes
                if node not in docstrings and not _belongs_to_assertion(node, parents)
            ]
            setup_occurrences = sum(
                len(
                    re.findall(
                        rf"(?<![A-Za-z0-9-]){re.escape(term)}(?![A-Za-z0-9-])",
                        value,
                        re.IGNORECASE,
                    )
                )
                for value in setup_constants
                for term in distinctive_terms
            )
            setup_occurrences += sum(value.casefold() in generic_service_terms_casefold for value in setup_constants)
            if setup_occurrences:
                violations.append(f"{relative}: {setup_occurrences} domain terms remain in setup; expected 0")
            continue

        constants = [value for _, value in string_nodes]
        observed = sum(
            len(re.findall(rf"(?<![A-Za-z0-9-]){re.escape(term)}(?![A-Za-z0-9-])", value))
            for value in constants
            for term in distinctive_terms
        )
        observed += sum(value in generic_service_terms for value in constants)
        maximum = maximum_occurrences.get(relative, 0)
        if observed > maximum:
            violations.append(f"{relative}: {observed} domain terms > ratchet {maximum}")

    # Eval sets intentionally embed the domain prompts, trajectories, and
    # expected responses. Naming every owned artifact keeps that necessary
    # coupling visible and makes a new eval set or vocabulary expansion fail
    # until the learner reviews it.
    evalset_maximum_occurrences = {
        Path("evals/ops.evalset.json"): 88,
        Path("evals/triage-report.evalset.json"): 10,
        Path("evals/workflow.evalset.json"): 9,
    }
    discovered_evalsets = {
        path.relative_to(python_root) for path in python_root.joinpath("evals").glob("*.evalset.json")
    }
    declared_evalsets = set(evalset_maximum_occurrences)
    if discovered_evalsets != declared_evalsets:
        missing = sorted(discovered_evalsets - declared_evalsets)
        stale = sorted(declared_evalsets - discovered_evalsets)
        violations.append(f"eval-set contract inventory changed: undeclared={missing}, missing={stale}")
    for relative, maximum in evalset_maximum_occurrences.items():
        content = python_root.joinpath(relative).read_text(encoding="utf-8")
        observed = sum(
            len(re.findall(rf"(?<![A-Za-z0-9-]){re.escape(term)}(?![A-Za-z0-9-])", content))
            for term in {*distinctive_terms, *generic_service_terms}
        )
        if observed > maximum:
            violations.append(f"{relative}: {observed} domain terms > ratchet {maximum}")

    assert not violations, "\n".join(violations)
