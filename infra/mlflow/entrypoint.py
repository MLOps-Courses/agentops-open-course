"""Initialize the course MLflow store, then replace this process with MLflow."""

from __future__ import annotations

import os

from mlflow import MlflowClient
from mlflow.cli import cli
from mlflow.utils.uri import append_to_uri_path

_DEFAULT_EXPERIMENT_ID = "0"


def initialize() -> None:
    """Give experiment zero the course-wide name without splitting lineage."""
    tracking_uri = os.environ["MLFLOW_BACKEND_STORE_URI"]
    experiment_name = os.environ["MLFLOW_EXPERIMENT_NAME"]
    expected_artifact_location = append_to_uri_path(os.environ["_MLFLOW_SERVER_ARTIFACT_ROOT"], "0")
    client = MlflowClient(tracking_uri=tracking_uri)

    experiment = client.get_experiment(_DEFAULT_EXPERIMENT_ID)
    if experiment is None:
        raise RuntimeError("MLflow did not initialize its default experiment")
    if experiment.artifact_location != expected_artifact_location:
        raise RuntimeError(
            f"experiment 0 artifact location is {experiment.artifact_location!r}, "
            f"expected {expected_artifact_location!r}"
        )
    if experiment.name == experiment_name:
        return

    collision = client.get_experiment_by_name(experiment_name)
    if collision is not None and collision.experiment_id != _DEFAULT_EXPERIMENT_ID:
        raise RuntimeError(f"experiment name {experiment_name!r} already belongs to id {collision.experiment_id}")

    client.rename_experiment(_DEFAULT_EXPERIMENT_ID, experiment_name)


def main() -> None:
    """Apply safe defaults, initialize experiment zero, and serve MLflow."""
    artifacts_destination = os.getenv("MLFLOW_ARTIFACTS_DESTINATION", "/var/lib/mlflow/artifacts")
    allowed_hosts = os.getenv(
        "MLFLOW_ALLOWED_HOSTS",
        "mlflow:*,mlflow.agentops.svc.cluster.local:*,localhost:*,127.0.0.1:*",
    )
    backend_store_uri = os.getenv("MLFLOW_BACKEND_STORE_URI", "sqlite:////var/lib/mlflow/mlflow.db")
    artifact_root = os.getenv("MLFLOW_DEFAULT_ARTIFACT_ROOT", "mlflow-artifacts:/")
    host = os.getenv("MLFLOW_HOST", "127.0.0.1")

    os.environ["_MLFLOW_SERVER_ARTIFACT_ROOT"] = artifact_root
    os.environ["MLFLOW_BACKEND_STORE_URI"] = backend_store_uri
    os.environ.setdefault("MLFLOW_EXPERIMENT_NAME", "agentops-agent")
    initialize()

    cli.main(
        args=[
            "server",
            "--host",
            host,
            "--port",
            "5000",
            "--workers",
            "1",
            "--backend-store-uri",
            backend_store_uri,
            "--default-artifact-root",
            artifact_root,
            "--artifacts-destination",
            artifacts_destination,
            "--serve-artifacts",
            "--expose-prometheus",
            "/var/lib/mlflow/prometheus",
            "--allowed-hosts",
            allowed_hosts,
        ],
        prog_name="mlflow",
    )


if __name__ == "__main__":
    main()
