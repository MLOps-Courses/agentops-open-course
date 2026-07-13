"""Knowledge tools over the runbook library — the Ops Copilot's memory/RAG (Chapter 3.4).

The runbooks in ``agents/data/runbooks`` are the agent's long-term knowledge. ``get_runbook``
fetches one by its exact slug (an incident row carries its ``runbook`` slug); ``search_runbooks``
does a simple, deterministic keyword search when the slug is not known. Keyword retrieval keeps
the course fully offline — swap in a vector store or ADK ``MemoryService`` for semantic search.
"""

from __future__ import annotations

import logging
import re
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from google.adk.agents.llm_agent import ToolUnion

from . import data
from .config import settings
from .models import normalize_slug
from .resilience import with_resilience

logger = logging.getLogger(__name__)

# Terms shorter than this carry little retrieval signal, so drop them (a length filter, not a stop-list).
_MIN_TERM_LENGTH = 3


def _terms(query: str) -> list[str]:
    """Split a query into lowercase alphanumeric search terms, dropping very short ones."""
    return [term for term in re.findall(r"[a-z0-9]+", query.lower()) if len(term) >= _MIN_TERM_LENGTH]


def get_runbook(slug: str) -> dict[str, Any]:
    """Fetch a runbook by its exact slug (e.g. an incident's ``runbook`` field).

    Args:
        slug: The runbook identifier, e.g. ``high-latency`` or ``service-down``.

    Returns:
        A dict with the ``slug`` and its markdown ``content``, or an ``error`` if unknown.
    """
    # Parse at the boundary: normalize once here, then work only with the trusted
    # value — read, error message, and result all speak the normalized slug.
    normalized = normalize_slug(slug)
    if normalized is None:
        known = ", ".join(data.list_runbook_slugs())
        return {"error": f"Invalid runbook slug {slug!r}. Available runbooks: {known}."}
    content = data.read_runbook(normalized)
    if content is None:
        known = ", ".join(data.list_runbook_slugs())
        return {"error": f"No runbook named {normalized!r}. Available runbooks: {known}."}
    return {"slug": normalized, "content": content}


def search_runbooks(query: str, limit: int = 3) -> dict[str, Any]:
    """Search the runbook knowledge base for guidance relevant to a free-text query.

    Uses TF-IDF-style scoring by default: a term counts more when it is rare across
    runbooks (so ubiquitous words like "service" don't dominate), and a term matching
    a runbook's slug gets a strong boost. Deterministic and offline. With
    ``AGENT_SEMANTIC_RETRIEVAL=true`` it ranks by local-embedding similarity instead,
    falling back to this keyword scorer when the embedding model is unavailable.

    Args:
        query: What you are trying to resolve, e.g. ``database connection pool exhausted``.
        limit: The maximum number of runbooks to return (most relevant first).

    Returns:
        A dict with ``count`` and a ``runbooks`` list (slug + markdown content), best match first.
    """
    if limit <= 0:  # a non-positive limit falls back to the default
        limit = 3
    if settings.semantic_retrieval:
        # Imported lazily so the default offline path never touches the vector stack.
        from .retrieval import EmbeddingUnavailableError, semantic_search

        try:
            matches = semantic_search(query, limit)
            return {
                "count": len(matches),
                "retrieval": "semantic",
                "runbooks": [{"slug": row["slug"], "content": row["content"]} for row in matches],
            }
        except EmbeddingUnavailableError as error:
            # An explicit, logged fallback — never a silent degradation.
            logger.warning("Semantic retrieval unavailable, falling back to keywords: %s", error)
    terms = _terms(query)
    contents = {slug: (data.read_runbook(slug) or "") for slug in data.list_runbook_slugs()}
    total = len(contents) or 1
    # Document frequency: how many runbooks contain each distinct term.
    doc_freq = {term: sum(term in text.lower() for text in contents.values()) for term in set(terms)}

    scored: list[tuple[float, str, str]] = []
    for slug, content in contents.items():
        haystack = content.lower()
        score = 0.0
        for term in terms:
            frequency = doc_freq[term]
            if frequency:
                score += haystack.count(term) * (total / frequency)  # rarer term → higher weight
            if term in slug:
                score += total * 5  # the slug names the failure mode: a strong signal
        if score > 0:
            scored.append((score, slug, content))
    # Score descending, then slug ascending — the slug tie-break keeps ordering deterministic
    # for equal scores, which matters for reproducible evals.
    scored.sort(key=lambda row: (-row[0], row[1]))
    top = scored[:limit]
    return {"count": len(top), "runbooks": [{"slug": slug, "content": content} for _, slug, content in top]}


# The knowledge tools registered on the Ops Copilot (Ch. 3.4), wrapped with the
# same deadline/retry policy as the read tools — runbook reads are idempotent.
KNOWLEDGE_TOOLS: list[ToolUnion] = [with_resilience(get_runbook), with_resilience(search_runbooks)]
