# syntax=docker/dockerfile:1.11@sha256:10c699f1b6c8bdc8f6b4ce8974855dd8542f1768c26eb240237b8f1c9c6c9976
FROM ghcr.io/astral-sh/uv:0.11.28@sha256:0f36cb9361a3346885ca3677e3767016687b5a170c1a6b88465ec14aefec90aa AS uv

FROM python:3.13-slim-bookworm@sha256:fcbd8dfc2605ba7c2eca646846c5e892b2931e41f6227985154a596f26ab8ed7

ENV PATH="/opt/mlflow/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1

RUN groupadd --gid 10002 mlflow \
    && useradd --uid 10002 --gid 10002 --no-create-home --shell /usr/sbin/nologin mlflow \
    && mkdir -p /opt/mlflow /var/lib/mlflow \
    && chown -R mlflow:mlflow /opt/mlflow /var/lib/mlflow

WORKDIR /opt/mlflow
COPY pyproject.toml uv.lock ./
RUN --mount=type=bind,from=uv,source=/uv,target=/usr/local/bin/uv \
    UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy \
    uv sync --frozen --no-cache --no-dev --no-install-project \
    && chown -R mlflow:mlflow /opt/mlflow

COPY --chmod=0444 initialize.py /opt/mlflow/initialize.py
COPY --chmod=0555 entrypoint.sh /usr/local/bin/mlflow-entrypoint

USER 10002:10002
EXPOSE 5000
ENTRYPOINT ["/usr/local/bin/mlflow-entrypoint"]
