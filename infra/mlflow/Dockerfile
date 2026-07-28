# syntax=docker/dockerfile:1.11@sha256:10c699f1b6c8bdc8f6b4ce8974855dd8542f1768c26eb240237b8f1c9c6c9976
FROM ghcr.io/astral-sh/uv:0.11.32@sha256:df4cae8f3a96d175e2e5f992e597550000edbe78fdc2594d5cd8de1a217f504c AS uv

FROM python:3.14.6-slim-trixie@sha256:cea0e6040540fb2b965b6e7fb5ffa00871e632eef63719f0ea54bca189ce14a6 AS build

ENV PATH="/opt/mlflow/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1

RUN mkdir -p /opt/mlflow /var/lib/mlflow/artifacts /var/lib/mlflow/prometheus \
    && chown -R 10002:10002 /opt/mlflow /var/lib/mlflow

WORKDIR /opt/mlflow
COPY pyproject.toml uv.lock ./
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
    UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy \
    uv sync --frozen --no-cache --no-dev --no-install-project \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python3 \
    && ln -sfn /usr/bin/python3 /opt/mlflow/.venv/bin/python3.13 \
    && chown -R 10002:10002 /opt/mlflow

COPY --chmod=0444 entrypoint.py /opt/mlflow/entrypoint.py

FROM cgr.dev/chainguard/wolfi-base@sha256:003627df3c1e1bba0c4116afcddb314aca9594ee2328c7e876a8081a6c988b2e

RUN apk add --no-cache \
    libstdc++=16.1.0-r4 \
    python-3.13=3.13.14-r3

ENV PATH="/opt/mlflow/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    MLFLOW_HOST=0.0.0.0

WORKDIR /opt/mlflow
COPY --from=build --chown=10002:10002 /opt/mlflow /opt/mlflow
COPY --from=build --chown=10002:10002 /var/lib/mlflow /var/lib/mlflow

USER 10002:10002
EXPOSE 5000
ENTRYPOINT ["python", "/opt/mlflow/entrypoint.py"]
