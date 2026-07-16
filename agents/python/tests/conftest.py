"""Shared test fixtures.

Every test runs against a throwaway copy of the bundled dataset so tools that write
(the audit log, mock actions) never mutate the committed ``agents/data/incidents.db``.
"""

import os
import shutil

import pytest

# Collection imports the configured agent, so remove ambient provider/runtime
# settings before importing any agent module. Tests opt into individual values
# with ``monkeypatch`` and telemetry remains disabled for the whole pytest process.
_RUNTIME_ENV_PREFIXES = ("AGENT_", "GOOGLE_", "MLFLOW_", "OPENAI_", "OTEL_")
for _name in tuple(os.environ):
    if _name.startswith(_RUNTIME_ENV_PREFIXES):
        os.environ.pop(_name)
os.environ["OTEL_SDK_DISABLED"] = "true"

from agent import config  # noqa: E402 - provider env must be cleared before importing agent settings


@pytest.fixture(autouse=True)
def isolated_data_dir(tmp_path, monkeypatch):
    """Point the agent at a fresh, disposable copy of the dataset for the duration of a test."""
    destination = tmp_path / "data"
    shutil.copytree(config._DEFAULT_DATA_DIR, destination)  # noqa: SLF001 — test setup mirrors the default
    monkeypatch.setattr(config.settings, "data_dir", destination)
    monkeypatch.setattr(config.settings, "state_dir", tmp_path / "state")
    return destination
