"""Unit tests for OpenAI-compatible and optional Gemini model selection."""

import asyncio
from collections.abc import AsyncGenerator
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest
from google.adk.models import Gemini, OpenAILlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types
from openai import AsyncOpenAI
from pydantic import SecretStr

from agent import model
from agent.config import ModelProvider


class _ProviderError(RuntimeError):
    """The ``status_code`` shape that OpenAI-compatible SDK errors expose."""

    def __init__(self, message: str, status_code: int) -> None:
        super().__init__(message)
        self.status_code = status_code


class _StubLlm(model.BaseLlm):
    """A model stub that answers, raises up front, or raises after one chunk."""

    reply: str = ""
    fail_status: int | None = None
    fail_after_yield: bool = False

    async def generate_content_async(
        self, llm_request: LlmRequest, stream: bool = False
    ) -> AsyncGenerator[LlmResponse]:
        del llm_request, stream
        if self.fail_status is not None:
            raise _ProviderError(f"{self.model} is down", self.fail_status)
        yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text=self.reply)]))
        if self.fail_after_yield:
            raise _ProviderError(f"{self.model} dropped mid-stream", 503)


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


def test_generation_config_is_explicit_only_when_requested(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_temperature", None)
    assert model.build_generation_config() is None
    monkeypatch.setattr(model.settings, "model_temperature", 0.0)
    config = model.build_generation_config()
    assert config is not None
    assert config.temperature == 0


def test_openai_adapter_forwards_adk_sampling_temperature(monkeypatch) -> None:
    configured = model.ResilientOpenAILlm(
        model="qwen3:4b-instruct",
        openai_base_url="http://localhost:11434/v1",
        openai_api_key=SecretStr("local-marker"),
        timeout_s=10,
        retries=0,
    )
    captured: dict = {}

    class Completions:
        async def create(self, **kwargs):
            captured.update(kwargs)
            return SimpleNamespace(
                choices=[SimpleNamespace(message=SimpleNamespace(content="ok", tool_calls=None))],
                usage=SimpleNamespace(prompt_tokens=2, completion_tokens=1, total_tokens=3),
            )

    client = SimpleNamespace(chat=SimpleNamespace(completions=Completions()))
    monkeypatch.setitem(configured.__dict__, "_openai_client", client)
    request = LlmRequest(
        contents=[types.Content(role="user", parts=[types.Part(text="hi")])],
        config=types.GenerateContentConfig(temperature=0),
    )

    async def run() -> None:
        responses = [response async for response in configured.generate_content_async(request)]
        content = responses[0].content
        assert content is not None
        assert content.parts
        assert content.parts[0].text == "ok"

    asyncio.run(run())
    assert captured["temperature"] == 0


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
    assert not isinstance(configured, model.FallbackModel)


def test_build_model_with_fallback_wraps_two_distinct_models(monkeypatch) -> None:
    monkeypatch.setattr(model.settings, "model_provider", ModelProvider.OPENAI_COMPATIBLE)
    monkeypatch.setattr(model.settings, "openai_base_url", "http://localhost:4000/v1")
    monkeypatch.setattr(model.settings, "openai_api_key", SecretStr("local-not-a-secret"))
    monkeypatch.setattr(model.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(model.settings, "model_fallback", "qwen3:1.7b")
    configured = model.build_model()
    assert isinstance(configured, model.FallbackModel)
    assert [entry.model for entry in configured.models if isinstance(entry, model.BaseLlm)] == [
        "qwen3:4b-instruct",
        "qwen3:1.7b",
    ]
    assert configured.model == "qwen3:4b-instruct"  # spans and reports attribute the primary


def _chain(primary: _StubLlm, fallback: _StubLlm) -> model.FallbackModel:
    return model.FallbackModel(models=[primary, fallback])


@pytest.mark.parametrize("status", [429, 500, 502, 503, 504])
def test_fallback_engages_on_a_retriable_status_before_responding(status) -> None:
    primary = _StubLlm(model="primary", fail_status=status)
    fallback = _StubLlm(model="fallback", reply="from fallback")
    assert asyncio.run(_collect(_chain(primary, fallback))) == ["from fallback"]


@pytest.mark.parametrize("status", [400, 401, 404, 408])
def test_fallback_does_not_mask_a_request_or_ambiguous_timeout_error(status) -> None:
    """A bad request fails on every model; a 408 may already have been processed and billed."""
    primary = _StubLlm(model="primary", fail_status=status)
    fallback = _StubLlm(model="fallback", reply="from fallback")
    with pytest.raises(_ProviderError, match="primary is down"):
        asyncio.run(_collect(_chain(primary, fallback)))


def test_healthy_primary_is_used_and_fallback_untouched() -> None:
    primary = _StubLlm(model="primary", reply="from primary")
    fallback = _StubLlm(model="fallback", fail_status=503)  # would raise if ever called
    assert asyncio.run(_collect(_chain(primary, fallback))) == ["from primary"]


def test_mid_stream_failure_is_not_masked_by_fallback() -> None:
    primary = _StubLlm(model="primary", reply="partial", fail_after_yield=True)
    fallback = _StubLlm(model="fallback", reply="from fallback")
    with pytest.raises(_ProviderError, match="dropped mid-stream"):
        asyncio.run(_collect(_chain(primary, fallback)))


def test_both_models_down_surfaces_the_fallback_error() -> None:
    primary = _StubLlm(model="primary", fail_status=503)
    fallback = _StubLlm(model="fallback", fail_status=503)
    with pytest.raises(_ProviderError, match="fallback is down"):
        asyncio.run(_collect(_chain(primary, fallback)))


def test_close_model_closes_materialized_primary_and_fallback_clients(monkeypatch) -> None:
    closed = []

    async def record_close() -> None:
        closed.append(asyncio.get_running_loop())

    primary = model.ResilientOpenAILlm(
        model="primary",
        openai_base_url="http://localhost:11434/v1",
        openai_api_key=SecretStr("local-marker"),
        timeout_s=10,
        retries=0,
    )
    fallback = model.ResilientOpenAILlm(
        model="fallback",
        openai_base_url="http://localhost:11434/v1",
        openai_api_key=SecretStr("local-marker"),
        timeout_s=10,
        retries=0,
    )
    primary_client = AsyncMock(spec=AsyncOpenAI)
    primary_client.close.side_effect = record_close
    fallback_client = AsyncMock(spec=AsyncOpenAI)
    fallback_client.close.side_effect = record_close
    monkeypatch.setitem(primary.__dict__, "_openai_client", primary_client)
    monkeypatch.setitem(fallback.__dict__, "_openai_client", fallback_client)
    chain = model.FallbackModel(models=[primary, fallback])

    async def close() -> None:
        expected_loop = asyncio.get_running_loop()
        await model.close_model(chain)
        assert closed == [expected_loop, expected_loop]

    asyncio.run(close())


def test_close_model_does_not_materialize_an_unused_client() -> None:
    configured = model.ResilientOpenAILlm(
        model="unused",
        openai_base_url="http://localhost:11434/v1",
        openai_api_key=SecretStr("local-marker"),
        timeout_s=10,
        retries=0,
    )

    asyncio.run(model.close_model(configured))

    assert "_openai_client" not in configured.__dict__


def test_close_model_closes_materialized_gemini_async_and_sync_clients(monkeypatch) -> None:
    calls = []

    class AsyncClient:
        async def aclose(self) -> None:
            calls.append(("async", asyncio.get_running_loop()))

    class Client:
        aio = AsyncClient()

        def close(self) -> None:
            calls.append(("sync", asyncio.get_running_loop()))

    configured = Gemini(model="gemini-test")
    monkeypatch.setitem(configured.__dict__, "api_client", Client())

    async def close() -> None:
        expected_loop = asyncio.get_running_loop()
        await model.close_model(configured)
        assert calls == [("async", expected_loop), ("sync", expected_loop)]

    asyncio.run(close())


def test_close_model_skips_fallback_entries_named_by_string() -> None:
    """ADK resolves string entries lazily; closing must not materialize or touch them."""
    closed = model.ResilientOpenAILlm(
        model="primary",
        openai_base_url="http://localhost:11434/v1",
        openai_api_key=SecretStr("local-marker"),
        timeout_s=10,
        retries=0,
    )
    chain = model.FallbackModel(models=[closed, "unresolved-fallback-name"])
    asyncio.run(model.close_model(chain))
    assert "_openai_client" not in closed.__dict__
