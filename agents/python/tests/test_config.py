"""Unit tests for fail-fast settings validation and the config:check CLI."""

import re
from pathlib import Path

import pytest
from pydantic import AliasChoices, ValidationError

from agent import config_check
from agent.config import ModelProvider, Settings

# Environment variables that would leak a developer's real setup into a test.
_AMBIENT_VARS = (
    "AGENT_GATEWAY_ENABLED",
    "AGENT_MODEL_PROVIDER",
    "AGENT_MCP_URL",
    "AGENT_MODEL",
    "GOOGLE_API_KEY",
    "GOOGLE_CLOUD_LOCATION",
    "GOOGLE_CLOUD_PROJECT",
    "GOOGLE_GENAI_USE_ENTERPRISE",
    "OPENAI_BASE_URL",
    "OPENAI_API_KEY",
)


@pytest.fixture(autouse=True)
def clean_environment(monkeypatch, tmp_path):
    """Isolate each test from ambient provider variables and filesystem state."""
    for name in _AMBIENT_VARS:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.chdir(tmp_path)


def test_default_settings_are_valid() -> None:
    settings = Settings()
    assert settings.model_provider is ModelProvider.OPENAI_COMPATIBLE
    assert settings.model == "qwen3:4b-instruct"
    assert settings.openai_base_url == "http://127.0.0.1:11434/v1"
    assert settings.openai_api_key is not None
    assert settings.openai_api_key.get_secret_value() == "local-ollama"
    assert settings.mcp_url is None
    assert settings.a2a_bind_host == "127.0.0.1"
    assert settings.a2a_host == "localhost"
    assert settings.embedding_timeout_s == 120.0
    assert settings.sanitize_tool_output is True


def test_settings_ignore_local_dotenv(tmp_path) -> None:
    (tmp_path / ".env").write_text("AGENT_MODEL=dotenv-must-not-load\n", encoding="utf-8")
    assert Settings().model == "qwen3:4b-instruct"


def test_removed_gateway_flag_fails_with_migration_guidance(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_GATEWAY_ENABLED", "true")
    with pytest.raises(ValidationError, match="AGENT_GATEWAY_ENABLED was removed") as excinfo:
        Settings()
    message = str(excinfo.value)
    assert "AGENT_MODEL_PROVIDER=openai-compatible" in message
    assert "OPENAI_BASE_URL" in message


def test_openai_compatible_provider_requires_base_url() -> None:
    with pytest.raises(ValidationError, match="requires OPENAI_BASE_URL"):
        Settings(
            model_provider=ModelProvider.OPENAI_COMPATIBLE,
            openai_base_url=None,
            openai_api_key="local-ollama",
        )


def test_openai_compatible_provider_requires_api_key() -> None:
    with pytest.raises(ValidationError, match="requires OPENAI_API_KEY"):
        Settings(
            model_provider=ModelProvider.OPENAI_COMPATIBLE,
            openai_base_url="http://127.0.0.1:4000/v1",
            openai_api_key=None,
        )


def test_openai_compatible_error_names_direct_and_gateway_endpoints() -> None:
    with pytest.raises(ValidationError, match="direct Ollama") as excinfo:
        Settings(model_provider=ModelProvider.OPENAI_COMPATIBLE, openai_base_url=None)
    assert "host agentgateway" in str(excinfo.value)


def test_valid_openai_compatible_combination() -> None:
    settings = Settings(
        model_provider=ModelProvider.OPENAI_COMPATIBLE,
        openai_base_url="http://127.0.0.1:4000/v1",
        openai_api_key="local-agentgateway",
    )
    assert settings.openai_base_url == "http://127.0.0.1:4000/v1"


def test_mcp_url_must_be_http() -> None:
    with pytest.raises(ValidationError, match="AGENT_MCP_URL must be an http"):
        Settings(mcp_url="ftp://example.invalid/mcp")


def test_a2a_bind_and_advertised_hosts_are_distinct() -> None:
    settings = Settings(
        a2a_bind_host="0.0.0.0",  # noqa: S104 - Kubernetes explicitly opts into a container-wide bind
        a2a_host="agentops-agent.localhost",
    )
    assert settings.a2a_bind_host == "0.0.0.0"  # noqa: S104 - verifies the explicit opt-in
    assert settings.a2a_host == "agentops-agent.localhost"
    with pytest.raises(ValidationError):
        Settings(a2a_bind_host="")


def test_openai_compatible_env_vars_are_parsed(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:4000/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "super-sensitive")
    settings = Settings()
    assert settings.model_provider is ModelProvider.OPENAI_COMPATIBLE
    assert "super-sensitive" not in repr(settings)  # SecretStr masks the key


def test_gemini_api_key_provider_does_not_require_openai_configuration() -> None:
    settings = Settings(
        model_provider=ModelProvider.GEMINI,
        google_api_key="gemini-secret",
        openai_base_url=None,
        openai_api_key=None,
    )
    assert settings.model_provider is ModelProvider.GEMINI
    assert settings.google_api_key is not None
    assert settings.google_api_key.get_secret_value() == "gemini-secret"


def test_gemini_provider_requires_an_explicit_auth_path() -> None:
    with pytest.raises(ValidationError, match="requires either GOOGLE_API_KEY"):
        Settings(model_provider=ModelProvider.GEMINI)


def test_gemini_enterprise_provider_accepts_the_adc_course_path() -> None:
    settings = Settings(
        model_provider=ModelProvider.GEMINI,
        google_genai_use_enterprise=True,
        google_cloud_project="agentops-open-course",
        google_cloud_location="global",
    )
    assert settings.google_genai_use_enterprise is True
    assert settings.google_cloud_project == "agentops-open-course"
    assert settings.google_cloud_location == "global"


@pytest.mark.parametrize(
    ("project", "location", "missing"),
    [(None, "global", "project"), ("agentops-open-course", None, "location")],
)
def test_gemini_enterprise_provider_requires_project_and_location(
    project: str | None,
    location: str | None,
    missing: str,
) -> None:
    with pytest.raises(ValidationError, match=f"GOOGLE_CLOUD_{missing.upper()}"):
        Settings(
            model_provider=ModelProvider.GEMINI,
            google_genai_use_enterprise=True,
            google_cloud_project=project,
            google_cloud_location=location,
        )


def test_gemini_provider_rejects_ambiguous_api_key_and_enterprise_auth() -> None:
    with pytest.raises(ValidationError, match="cannot combine GOOGLE_API_KEY"):
        Settings(
            model_provider=ModelProvider.GEMINI,
            google_api_key="gemini-secret",
            google_genai_use_enterprise=True,
            google_cloud_project="agentops-open-course",
            google_cloud_location="global",
        )


def test_gemini_environment_aliases_are_parsed_and_key_is_masked(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "gemini")
    monkeypatch.setenv("GOOGLE_API_KEY", "gemini-sensitive")
    settings = Settings()
    assert settings.model_provider is ModelProvider.GEMINI
    assert "gemini-sensitive" not in repr(settings)


def test_config_check_reports_valid_configuration(capsys) -> None:
    assert config_check.main() == 0
    out = capsys.readouterr().out
    assert "Agent configuration is valid" in out
    assert "- model_provider = openai-compatible" in out


def test_config_check_masks_secrets(monkeypatch, capsys) -> None:
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:4000/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "super-sensitive")
    assert config_check.main() == 0
    out = capsys.readouterr().out
    assert "**********" in out
    assert "super-sensitive" not in out


def test_config_check_masks_gemini_api_key(monkeypatch, capsys) -> None:
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "gemini")
    monkeypatch.setenv("GOOGLE_API_KEY", "gemini-sensitive")
    assert config_check.main() == 0
    out = capsys.readouterr().out
    assert "**********" in out
    assert "gemini-sensitive" not in out


def test_config_check_fails_with_actionable_errors(monkeypatch, capsys) -> None:
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("OPENAI_BASE_URL", "")
    monkeypatch.setenv("OPENAI_API_KEY", "")
    assert config_check.main() == 1
    err = capsys.readouterr().err
    assert "OPENAI_BASE_URL" in err
    assert "OPENAI_API_KEY" in err


def test_prompt_uri_must_be_a_registry_uri() -> None:
    with pytest.raises(ValidationError, match="AGENT_PROMPT_URI"):
        Settings(prompt_uri="agentops-agent-instruction/2")
    settings = Settings(prompt_uri="prompts:/agentops-agent-instruction/2")
    assert settings.prompt_uri == "prompts:/agentops-agent-instruction/2"


def test_component_env_example_documents_every_active_settings_variable() -> None:
    example = (Path(__file__).parents[1] / ".env.example").read_text(encoding="utf-8")
    documented = set(re.findall(r"(?m)^#?\s*([A-Z][A-Z0-9_]+)=", example))
    expected: set[str] = set()
    for name, field in Settings.model_fields.items():
        if field.exclude:
            continue
        alias = field.validation_alias
        if isinstance(alias, AliasChoices):
            expected.update(choice for choice in alias.choices if isinstance(choice, str))
        elif isinstance(alias, str):
            expected.add(alias)
        else:
            expected.add(f"AGENT_{name.upper()}")
    assert expected <= documented
