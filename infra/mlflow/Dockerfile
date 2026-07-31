# syntax=docker/dockerfile:1.11@sha256:10c699f1b6c8bdc8f6b4ce8974855dd8542f1768c26eb240237b8f1c9c6c9976
# --8<-- [start:base-images]
FROM ghcr.io/astral-sh/uv:0.12.0@sha256:606e70c71c852d03f611b1e56a195d08648507018a7057fab82c4974c4eae105 AS uv

FROM python:3.13.14-slim-trixie@sha256:6771159cd4fa5d9bba1258caf0b82e6b73458c694d178ad97c5e925c2d0e1a91 AS build
# --8<-- [end:base-images]

ENV PATH="/opt/mlflow/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1

RUN mkdir -p /opt/mlflow /var/lib/mlflow/artifacts /var/lib/mlflow/prometheus \
    && chown -R 10002:10002 /opt/mlflow /var/lib/mlflow

WORKDIR /opt/mlflow
COPY pyproject.toml uv.lock ./
# --8<-- [start:venv-repoint]
# The build stage installs Python under /usr/local; the Wolfi runtime exposes it
# under /usr/bin, so the venv's interpreter links are repointed here — the last
# build-stage step that runs Python.
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
    UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy \
    uv sync --frozen --no-cache --no-dev --no-install-project \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python3 \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python3.13 \
    && chown -R 10002:10002 /opt/mlflow
# --8<-- [end:venv-repoint]

COPY --chmod=0444 entrypoint.py /opt/mlflow/entrypoint.py

# --8<-- [start:runtime-base]
FROM cgr.dev/chainguard/wolfi-base@sha256:003627df3c1e1bba0c4116afcddb314aca9594ee2328c7e876a8081a6c988b2e
# --8<-- [end:runtime-base]

ARG AGENT_SOURCE_COMMIT=unknown
ARG OCI_CREATED=1970-01-01T00:00:00Z
ARG OCI_VERSION=dev
LABEL org.opencontainers.image.created="${OCI_CREATED}" \
      org.opencontainers.image.description="Locked self-hosted MLflow server for the AgentOps Open Course" \
      org.opencontainers.image.documentation="https://agentops-open-course.fmind.dev/" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${AGENT_SOURCE_COMMIT}" \
      org.opencontainers.image.source="https://github.com/MLOps-Courses/agentops-open-course" \
      org.opencontainers.image.title="AgentOps MLflow" \
      org.opencontainers.image.version="${OCI_VERSION}"

# --8<-- [start:apk-pins]
# Same rolling-repository caveat as the agent image: Wolfi drops superseded
# package versions, so these exact pins periodically stop resolving. Refresh
# them to the current versions rather than unpinning.
RUN apk add --no-cache \
    libstdc++=16.1.0-r4 \
    python-3.13=3.13.14-r3
# --8<-- [end:apk-pins]

ENV PATH="/opt/mlflow/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    MLFLOW_HOST=0.0.0.0

WORKDIR /opt/mlflow
COPY --from=build --chown=10002:10002 /opt/mlflow /opt/mlflow
COPY --from=build --chown=10002:10002 /var/lib/mlflow /var/lib/mlflow

USER 10002:10002
EXPOSE 5000
ENTRYPOINT ["python", "/opt/mlflow/entrypoint.py"]
