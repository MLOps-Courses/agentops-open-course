"""OpenTelemetry setup for the Ops Copilot (Chapter 7.1).

ADK already emits OTel spans for every model call, tool call, and workflow node. This helper
wires the exporters so those spans reach self-hosted MLflow or another OTLP-compatible
collector from the standard ``OTEL_*`` environment variables. It is a no-op unless an endpoint
is configured, so importing it is safe. ``adk web`` also exposes its built-in trace view.
"""

from __future__ import annotations

import os

from google.adk.telemetry.setup import maybe_set_otel_providers


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
