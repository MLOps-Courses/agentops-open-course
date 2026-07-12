"""Shared test fixtures.

Every test runs against a throwaway copy of the bundled dataset so tools that write
(the audit log, mock actions) never mutate the committed ``agents/data/incidents.db``.
"""

import shutil

import pytest

from agent import config


@pytest.fixture(autouse=True)
def isolated_data_dir(tmp_path, monkeypatch):
    """Point the agent at a fresh, disposable copy of the dataset for the duration of a test."""
    destination = tmp_path / "data"
    shutil.copytree(config._DEFAULT_DATA_DIR, destination)  # noqa: SLF001 — test setup mirrors the default
    monkeypatch.setattr(config.settings, "data_dir", destination)
    monkeypatch.setattr(config.settings, "state_dir", tmp_path / "state")
    return destination
