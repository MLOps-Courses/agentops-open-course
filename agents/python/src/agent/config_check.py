"""Actionable configuration check — ``mise run config:check`` (Chapter 1.5).

Loads settings exactly the way every runtime entrypoint does, prints the
resolved effective configuration with secrets masked, and exits non-zero with
the validation errors when a combination is invalid. Extends the ``doctor``
philosophy from the host toolchain to the agent's environment variables.
"""

from __future__ import annotations

import sys

from pydantic import SecretStr, ValidationError


def main() -> int:
    """Validate and print the resolved configuration; return a process exit code."""
    try:
        # The import itself constructs the module-level ``Settings()`` — the same
        # fail-fast path `adk run` takes — and the fresh instance re-reads the
        # current environment when the module was already imported (tests).
        from .config import Settings

        resolved = Settings()
    except ValidationError as error:
        print("Agent configuration is invalid:", file=sys.stderr)
        for issue in error.errors():
            print(f"- {issue['msg']}", file=sys.stderr)
        return 1
    print("Agent configuration is valid. Resolved settings (secrets masked):")
    for name, value in sorted(resolved.model_dump().items()):
        masked = "**********" if isinstance(value, SecretStr) else value
        print(f"- {name} = {masked}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
