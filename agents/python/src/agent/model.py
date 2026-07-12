"""Select native Gemini or agentgateway without adding LiteLLM."""

from __future__ import annotations

import os
from functools import cached_property

from google.adk.models import BaseLlm, Gemini, OpenAILlm
from google.genai import types
from openai import AsyncOpenAI

from .config import settings


class ResilientOpenAILlm(OpenAILlm):
    """ADK's OpenAI-compatible client with the course's deadline and retry policy.

    ``OpenAILlm`` builds its ``AsyncOpenAI()`` client with SDK defaults; this
    override is the documented seam to inject a per-request timeout and bounded
    retries (the SDK backs off exponentially on 408/429/5xx by itself).
    """

    @cached_property
    def _openai_client(self) -> AsyncOpenAI:
        return AsyncOpenAI(timeout=settings.model_timeout_s, max_retries=settings.max_retries)


def _retry_options() -> types.HttpRetryOptions:
    """Map the course retry settings onto the google-genai retry policy."""
    return types.HttpRetryOptions(
        attempts=settings.max_retries + 1,
        initial_delay=settings.retry_backoff_s,
        max_delay=settings.model_timeout_s,
    )


def build_model() -> str | BaseLlm:
    """Return the configured ADK model implementation.

    Native mode uses ADK's Gemini integration with an explicit retry policy for
    transient provider failures. Gateway mode uses ADK's OSS OpenAI-compatible
    client; ``OPENAI_BASE_URL`` then points at agentgateway, which translates
    the governed request to Ollama, Vertex AI, or another backend.
    """
    if not settings.gateway_enabled:
        return Gemini(model=settings.model, retry_options=_retry_options())
    # Settings enforces this combination at startup; the guard also covers
    # programmatic mutation of the module-level settings (tests, notebooks).
    if not settings.openai_base_url or not settings.openai_api_key:
        raise ValueError(
            "AGENT_GATEWAY_ENABLED=true requires OPENAI_BASE_URL and OPENAI_API_KEY; "
            "run `mise run config:check` for the resolved configuration."
        )
    # The OpenAI SDK reads its endpoint and key from the environment. Mirror the
    # resolved settings there so values sourced from a .env file also reach it.
    os.environ.setdefault("OPENAI_BASE_URL", settings.openai_base_url)
    os.environ.setdefault("OPENAI_API_KEY", settings.openai_api_key.get_secret_value())
    return ResilientOpenAILlm(model=settings.model)
