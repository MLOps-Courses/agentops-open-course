# syntax=docker/dockerfile:1.11@sha256:10c699f1b6c8bdc8f6b4ce8974855dd8542f1768c26eb240237b8f1c9c6c9976
# Ops Copilot — Python track container image (Chapter 6.1).
#
# A minimal, non-root, multi-stage build: uv resolves the locked dependencies
# into a self-contained virtualenv, and a minimal Wolfi runtime carries
# only that venv, the bundled dataset, and an empty state directory. Build and
# runtime bases are digest-pinned for reproducible multi-arch builds.
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

# The build image installs Python under /usr/local, while Wolfi exposes it
# under /usr/bin. Repoint the venv after the last build-stage Python invocation.
RUN ln -sfn /usr/bin/python3 /app/.venv/bin/python \
 && ln -sfn /usr/bin/python3 /app/.venv/bin/python3 \
 && ln -sfn /usr/bin/python3 /app/.venv/bin/python3.13 \
 && install -d -o 10001 -g 10001 /app/state

# --- Stage 2: minimal, non-root runtime --------------------------------------
FROM cgr.dev/chainguard/wolfi-base@sha256:02dab76bd852a70556b5b2002195c8a5fdab77d323c433bf6642aab080489795 AS runtime

# Exact apk pins keep the runtime reproducible. Wolfi is a *rolling* repository:
# it removes superseded package versions, so these pins periodically stop
# resolving ("no such package") until Renovate opens a PR bumping them. That is
# expected drift, not a broken course — refresh the pins to the current versions
# (or `mise run` the Renovate update) and rebuild. See docs 6.1.
RUN apk add --no-cache \
    libstdc++=16.1.0-r4 \
    python-3.13=3.13.14-r2

ENV PATH=/app/.venv/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    AGENT_DATA_DIR=/app/data \
    AGENT_STATE_DIR=/app/state \
    AGENT_A2A_BIND_HOST=0.0.0.0 \
    ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS=false \
    OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=false

WORKDIR /app

# The resolved venv (including the agent package) and bundled Ops Copilot data.
COPY --from=build /app/.venv /app/.venv
COPY --chown=10001:10001 data /app/data
COPY --from=build --chown=10001:10001 /app/state /app/state

USER 10001:10001
EXPOSE 8080

# The image explicitly opts into a container-wide bind so kagent can reach A2A;
# the non-container host default remains loopback-only.
ENTRYPOINT ["python", "-m", "agent.server"]
