"""Unit tests for native and gateway model selection."""

import pytest
from google.adk.models import Gemini, OpenAILlm
from pydantic import SecretStr

from agent import model


def test_native_model_uses_gemini_with_retry_policy(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "gateway_enabled", False)
    configured = model.build_model()
    assert isinstance(configured, Gemini)
    assert configured.model == model.settings.model
    assert configured.retry_options is not None
    assert configured.retry_options.attempts == model.settings.max_retries + 1


def test_gateway_model_uses_openai_compatible_adk_client(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "gateway_enabled", True)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    monkeypatch.setenv("OPENAI_BASE_URL", "http://localhost:4000/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "local-not-a-secret")
    configured = model.build_model()
    assert isinstance(configured, OpenAILlm)
    assert configured.model == model.settings.model
    client = configured._openai_client  # noqa: SLF001 — asserts the resilience seam
    assert client.timeout == model.settings.model_timeout_s
    assert client.max_retries == model.settings.max_retries


def test_gateway_model_exports_credentials_from_settings(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "gateway_enabled", True)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("from-dotenv-file"))
    monkeypatch.delenv("OPENAI_BASE_URL", raising=False)
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    assert isinstance(model.build_model(), OpenAILlm)
    assert model.os.environ["OPENAI_BASE_URL"] == "http://localhost:4000/v1"
    assert model.os.environ["OPENAI_API_KEY"] == "from-dotenv-file"


def test_gateway_model_requires_endpoint_and_key(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "gateway_enabled", True)
    monkeypatch.setattr(model.settings, "openai_base_url", None)
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    with pytest.raises(ValueError, match="OPENAI_BASE_URL"):
        model.build_model()
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", None)
    with pytest.raises(ValueError, match="OPENAI_API_KEY"):
        model.build_model()
