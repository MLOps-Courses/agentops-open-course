"""Retrieval-quality evaluation — keyword baseline vs semantic embeddings (Ch. 3.4).

Adopt techniques with evidence: semantic retrieval is only worth enabling if it
beats the deterministic keyword scorer on this dataset. Ground truth comes from
the dataset itself — every incident names its runbook — so hit-rate@k needs no
hand labeling. The semantic side calls the local Ollama embeddings endpoint and
therefore stays outside the offline test gate (`mise run eval:retrieval`).
"""

from __future__ import annotations

import os
from pathlib import Path

import mlflow

from agent import data
from agent.memory import search_runbooks
from agent.retrieval import semantic_search

_TRACKING_URI = os.environ.get("MLFLOW_TRACKING_URI", f"sqlite:///{Path(__file__).parent / 'mlflow.db'}")
_EXPERIMENT = os.environ.get("MLFLOW_EXPERIMENT_NAME", "agentops-agent")
_K_VALUES = (1, 3)


def cases() -> list[tuple[str, str]]:
    """Query/expected-runbook pairs derived from the incident records."""
    return [(f"{incident.title}. {incident.summary}", incident.runbook) for incident in data.list_incidents()]


def keyword_slugs(query: str, k: int) -> list[str]:
    """Top-k slugs from the deterministic keyword scorer (the baseline)."""
    result = search_runbooks(query, limit=k)
    return [row["slug"] for row in result["runbooks"]]


def semantic_slugs(query: str, k: int) -> list[str]:
    """Top-k slugs from the embedding ranker (requires local Ollama)."""
    return [row["slug"] for row in semantic_search(query, limit=k)]


def hit_rate(retrieve, k: int) -> float:
    """Fraction of incidents whose runbook appears in the retriever's top-k."""
    pairs = cases()
    hits = sum(expected in retrieve(query, k) for query, expected in pairs)
    return hits / len(pairs)


def main() -> None:
    """Score both retrievers, log the comparison to MLflow, and print a verdict."""
    mlflow.set_tracking_uri(_TRACKING_URI)
    mlflow.set_experiment(_EXPERIMENT)
    metrics: dict[str, float] = {}
    for k in _K_VALUES:
        metrics[f"keyword_hit_rate_at_{k}"] = hit_rate(keyword_slugs, k)
        metrics[f"semantic_hit_rate_at_{k}"] = hit_rate(semantic_slugs, k)
    with mlflow.start_run(run_name="retrieval-eval"):
        mlflow.log_metrics(metrics)
        mlflow.set_tag("eval", "retrieval-quality")

    print(f"Retrieval quality over {len(cases())} incident queries:")  # noqa: T201 - CLI output
    for name, value in sorted(metrics.items()):
        print(f"  {name}: {value:.2f}")  # noqa: T201
    for k in _K_VALUES:
        keyword, semantic = metrics[f"keyword_hit_rate_at_{k}"], metrics[f"semantic_hit_rate_at_{k}"]
        verdict = "beats" if semantic > keyword else ("matches" if semantic == keyword else "loses to")
        print(f"  @k={k}: semantic {verdict} the keyword baseline")  # noqa: T201


if __name__ == "__main__":
    main()
