# syntax=docker/dockerfile:1
# Ops Copilot — Python track container image (Chapter 6.1).
#
# A minimal, non-root, multi-stage build: uv resolves the locked
# dependencies into a self-contained virtualenv, and a slim runtime carries only
# that venv, the agent source, and the bundled dataset. Distroless (gcr.io/distroless/python3)
# tracks Debian's Python 3.11, but this agent needs 3.13 (see pyproject `requires-python`),
# so the runtime is python:3.13-slim — the closest "distroless-spirit" base that matches.
# Both tools and Python are version-and-digest pinned for reproducible multi-arch builds.
#
# The image serves the agent over A2A on :8080 — the contract kagent's `type: BYO`
# Agent expects (it deploys this image and probes /.well-known/agent-card.json).
#
# Build context is the repo's agents/ directory, so both python/ and data/ are visible.
# From the repo root:
# docker build -f agents/python/Dockerfile -t agentops-agent:dev agents

# --- Stage 1: resolve dependencies into /app/.venv with uv -------------------
FROM ghcr.io/astral-sh/uv:0.11.28@sha256:0f36cb9361a3346885ca3677e3767016687b5a170c1a6b88465ec14aefec90aa AS uv
FROM python:3.13.13-slim-trixie@sha256:aa938a849bcb82dce8f49480f056ab82bf5c1c3ebc294f0430f37b6820e7f286 AS build

COPY --from=uv /uv /uvx /bin/

# Byte-compile for faster cold starts; copy (not hardlink) so the venv is portable
# across the stage boundary; never fetch a managed interpreter (use the base image's).
ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/app/.venv \
    VIRTUAL_ENV=/app/.venv

WORKDIR /app

# Locked runtime dependencies only (cached until pyproject.toml / uv.lock change).
COPY python/pyproject.toml python/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

# Install the project non-editably so runtime package metadata (including the
# A2A card version) is available without carrying a duplicate source tree.
COPY python/README.md ./README.md
COPY python/src ./src
RUN uv sync --frozen --no-dev --no-editable

# --- Stage 2: slim, non-root runtime -----------------------------------------
FROM python:3.13.13-slim-trixie@sha256:aa938a849bcb82dce8f49480f056ab82bf5c1c3ebc294f0430f37b6820e7f286 AS runtime

# Apply Debian security fixes published after the pinned base snapshot, then run
# as an unprivileged user. CI scans fail if a later fixed HIGH/CRITICAL remains.
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get upgrade --yes --no-install-recommends \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 app \
 && useradd --uid 10001 --gid app --no-create-home --shell /usr/sbin/nologin app

ENV PATH=/app/.venv/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    AGENT_DATA_DIR=/app/data \
    AGENT_STATE_DIR=/app/state \
    ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS=false \
    OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=false

WORKDIR /app

# The resolved venv (including the agent package) and bundled Ops Copilot data.
COPY --from=build /app/.venv /app/.venv
COPY data /app/data
RUN install -d -o 10001 -g 10001 /app/state

USER app
EXPOSE 8080

# Serve A2A on :8080 (0.0.0.0 so kagent/the ingress can reach it).
ENTRYPOINT ["python", "-m", "agent.server"]
