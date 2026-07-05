"""Unit test for telemetry setup (Ch. 7.1)."""

from agent.telemetry import setup_telemetry


def test_setup_telemetry_is_a_safe_noop_without_an_endpoint(monkeypatch) -> None:
    # With no OTLP endpoint configured, setup must not raise and must export nothing.
    for var in ("OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"):
        monkeypatch.delenv(var, raising=False)
    setup_telemetry()  # no exception == pass
