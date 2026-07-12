"""Semantic runbook retrieval — local embeddings in SQLite (Chapter 3.4).

RAG, kept minimal and honest: ``sqlite-vec`` stores runbook-chunk vectors inside
the SQLite the course already uses (no new database), and ``nomic-embed-text``
via the local Ollama embeddings endpoint keeps everything account-free. The
technique ships as an *option* (``AGENT_SEMANTIC_RETRIEVAL=false`` by default)
and is only worth enabling if it beats the keyword baseline on the retrieval
eval (``mise run eval:retrieval``) — adopt with evidence, not fashion. On any
embedding failure the caller falls back to the deterministic keyword scorer.
"""

from __future__ import annotations

import re
import sqlite3
import struct
from contextlib import closing
from typing import Any

import httpx
import sqlite_vec

from . import data
from .config import settings

# Chunking: split on Markdown H2/H3 boundaries so each chunk is one procedure
# step or symptom block — the retrieval unit an on-call engineer thinks in.
_HEADING = re.compile(r"^#{2,3}\s", re.MULTILINE)


class EmbeddingUnavailableError(RuntimeError):
    """The embeddings endpoint failed; callers should fall back to keywords."""


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
        embeddings = response.json()["embeddings"]
    except (httpx.HTTPError, KeyError, ValueError) as error:
        raise EmbeddingUnavailableError(
            f"Embeddings unavailable at {settings.embeddings_url} with model {settings.embedding_model!r}; "
            "start Ollama and `ollama pull nomic-embed-text`, or unset AGENT_SEMANTIC_RETRIEVAL."
        ) from error
    if len(embeddings) != len(texts):
        raise EmbeddingUnavailableError("Embeddings endpoint returned a mismatched batch.")
    return embeddings


def _serialize(vector: list[float]) -> bytes:
    """Pack a float vector in sqlite-vec's compact binary format."""
    return struct.pack(f"{len(vector)}f", *vector)


def _connect() -> sqlite3.Connection:
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(settings.state_dir / "vectors.db", timeout=5)
    connection.enable_load_extension(True)
    sqlite_vec.load(connection)
    connection.enable_load_extension(False)
    return connection


def index_runbooks() -> int:
    """(Re)build the vector index over runbook chunks; returns the chunk count.

    Embeddings are computed once and stored in the disposable state dir —
    ``mise run data:reset`` clears them like every other runtime artifact.
    """
    chunks: list[tuple[str, str]] = []
    for slug in data.list_runbook_slugs():
        content = data.read_runbook(slug) or ""
        chunks.extend((slug, chunk) for chunk in chunk_runbook(slug, content))
    vectors = embed_texts([chunk for _, chunk in chunks])
    dimensions = len(vectors[0])
    with closing(_connect()) as connection:
        connection.execute("DROP TABLE IF EXISTS runbook_chunks")
        connection.execute(
            f"CREATE VIRTUAL TABLE runbook_chunks USING vec0(embedding float[{dimensions}], slug TEXT, chunk TEXT)"
        )
        connection.executemany(
            "INSERT INTO runbook_chunks (embedding, slug, chunk) VALUES (?, ?, ?)",
            [(_serialize(vector), slug, chunk) for (slug, chunk), vector in zip(chunks, vectors, strict=True)],
        )
        connection.commit()
    return len(chunks)


def _index_is_ready(connection: sqlite3.Connection) -> bool:
    row = connection.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'runbook_chunks'"
    ).fetchone()
    return row is not None


def semantic_search(query: str, limit: int = 3) -> list[dict[str, Any]]:
    """Rank runbooks by cosine distance between the query and indexed chunks.

    Builds the index on first use. Raises ``EmbeddingUnavailableError`` when the
    embedding endpoint cannot serve — the caller logs and falls back to keywords.
    """
    with closing(_connect()) as connection:
        if not _index_is_ready(connection):
            index_runbooks()
        vector = embed_texts([query])[0]
        rows = connection.execute(
            """
            SELECT slug, chunk, distance FROM runbook_chunks
            WHERE embedding MATCH ? AND k = ?
            ORDER BY distance
            """,
            (_serialize(vector), max(limit * 3, limit)),
        ).fetchall()
    # Deduplicate chunks back to whole runbooks, best (smallest) distance first.
    best: dict[str, float] = {}
    for slug, _chunk, distance in rows:
        if slug not in best or distance < best[slug]:
            best[slug] = distance
    ranked = sorted(best.items(), key=lambda item: (item[1], item[0]))[:limit]
    return [
        {"slug": slug, "content": data.read_runbook(slug) or "", "distance": round(distance, 4)}
        for slug, distance in ranked
    ]
