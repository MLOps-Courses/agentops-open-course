"""Token/cost regression evidence for the AgentOps Agent (Chapters 4.4 and 7.3).

A prompt or model change can keep every behavioral scorer green while quietly
doubling the tokens or model calls a case costs. The trajectory scorers match
`IN_ORDER` and deliberately tolerate extra reads (Chapter 4.4), so they never
catch that waste. This script runs each committed eval case, records its token
and model-call usage, and compares it against a committed baseline; a case that
grows beyond the tolerance is reported as a regression.

It is model-backed evidence, not a merge gate — like the other live evals it
belongs in the weekly `eval.yml` workflow (Chapter 4.3), not `ci.yml`. No token
counts are committed until you measure them: run `--update` to (re)generate
`cost_baseline.json` from real measurements on your configured model, review the
diff, and commit it. Set `AGENT_COST_TOLERANCE` (default 0.25) to tune strictness.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from typing import Any

try:  # pytest imports this as ``evals.cost_eval``; the CLI runs it with ``evals/`` on sys.path[0]
    from evals.mlflow_eval import _load_cases, ask
except ModuleNotFoundError:  # pragma: no cover - script-invocation fallback
    from mlflow_eval import _load_cases, ask  # ty: ignore[unresolved-import]

_BASELINE = Path(__file__).parent / "cost_baseline.json"
_METRICS = ("total_tokens", "model_calls")
_DEFAULT_TOLERANCE = 0.25


def regressions(
    observed: dict[str, dict[str, int]],
    baseline: dict[str, dict[str, int]],
    tolerance: float = _DEFAULT_TOLERANCE,
) -> list[str]:
    """Return one message per case/metric that exceeds its baseline by > tolerance.

    A missing case (renamed or removed) is not a regression; a new case with no
    baseline is reported by ``main`` as "record a baseline", not here. Only a
    non-zero baseline can regress — a baseline of 0 has no meaningful ratio.
    """
    lines: list[str] = []
    for eval_id in sorted(baseline):
        current = observed.get(eval_id)
        if current is None:
            continue
        for metric in _METRICS:
            base_value = baseline[eval_id].get(metric, 0)
            now = current.get(metric, 0)
            allowed = base_value * (1 + tolerance)
            if base_value and now > allowed:
                lines.append(
                    f"{eval_id} {metric}: {now} > {allowed:.0f} (baseline {base_value}, +{tolerance:.0%} tolerance)"
                )
    return lines


def measure() -> dict[str, dict[str, int]]:
    """Run every committed eval case and return its per-case usage totals."""
    observed: dict[str, dict[str, int]] = {}
    for case in _load_cases():
        inputs: dict[str, Any] = case["inputs"]
        eval_id = inputs["eval_id"]
        usage = ask(inputs["turns"], eval_id)["usage"]
        observed[eval_id] = {metric: int(usage[metric]) for metric in _METRICS}
    return observed


def _write_baseline(observed: dict[str, dict[str, int]]) -> None:
    _BASELINE.write_text(json.dumps(observed, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main() -> None:
    """Measure per-case usage, then record or compare against the baseline."""
    update = "--update" in sys.argv[1:]
    observed = measure()
    for eval_id in sorted(observed):
        usage = observed[eval_id]
        print(f"  {eval_id}: {usage['total_tokens']} tokens, {usage['model_calls']} model calls")  # noqa: T201

    if update or not _BASELINE.exists():
        _write_baseline(observed)
        reason = "Updated" if update else "No baseline found; recorded"
        print(f"\n{reason} {_BASELINE.name} from this run's measurements. Review the diff and commit it.")  # noqa: T201
        return

    baseline = json.loads(_BASELINE.read_text(encoding="utf-8"))
    tolerance = float(os.environ.get("AGENT_COST_TOLERANCE", _DEFAULT_TOLERANCE))
    problems = regressions(observed, baseline, tolerance)
    if problems:
        raise SystemExit("Cost regression against cost_baseline.json:\n  " + "\n  ".join(problems))
    print(f"\nNo token/model-call regression beyond {tolerance:.0%} against {_BASELINE.name}.")  # noqa: T201


if __name__ == "__main__":
    main()
