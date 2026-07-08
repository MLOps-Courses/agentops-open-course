"""Typed, fail-fast configuration for the agent (loaded from the environment / .env)."""

from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict

# The bundled dataset lives at agents/data. This file is
# agents/python/src/agent/config.py, so the repo's agents/ dir is three parents up.
_DEFAULT_DATA_DIR = Path(__file__).resolve().parents[3] / "data"


class Settings(BaseSettings):
    """Agent settings. Native Gemini in Ch. 2-4; other providers arrive via agentgateway in Ch. 5."""

    model_config = SettingsConfigDict(env_file=".env", env_file_encoding="utf-8", env_prefix="AGENT_", extra="ignore")

    # Set an explicit model — ADK defaults churn. Override with AGENT_MODEL.
    model: str = "gemini-3.5-flash"

    # Where the bundled Ops Copilot dataset lives. Override with AGENT_DATA_DIR (e.g. in containers).
    data_dir: Path = _DEFAULT_DATA_DIR


settings = Settings()
