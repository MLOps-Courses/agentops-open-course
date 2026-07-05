# syntax=docker/dockerfile:1
# Ops Copilot — Python track container image (Chapter 6.1).
#
# A minimal, non-root, multi-stage build: a uv stage resolves the locked
# dependencies into a self-contained virtualenv, and a slim runtime carries only
# that venv, the agent source, and the bundled dataset. Distroless (gcr.io/distroless/python3)
# tracks Debian's Python 3.11, but this agent needs 3.13 (see pyproject `requires-python`),
# so the runtime is python:3.13-slim — the closest "distroless-spirit" base that matches.
# Pin both bases by @sha256 digest in production for reproducible, auditable builds.
#
# The image serves the agent over A2A on :8080 — the contract kagent's `type: BYO`
# Agent expects (it deploys this image and probes /.well-known/agent-card.json).
#
# Build context is the repo's agents/ directory, so both python/ and data/ are visible.
# From the repo root:
# docker build -f agents/python/Dockerfile -t k3d-registry.localhost:5050/agentops-agent:latest agents

# --- Stage 1: resolve dependencies into /app/.venv with uv -------------------
FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS build

# Byte-compile for faster cold starts; copy (not hardlink) so the venv is portable
# across the stage boundary; never fetch a managed interpreter (use the base image's).
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/app/.venv \
    VIRTUAL_ENV=/app/.venv

WORKDIR /app

# Locked runtime dependencies only (cached until pyproject.toml / uv.lock change).
# --no-install-project: install deps but not the agent package (we run it from source).
COPY python/pyproject.toml python/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

# A2A serving needs google-adk's `a2a` extra (a2a-sdk). It is a deployment-only
# concern — kept out of pyproject.toml — and pinned to google-adk[a2a]'s range.
RUN uv pip install "a2a-sdk>=0.3.4,<0.4"

# --- Stage 2: slim, non-root runtime -----------------------------------------
FROM python:3.13-slim-bookworm AS runtime

# Run as an unprivileged user. The dataset dir is owned by it because the mock
# actions (restart_service, resolve_incident) write to incidents.db / audit_log.
RUN groupadd --system --gid 10001 app \
 && useradd --system --uid 10001 --gid app --no-create-home app

ENV PATH=/app/.venv/bin:$PATH \
    PYTHONPATH=/app/src \
    PYTHONUNBUFFERED=1 \
    AGENT_DATA_DIR=/app/data

WORKDIR /app

# The resolved venv, the agent source, and the bundled dataset (Ops Copilot data).
COPY --from=build /app/.venv /app/.venv
COPY python/src /app/src
COPY --chown=10001:10001 data /app/data

# A2A launcher: to_a2a() auto-builds the AgentCard from the agent and serves it at
# the root well-known path kagent's BYO readiness probe expects — no agent.json needed.
COPY <<'PY' /app/server.py
"""Serve the Ops Copilot over A2A (kagent BYO contract, Chapter 6.3)."""

import os

from google.adk.a2a.utils.agent_to_a2a import to_a2a

from agent.agent import root_agent

# A2A Starlette app; host/port only shape the advertised agent-card URL.
app = to_a2a(root_agent, host=os.environ.get("A2A_HOST", "0.0.0.0"), port=int(os.environ.get("PORT", "8080")))
PY

USER app
EXPOSE 8080

# Serve A2A on :8080 (0.0.0.0 so kagent/the ingress can reach it).
ENTRYPOINT ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8080"]
