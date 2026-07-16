"""OpenTelemetry setup for the Ops Copilot (Chapter 7.1).

ADK emits spans and configures OTLP providers; Python logging still needs an
explicit ``LoggingHandler`` bridge. This module installs one deduplicated bridge
on the ``agent`` logger when an OTLP logs endpoint is configured. Only that
handler sees a copied, locally redacted, bounded record with traceback bodies
removed; console handlers continue to receive the original record. With no
endpoint, setup remains a no-op. ``adk web`` also exposes its built-in trace view.
"""

from __future__ import annotations

import logging
import os
import threading
import warnings
from typing import Any

from google.adk.telemetry.setup import maybe_set_otel_providers
from opentelemetry import _logs as otel_logs
from opentelemetry.sdk._logs import LoggingHandler
from opentelemetry.semconv.attributes import exception_attributes

from .pii import redact_persisted_value

_AGENT_LOGGER_NAME = "agent"
_OTEL_LOG_MESSAGE_MAX_CHARS = 2048
_TRUNCATED_SUFFIX = "... [truncated]"
_STANDARD_LOG_RECORD_KEYS = frozenset(logging.makeLogRecord({}).__dict__) | {"asctime", "message"}
_HANDLER_LOCK = threading.Lock()


def _bounded_text(text: str) -> str:
    if len(text) <= _OTEL_LOG_MESSAGE_MAX_CHARS:
        return text
    return text[: _OTEL_LOG_MESSAGE_MAX_CHARS - len(_TRUNCATED_SUFFIX)] + _TRUNCATED_SUFFIX


def _bounded_value(value: Any) -> Any:
    """Cap every exported string after applying the local persistence policy."""

    def cap(redacted: Any) -> Any:
        if isinstance(redacted, str):
            return _bounded_text(redacted)
        if isinstance(redacted, dict):
            return {key: cap(item) for key, item in redacted.items()}
        if isinstance(redacted, list):
            return [cap(item) for item in redacted]
        if isinstance(redacted, tuple):
            return tuple(cap(item) for item in redacted)
        return redacted

    return cap(redact_persisted_value(value))


class _SafeOTelLogFilter(logging.Filter):
    """Return a safe copy for OTLP without mutating the console record."""

    def filter(self, record: logging.LogRecord) -> logging.LogRecord:
        values = vars(record).copy()
        exception_type = record.exc_info[0].__name__ if record.exc_info and record.exc_info[0] else None
        try:
            values["msg"] = _bounded_value(record.getMessage())
            for key, value in tuple(values.items()):
                if key not in _STANDARD_LOG_RECORD_KEYS:
                    values[key] = _bounded_value(value)
        except Exception:
            # A third-party local recognizer failure must drop content, never
            # fall back to exporting the original potentially sensitive body.
            values["msg"] = "[log body omitted: local redaction unavailable]"
            for key in tuple(values):
                if key not in _STANDARD_LOG_RECORD_KEYS:
                    values.pop(key)
        values["args"] = ()
        values["exc_info"] = None
        values["exc_text"] = None
        values["stack_info"] = None
        if exception_type:
            values[exception_attributes.EXCEPTION_TYPE] = exception_type
        return logging.makeLogRecord(values)


class _AgentOTelLoggingHandler(LoggingHandler):
    """Marker subclass used to make setup idempotent."""


def _otel_logging_configured() -> bool:
    disabled = os.environ.get("OTEL_SDK_DISABLED", "").strip().lower()
    if disabled in {"1", "true", "yes"}:
        return False
    return bool(os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"))


def _install_agent_log_handler() -> None:
    logger = logging.getLogger(_AGENT_LOGGER_NAME)
    with _HANDLER_LOCK:
        if any(isinstance(handler, _AgentOTelLoggingHandler) for handler in logger.handlers):
            return
        # The stable SDK still owns the handler in the locked 1.42 stack; its
        # suggested replacement lives in a separately versioned beta package.
        with warnings.catch_warnings():
            warnings.filterwarnings(
                "ignore",
                message=(
                    r"`LoggingHandler` in `opentelemetry-sdk` is deprecated\. "
                    r"Use the handler from `opentelemetry-instrumentation-logging` instead\."
                ),
                category=DeprecationWarning,
                module=r"opentelemetry\.sdk\._logs\._internal",
            )
            handler = _AgentOTelLoggingHandler(logger_provider=otel_logs.get_logger_provider())
        handler.addFilter(_SafeOTelLogFilter())
        logger.addHandler(handler)


def setup_telemetry() -> None:
    """Enable OTLP tracing/metrics/logs from ``OTEL_EXPORTER_OTLP_ENDPOINT`` (and friends).

    Call this once at process start for programmatic runs or ``adk run``. With no endpoint,
    nothing is exported.
    """
    # Content capture is opt-in: traces retain timing, model, tool, token, and
    # status metadata without duplicating user prompts or model responses.
    os.environ.setdefault("ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS", "false")
    os.environ.setdefault("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "false")
    maybe_set_otel_providers()
    if _otel_logging_configured():
        _install_agent_log_handler()
