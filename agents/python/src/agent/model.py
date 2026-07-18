"""Select direct Ollama, agentgateway, or optional Gemini without adding LiteLLM."""

from __future__ import annotations

import logging
from collections.abc import AsyncGenerator
from functools import cached_property
from typing import Any

from google.adk.models import BaseLlm, Gemini, OpenAILlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types
from openai import AsyncOpenAI
from pydantic import Field, SecretStr

from .config import ModelProvider, settings

logger = logging.getLogger(__name__)


class ResilientOpenAILlm(OpenAILlm):
    """ADK's OpenAI-compatible client with the course's deadline and retry policy.

    ``OpenAILlm`` builds its ``AsyncOpenAI()`` client with SDK defaults. Keep
    the compatibility shim small and pass the already-validated endpoint and
    marker/token directly instead of mutating process-wide environment state.
    """

    openai_base_url: str = Field(exclude=True)
    openai_api_key: SecretStr = Field(exclude=True, repr=False)
    timeout_s: float = Field(exclude=True)
    retries: int = Field(exclude=True)

    @cached_property
    def _openai_client(self) -> AsyncOpenAI:
        return AsyncOpenAI(
            base_url=self.openai_base_url,
            api_key=self.openai_api_key.get_secret_value(),
            timeout=self.timeout_s,
            max_retries=self.retries,
        )


def _retry_options() -> types.HttpRetryOptions:
    """Map the course retry settings onto the google-genai retry policy."""
    return types.HttpRetryOptions(
        attempts=settings.max_retries + 1,
        initial_delay=settings.retry_backoff_s,
        max_delay=min(settings.retry_backoff_s * (2**settings.max_retries), 30.0),
    )


def _gemini_client_kwargs(retry_options: types.HttpRetryOptions) -> dict[str, Any]:
    """Build explicit Gemini auth and per-request HTTP deadline options."""
    http_options = types.HttpOptions(
        timeout=max(1, round(settings.model_timeout_s * 1000)),
        retry_options=retry_options,
    )
    if settings.google_api_key and settings.google_api_key.get_secret_value().strip():
        return {
            "api_key": settings.google_api_key.get_secret_value(),
            "enterprise": False,
            "http_options": http_options,
        }
    if (
        not settings.google_genai_use_enterprise
        or not settings.google_cloud_project
        or not settings.google_cloud_location
    ):
        raise ValueError(
            "AGENT_MODEL_PROVIDER=gemini requires validated GOOGLE_API_KEY or enterprise ADC settings; "
            "run `mise run config:check`."
        )
    return {
        "enterprise": True,
        "project": settings.google_cloud_project,
        "location": settings.google_cloud_location,
        "http_options": http_options,
    }


class FallbackLlm(BaseLlm):
    """Try a primary model, then a secondary when the primary fails outright.

    Retries and timeouts (``model.py``/``resilience.py``) handle a *flaky* call;
    a fallback handles a *dead* one — the primary endpoint is down, overloaded,
    or the model was unloaded. The fallback only engages if the primary raises
    **before yielding any response**: once a (possibly streamed) turn has begun,
    switching models mid-answer would splice two different completions together,
    so a mid-flight error is re-raised instead.
    """

    primary: BaseLlm = Field(exclude=True)
    fallback: BaseLlm = Field(exclude=True)

    async def generate_content_async(
        self, llm_request: LlmRequest, stream: bool = False
    ) -> AsyncGenerator[LlmResponse]:
        yielded = False
        try:
            async for response in self.primary.generate_content_async(llm_request, stream=stream):
                yielded = True
                yield response
            return
        except Exception as error:  # a dead primary is exactly when the fallback earns its keep
            if yielded:
                raise
            logger.warning(
                "Primary model %s failed before responding, falling back to %s: %s",
                self.primary.model,
                self.fallback.model,
                error,
            )
        async for response in self.fallback.generate_content_async(llm_request, stream=stream):
            yield response


def _build_openai_compatible(model: str) -> ResilientOpenAILlm:
    """Build the OSS OpenAI-compatible client for a specific model name."""
    if not settings.openai_base_url or not settings.openai_api_key:
        raise ValueError(
            "AGENT_MODEL_PROVIDER=openai-compatible requires OPENAI_BASE_URL and OPENAI_API_KEY; "
            "run `mise run config:check` for the resolved configuration."
        )
    return ResilientOpenAILlm(
        model=model,
        openai_base_url=settings.openai_base_url,
        openai_api_key=settings.openai_api_key,
        timeout_s=settings.model_timeout_s,
        retries=settings.max_retries,
    )


def _build_single(model: str) -> BaseLlm:
    """Build one ADK model for ``model`` on the configured provider."""
    if settings.model_provider is ModelProvider.GEMINI:
        retry_options = _retry_options()
        return Gemini(
            model=model,
            retry_options=retry_options,
            client_kwargs=_gemini_client_kwargs(retry_options),
        )
    return _build_openai_compatible(model)


# --8<-- [start:build-model]
def build_model() -> str | BaseLlm:
    """Return the configured ADK model implementation.

    Gemini mode uses ADK's native integration. The default account-free mode
    uses ADK's OSS OpenAI-compatible client; ``OPENAI_BASE_URL`` chooses direct
    Ollama or an agentgateway route without changing application code. When
    ``AGENT_MODEL_FALLBACK`` names a second model, the primary is wrapped so a
    dead primary fails over to it on the same provider (Chapter 5.4).
    """
    primary = _build_single(settings.model)
    if settings.model_fallback is None:
        return primary
    return FallbackLlm(model=settings.model, primary=primary, fallback=_build_single(settings.model_fallback))


# --8<-- [end:build-model]
