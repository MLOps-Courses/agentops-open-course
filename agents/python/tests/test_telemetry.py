"""Unit tests for telemetry setup (Ch. 7.1)."""

import logging

import pytest
from opentelemetry.sdk._logs import LoggerProvider
from opentelemetry.sdk._logs.export import InMemoryLogRecordExporter, SimpleLogRecordProcessor
from opentelemetry.semconv.attributes import exception_attributes
from opentelemetry.trace import NonRecordingSpan, SpanContext, TraceFlags, TraceState, use_span

from agent import telemetry


@pytest.fixture(autouse=True)
def clean_agent_otel_handlers():
    """Keep global logging state isolated across telemetry tests."""
    logger = logging.getLogger("agent")
    for handler in tuple(logger.handlers):
        if isinstance(handler, telemetry._AgentOTelLoggingHandler):  # noqa: SLF001 - setup contract
            logger.removeHandler(handler)
            handler.close()
    yield
    for handler in tuple(logger.handlers):
        if isinstance(handler, telemetry._AgentOTelLoggingHandler):  # noqa: SLF001 - setup contract
            logger.removeHandler(handler)
            handler.close()


def test_setup_telemetry_is_a_safe_noop_without_an_endpoint(monkeypatch) -> None:
    # With no OTLP endpoint configured, setup must not raise or attach a handler.
    for var in (
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
        "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
        "ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS",
        "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT",
    ):
        monkeypatch.delenv(var, raising=False)
    telemetry.setup_telemetry()
    assert telemetry.os.environ["ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS"] == "false"
    assert telemetry.os.environ["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] == "false"
    assert not any(
        isinstance(handler, telemetry._AgentOTelLoggingHandler)  # noqa: SLF001 - setup contract
        for handler in logging.getLogger("agent").handlers
    )


def test_setup_telemetry_delegates_to_adk(monkeypatch) -> None:
    called = False

    def fake_setup() -> None:
        nonlocal called
        called = True

    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", fake_setup)
    telemetry.setup_telemetry()
    assert called


def test_setup_does_not_install_a_handler_when_the_otel_sdk_is_disabled(monkeypatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://collector:4318/v1/logs")
    monkeypatch.setenv("OTEL_SDK_DISABLED", "true")
    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", lambda: None)
    telemetry.setup_telemetry()
    assert not any(
        isinstance(handler, telemetry._AgentOTelLoggingHandler)  # noqa: SLF001 - setup contract
        for handler in logging.getLogger("agent").handlers
    )


def test_trace_only_endpoint_does_not_install_a_log_handler(monkeypatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/v1/traces")
    monkeypatch.setenv("OTEL_SDK_DISABLED", "false")
    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", lambda: None)
    telemetry.setup_telemetry()
    assert not any(
        isinstance(handler, telemetry._AgentOTelLoggingHandler)  # noqa: SLF001 - setup contract
        for handler in logging.getLogger("agent").handlers
    )


def test_combined_endpoint_installs_a_log_handler(monkeypatch) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
    monkeypatch.setenv("OTEL_SDK_DISABLED", "false")
    provider = LoggerProvider(shutdown_on_exit=False)
    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", lambda: None)
    monkeypatch.setattr(telemetry.otel_logs, "get_logger_provider", lambda: provider)
    telemetry.setup_telemetry()
    assert any(
        isinstance(handler, telemetry._AgentOTelLoggingHandler)  # noqa: SLF001 - setup contract
        for handler in logging.getLogger("agent").handlers
    )
    provider.shutdown()


def test_export_bounding_recurses_through_structured_values(monkeypatch) -> None:
    monkeypatch.setattr(telemetry, "redact_persisted_value", lambda value: value)
    long_text = "x" * 3000

    bounded = telemetry._bounded_value(  # noqa: SLF001 - structured export policy
        {"items": [long_text, 7], "tuple": ("short", long_text), "enabled": True}
    )

    assert bounded["items"][0].endswith("... [truncated]")
    assert len(bounded["items"][0]) == telemetry._OTEL_LOG_MESSAGE_MAX_CHARS  # noqa: SLF001
    assert bounded["items"][1] == 7
    assert bounded["tuple"][0] == "short"
    assert bounded["tuple"][1].endswith("... [truncated]")
    assert bounded["enabled"] is True


def test_export_filter_fails_closed_when_local_redaction_fails(monkeypatch) -> None:
    def fail_redaction(_value):
        raise RuntimeError("recognizer unavailable")

    monkeypatch.setattr(telemetry, "_bounded_value", fail_redaction)
    record = logging.makeLogRecord(
        {
            "name": "agent.test",
            "levelno": logging.ERROR,
            "levelname": "ERROR",
            "msg": "raw secret",
            "args": (),
            "credential": "never-export-this",
        }
    )

    filtered = telemetry._SafeOTelLogFilter().filter(record)  # noqa: SLF001 - fail-closed policy

    assert filtered.getMessage() == "[log body omitted: local redaction unavailable]"
    assert not hasattr(filtered, "credential")
    assert record.getMessage() == "raw secret"
    assert vars(record)["credential"] == "never-export-this"


def test_agent_logs_export_one_safe_trace_correlated_copy_without_duplicate_handlers(monkeypatch, caplog) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "http://collector:4318/v1/logs")
    monkeypatch.setenv("OTEL_SDK_DISABLED", "false")
    exporter = InMemoryLogRecordExporter()
    provider = LoggerProvider(shutdown_on_exit=False)
    provider.add_log_record_processor(SimpleLogRecordProcessor(exporter))
    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", lambda: None)
    monkeypatch.setattr(telemetry.otel_logs, "get_logger_provider", lambda: provider)

    telemetry.setup_telemetry()
    telemetry.setup_telemetry()
    handlers = [
        handler
        for handler in logging.getLogger("agent").handlers
        if isinstance(handler, telemetry._AgentOTelLoggingHandler)  # noqa: SLF001 - setup contract
    ]
    assert len(handlers) == 1

    trace_id = 0x123456789ABCDEF123456789ABCDEF12
    span_id = 0x123456789ABCDEF1
    span = NonRecordingSpan(
        SpanContext(
            trace_id=trace_id,
            span_id=span_id,
            is_remote=False,
            trace_flags=TraceFlags(TraceFlags.SAMPLED),
            trace_state=TraceState(),
        )
    )
    logger = logging.getLogger("agent.telemetry-test")
    message = "Email jane.doe@acme.com with token=super-secret-token-123456. " + ("x" * 3000)
    try:
        raise RuntimeError("raw exception body with api_key=never-export-this")
    except RuntimeError:
        with use_span(span, end_on_exit=False):
            logger.warning(message, exc_info=True)

    assert provider.force_flush()
    exported = exporter.get_finished_logs()
    assert len(exported) == 1
    record = exported[0].log_record
    body = str(record.body)
    assert record.trace_id == trace_id
    assert record.span_id == span_id
    assert len(body) <= telemetry._OTEL_LOG_MESSAGE_MAX_CHARS  # noqa: SLF001 - bounded export contract
    assert body.endswith("... [truncated]")
    assert "jane.doe@acme.com" not in body
    assert "super-secret-token-123456" not in body
    assert "<EMAIL_ADDRESS>" in body
    assert "token=<SECRET>" in body
    attributes = record.attributes
    assert attributes is not None
    assert attributes[exception_attributes.EXCEPTION_TYPE] == "RuntimeError"
    assert exception_attributes.EXCEPTION_MESSAGE not in attributes
    assert exception_attributes.EXCEPTION_STACKTRACE not in attributes
    assert "never-export-this" not in str(attributes)
    # The handler filter returned a copy, so pytest's console-side capture saw
    # the original record rather than the redacted OTLP body.
    assert "jane.doe@acme.com" in caplog.text
    assert "never-export-this" in caplog.text
    provider.shutdown()
