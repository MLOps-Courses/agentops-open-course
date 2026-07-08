"""OpenTelemetry setup for the Ops Copilot (Chapter 7.1).

ADK already emits OTel spans for every model call, tool call, and workflow node. This helper
wires the *exporters* so those spans reach a collector (Jaeger, Grafana Tempo, the gateway's
OTLP endpoint, …) from the standard ``OTEL_*`` environment variables. It is a no-op unless an
endpoint is configured, so importing it is always safe. ``adk web`` shows the same traces in
its built-in trace view without any setup.
"""

from __future__ import annotations

from google.adk.telemetry.setup import maybe_set_otel_providers


def setup_telemetry() -> None:
    """Enable OTLP tracing/metrics/logs from ``OTEL_EXPORTER_OTLP_ENDPOINT`` (and friends).

    Call this once at process start for programmatic runs or ``adk run``; ``adk web`` sets it
    up for you. No endpoint configured → nothing is exported.
    """
    maybe_set_otel_providers()
