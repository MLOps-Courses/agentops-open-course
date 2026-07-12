#!/bin/sh
set -eu

: "${MLFLOW_ARTIFACTS_DESTINATION:=/var/lib/mlflow/artifacts}"
: "${MLFLOW_ALLOWED_HOSTS:=mlflow:*,mlflow.agentops.svc.cluster.local:*,localhost:*,127.0.0.1:*}"
: "${MLFLOW_BACKEND_STORE_URI:=sqlite:////var/lib/mlflow/mlflow.db}"
: "${MLFLOW_DEFAULT_ARTIFACT_ROOT:=mlflow-artifacts:/}"
: "${MLFLOW_EXPERIMENT_NAME:=ops-copilot}"

export _MLFLOW_SERVER_ARTIFACT_ROOT="${MLFLOW_DEFAULT_ARTIFACT_ROOT}"
export MLFLOW_BACKEND_STORE_URI MLFLOW_EXPERIMENT_NAME
python /opt/mlflow/initialize.py

exec mlflow server \
	--host 0.0.0.0 \
	--port 5000 \
	--workers 1 \
	--backend-store-uri "${MLFLOW_BACKEND_STORE_URI}" \
	--default-artifact-root "${MLFLOW_DEFAULT_ARTIFACT_ROOT}" \
	--artifacts-destination "${MLFLOW_ARTIFACTS_DESTINATION}" \
	--serve-artifacts \
	--expose-prometheus /var/lib/mlflow/prometheus \
	--allowed-hosts "${MLFLOW_ALLOWED_HOSTS}"
