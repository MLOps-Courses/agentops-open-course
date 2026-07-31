"""Provenance-bound semantic runbook retrieval with a keyword fallback.

``sqlite-vec`` keeps local embeddings in disposable SQLite state. Every search
binds vectors to the normalized corpus, embedding model/artifact, dimensions,
and chunker version; a stale or failed generation is never queried.
"""

from __future__ import annotations

import hashlib
import json
import math
import re
import sqlite3
import struct
from contextlib import closing
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Final

import httpx
import sqlite_vec

from . import data
from .config import settings

_HEADING = re.compile(r"^#{2,3}\s", re.MULTILINE)
_INDEX_FORMAT_VERSION: Final = 1
_CHUNKER_VERSION: Final = "markdown-h2-h3-v1"
_METADATA_TABLE = "runbook_index_metadata"


class EmbeddingUnavailableError(RuntimeError):
    """The embeddings endpoint or compatible vector generation is unavailable."""


@dataclass(frozen=True, slots=True)
class _IndexSpec:
    corpus_sha256: str
    embedding_model: str
    model_digest: str
    chunker_version: str
    chunks: tuple[tuple[str, str], ...]


@dataclass(frozen=True, slots=True)
class _EmbeddingBatch:
    """Vectors bound to the stable model artifact observed around one request."""

    vectors: list[list[float]]
    model_digest: str


def _normalize_runbook(content: str) -> str:
    """Normalize platform line endings and trailing whitespace for provenance."""
    lines = content.replace("\r\n", "\n").replace("\r", "\n").splitlines()
    return "\n".join(line.rstrip() for line in lines).strip() + "\n"


def chunk_runbook(slug: str, content: str) -> list[str]:
    """Split one runbook into heading-bounded chunks, each prefixed with its slug."""
    pieces = [piece.strip() for piece in _HEADING.split(content) if piece.strip()]
    return [f"{slug}: {piece}" for piece in pieces] or [f"{slug}: {content.strip()}"]


def embed_texts(texts: list[str]) -> list[list[float]]:
    """Embed texts through the local Ollama endpoint (or a compatible stand-in)."""
    try:
        response = httpx.post(
            f"{settings.embeddings_url}/api/embed",
            json={"model": settings.embedding_model, "input": texts},
            timeout=settings.embedding_timeout_s,
        )
        response.raise_for_status()
        payload = response.json()
        embeddings = payload["embeddings"]
    except (httpx.HTTPError, KeyError, TypeError, ValueError) as error:
        raise EmbeddingUnavailableError(
            f"Embeddings unavailable at {settings.embeddings_url} with model {settings.embedding_model!r}; "
            "start Ollama and `ollama pull nomic-embed-text`, or unset AGENT_SEMANTIC_RETRIEVAL."
        ) from error
    if not isinstance(embeddings, list) or len(embeddings) != len(texts):
        raise EmbeddingUnavailableError("Embeddings endpoint returned a mismatched batch.")
    parsed: list[list[float]] = []
    for vector in embeddings:
        if not isinstance(vector, list) or not vector:
            raise EmbeddingUnavailableError("Embeddings endpoint returned an empty or invalid vector.")
        if any(
            isinstance(value, bool) or not isinstance(value, int | float) or not math.isfinite(value)
            for value in vector
        ):
            raise EmbeddingUnavailableError("Embeddings endpoint returned a non-finite or non-numeric vector.")
        parsed.append([float(value) for value in vector])
    return parsed


def resolve_embedding_model_digest() -> str | None:
    """Return Ollama's immutable model digest when the endpoint exposes it."""
    try:
        response = httpx.get(
            f"{settings.embeddings_url}/api/tags",
            timeout=min(settings.embedding_timeout_s, 10.0),
        )
        response.raise_for_status()
        models = response.json()["models"]
    except (httpx.HTTPError, KeyError, TypeError, ValueError):
        return None
    if not isinstance(models, list):
        return None
    requested = settings.embedding_model
    aliases = {requested, f"{requested}:latest"} if ":" not in requested else {requested}
    for model in models:
        if not isinstance(model, dict) or model.get("name") not in aliases:
            continue
        digest = model.get("digest")
        return digest if isinstance(digest, str) and digest else None
    return None


def _serialize(vector: list[float]) -> bytes:
    """Pack a float vector in sqlite-vec's compact binary format."""
    return struct.pack(f"{len(vector)}f", *vector)


def _connect() -> sqlite3.Connection:
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(
        settings.state_dir / "vectors.db",
        timeout=max(5.0, settings.embedding_timeout_s + 5.0),
    )
    connection.enable_load_extension(True)
    sqlite_vec.load(connection)
    connection.enable_load_extension(False)
    return connection


def _current_model_digest() -> str:
    """Normalize missing model metadata to the persisted empty marker."""
    return resolve_embedding_model_digest() or ""


def _embed_verified(texts: list[str], *, expected_digest: str | None = None) -> _EmbeddingBatch:
    """Embed once and bind the vectors to a stable model artifact observation."""
    before = _current_model_digest()
    if expected_digest is not None and before != expected_digest:
        raise EmbeddingUnavailableError(
            "The embedding model artifact changed while vectors were generated "
            f"({expected_digest or 'unresolved'} -> {before or 'unresolved'}); retry with a stable model."
        )
    vectors = embed_texts(texts)
    after = _current_model_digest()
    if after != before:
        raise EmbeddingUnavailableError(
            "The embedding model artifact changed while vectors were generated "
            f"({before or 'unresolved'} -> {after or 'unresolved'}); retry with a stable model."
        )
    return _EmbeddingBatch(vectors=vectors, model_digest=after)


def _index_spec(*, model_digest: str | None = None) -> _IndexSpec:
    corpus: list[tuple[str, str]] = []
    chunks: list[tuple[str, str]] = []
    for slug in data.list_runbook_slugs():
        content = _normalize_runbook(data.read_runbook(slug) or "")
        corpus.append((slug, content))
        chunks.extend((slug, chunk) for chunk in chunk_runbook(slug, content))
    if not chunks:
        raise ValueError(
            f"No runbook chunks to index: {settings.data_dir / 'runbooks'} is empty. "
            "Seed the runbook library before enabling semantic retrieval."
        )
    encoded = json.dumps(corpus, separators=(",", ":"), ensure_ascii=True).encode()
    return _IndexSpec(
        corpus_sha256=hashlib.sha256(encoded).hexdigest(),
        embedding_model=settings.embedding_model,
        model_digest=_current_model_digest() if model_digest is None else model_digest,
        chunker_version=_CHUNKER_VERSION,
        chunks=tuple(chunks),
    )


def _metadata(connection: sqlite3.Connection) -> dict[str, Any] | None:
    try:
        row = connection.execute(
            """
            SELECT format_version, corpus_sha256, embedding_model, model_digest,
                   dimensions, chunker_version, built_at, chunk_count
            FROM runbook_index_metadata
            WHERE singleton = 1
            """
        ).fetchone()
    except sqlite3.Error:
        return None
    if row is None:
        return None
    keys = (
        "format_version",
        "corpus_sha256",
        "embedding_model",
        "model_digest",
        "dimensions",
        "chunker_version",
        "built_at",
        "chunk_count",
    )
    return dict(zip(keys, row, strict=True))


def _index_is_ready(
    connection: sqlite3.Connection,
    spec: _IndexSpec,
    *,
    dimensions: int | None,
) -> bool:
    table = connection.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'runbook_chunks'"
    ).fetchone()
    metadata = _metadata(connection)
    if table is None or metadata is None:
        return False
    expected = {
        "format_version": _INDEX_FORMAT_VERSION,
        "corpus_sha256": spec.corpus_sha256,
        "embedding_model": spec.embedding_model,
        "model_digest": spec.model_digest,
        "chunker_version": spec.chunker_version,
        "chunk_count": len(spec.chunks),
    }
    if any(metadata.get(key) != value for key, value in expected.items()):
        return False
    return dimensions is None or metadata["dimensions"] == dimensions


def _validate_vectors(vectors: list[list[float]], expected_count: int) -> int:
    if len(vectors) != expected_count:
        raise EmbeddingUnavailableError("Embeddings endpoint returned a mismatched index batch.")
    if not vectors or not vectors[0]:
        raise EmbeddingUnavailableError("Embeddings endpoint returned an empty vector.")
    dimensions = len(vectors[0])
    if any(len(vector) != dimensions for vector in vectors):
        raise EmbeddingUnavailableError("Embeddings endpoint returned inconsistent vector dimensions.")
    return dimensions


def _ensure_index(spec: _IndexSpec, *, dimensions: int | None = None) -> int:
    """Build one complete generation while serializing concurrent first use."""
    with closing(_connect()) as connection:
        connection.isolation_level = None
        connection.execute("BEGIN IMMEDIATE")
        try:
            if _index_is_ready(connection, spec, dimensions=dimensions):
                connection.execute("COMMIT")
                return len(spec.chunks)
            batch = _embed_verified(
                [chunk for _, chunk in spec.chunks],
                expected_digest=spec.model_digest,
            )
            vectors = batch.vectors
            built_dimensions = _validate_vectors(vectors, len(spec.chunks))
            if dimensions is not None and built_dimensions != dimensions:
                raise EmbeddingUnavailableError(
                    "Query and corpus embeddings have different dimensions; refresh the embedding model and retry."
                )
            connection.execute("DROP TABLE IF EXISTS runbook_chunks")
            connection.execute(f"DROP TABLE IF EXISTS {_METADATA_TABLE}")
            connection.execute(
                f"""
                CREATE TABLE {_METADATA_TABLE} (
                    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
                    format_version INTEGER NOT NULL,
                    corpus_sha256 TEXT NOT NULL,
                    embedding_model TEXT NOT NULL,
                    model_digest TEXT NOT NULL,
                    dimensions INTEGER NOT NULL CHECK (dimensions > 0),
                    chunker_version TEXT NOT NULL,
                    built_at TEXT NOT NULL,
                    chunk_count INTEGER NOT NULL CHECK (chunk_count > 0)
                )
                """
            )
            connection.execute(
                f"CREATE VIRTUAL TABLE runbook_chunks USING vec0("
                f"embedding float[{built_dimensions}], slug TEXT, chunk TEXT)"
            )
            connection.executemany(
                "INSERT INTO runbook_chunks (embedding, slug, chunk) VALUES (?, ?, ?)",
                [(_serialize(vector), slug, chunk) for (slug, chunk), vector in zip(spec.chunks, vectors, strict=True)],
            )
            connection.execute(
                """
                INSERT INTO runbook_index_metadata
                    (singleton, format_version, corpus_sha256, embedding_model,
                     model_digest, dimensions, chunker_version, built_at, chunk_count)
                VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    _INDEX_FORMAT_VERSION,
                    spec.corpus_sha256,
                    spec.embedding_model,
                    spec.model_digest,
                    built_dimensions,
                    spec.chunker_version,
                    datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z"),
                    len(spec.chunks),
                ),
            )
            connection.execute("COMMIT")
        except BaseException:
            connection.execute("ROLLBACK")
            raise
    return len(spec.chunks)


def index_runbooks() -> int:
    """Build or reuse the complete vector generation for the current provenance."""
    return _ensure_index(_index_spec(model_digest=_current_model_digest()))


def index_provenance() -> dict[str, Any] | None:
    """Return reproducibility metadata for the last complete index generation."""
    with closing(_connect()) as connection:
        return _metadata(connection)


def semantic_search(query: str, limit: int = 3) -> list[dict[str, Any]]:
    """Rank runbooks only after proving vectors match current provenance."""
    if limit <= 0:
        raise ValueError(f"semantic_search limit must be positive, got {limit}.")
    query_batch = _embed_verified([query])
    vector = query_batch.vectors[0]
    if not vector:
        raise EmbeddingUnavailableError("Embeddings endpoint returned an empty query vector.")
    spec = _index_spec(model_digest=query_batch.model_digest)
    try:
        _ensure_index(spec, dimensions=len(vector))
    except (OSError, sqlite3.Error) as error:
        raise EmbeddingUnavailableError(
            "Semantic index rebuild failed safely; using keyword retrieval until a complete generation is available."
        ) from error

    rows: list[tuple[str, str, float]] | None = None
    # A different process can switch configurations between ensure and read.
    # Re-check under the read transaction and rebuild once if that happened.
    try:
        for _attempt in range(2):
            with closing(_connect()) as connection:
                connection.isolation_level = None
                connection.execute("BEGIN")
                if _index_is_ready(connection, spec, dimensions=len(vector)):
                    rows = connection.execute(
                        """
                        SELECT slug, chunk, distance FROM runbook_chunks
                        WHERE embedding MATCH ? AND k = ?
                        ORDER BY distance
                        """,
                        (_serialize(vector), limit * 3),
                    ).fetchall()
                    connection.execute("COMMIT")
                    break
                connection.execute("ROLLBACK")
            _ensure_index(spec, dimensions=len(vector))
    except (OSError, sqlite3.Error) as error:
        raise EmbeddingUnavailableError(
            "Semantic index read failed safely; using keyword retrieval until the index can be rebuilt."
        ) from error
    if rows is None:
        raise EmbeddingUnavailableError(
            "Semantic index provenance changed concurrently; retry or use keyword retrieval."
        )

    best: dict[str, float] = {}
    for slug, _chunk, distance in rows:
        if slug not in best or distance < best[slug]:
            best[slug] = distance
    ranked = sorted(best.items(), key=lambda item: (item[1], item[0]))[:limit]
    return [
        {"slug": slug, "content": data.read_runbook(slug) or "", "distance": round(distance, 4)}
        for slug, distance in ranked
    ]
