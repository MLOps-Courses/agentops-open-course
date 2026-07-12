"""Unit tests for telemetry setup (Ch. 7.1)."""

from agent import telemetry


def test_setup_telemetry_is_a_safe_noop_without_an_endpoint(monkeypatch) -> None:
    # With no OTLP endpoint configured, setup must not raise and must export nothing.
    for var in (
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
        "ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS",
        "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT",
    ):
        monkeypatch.delenv(var, raising=False)
    telemetry.setup_telemetry()  # no exception == pass
    assert telemetry.os.environ["ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS"] == "false"
    assert telemetry.os.environ["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] == "false"


def test_setup_telemetry_delegates_to_adk(monkeypatch) -> None:
    called = False

    def fake_setup() -> None:
        nonlocal called
        called = True

    monkeypatch.setattr(telemetry, "maybe_set_otel_providers", fake_setup)
    telemetry.setup_telemetry()
    assert called
