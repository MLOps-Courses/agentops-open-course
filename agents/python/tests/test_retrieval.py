"""Unit tests for semantic retrieval (fake embedder) and the keyword fallback (Ch. 3.4)."""

import logging
import zlib

import pytest

from agent import memory, retrieval
from agent.config import settings

# A deterministic fake embedder: each vector is a bag-of-characters projection,
# so texts sharing words land near each other — enough to test ranking plumbing.
_DIMENSIONS = 32


def _fake_vector(text: str) -> list[float]:
    vector = [0.0] * _DIMENSIONS
    for token in text.lower().split():
        vector[zlib.crc32(token.encode()) % _DIMENSIONS] += 1.0
    norm = sum(value * value for value in vector) ** 0.5 or 1.0
    return [value / norm for value in vector]


@pytest.fixture
def fake_embedder(monkeypatch):
    calls: list[list[str]] = []

    def embed(texts: list[str]) -> list[list[float]]:
        calls.append(texts)
        return [_fake_vector(text) for text in texts]

    monkeypatch.setattr(retrieval, "embed_texts", embed)
    return calls


def test_chunking_splits_on_headings_and_keeps_the_slug() -> None:
    content = "# Title\nintro\n\n## Symptoms\nslow queries\n\n### Fix\nrestart the pool"
    chunks = retrieval.chunk_runbook("high-latency", content)
    assert len(chunks) == 3
    assert all(chunk.startswith("high-latency: ") for chunk in chunks)


def test_chunking_never_returns_empty() -> None:
    assert retrieval.chunk_runbook("empty", "   ") == ["empty: "]


@pytest.mark.usefixtures("fake_embedder")
def test_index_and_search_rank_by_similarity() -> None:
    count = retrieval.index_runbooks()
    assert count > 0
    results = retrieval.semantic_search("cascade failure upstream dependency chain", limit=3)
    assert len(results) == 3
    slugs = [row["slug"] for row in results]
    assert "cascade-failure" in slugs
    assert results[0]["distance"] <= results[-1]["distance"]  # best match first
    assert all(row["content"] for row in results)


def test_search_builds_the_index_on_first_use(fake_embedder) -> None:
    results = retrieval.semantic_search("disk almost full on the node", limit=2)
    assert len(results) == 2
    assert len(fake_embedder) >= 2  # one batch for the index, one for the query


@pytest.mark.usefixtures("fake_embedder")
def test_search_runbooks_uses_semantic_when_enabled(monkeypatch) -> None:
    monkeypatch.setattr(settings, "semantic_retrieval", True)
    result = memory.search_runbooks("cache eviction storm memory pressure")
    assert result["retrieval"] == "semantic"
    assert result["count"] > 0


def test_search_runbooks_falls_back_to_keywords_with_a_log(monkeypatch, caplog) -> None:
    monkeypatch.setattr(settings, "semantic_retrieval", True)

    def unavailable(_texts):
        raise retrieval.EmbeddingUnavailableError("Ollama is down")

    monkeypatch.setattr(retrieval, "embed_texts", unavailable)
    with caplog.at_level(logging.WARNING):
        result = memory.search_runbooks("database connection pool exhausted")
    assert "retrieval" not in result  # the keyword result shape is unchanged
    assert result["count"] > 0
    assert any("falling back to keywords" in message for message in caplog.messages)


def test_offline_default_never_touches_the_vector_stack(monkeypatch) -> None:
    monkeypatch.setattr(settings, "semantic_retrieval", False)

    def explode(_texts):  # would fail loudly if the offline path imported/used it
        raise AssertionError("embed_texts must not be called when the flag is off")

    monkeypatch.setattr(retrieval, "embed_texts", explode)
    result = memory.search_runbooks("high latency after deploy")
    assert result["count"] > 0


def test_embedding_error_is_actionable(monkeypatch) -> None:
    monkeypatch.setattr(settings, "embeddings_url", "http://127.0.0.1:1")  # nothing listens
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="ollama pull nomic-embed-text"):
        retrieval.embed_texts(["hello"])
