"""Unit tests for OpenAI-compatible and optional Gemini model selection."""

from collections.abc import AsyncGenerator

import pytest
from google.adk.models import Gemini, OpenAILlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types
from pydantic import SecretStr

from agent import model
from agent.config import ModelProvider
from agent.model import FallbackLlm


class _StubLlm(model.BaseLlm):
    """A model stub that answers, raises up front, or raises after one chunk."""

    reply: str = ""
    fail: bool = False
    fail_after_yield: bool = False

    async def generate_content_async(
        self, llm_request: LlmRequest, stream: bool = False
    ) -> AsyncGenerator[LlmResponse]:
        del llm_request, stream
        if self.fail:
            raise ConnectionError(f"{self.model} is down")
        yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text=self.reply)]))
        if self.fail_after_yield:
            raise ConnectionError(f"{self.model} dropped mid-stream")


async def _collect(llm: model.BaseLlm) -> list[str]:
    request = LlmRequest(contents=[types.Content(role="user", parts=[types.Part(text="hi")])])
    return [
        response.content.parts[0].text or ""
        async for response in llm.generate_content_async(request)
        if response.content is not None and response.content.parts
    ]


def test_optional_gemini_model_uses_native_retry_policy(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.GEMINI)
    monkeypatch.setattr(model.settings, "model", "gemini-test")
    monkeypatch.setattr(model.settings, "google_api_key", SecretStr("gemini-test-key"))
    monkeypatch.setattr(model.settings, "google_genai_use_enterprise", False)
    configured = model.build_model()
    assert isinstance(configured, Gemini)
    assert configured.model == model.settings.model
    assert configured.retry_options is not None
    assert configured.retry_options.attempts == model.settings.max_retries + 1
    assert configured.retry_options.max_delay == min(
        model.settings.retry_backoff_s * (2**model.settings.max_retries),
        30.0,
    )
    assert configured.client_kwargs is not None
    assert configured.client_kwargs["enterprise"] is False
    http_options = configured.client_kwargs["http_options"]
    assert isinstance(http_options, model.types.HttpOptions)
    assert http_options.timeout == round(model.settings.model_timeout_s * 1000)
    assert http_options.retry_options is configured.retry_options
    client = configured.api_client
    assert client._api_client._http_options.timeout == round(  # noqa: SLF001 - locked SDK request deadline
        model.settings.model_timeout_s * 1000
    )


def test_optional_gemini_enterprise_model_passes_explicit_adc_scope(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.GEMINI)
    monkeypatch.setattr(model.settings, "model", "gemini-test")
    monkeypatch.setattr(model.settings, "google_api_key", None)
    monkeypatch.setattr(model.settings, "google_genai_use_enterprise", True)
    monkeypatch.setattr(model.settings, "google_cloud_project", "agentops-open-course")
    monkeypatch.setattr(model.settings, "google_cloud_location", "global")
    configured = model.build_model()
    assert isinstance(configured, Gemini)
    assert configured.client_kwargs is not None
    assert configured.client_kwargs["enterprise"] is True
    assert configured.client_kwargs["project"] == "agentops-open-course"
    assert configured.client_kwargs["location"] == "global"


def test_openai_compatible_model_uses_validated_endpoint(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    configured = model.build_model()
    assert isinstance(configured, OpenAILlm)
    assert configured.model == model.settings.model
    client = configured._openai_client  # noqa: SLF001 — asserts the resilience seam
    assert str(client.base_url) == "http://localhost:4000/v1/"
    assert client.timeout == model.settings.model_timeout_s
    assert client.max_retries == model.settings.max_retries


def test_openai_model_uses_validated_settings_without_mutating_environment(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("from-dotenv-file"))
    monkeypatch.setenv("OPENAI_BASE_URL", "http://ambient.invalid/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "ambient-secret")
    configured = model.build_model()
    assert isinstance(configured, OpenAILlm)
    client = configured._openai_client  # noqa: SLF001
    assert str(client.base_url) == "http://localhost:4000/v1/"
    assert client.api_key == "from-dotenv-file"
    assert model.settings.openai_api_key is not None
    assert model.settings.openai_api_key.get_secret_value() not in repr(configured)


def test_openai_compatible_model_requires_endpoint_and_key(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "openai_base_url", None)
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    with pytest.raises(ValueError, match="OPENAI_BASE_URL"):
        model.build_model()
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", None)
    with pytest.raises(ValueError, match="OPENAI_API_KEY"):
        model.build_model()


def test_build_model_without_fallback_returns_bare_primary(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    monkeypatch.setattr(model.settings, "model_fallback", None)
    configured = model.build_model()
    assert isinstance(configured, OpenAILlm)
    assert not isinstance(configured, FallbackLlm)


def test_build_model_with_fallback_wraps_two_distinct_models(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    monkeypatch.setattr(model.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(model.settings, "model_fallback", "qwen3:1.7b")
    configured = model.build_model()
    assert isinstance(configured, FallbackLlm)
    assert configured.primary.model == "qwen3:4b-instruct"
    assert configured.fallback.model == "qwen3:1.7b"


def test_fallback_engages_only_when_primary_fails_before_responding() -> None:
    import asyncio

    primary = _StubLlm(model="primary", fail=True)
    fallback = _StubLlm(model="fallback", reply="from fallback")
    chain = FallbackLlm(model="primary", primary=primary, fallback=fallback)
    assert asyncio.run(_collect(chain)) == ["from fallback"]


def test_healthy_primary_is_used_and_fallback_untouched() -> None:
    import asyncio

    primary = _StubLlm(model="primary", reply="from primary")
    fallback = _StubLlm(model="fallback", fail=True)  # would raise if ever called
    chain = FallbackLlm(model="primary", primary=primary, fallback=fallback)
    assert asyncio.run(_collect(chain)) == ["from primary"]


def test_mid_stream_failure_is_not_masked_by_fallback() -> None:
    import asyncio

    primary = _StubLlm(model="primary", reply="partial", fail_after_yield=True)
    fallback = _StubLlm(model="fallback", reply="from fallback")
    chain = FallbackLlm(model="primary", primary=primary, fallback=fallback)
    with pytest.raises(ConnectionError, match="dropped mid-stream"):
        asyncio.run(_collect(chain))


def test_both_models_down_surfaces_the_fallback_error() -> None:
    import asyncio

    primary = _StubLlm(model="primary", fail=True)
    fallback = _StubLlm(model="fallback", fail=True)
    chain = FallbackLlm(model="primary", primary=primary, fallback=fallback)
    with pytest.raises(ConnectionError, match="fallback is down"):
        asyncio.run(_collect(chain))
