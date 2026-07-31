"""Token/cost regression evidence for the AgentOps Agent (Chapters 4.4 and 7.3).

A prompt or model change can keep every behavioral scorer green while quietly
doubling the tokens or model calls a case costs. The trajectory scorers match
`IN_ORDER` and deliberately tolerate extra reads (Chapter 4.4), so they never
catch that waste. This script measures each committed eval case, records its
token and model-call usage, and compares it against a committed baseline; a case
that grows beyond the tolerance is reported as a regression. It calls the model
when run alone and can reuse the immediately preceding MLflow transcript in the
scheduled workflow.

It is model-backed evidence, not a merge gate — like the other live evals it
belongs in the weekly `eval.yml` workflow (Chapter 4.3), not `ci.yml`. No token
counts are committed until you measure them: run `--update` to (re)generate
`cost_baseline.json` from real measurements on your configured model, review the
diff, and commit it. Set `AGENT_COST_TOLERANCE` (default 0.25) to tune strictness.
"""

from __future__ import annotations

import json
import math
import os
import sys
from http.client import HTTPConnection, HTTPException
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

try:  # pytest imports this as ``evals.cost_eval``; the CLI runs it with ``evals/`` on sys.path[0]
    from evals.mlflow_eval import (
        _evaluation_contract_digest,
        _load_cases,
        _prompt_selection,
        ask,
        load_model_observations,
        provider_error_messages,
        tool_trajectory,
    )
    from evals.run_adk_eval import REQUIRED_LIVE_CASES
except ModuleNotFoundError:  # pragma: no cover - script-invocation fallback
    from mlflow_eval import (  # ty: ignore[unresolved-import]
        _evaluation_contract_digest,
        _load_cases,
        _prompt_selection,
        ask,
        load_model_observations,
        provider_error_messages,
        tool_trajectory,
    )
    from run_adk_eval import REQUIRED_LIVE_CASES  # ty: ignore[unresolved-import]

from agent.config import settings

_BASELINE = Path(__file__).parent / "cost_baseline.json"
_OBSERVED = Path(__file__).parent / "cost-observed.json"
_METRICS = ("total_tokens", "model_calls")
_DEFAULT_TOLERANCE = 0.25
_COMPARABLE_IDENTITY_FIELDS = (
    "model_provider",
    "model",
    "model_digest",
    "prompt_selection",
    "evaluation_contract_digest",
    "context_length",
    "ollama_version",
    "temperature",
)


def regressions(
    observed: dict[str, dict[str, int]],
    baseline: dict[str, dict[str, int]],
    tolerance: float = _DEFAULT_TOLERANCE,
) -> list[str]:
    """Return case-set mismatches and metrics that exceed the baseline tolerance.

    The eval-contract digest rejects normal case additions/removals before model
    work starts. The explicit set check here still fails closed if a baseline or
    observation was edited independently of that contract. A non-positive
    baseline is unusable evidence and is reported for replacement.
    """
    lines: list[str] = []
    baseline_ids = set(baseline)
    observed_ids = set(observed)
    lines.extend(
        f"{eval_id}: missing from the observation; regenerate and review the baseline"
        for eval_id in sorted(baseline_ids - observed_ids)
    )
    lines.extend(
        f"{eval_id}: no reviewed baseline; regenerate and review the baseline"
        for eval_id in sorted(observed_ids - baseline_ids)
    )
    for eval_id in sorted(baseline_ids & observed_ids):
        current = observed[eval_id]
        for metric in _METRICS:
            base_value = baseline[eval_id].get(metric, 0)
            now = current.get(metric, 0)
            if isinstance(base_value, bool) or not isinstance(base_value, int) or base_value <= 0:
                lines.append(f"{eval_id} {metric}: baseline {base_value!r} is not comparable; regenerate and review it")
                continue
            allowed = base_value * (1 + tolerance)
            if now > allowed:
                lines.append(
                    f"{eval_id} {metric}: {now} > {allowed:g} (baseline {base_value}, +{tolerance:.0%} tolerance)"
                )
    return lines


def measure() -> dict[str, dict[str, int]]:
    """Measure every committed eval case and return its per-case usage totals."""
    cases = _load_cases()
    observed_path = os.environ.get("AGENT_EVAL_OBSERVED_PATH")
    retained = (
        load_model_observations(
            Path(observed_path),
            expected_cases=cases,
            model_digest=_model_digest(),
        )
        if observed_path
        else None
    )
    observed: dict[str, dict[str, int]] = {}
    for case in cases:
        inputs: dict[str, Any] = case["inputs"]
        eval_id = inputs["eval_id"]
        result = retained[eval_id] if retained is not None else ask(inputs["turns"], eval_id)
        if errors := provider_error_messages(result):
            raise SystemExit(f"Measured model usage case {eval_id!r} contains provider errors: {'; '.join(errors)}")
        if eval_id in REQUIRED_LIVE_CASES and not tool_trajectory(
            outputs=result,
            expectations=case["expectations"],
        ):
            raise SystemExit(
                f"Measured model usage required case {eval_id!r} missed its tool trajectory; "
                "refusing to compare or update a cost baseline."
            )
        usage = result.get("usage")
        observed[eval_id] = _usage_cases(
            {eval_id: usage},
            source="Measured model usage",
        )[eval_id]
    return observed


def _direct_ollama_endpoint() -> tuple[str, int] | None:
    """Return the local Ollama host/port only for the direct default topology."""
    if str(settings.model_provider) != "openai-compatible" or not settings.openai_base_url:
        return None
    parsed = urlsplit(settings.openai_base_url)
    if (
        parsed.scheme != "http"
        or parsed.hostname not in {"127.0.0.1", "localhost"}
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
        or parsed.path.rstrip("/") != "/v1"
    ):
        return None
    try:
        port = parsed.port
    except ValueError:
        return None
    if port != 11434:
        return None
    return parsed.hostname, port


def _model_digest() -> str | None:
    """Resolve an explicit digest, or discover direct local Ollama without remote calls."""
    if explicit := os.environ.get("EVAL_MODEL_DIGEST"):
        return explicit
    endpoint = _direct_ollama_endpoint()
    if endpoint is None:
        return None
    connection = HTTPConnection(*endpoint, timeout=2)
    try:
        # HTTPConnection talks to the validated loopback endpoint directly and
        # never honors HTTP_PROXY/HTTPS_PROXY from the learner's shell.
        connection.request("GET", "/api/tags", headers={"Accept": "application/json"})
        response = connection.getresponse()
        if response.status != 200:
            return None
        document = json.load(response)
    except (OSError, HTTPException, TypeError, ValueError, json.JSONDecodeError):
        return None
    finally:
        connection.close()
    models = document.get("models") if isinstance(document, dict) else None
    if not isinstance(models, list):
        return None
    for candidate in models:
        if not isinstance(candidate, dict):
            continue
        if candidate.get("name") != settings.model and candidate.get("model") != settings.model:
            continue
        digest = candidate.get("digest")
        return digest if isinstance(digest, str) and digest else None
    return None


def _model_metadata(model_digest: str | None) -> dict[str, Any]:
    """Load the scheduled Ollama runtime identity, or mark local evidence unknown."""
    metadata_path = os.environ.get("EVAL_MODEL_METADATA_PATH")
    if not metadata_path:
        return {
            "context_length": None,
            "ollama_version": None,
            "temperature": settings.model_temperature,
        }
    document = _read_json(Path(metadata_path))
    context_length = document.get("context_length") if isinstance(document, dict) else None
    ollama_version = document.get("ollama_version") if isinstance(document, dict) else None
    temperature = document.get("temperature") if isinstance(document, dict) else None
    valid_temperature = (
        not isinstance(temperature, bool)
        and isinstance(temperature, (int, float))
        and math.isfinite(temperature)
        and 0 <= temperature <= 2
    )
    if (
        not isinstance(document, dict)
        or document.get("model") != settings.model
        or document.get("digest") != model_digest
        or isinstance(context_length, bool)
        or not isinstance(context_length, int)
        or context_length <= 0
        or not isinstance(ollama_version, str)
        or not ollama_version
        or not valid_temperature
        or temperature != settings.model_temperature
    ):
        raise SystemExit(
            f"{Path(metadata_path).name} does not match the configured model, digest, context, "
            "Ollama version, or sampling temperature."
        )
    return {
        "context_length": context_length,
        "ollama_version": ollama_version,
        "temperature": temperature,
    }


def _current_identity(model_digest: str | None) -> dict[str, Any]:
    """Identify the prompt, eval contract, source, model, and serving runtime."""
    source_revision = os.environ.get("GITHUB_SHA") or None
    cases = _load_cases()
    return {
        "model_provider": str(settings.model_provider),
        "model": settings.model,
        "model_digest": model_digest,
        "source_revision": source_revision,
        "prompt_selection": _prompt_selection(),
        "evaluation_contract_digest": _evaluation_contract_digest(cases),
        **_model_metadata(model_digest),
    }


def _measurement(observed: dict[str, dict[str, int]], identity: dict[str, Any]) -> dict[str, Any]:
    """Attach the complete evidence identity required to interpret usage."""
    return {
        "schema_version": 2,
        **identity,
        "cases": observed,
    }


def _write_json(path: Path, value: dict[str, Any]) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def _read_json(path: Path) -> Any:
    """Read one evidence document with an actionable failure at the boundary."""
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"{path.name} is unreadable or invalid JSON: {error}") from None


def _usage_cases(value: Any, *, source: str) -> dict[str, dict[str, int]]:
    """Validate that each case has comparable positive model usage."""
    if not isinstance(value, dict) or not value:
        raise SystemExit(f"{source} must contain at least one case with positive integer usage.")
    validated: dict[str, dict[str, int]] = {}
    for eval_id, metrics in value.items():
        if not isinstance(eval_id, str) or not eval_id or not isinstance(metrics, dict):
            raise SystemExit(f"{source} contains an invalid case; regenerate it from real model usage.")
        case: dict[str, int] = {}
        for metric in _METRICS:
            measurement = metrics.get(metric)
            if isinstance(measurement, bool) or not isinstance(measurement, int) or measurement <= 0:
                raise SystemExit(
                    f"{source} case {eval_id!r} needs a positive integer {metric}; "
                    "regenerate it from model usage metadata."
                )
            case[metric] = measurement
        validated[eval_id] = case
    return validated


def _valid_optional_text(value: Any) -> bool:
    """Return whether a provenance string is absent or non-empty."""
    return value is None or (isinstance(value, str) and bool(value))


def _valid_optional_temperature(value: Any) -> bool:
    """Return whether a sampling temperature is absent or in ADK's supported range."""
    return value is None or (
        not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value) and 0 <= value <= 2
    )


def _baseline_cases(document: Any, *, identity: dict[str, Any]) -> dict[str, dict[str, int]]:
    """Validate baseline identity and return its per-case measurements."""
    if (
        not isinstance(document, dict)
        or type(document.get("schema_version")) is not int
        or document["schema_version"] != 2
        or any(field not in document for field in (*_COMPARABLE_IDENTITY_FIELDS, "source_revision"))
        or not isinstance(document.get("cases"), dict)
        or not _valid_optional_text(document.get("model_digest"))
        or not _valid_optional_text(document.get("source_revision"))
        or not isinstance(document.get("prompt_selection"), str)
        or not document["prompt_selection"]
        or not isinstance(document.get("evaluation_contract_digest"), str)
        or not document["evaluation_contract_digest"]
        or (
            document.get("context_length") is not None
            and (
                isinstance(document["context_length"], bool)
                or not isinstance(document["context_length"], int)
                or document["context_length"] <= 0
            )
        )
        or not _valid_optional_text(document.get("ollama_version"))
        or not _valid_optional_temperature(document.get("temperature"))
    ):
        raise SystemExit("cost_baseline.json has an unsupported shape; regenerate it with --update.")
    baseline_model = document.get("model")
    if baseline_model != identity["model"]:
        raise SystemExit(
            f"Cost baseline targets model {baseline_model!r}, not {identity['model']!r}; "
            "inspect cost-observed.json or record a model-specific baseline."
        )
    baseline_provider = document.get("model_provider")
    current_provider = identity["model_provider"]
    if baseline_provider != current_provider:
        raise SystemExit(
            f"Cost baseline targets provider {baseline_provider!r}, not {current_provider!r}; "
            "record a provider-specific baseline."
        )
    if document["model_digest"] != identity["model_digest"]:
        raise SystemExit(
            f"Cost baseline model digest {document['model_digest']!r} does not match "
            f"{identity['model_digest']!r}; "
            "review the model change and regenerate with --update."
        )
    for field in _COMPARABLE_IDENTITY_FIELDS[3:]:
        if document[field] != identity[field]:
            raise SystemExit(
                f"Cost baseline {field} {document[field]!r} does not match {identity[field]!r}; "
                "review the evidence change and regenerate with --update."
            )
    return _usage_cases(document["cases"], source="cost_baseline.json")


def _tolerance(raw: str | None) -> float:
    """Parse a finite, non-negative cost-growth tolerance."""
    try:
        tolerance = float(raw) if raw else _DEFAULT_TOLERANCE
    except ValueError:
        raise SystemExit(f"AGENT_COST_TOLERANCE must be a finite non-negative number, got {raw!r}.") from None
    if not math.isfinite(tolerance) or tolerance < 0:
        raise SystemExit(f"AGENT_COST_TOLERANCE must be a finite non-negative number, got {raw!r}.")
    return tolerance


def main() -> None:
    """Measure per-case usage, then record or compare against the baseline."""
    update = "--update" in sys.argv[1:]
    retained_transcript = bool(os.environ.get("AGENT_EVAL_OBSERVED_PATH"))
    model_digest = _model_digest()
    identity = _current_identity(model_digest)
    baseline: dict[str, dict[str, int]] | None = None
    tolerance = _DEFAULT_TOLERANCE
    if not update and _BASELINE.exists() and not retained_transcript:
        baseline = _baseline_cases(
            _read_json(_BASELINE),
            identity=identity,
        )
        tolerance = _tolerance(os.environ.get("AGENT_COST_TOLERANCE"))

    observed = _usage_cases(measure(), source="Measured model usage")
    measurement = _measurement(observed, identity)
    _write_json(_OBSERVED, measurement)
    for eval_id in sorted(observed):
        usage = observed[eval_id]
        print(f"  {eval_id}: {usage['total_tokens']} tokens, {usage['model_calls']} model calls")  # noqa: T201

    # The scheduled lane already paid for and retained this transcript. Preserve
    # its candidate evidence before an intentional prompt/model identity change
    # rejects comparison with the old baseline. Standalone runs still fail fast
    # above so a stale baseline never triggers avoidable model calls.
    if not update and _BASELINE.exists() and baseline is None:
        baseline = _baseline_cases(
            _read_json(_BASELINE),
            identity=identity,
        )
        tolerance = _tolerance(os.environ.get("AGENT_COST_TOLERANCE"))

    if update or not _BASELINE.exists():
        _write_json(_BASELINE, measurement)
        reason = "Updated" if update else "No baseline found; recorded"
        print(f"\n{reason} {_BASELINE.name} from this run's measurements. Review the diff and commit it.")  # noqa: T201
        return

    if baseline is None:  # defensive: the existing-baseline branch above owns comparison
        raise RuntimeError("cost baseline was not loaded")
    problems = regressions(observed, baseline, tolerance)
    if problems:
        raise SystemExit("Cost regression against cost_baseline.json:\n  " + "\n  ".join(problems))
    print(f"\nNo token/model-call regression beyond {tolerance:.0%} against {_BASELINE.name}.")  # noqa: T201


if __name__ == "__main__":
    main()
