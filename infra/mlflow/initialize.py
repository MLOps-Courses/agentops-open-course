"""Initialize the single-replica MLflow store before serving requests."""

from __future__ import annotations

import os

from mlflow import MlflowClient
from mlflow.utils.uri import append_to_uri_path

_DEFAULT_EXPERIMENT_ID = "0"


def main() -> None:
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


if __name__ == "__main__":
    main()
