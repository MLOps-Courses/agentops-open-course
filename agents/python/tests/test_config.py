"""Unit tests for fail-fast settings validation and the config:check CLI."""

import pytest
from pydantic import ValidationError

from agent import config_check
from agent.config import Settings

# Environment variables that would leak a developer's real setup into a test.
_AMBIENT_VARS = (
    "AGENT_GATEWAY_ENABLED",
    "AGENT_MCP_URL",
    "AGENT_MODEL",
    "OPENAI_BASE_URL",
    "OPENAI_API_KEY",
)


@pytest.fixture(autouse=True)
def clean_environment(monkeypatch, tmp_path):
    """Isolate each test from ambient env vars and any developer .env file."""
    for name in _AMBIENT_VARS:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.chdir(tmp_path)  # Settings reads .env relative to the cwd


def test_default_settings_are_valid() -> None:
    settings = Settings()
    assert settings.gateway_enabled is False
    assert settings.mcp_url is None


def test_gateway_requires_base_url() -> None:
    with pytest.raises(ValidationError, match="requires OPENAI_BASE_URL"):
        Settings(gateway_enabled=True, openai_api_key="local-agentgateway")


def test_gateway_requires_api_key() -> None:
    with pytest.raises(ValidationError, match="requires OPENAI_API_KEY"):
        Settings(gateway_enabled=True, openai_base_url="http://127.0.0.1:4000/v1")


def test_gateway_error_names_the_fix() -> None:
    with pytest.raises(ValidationError, match="mise run gateway:host"):
        Settings(gateway_enabled=True)


def test_valid_gateway_combination() -> None:
    settings = Settings(
        gateway_enabled=True,
        openai_base_url="http://127.0.0.1:4000/v1",
        openai_api_key="local-agentgateway",
    )
    assert settings.openai_base_url == "http://127.0.0.1:4000/v1"


def test_mcp_url_must_be_http() -> None:
    with pytest.raises(ValidationError, match="AGENT_MCP_URL must be an http"):
        Settings(mcp_url="ftp://example.invalid/mcp")


def test_gateway_env_vars_are_mirrored(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_GATEWAY_ENABLED", "true")
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:4000/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "super-sensitive")
    settings = Settings()
    assert settings.gateway_enabled is True
    assert "super-sensitive" not in repr(settings)  # SecretStr masks the key


def test_config_check_reports_valid_configuration(capsys) -> None:
    assert config_check.main() == 0
    out = capsys.readouterr().out
    assert "Agent configuration is valid" in out
    assert "- gateway_enabled = False" in out


def test_config_check_masks_secrets(monkeypatch, capsys) -> None:
    monkeypatch.setenv("AGENT_GATEWAY_ENABLED", "true")
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:4000/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "super-sensitive")
    assert config_check.main() == 0
    out = capsys.readouterr().out
    assert "**********" in out
    assert "super-sensitive" not in out


def test_config_check_fails_with_actionable_errors(monkeypatch, capsys) -> None:
    monkeypatch.setenv("AGENT_GATEWAY_ENABLED", "true")
    assert config_check.main() == 1
    err = capsys.readouterr().err
    assert "OPENAI_BASE_URL" in err
    assert "OPENAI_API_KEY" in err


def test_prompt_uri_must_be_a_registry_uri() -> None:
    with pytest.raises(ValidationError, match="AGENT_PROMPT_URI"):
        Settings(prompt_uri="ops-copilot-instruction/2")
    settings = Settings(prompt_uri="prompts:/ops-copilot-instruction/2")
    assert settings.prompt_uri == "prompts:/ops-copilot-instruction/2"
