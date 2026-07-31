"""Create or verify a local, sanitized course-completion manifest.

The manifest is intentionally modest: it records an exact source revision, the
deterministic gates executed at that revision, and hashes of repository-owned
inputs. It is not a certificate and contains no command output, environment, or
credential-bearing telemetry.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import pathlib
import shutil
import subprocess
import sys
from typing import Final

ROOT: Final = pathlib.Path(__file__).resolve().parent.parent
DEFAULT_OUTPUT: Final = ROOT / ".agents/tmp/course-completion.json"
GATES: Final = (("mise", "run", "check:core"), ("mise", "run", "test"))
ARTIFACTS: Final = (
    "mise.lock",
    "uv.lock",
    "agents/python/uv.lock",
    "agents/data/incidents.db",
    "agents/python/evals/ops.evalset.json",
)


def git(*arguments: str) -> str:
    """Run a read-only Git query and return trimmed output."""
    executable = shutil.which("git")
    if executable is None:
        raise RuntimeError("git is required to record course evidence")
    result = subprocess.run(  # noqa: S603 - resolved executable; arguments are repository-owned.
        (executable, *arguments),
        cwd=ROOT,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    )
    return result.stdout.strip()


def sha256(path: pathlib.Path) -> str:
    """Hash one evidence input without reading it into a manifest."""
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require_clean_revision() -> str:
    """Return HEAD only when the checkout matches a reproducible clean clone."""
    dirty = git("status", "--porcelain", "--untracked-files=all")
    if dirty:
        raise ValueError(
            "tracked or untracked source is dirty; commit, remove, or restore it before recording evidence"
        )
    return git("rev-parse", "HEAD")


def run_gates() -> list[dict[str, str]]:
    """Execute the deterministic course gates without retaining their output."""
    results: list[dict[str, str]] = []
    for command in GATES:
        completed = subprocess.run(  # noqa: S603 - every command is the fixed GATES constant.
            command,
            cwd=ROOT,
            check=False,
        )
        if completed.returncode:
            raise RuntimeError(f"{' '.join(command)} failed with exit code {completed.returncode}")
        results.append({"command": " ".join(command), "result": "passed"})
    return results


def artifact_records() -> list[dict[str, str]]:
    """Return stable hashes of the inputs most relevant to learner proof."""
    return [{"path": name, "sha256": sha256(ROOT / name)} for name in ARTIFACTS]


def create(output: pathlib.Path) -> None:
    """Run the gates and write one sanitized manifest."""
    revision = require_clean_revision()
    gates = run_gates()
    manifest = {
        "format": 1,
        "course": "MLOps-Courses/agentops-open-course",
        "revision": revision,
        "generated_at": datetime.datetime.now(datetime.UTC).replace(microsecond=0).isoformat(),
        "claim": "deterministic offline course gates at the recorded source revision",
        "gates": gates,
        "artifacts": artifact_records(),
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    sys.stdout.write(f"course evidence: wrote {output}\n")


def verify(manifest_path: pathlib.Path) -> None:
    """Verify source and artifacts, then rerun the same gates."""
    document = json.loads(manifest_path.read_text(encoding="utf-8"))
    if not isinstance(document, dict) or document.get("format") != 1:
        raise ValueError("manifest format must be 1")
    revision = require_clean_revision()
    if document.get("revision") != revision:
        raise ValueError(f"manifest revision {document.get('revision')!r} does not match HEAD {revision}")
    expected_gates = [{"command": " ".join(command), "result": "passed"} for command in GATES]
    if document.get("gates") != expected_gates:
        raise ValueError("manifest gate inventory does not match this revision's course contract")
    if document.get("artifacts") != artifact_records():
        raise ValueError("manifest artifact hashes do not match this checkout")
    run_gates()
    sys.stdout.write(f"course evidence: verified {revision}\n")


def parser() -> argparse.ArgumentParser:
    """Build the small two-command CLI."""
    command = argparse.ArgumentParser(description=__doc__)
    subcommands = command.add_subparsers(dest="action", required=True)
    create_parser = subcommands.add_parser("create", help="run gates and create a manifest")
    create_parser.add_argument("--output", type=pathlib.Path, default=DEFAULT_OUTPUT)
    verify_parser = subcommands.add_parser("verify", help="verify a manifest from its exact clean clone")
    verify_parser.add_argument("manifest", type=pathlib.Path)
    return command


def main(argv: list[str]) -> int:
    """Dispatch the requested evidence operation with concise errors."""
    arguments = parser().parse_args(argv[1:])
    try:
        if arguments.action == "create":
            create(arguments.output)
        else:
            verify(arguments.manifest)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError, json.JSONDecodeError) as error:
        sys.stderr.write(f"course evidence: {error}\n")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
