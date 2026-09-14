"""Load the Gemini provider key into the explicitly selected local course cluster."""

from __future__ import annotations

import json
import os
import shutil
import subprocess


def main() -> None:
    key = os.environ.get("GOOGLE_API_KEY", "").strip()
    if not key:
        raise SystemExit("Set GOOGLE_API_KEY in the root .env before running platform:credentials.")
    kubectl = shutil.which("kubectl")
    if not kubectl:
        raise SystemExit("kubectl is missing; run mise run install:platform first.")
    context = subprocess.check_output(  # noqa: S603 - resolved kubectl with fixed arguments
        [kubectl, "config", "current-context"], text=True
    ).strip()
    if context != "k3d-local":
        raise SystemExit("Select the course-owned k3d-local context before creating its provider secret.")
    manifest = {
        "apiVersion": "v1",
        "kind": "Secret",
        "type": "Opaque",
        "metadata": {"name": "gemini-provider", "namespace": "agentops"},
        "stringData": {"GOOGLE_API_KEY": key},
    }
    # stdin keeps the key out of arguments, files, and shell history. Server-side
    # apply avoids copying it into a kubectl last-applied annotation.
    result = subprocess.run(  # noqa: S603 - resolved executable; credential is sent only through stdin
        [
            kubectl,
            "--context",
            "k3d-local",
            "apply",
            "--server-side",
            "--field-manager",
            "agentops-course",
            "--filename",
            "-",
        ],
        input=json.dumps(manifest),
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        # kubectl may echo submitted resources in errors. Never print provider content.
        raise SystemExit("Provider secret was not applied. Check cluster access, namespace, and field ownership.")
    print("Gemini provider secret configured in k3d-local/agentops; no model call made.")


if __name__ == "__main__":
    main()
