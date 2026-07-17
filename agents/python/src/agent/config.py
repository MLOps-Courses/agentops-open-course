"""Typed, fail-fast configuration for local and deployed agent runtimes."""

from __future__ import annotations

from enum import StrEnum
from pathlib import Path

from pydantic import AliasChoices, Field, SecretStr, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

# The committed dataset is immutable input. Runtime SQLite state lives under
# agents/python/.state by default so approving a mock action never dirties git.
_DEFAULT_DATA_DIR = Path(__file__).resolve().parents[3] / "data"
_DEFAULT_STATE_DIR = Path(__file__).resolve().parents[2] / ".state"


class ModelProvider(StrEnum):
    """Supported ADK model adapters."""

    OPENAI_COMPATIBLE = "openai-compatible"
    GEMINI = "gemini"


class Settings(BaseSettings):
    """Agent settings parsed once from ``AGENT_*`` and provider SDK variables.

    Cross-field dependencies are validated at construction — parse, don't
    validate scattered — so a bad combination fails at startup with a message
    that names the fix instead of surfacing as a stack trace deep in a turn.
    """

    model_config = SettingsConfigDict(
        env_prefix="AGENT_",
        extra="ignore",
        # Provider SDK aliases stay constructible by field name in tests.
        populate_by_name=True,
        # Allow AGENT_MODEL_TIMEOUT_S → model_timeout_s despite pydantic's
        # reserved ``model_`` namespace; Settings defines no model_* methods.
        protected_namespaces=(),
    )

    # --8<-- [start:settings-provider-fields]
    model_provider: ModelProvider = ModelProvider.OPENAI_COMPATIBLE
    model: str = Field(default="qwen3:4b-instruct", min_length=1)

    # ``openai-compatible`` describes the ADK client contract, not the
    # deployment topology. Point this URL directly at Ollama for the account-free
    # first run, or at agentgateway when the governed data plane is introduced.
    openai_base_url: str | None = Field(
        default="http://127.0.0.1:11434/v1",
        validation_alias=AliasChoices("OPENAI_BASE_URL"),
    )
    openai_api_key: SecretStr | None = Field(
        default=SecretStr("local-ollama"),
        validation_alias=AliasChoices("OPENAI_API_KEY"),
    )

    # Optional Gemini paths: an AI Studio API key, or Enterprise/Vertex via ADC
    # with an explicit project and location.
    google_api_key: SecretStr | None = Field(
        default=None,
        validation_alias=AliasChoices("GOOGLE_API_KEY"),
    )
    google_genai_use_enterprise: bool = Field(
        default=False,
        validation_alias=AliasChoices("GOOGLE_GENAI_USE_ENTERPRISE"),
    )
    google_cloud_project: str | None = Field(
        default=None,
        validation_alias=AliasChoices("GOOGLE_CLOUD_PROJECT"),
    )
    google_cloud_location: str | None = Field(
        default=None,
        validation_alias=AliasChoices("GOOGLE_CLOUD_LOCATION"),
    )
    # --8<-- [end:settings-provider-fields]
    deprecated_gateway_enabled: str | None = Field(
        default=None,
        validation_alias=AliasChoices("AGENT_GATEWAY_ENABLED"),
        exclude=True,
        repr=False,
    )

    mcp_url: str | None = None
    # Bearer token sent to a JWT/API-key-secured gateway MCP route (Ch. 5.5).
    # Unset for the default unauthenticated local route.
    mcp_token: SecretStr | None = None

    # Optional host dev/eval prompt-registry pin (Ch. 7.0), e.g.
    # prompts:/agentops-agent-instruction/2. The minimal production image omits
    # MLflow and uses the committed instruction; unset needs no MLflow server.
    prompt_uri: str | None = None

    data_dir: Path = _DEFAULT_DATA_DIR
    state_dir: Path = _DEFAULT_STATE_DIR

    # Bind and advertised A2A addresses are deliberately separate. The host
    # runtime stays loopback-only; Kubernetes explicitly opts into 0.0.0.0.
    # Never advertise 0.0.0.0: it is a listener, not a callable endpoint.
    a2a_bind_host: str = Field(default="127.0.0.1", min_length=1)
    a2a_host: str = Field(default="localhost", min_length=1)
    a2a_port: int = Field(default=8080, ge=1, le=65535)
    a2a_protocol: str = Field(default="http", pattern=r"^https?$")
    a2a_max_llm_calls: int = Field(default=12, ge=1, le=100)
    # Opt-in per-token model streaming for A2A requests (Ch. 3.6). Off by default:
    # chunked output weakens PII redaction (entities spanning chunk boundaries are
    # already sent) and the gateway path reports no usage on streamed responses.
    a2a_streaming: bool = False
    # Graceful-shutdown drain: how long in-flight A2A requests may finish after
    # SIGTERM. Kubernetes' terminationGracePeriodSeconds must exceed this.
    drain_timeout_s: float = Field(default=10.0, gt=0, le=300)

    # Resilience: bounded retries with exponential backoff for idempotent reads
    # and model calls; guarded write actions are never retried (Chapter 4.5).
    model_timeout_s: float = Field(default=60.0, gt=0, le=600)
    tool_timeout_s: float = Field(default=30.0, gt=0, le=600)
    max_retries: int = Field(default=2, ge=0, le=10)
    retry_backoff_s: float = Field(default=0.5, gt=0, le=30)

    # Default-on prompt-injection hardening for tool/retrieval content (Ch. 4.6):
    # spotlight untrusted text and neutralize known injection markers.
    sanitize_tool_output: bool = True

    # Opt-in semantic runbook retrieval (Ch. 3.4). Off by default so the test
    # gate stays deterministic and model-free; requires a local Ollama with the
    # embedding model pulled. Falls back to the keyword scorer on failure.
    semantic_retrieval: bool = False
    embeddings_url: str = Field(default="http://127.0.0.1:11434", min_length=1)
    embedding_model: str = Field(default="nomic-embed-text", min_length=1)
    # A cold local embedding model can take substantially longer to load than
    # an ordinary tool call. Keep that startup bounded without forcing every
    # read tool to inherit the same generous deadline.
    embedding_timeout_s: float = Field(default=120.0, gt=0, le=600)

    # Token budgeting and cost attribution (Chapter 7.3). ``None`` disables the
    # budget; prices default to 0 because the reference path is local Ollama.
    max_tokens_per_session: int | None = Field(default=None, ge=1)
    input_price_per_1k: float = Field(default=0.0, ge=0)
    output_price_per_1k: float = Field(default=0.0, ge=0)

    @model_validator(mode="after")
    def _actionable_cross_field_checks(self) -> Settings:
        """Reject invalid combinations with errors that say what to set and why."""
        # --8<-- [start:settings-provider-validation]
        provider_problems: list[str] = []
        if self.deprecated_gateway_enabled is not None:
            provider_problems.append(
                "AGENT_GATEWAY_ENABLED was removed. Keep AGENT_MODEL_PROVIDER=openai-compatible "
                "and select direct Ollama or agentgateway with OPENAI_BASE_URL "
                "(http://127.0.0.1:11434/v1 or http://127.0.0.1:4000/v1)."
            )
        if self.model_provider is ModelProvider.OPENAI_COMPATIBLE and not self.openai_base_url:
            provider_problems.append(
                "AGENT_MODEL_PROVIDER=openai-compatible requires OPENAI_BASE_URL. Use "
                "http://127.0.0.1:11434/v1 for direct Ollama or http://127.0.0.1:4000/v1 "
                "for the host agentgateway model route."
            )
        if self.model_provider is ModelProvider.OPENAI_COMPATIBLE and (
            not self.openai_api_key or not self.openai_api_key.get_secret_value().strip()
        ):
            provider_problems.append(
                "AGENT_MODEL_PROVIDER=openai-compatible requires OPENAI_API_KEY. Ollama and the open "
                "local gateway accept a non-secret marker such as local-ollama."
            )
        google_api_key = self.google_api_key.get_secret_value().strip() if self.google_api_key else ""
        if self.model_provider is ModelProvider.GEMINI:
            if google_api_key and self.google_genai_use_enterprise:
                provider_problems.append(
                    "AGENT_MODEL_PROVIDER=gemini cannot combine GOOGLE_API_KEY with "
                    "GOOGLE_GENAI_USE_ENTERPRISE=true in this course. Choose AI Studio API-key auth "
                    "or the ADC-backed enterprise path."
                )
            elif self.google_genai_use_enterprise:
                missing_enterprise = [
                    name
                    for name, value in (
                        ("GOOGLE_CLOUD_PROJECT", self.google_cloud_project),
                        ("GOOGLE_CLOUD_LOCATION", self.google_cloud_location),
                    )
                    if not isinstance(value, str) or not value.strip()
                ]
                if missing_enterprise:
                    provider_problems.append(
                        "AGENT_MODEL_PROVIDER=gemini with GOOGLE_GENAI_USE_ENTERPRISE=true requires "
                        + " and ".join(missing_enterprise)
                        + " for the ADC-backed course path."
                    )
            elif not google_api_key:
                provider_problems.append(
                    "AGENT_MODEL_PROVIDER=gemini requires either GOOGLE_API_KEY for AI Studio, or "
                    "GOOGLE_GENAI_USE_ENTERPRISE=true with GOOGLE_CLOUD_PROJECT and "
                    "GOOGLE_CLOUD_LOCATION for ADC."
                )
        if provider_problems:
            raise ValueError("\n".join(provider_problems))
        # --8<-- [end:settings-provider-validation]
        problems: list[str] = []
        if self.prompt_uri and not self.prompt_uri.startswith("prompts:/"):
            problems.append(
                f"AGENT_PROMPT_URI must look like prompts:/agentops-agent-instruction/2, got {self.prompt_uri!r}. "
                "Unset it to use the committed instruction."
            )
        if self.mcp_url and not self.mcp_url.startswith(("http://", "https://")):
            problems.append(
                f"AGENT_MCP_URL must be an http(s) URL such as http://127.0.0.1:3000/mcp, got {self.mcp_url!r}. "
                "Unset it to use the in-process stdio MCP server."
            )
        if problems:
            raise ValueError("\n".join(problems))
        return self


settings = Settings()
