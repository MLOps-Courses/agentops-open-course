"""Typed, fail-fast configuration for local and deployed agent runtimes."""

from __future__ import annotations

from pathlib import Path

from pydantic import AliasChoices, Field, SecretStr, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

# The committed dataset is immutable input. Runtime SQLite state lives under
# agents/python/.state by default so approving a mock action never dirties git.
_DEFAULT_DATA_DIR = Path(__file__).resolve().parents[3] / "data"
_DEFAULT_STATE_DIR = Path(__file__).resolve().parents[2] / ".state"


class Settings(BaseSettings):
    """Agent settings parsed once from ``AGENT_*`` environment variables.

    Cross-field dependencies are validated at construction — parse, don't
    validate scattered — so a bad combination fails at startup with a message
    that names the fix instead of surfacing as a stack trace deep in a turn.
    """

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        env_prefix="AGENT_",
        extra="ignore",
        # Aliased fields (OPENAI_*) stay constructible by field name in tests.
        populate_by_name=True,
        # Allow AGENT_MODEL_TIMEOUT_S → model_timeout_s despite pydantic's
        # reserved ``model_`` namespace; Settings defines no model_* methods.
        protected_namespaces=(),
    )

    model: str = Field(default="gemini-3.5-flash", min_length=1)
    gateway_enabled: bool = False
    mcp_url: str | None = None
    # Bearer token sent to a JWT/API-key-secured gateway MCP route (Ch. 5.5).
    # Unset for the default unauthenticated local route.
    mcp_token: SecretStr | None = None

    # Optional prompt-registry pin (Ch. 7.0), e.g. prompts:/ops-copilot-instruction/2.
    # Unset = the committed instruction, so the offline gate needs no MLflow server.
    prompt_uri: str | None = None

    # The OpenAI SDK inside ADK's OpenAILlm needs these two. They are mirrored
    # here (unprefixed aliases) so a missing one fails at startup, not mid-turn.
    openai_base_url: str | None = Field(default=None, validation_alias=AliasChoices("OPENAI_BASE_URL"))
    openai_api_key: SecretStr | None = Field(default=None, validation_alias=AliasChoices("OPENAI_API_KEY"))

    data_dir: Path = _DEFAULT_DATA_DIR
    state_dir: Path = _DEFAULT_STATE_DIR

    # Bind and advertised A2A addresses are deliberately separate. Never
    # advertise 0.0.0.0: it is a listener address, not a callable endpoint.
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

    # Opt-in prompt-injection hardening for tool/retrieval content (Ch. 4.6):
    # spotlight untrusted text and neutralize known injection markers.
    sanitize_tool_output: bool = False

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
        problems: list[str] = []
        if self.gateway_enabled and not self.openai_base_url:
            problems.append(
                "AGENT_GATEWAY_ENABLED=true requires OPENAI_BASE_URL, the gateway's OpenAI-compatible "
                "endpoint (local default: http://127.0.0.1:4000/v1). Start it with `mise run gateway:host` "
                "and see docs '5. Gateway'."
            )
        if self.gateway_enabled and not self.openai_api_key:
            problems.append(
                "AGENT_GATEWAY_ENABLED=true requires OPENAI_API_KEY. Use a non-secret local marker such as "
                "local-agentgateway when gateway authentication is off; see .env.example."
            )
        if self.prompt_uri and not self.prompt_uri.startswith("prompts:/"):
            problems.append(
                f"AGENT_PROMPT_URI must look like prompts:/ops-copilot-instruction/2, got {self.prompt_uri!r}. "
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
