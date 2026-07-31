"""Typed vocabulary for the reference operations domain.

The application and its deterministic evaluators share this single owner for
seed-specific identifiers. Portable behavior should consume these values while
behavior-specific negative examples remain explicit at their contract site.
"""

from dataclasses import astuple, dataclass


@dataclass(frozen=True, slots=True)
class IncidentVocabulary:
    """Incident identifiers shipped in the immutable reference seed."""

    checkout_latency: str
    inventory_down: str
    payments_errors: str
    auth_errors: str
    search_latency: str
    checkout_disk: str
    cache_memory: str
    database_cascade: str
    checkout_cascade: str
    gateway_memory: str

    def values(self) -> tuple[str, ...]:
        """Return identifiers in stable field order."""
        return astuple(self)


@dataclass(frozen=True, slots=True)
class ServiceVocabulary:
    """Service names shipped in the immutable reference seed."""

    checkout: str
    payments: str
    auth: str
    search: str
    inventory: str
    database: str
    cache: str
    gateway: str

    def values(self) -> tuple[str, ...]:
        """Return service names in stable field order."""
        return astuple(self)


@dataclass(frozen=True, slots=True)
class RunbookVocabulary:
    """Runbook slugs shipped with the reference agent."""

    cascade_failure: str
    deployment_rollback: str
    disk_full: str
    elevated_errors: str
    high_latency: str
    memory_leak: str
    service_down: str

    def values(self) -> tuple[str, ...]:
        """Return runbook slugs in stable field order."""
        return astuple(self)


@dataclass(frozen=True, slots=True)
class DomainVocabulary:
    """Domain-owned identifiers and the service dependency graph."""

    incidents: IncidentVocabulary
    services: ServiceVocabulary
    runbooks: RunbookVocabulary
    dependency_edges: tuple[tuple[str, str], ...]


REFERENCE_DOMAIN = DomainVocabulary(
    incidents=IncidentVocabulary(
        checkout_latency="INC-001",
        inventory_down="INC-002",
        payments_errors="INC-003",
        auth_errors="INC-004",
        search_latency="INC-005",
        checkout_disk="INC-006",
        cache_memory="INC-007",
        database_cascade="INC-008",
        checkout_cascade="INC-009",
        gateway_memory="INC-010",
    ),
    services=ServiceVocabulary(
        checkout="checkout",
        payments="payments",
        auth="auth",
        search="search",
        inventory="inventory",
        database="database",
        cache="cache",
        gateway="api-gateway",
    ),
    runbooks=RunbookVocabulary(
        cascade_failure="cascade-failure",
        deployment_rollback="deployment-rollback",
        disk_full="disk-full",
        elevated_errors="elevated-errors",
        high_latency="high-latency",
        memory_leak="memory-leak",
        service_down="service-down",
    ),
    dependency_edges=(
        ("cache", "database"),
        ("database", "checkout"),
        ("inventory", "checkout"),
    ),
)

__all__ = [
    "REFERENCE_DOMAIN",
    "DomainVocabulary",
    "IncidentVocabulary",
    "RunbookVocabulary",
    "ServiceVocabulary",
]
