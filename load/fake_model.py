#!/usr/bin/env -S uv run --quiet --script
# /// script
# requires-python = ">=3.13"
# dependencies = [
#     "rich>=15.0.0",
#     "typer>=0.24.2",
# ]
# ///
"""Deterministic OpenAI-compatible upstream for platform-only load tests."""

from __future__ import annotations

import json
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Annotated, Any, ClassVar

import typer
from rich.console import Console

app = typer.Typer(add_completion=False, rich_markup_mode="rich")
err = Console(stderr=True)


def _response(request: dict[str, Any]) -> dict[str, Any]:
    """Return the smallest chat-completion response accepted by OpenAI clients."""
    model = request.get("model")
    model_name = model if isinstance(model, str) else "agentops-fake"
    return {
        "id": "chatcmpl-agentops-fake",
        "object": "chat.completion",
        "created": 0,
        "model": model_name,
        "choices": [
            {
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": "Fake model response for platform latency measurement.",
                },
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
    }


class FakeModelHandler(BaseHTTPRequestHandler):
    """Serve health and OpenAI-compatible chat-completion requests."""

    server_version = "AgentOpsFakeModel/1.0"
    protocol_version = "HTTP/1.1"
    allowed_paths: ClassVar[set[str]] = {"/v1/chat/completions", "/api/chat"}

    def do_GET(self) -> None:
        if self.path != "/healthz":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self._write_json(HTTPStatus.OK, {"status": "ok"})

    def do_POST(self) -> None:
        if self.path not in self.allowed_paths:
            self.send_error(HTTPStatus.NOT_FOUND)
            return

        try:
            length = int(self.headers.get("Content-Length", "0"))
            request = json.loads(self.rfile.read(length))
            if not isinstance(request, dict):
                raise ValueError("request body must be a JSON object")
            if request.get("stream") is True:
                raise ValueError("streaming is intentionally unsupported; keep AGENT_A2A_STREAMING=false")
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": {"message": str(error)}})
            return

        self._write_json(HTTPStatus.OK, _response(request))

    def log_message(self, format: str, *args: object) -> None:  # noqa: A002 - stdlib override
        """Keep load-test output focused on k6 rather than per-request access logs."""

    def _write_json(self, status: HTTPStatus, payload: dict[str, Any]) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


@app.command()
def main(
    host: Annotated[str, typer.Option(help="Bind address.")] = "127.0.0.1",
    port: Annotated[int, typer.Option(help="Bind port.", min=1, max=65535)] = 11434,
) -> None:
    """Run the deterministic upstream on the Ollama-compatible host port."""
    try:
        with ThreadingHTTPServer((host, port), FakeModelHandler) as server:
            err.print(f"[dim]Fake model listening on http://{host}:{port}[/dim]")
            try:
                server.serve_forever()
            except KeyboardInterrupt:
                return
    except Exception:
        err.print_exception(show_locals=True)
        raise typer.Exit(code=1) from None


if __name__ == "__main__":
    app()
