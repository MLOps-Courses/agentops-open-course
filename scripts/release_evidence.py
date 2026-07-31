"""Validate and minimize model-backed evidence for one exact release candidate."""

from __future__ import annotations

import argparse
import json
import re
import sys
import tomllib
from collections.abc import Sequence
from pathlib import Path
from typing import Any, Final

_MODEL_KEYS: Final = {
    "model",
    "digest",
    "ollama_version",
    "context_length",
    "temperature",
}
_SIGNAL_KEYS: Final = {
    "adk_trajectory",
    "structured_report",
    "bounded_workflow",
    "mlflow_scorers",
    "cost_regression",
    "groundedness",
}
_VERDICT_KEYS: Final = {
    "schema_version",
    "repository",
    "sha",
    "run",
    "model",
    "signals",
    "overall",
    "privacy",
}
_SCORER_PACKAGES: Final = {
    "google-adk",
    "mlflow",
    "mlflow-skinny",
    "openai",
    "rouge-score",
}
_PRIVACY: Final = "sanitized verdict only; no prompts, responses, tool data, or model transcripts"


def _object(path: Path) -> dict[str, Any]:
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict):
        raise ValueError(f"{path} must contain one JSON object")
    return document


def _scorer_versions(lock_path: Path) -> dict[str, str]:
    lock = tomllib.loads(lock_path.read_text(encoding="utf-8"))
    packages = lock.get("package")
    if not isinstance(packages, list):
        raise ValueError(f"{lock_path} has no package inventory")
    versions = {
        package["name"]: package["version"]
        for package in packages
        if isinstance(package, dict)
        and package.get("name") in _SCORER_PACKAGES
        and isinstance(package.get("version"), str)
    }
    if set(versions) != _SCORER_PACKAGES:
        missing = ", ".join(sorted(_SCORER_PACKAGES - set(versions)))
        raise ValueError(f"qualifying scorer versions are incomplete: {missing}")
    return versions


def validate_release_evidence(
    model_path: Path,
    verdict_path: Path,
    lock_path: Path,
    *,
    repository: str,
    sha: str,
    run_id: int,
    run_attempt: int,
) -> dict[str, Any]:
    """Return a whitelisted release asset or reject inconsistent/raw evidence."""
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("release evidence SHA must be a full lowercase commit")
    if type(run_id) is not int or run_id < 1:
        raise ValueError("release evidence run ID must be positive")
    if type(run_attempt) is not int or run_attempt < 1:
        raise ValueError("release evidence run attempt must be positive")

    model = _object(model_path)
    if set(model) != _MODEL_KEYS:
        raise ValueError("qualifying Eval model lineage contains unexpected fields")
    if model.get("model") != "qwen3:4b-instruct":
        raise ValueError("qualifying Eval must use qwen3:4b-instruct")
    digest = model.get("digest")
    if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ValueError("qualifying Eval model digest is not an immutable SHA-256")
    if type(model.get("context_length")) is not int or model["context_length"] != 8192:
        raise ValueError("qualifying Eval must use the 8192-token serving window")
    if type(model.get("temperature")) is not int or model["temperature"] != 0:
        raise ValueError("qualifying Eval must use temperature 0")
    ollama_version = model.get("ollama_version")
    if not isinstance(ollama_version, str) or not ollama_version.strip():
        raise ValueError("qualifying Eval did not record the Ollama version")

    verdict = _object(verdict_path)
    if set(verdict) != _VERDICT_KEYS:
        raise ValueError("qualifying Eval verdict contains unexpected fields")
    signals = verdict.get("signals")
    if not isinstance(signals, dict) or set(signals) != _SIGNAL_KEYS:
        raise ValueError("qualifying Eval verdict has an incomplete signal inventory")
    failed = sorted(name for name, outcome in signals.items() if outcome != "success")
    if failed:
        raise ValueError("qualifying Eval verdict contains non-success signals: " + ", ".join(failed))

    run = verdict.get("run")
    expected_url = f"https://github.com/{repository}/actions/runs/{run_id}"
    if (
        type(verdict.get("schema_version")) is not int
        or verdict["schema_version"] != 1
        or verdict.get("repository") != repository
        or verdict.get("sha") != sha
        or not isinstance(run, dict)
        or set(run) != {"id", "attempt", "url"}
        or type(run.get("id")) is not int
        or run.get("id") != run_id
        or type(run.get("attempt")) is not int
        or run.get("attempt") != run_attempt
        or run.get("url") != expected_url
    ):
        raise ValueError("qualifying Eval verdict does not identify the exact run and source")
    verdict_model = verdict.get("model")
    if (
        not isinstance(verdict_model, dict)
        or set(verdict_model) != {"name", "digest"}
        or verdict_model.get("name") != model["model"]
        or verdict_model.get("digest") != model["digest"]
    ):
        raise ValueError("qualifying Eval verdict and model lineage disagree")
    if verdict.get("overall") != "success":
        raise ValueError("qualifying Eval verdict is not successful")
    if verdict.get("privacy") != _PRIVACY:
        raise ValueError("qualifying Eval verdict has an unexpected privacy contract")

    return {
        "schema_version": 1,
        "sha": sha,
        "eval_run": run,
        "model": model,
        "signals": signals,
        "scorer_runtime_versions": _scorer_versions(lock_path),
        "privacy": "sanitized lineage and verdict only; no prompts, responses, tool data, or case payloads",
    }


def main(argv: Sequence[str] | None = None) -> None:
    """Validate the source artifacts and print the minimized release JSON."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", type=Path, required=True)
    parser.add_argument("--verdict", type=Path, required=True)
    parser.add_argument("--lock", type=Path, required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--run-id", type=int, required=True)
    parser.add_argument("--run-attempt", type=int, required=True)
    arguments = parser.parse_args(argv)
    try:
        evidence = validate_release_evidence(
            arguments.model,
            arguments.verdict,
            arguments.lock,
            repository=arguments.repository,
            sha=arguments.sha,
            run_id=arguments.run_id,
            run_attempt=arguments.run_attempt,
        )
    except (OSError, ValueError, json.JSONDecodeError, tomllib.TOMLDecodeError) as error:
        raise SystemExit(f"error: {error}") from error
    sys.stdout.write(json.dumps(evidence, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
