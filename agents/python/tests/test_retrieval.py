"""Unit tests for semantic retrieval (fake embedder) and the keyword fallback (Ch. 3.4)."""

import logging
import math
import sqlite3
import zlib
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing

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
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: "sha256:model-a")
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
    provenance = retrieval.index_provenance()
    assert provenance is not None
    assert provenance["format_version"] == 1
    assert provenance["embedding_model"] == settings.embedding_model
    assert provenance["model_digest"] == "sha256:model-a"
    assert provenance["dimensions"] == _DIMENSIONS
    assert provenance["chunker_version"] == "markdown-h2-h3-v1"
    assert provenance["chunk_count"] == count
    results = retrieval.semantic_search("cascade failure upstream dependency chain", limit=3)
    assert len(results) == 3
    slugs = [row["slug"] for row in results]
    assert "cascade-failure" in slugs
    assert results[0]["distance"] <= results[-1]["distance"]  # best match first
    assert all(row["content"] for row in results)


def test_provenance_is_absent_before_the_first_complete_generation() -> None:
    assert retrieval.index_provenance() is None


@pytest.mark.usefixtures("fake_embedder")
def test_provenance_is_absent_when_an_interrupted_generation_has_no_metadata_row() -> None:
    retrieval.index_runbooks()
    with closing(retrieval._connect()) as connection:  # noqa: SLF001 - interrupted generation fixture
        connection.execute("DELETE FROM runbook_index_metadata")
        connection.commit()

    assert retrieval.index_provenance() is None


@pytest.mark.usefixtures("fake_embedder")
def test_index_runbooks_rejects_an_empty_corpus(monkeypatch) -> None:
    # An empty runbook directory must fail with a contextual error, not IndexError.
    monkeypatch.setattr(retrieval.data, "list_runbook_slugs", list)
    with pytest.raises(ValueError, match="No runbook chunks to index"):
        retrieval.index_runbooks()


@pytest.mark.usefixtures("fake_embedder")
@pytest.mark.parametrize("limit", [0, -3])
def test_semantic_search_rejects_a_non_positive_limit(limit) -> None:
    with pytest.raises(ValueError, match="limit must be positive"):
        retrieval.semantic_search("anything", limit=limit)


def test_search_builds_the_index_on_first_use(fake_embedder) -> None:
    results = retrieval.semantic_search("disk almost full on the node", limit=2)
    assert len(results) == 2
    assert len(fake_embedder) >= 2  # one batch for the index, one for the query
    assert fake_embedder.count(["disk almost full on the node"]) == 1


def _index_batches(calls: list[list[str]]) -> list[list[str]]:
    return [batch for batch in calls if len(batch) > 1]


def test_corpus_edit_invalidates_the_complete_generation_once(fake_embedder) -> None:
    retrieval.semantic_search("disk pressure", limit=2)
    before = len(_index_batches(fake_embedder))
    runbook = settings.data_dir / "runbooks" / "disk-full.md"
    runbook.write_text(
        runbook.read_text(encoding="utf-8") + "\n## New check\nInspect inode pressure.\n", encoding="utf-8"
    )

    retrieval.semantic_search("inode pressure", limit=2)
    retrieval.semantic_search("inode pressure", limit=2)

    assert len(_index_batches(fake_embedder)) == before + 1


def test_model_and_artifact_digest_changes_each_invalidate_once(fake_embedder, monkeypatch) -> None:
    retrieval.semantic_search("disk pressure", limit=2)
    monkeypatch.setattr(settings, "embedding_model", "replacement-embed")
    retrieval.semantic_search("disk pressure", limit=2)
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: "sha256:model-b")
    retrieval.semantic_search("disk pressure", limit=2)
    retrieval.semantic_search("disk pressure", limit=2)

    assert len(_index_batches(fake_embedder)) == 3


def test_search_rejects_a_model_swap_during_query_embedding(monkeypatch) -> None:
    digests = iter(("sha256:model-a", "sha256:model-b"))
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: next(digests))
    monkeypatch.setattr(retrieval, "embed_texts", lambda texts: [_fake_vector(text) for text in texts])

    with pytest.raises(retrieval.EmbeddingUnavailableError, match="artifact changed"):
        retrieval.semantic_search("disk pressure", limit=2)

    assert retrieval.index_provenance() is None


@pytest.mark.usefixtures("fake_embedder")
def test_search_rejects_a_model_swap_during_corpus_embedding(monkeypatch) -> None:
    retrieval.semantic_search("disk pressure", limit=2)
    previous = retrieval.index_provenance()
    runbook = settings.data_dir / "runbooks" / "disk-full.md"
    runbook.write_text(
        runbook.read_text(encoding="utf-8") + "\n## New check\nInspect inode pressure.\n",
        encoding="utf-8",
    )
    digests = iter(("sha256:model-a", "sha256:model-a", "sha256:model-a", "sha256:model-b"))
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: next(digests))

    with pytest.raises(retrieval.EmbeddingUnavailableError, match="artifact changed"):
        retrieval.semantic_search("inode pressure", limit=2)

    assert retrieval.index_provenance() == previous


def test_dimension_and_chunker_changes_each_invalidate_once(monkeypatch) -> None:
    dimensions = _DIMENSIONS
    calls: list[list[str]] = []

    def embed(texts: list[str]) -> list[list[float]]:
        calls.append(texts)
        return [[*_fake_vector(text), *([0.0] * (dimensions - _DIMENSIONS))] for text in texts]

    monkeypatch.setattr(retrieval, "embed_texts", embed)
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: "sha256:model-a")
    retrieval.semantic_search("disk pressure", limit=2)
    dimensions += 1
    retrieval.semantic_search("disk pressure", limit=2)
    monkeypatch.setattr(retrieval, "_CHUNKER_VERSION", "markdown-h2-h3-v2")
    retrieval.semantic_search("disk pressure", limit=2)
    retrieval.semantic_search("disk pressure", limit=2)

    assert len(_index_batches(calls)) == 3


def test_concurrent_first_use_builds_one_generation(fake_embedder) -> None:
    def search(_index: int):
        return retrieval.semantic_search("disk pressure", limit=2)

    with ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(search, range(4)))

    assert all(result for result in results)
    assert len(_index_batches(fake_embedder)) == 1


@pytest.mark.usefixtures("fake_embedder")
def test_failed_rebuild_preserves_old_generation_but_does_not_present_it_as_current(monkeypatch) -> None:
    retrieval.semantic_search("disk pressure", limit=2)
    previous = retrieval.index_provenance()
    monkeypatch.setattr(settings, "embedding_model", "broken-embed")

    def fail_rebuild(texts: list[str]) -> list[list[float]]:
        if len(texts) > 1:
            raise retrieval.EmbeddingUnavailableError("rebuild failed")
        return [_fake_vector(text) for text in texts]

    monkeypatch.setattr(retrieval, "embed_texts", fail_rebuild)
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="rebuild failed"):
        retrieval.semantic_search("disk pressure", limit=2)

    assert retrieval.index_provenance() == previous
    with closing(retrieval._connect()) as connection:  # noqa: SLF001 - stale-generation contract
        assert not retrieval._index_is_ready(  # noqa: SLF001
            connection,
            retrieval._index_spec(),  # noqa: SLF001
            dimensions=_DIMENSIONS,
        )


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
    assert result["retrieval"] == "keyword"  # the fallback is visible in the result, not only the log
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


def test_embedding_request_has_a_cold_start_timeout(monkeypatch) -> None:
    class _Response:
        def raise_for_status(self) -> None:
            return

        def json(self) -> dict[str, list[list[float]]]:
            return {"embeddings": [[1.0, 0.0]]}

    seen: dict[str, float] = {}

    def post(url, *, json, timeout):
        del url, json
        seen["timeout"] = timeout
        return _Response()

    monkeypatch.setattr(retrieval.httpx, "post", post)
    monkeypatch.setattr(settings, "embedding_timeout_s", 123.0)
    assert retrieval.embed_texts(["hello"]) == [[1.0, 0.0]]
    assert seen == {"timeout": 123.0}


@pytest.mark.parametrize(
    ("embeddings", "message"),
    [
        ([], "mismatched batch"),
        ([[]], "empty or invalid vector"),
        (["not-a-vector"], "empty or invalid vector"),
        ([[math.inf]], "non-finite or non-numeric"),
        ([[True]], "non-finite or non-numeric"),
        ([["one"]], "non-finite or non-numeric"),
    ],
)
def test_embedding_response_validation_rejects_unsafe_vectors(monkeypatch, embeddings, message) -> None:
    class _Response:
        def raise_for_status(self) -> None:
            return

        def json(self) -> dict:
            return {"embeddings": embeddings}

    monkeypatch.setattr(retrieval.httpx, "post", lambda *_args, **_kwargs: _Response())
    with pytest.raises(retrieval.EmbeddingUnavailableError, match=message):
        retrieval.embed_texts(["hello"])


def test_embedding_model_digest_uses_the_resolved_ollama_artifact(monkeypatch) -> None:
    class _Response:
        def raise_for_status(self) -> None:
            return

        def json(self) -> dict[str, list[dict[str, str]]]:
            return {
                "models": [
                    {"name": "another-model:latest", "digest": "sha256:other"},
                    {"name": "nomic-embed-text:latest", "digest": "sha256:resolved"},
                ]
            }

    monkeypatch.setattr(retrieval.httpx, "get", lambda *_args, **_kwargs: _Response())
    assert retrieval.resolve_embedding_model_digest() == "sha256:resolved"


def test_embedding_model_digest_is_optional_when_metadata_is_unavailable(monkeypatch) -> None:
    class _Response:
        def __init__(self, models) -> None:
            self._models = models

        def raise_for_status(self) -> None:
            return

        def json(self) -> dict:
            return {"models": self._models}

    monkeypatch.setattr(retrieval.httpx, "get", lambda *_args, **_kwargs: _Response("invalid"))
    assert retrieval.resolve_embedding_model_digest() is None

    monkeypatch.setattr(
        retrieval.httpx,
        "get",
        lambda *_args, **_kwargs: _Response([None, {"name": "another-model:latest", "digest": "sha256:other"}]),
    )
    assert retrieval.resolve_embedding_model_digest() is None

    def unavailable(*_args, **_kwargs):
        raise retrieval.httpx.ConnectError("Ollama is unavailable")

    monkeypatch.setattr(retrieval.httpx, "get", unavailable)
    assert retrieval.resolve_embedding_model_digest() is None


def test_embedding_model_digest_rejects_an_empty_digest_for_an_exact_tag(monkeypatch) -> None:
    class _Response:
        def raise_for_status(self) -> None:
            return

        def json(self) -> dict[str, list[dict[str, str]]]:
            return {"models": [{"name": "nomic-embed-text:latest", "digest": ""}]}

    monkeypatch.setattr(settings, "embedding_model", "nomic-embed-text:latest")
    monkeypatch.setattr(retrieval.httpx, "get", lambda *_args, **_kwargs: _Response())
    assert retrieval.resolve_embedding_model_digest() is None


@pytest.mark.parametrize(
    ("vectors", "expected_count", "message"),
    [
        ([[1.0]], 2, "mismatched index batch"),
        ([], 0, "empty vector"),
        ([[1.0], [1.0, 2.0]], 2, "inconsistent vector dimensions"),
    ],
)
def test_index_vector_validation_rejects_incomplete_generations(vectors, expected_count, message) -> None:
    with pytest.raises(retrieval.EmbeddingUnavailableError, match=message):
        retrieval._validate_vectors(vectors, expected_count)  # noqa: SLF001 - index generation contract


def test_search_rejects_query_and_corpus_dimension_mismatch(monkeypatch) -> None:
    def mismatched_embedder(texts: list[str]) -> list[list[float]]:
        if len(texts) == 1:
            return [[1.0, 0.0]]
        return [_fake_vector(text) for text in texts]

    monkeypatch.setattr(retrieval, "embed_texts", mismatched_embedder)
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: "sha256:model-a")
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="different dimensions"):
        retrieval.semantic_search("disk pressure")

    assert retrieval.index_provenance() is None


def test_search_rejects_an_empty_query_vector(monkeypatch) -> None:
    monkeypatch.setattr(retrieval, "embed_texts", lambda _texts: [[]])
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="empty query vector"):
        retrieval.semantic_search("disk pressure")


def test_search_maps_sqlite_rebuild_failure_to_keyword_fallback(monkeypatch) -> None:
    monkeypatch.setattr(retrieval, "embed_texts", lambda _texts: [[1.0, 0.0]])
    monkeypatch.setattr(retrieval, "resolve_embedding_model_digest", lambda: "sha256:model-a")

    def fail_rebuild(_spec, *, dimensions=None):
        del dimensions
        raise sqlite3.OperationalError("database is locked")

    monkeypatch.setattr(retrieval, "_ensure_index", fail_rebuild)
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="rebuild failed safely"):
        retrieval.semantic_search("disk pressure")


def test_search_rebuilds_once_when_provenance_changes_between_ensure_and_read(fake_embedder, monkeypatch) -> None:
    retrieval.index_runbooks()
    original_ready = retrieval._index_is_ready  # noqa: SLF001 - controlled cross-process race
    checks = 0

    def changes_once(connection, spec, *, dimensions):
        nonlocal checks
        checks += 1
        if checks == 2:
            return False
        return original_ready(connection, spec, dimensions=dimensions)

    monkeypatch.setattr(retrieval, "_index_is_ready", changes_once)
    results = retrieval.semantic_search("disk pressure", limit=2)

    assert results
    assert checks == 4
    assert len(_index_batches(fake_embedder)) == 1


@pytest.mark.usefixtures("fake_embedder")
def test_search_maps_sqlite_read_failure_to_keyword_fallback(monkeypatch) -> None:
    retrieval.index_runbooks()
    monkeypatch.setattr(retrieval, "_ensure_index", lambda _spec, *, dimensions=None: dimensions or 0)

    def fail_read(_connection, _spec, *, dimensions):
        del dimensions
        raise sqlite3.OperationalError("database is locked")

    monkeypatch.setattr(retrieval, "_index_is_ready", fail_read)
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="read failed safely"):
        retrieval.semantic_search("disk pressure")


@pytest.mark.usefixtures("fake_embedder")
def test_search_fails_closed_when_provenance_changes_twice(monkeypatch) -> None:
    retrieval.index_runbooks()
    rebuilds = 0

    def record_rebuild(_spec, *, dimensions=None):
        nonlocal rebuilds
        del dimensions
        rebuilds += 1
        return 0

    monkeypatch.setattr(retrieval, "_ensure_index", record_rebuild)
    monkeypatch.setattr(retrieval, "_index_is_ready", lambda *_args, **_kwargs: False)
    with pytest.raises(retrieval.EmbeddingUnavailableError, match="provenance changed concurrently"):
        retrieval.semantic_search("disk pressure")

    assert rebuilds == 3
