"""Run ADK evaluation with an honest process exit status.

The locked ADK CLI prints metric failures in its summary but still exits successfully.
This wrapper preserves the live CLI output and turns that summary into the
process contract used by local tasks and scheduled CI.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

try:  # package import under pytest; direct CLI runs with ``evals/`` on sys.path[0]
    from evals.runtime import require_attributable_runtime
except ModuleNotFoundError:  # pragma: no cover - direct script fallback
    from runtime import require_attributable_runtime  # ty: ignore[unresolved-import]

_SUMMARY_HEADER = re.compile(r"(?m)^Eval Run Summary\s*$")
_SUMMARY = re.compile(
    r"(?m)^[^\s:\r\n][^:\r\n]*:\s*\r?\n"
    r"^[ \t]+Tests passed:\s*(\d+)\s*\r?\n"
    r"^[ \t]+Tests failed:\s*(\d+)\s*$"
)

REQUIRED_LIVE_CASES = (
    "investigation-recalls-context",
    "remediation-loads-skill",
    "restart-needs-approval",
    "resolve-needs-approval",
)


def summary_counts(output: str) -> tuple[int, int] | None:
    """Return counts from one subprocess's final authoritative ADK summary."""
    headers = list(_SUMMARY_HEADER.finditer(output))
    if not headers:
        return None
    # Model text is untrusted and may contain summary-shaped lines. ADK emits its
    # authoritative run summary last, after every model response and tool event.
    summary_section = output[headers[-1].end() :]
    summaries = [(int(passed), int(failed)) for passed, failed in _SUMMARY.findall(summary_section)]
    if not summaries:
        return None
    return sum(item[0] for item in summaries), sum(item[1] for item in summaries)


def verdict_counts(passed: int, failed: int, min_pass_rate: float = 1.0) -> tuple[int, str]:
    """Return a truthful exit code for authoritative aggregate case counts."""
    total = passed + failed
    if not total:
        return 2, "ADK evaluation ran zero cases."
    pass_rate = passed / total
    message = (
        f"ADK evaluation case pass rate: {passed}/{total} ({pass_rate:.0%}); "
        f"required aggregate floor: {min_pass_rate:.0%}."
    )
    return (0, f"{message} Floor met.") if pass_rate >= min_pass_rate else (1, f"{message} Floor missed.")


def verdict(output: str, process_returncode: int, min_pass_rate: float = 1.0) -> tuple[int, str]:
    """Return a truthful exit code for one ADK process and its case floor."""
    if process_returncode:
        return process_returncode, f"ADK evaluation process failed with exit code {process_returncode}."
    counts = summary_counts(output)
    if counts is None:
        return 2, "ADK evaluation produced no run summary."
    return verdict_counts(*counts, min_pass_rate)


def pass_rate(value: str) -> float:
    """Parse a positive pass-rate floor no greater than one."""
    try:
        parsed = float(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be a number greater than 0 and at most 1") from error
    if not 0 < parsed <= 1:
        raise argparse.ArgumentTypeError("must be greater than 0 and at most 1")
    return parsed


def eval_case_ids(eval_set: Path) -> list[str]:
    """Return validated case ids in their committed order."""
    try:
        document = json.loads(eval_set.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"{eval_set} is unreadable or invalid JSON: {error}") from None
    cases = document.get("eval_cases") if isinstance(document, dict) else None
    if not isinstance(cases, list) or not cases:
        raise SystemExit(f"{eval_set} must contain at least one eval case.")

    eval_ids: list[str] = []
    for case in cases:
        eval_id = case.get("eval_id") if isinstance(case, dict) else None
        if not isinstance(eval_id, str) or not eval_id or any(separator in eval_id for separator in ",:"):
            raise SystemExit(f"{eval_set} contains an invalid eval_id for ADK selection.")
        if eval_id in eval_ids:
            raise SystemExit(f"{eval_set} contains duplicate eval_id {eval_id!r}.")
        eval_ids.append(eval_id)
    return eval_ids


def eval_case_selectors(eval_set: Path) -> list[str]:
    """Return one ADK file selector per case so local-model inference stays serial."""
    eval_ids = eval_case_ids(eval_set)
    return [f"{eval_set}:{eval_id}" for eval_id in eval_ids]


def parse_args() -> argparse.Namespace:
    """Parse the eval set and optional shared ADK paths."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("eval_set", type=Path)
    parser.add_argument("--agent", type=Path, default=Path("src/agent"))
    parser.add_argument("--config", type=Path, default=Path("evals/test_config.json"))
    parser.add_argument(
        "--min-pass-rate",
        type=pass_rate,
        default=1.0,
        help="minimum aggregate fraction of strictly passing eval cases (default: 1.0)",
    )
    parser.add_argument(
        "--required-case",
        action="append",
        default=[],
        help="eval id that must pass its strict case contract regardless of the aggregate floor (repeatable)",
    )
    return parser.parse_args()


def main() -> None:  # pragma: no cover - the model-backed subprocess belongs to the scheduled lane
    """Stream ADK output, then enforce the reported evaluation verdict."""
    require_attributable_runtime()
    args = parse_args()
    eval_ids = eval_case_ids(args.eval_set)
    required_cases = set(args.required_case)
    unknown_required = required_cases - set(eval_ids)
    if unknown_required:
        names = ", ".join(repr(name) for name in sorted(unknown_required))
        raise SystemExit(f"Required ADK cases are absent from {args.eval_set}: {names}.")
    total_passed = total_failed = 0
    failed_required_cases: list[str] = []
    # ADK defaults to four concurrent inference cases. A single local CPU model
    # queues those requests until their client timeouts expire, so select one
    # case per subprocess and preserve ADK's native evaluator/output unchanged.
    for eval_id in eval_ids:
        selector = f"{args.eval_set}:{eval_id}"
        with tempfile.TemporaryDirectory(prefix="agentops-adk-eval-") as state_dir:
            command = [
                sys.executable,
                "-m",
                "evals.governed_adk_eval",
                "eval",
                str(args.agent),
                selector,
                "--config_file_path",
                str(args.config),
            ]
            child_env = {**os.environ, "AGENT_STATE_DIR": state_dir}
            process = subprocess.Popen(  # noqa: S603 - fixed executable and argv, never a shell
                command,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1,
                env=child_env,
            )
            if process.stdout is None:  # defensive: PIPE above guarantees a stream
                raise RuntimeError("ADK evaluation process has no output stream.")
            lines: list[str] = []
            for line in process.stdout:
                lines.append(line)
                print(line, end="", flush=True)  # noqa: T201 - preserve the evaluator's live CLI output
            process_returncode = process.wait()
        if process_returncode:
            returncode, message = verdict("".join(lines), process_returncode, args.min_pass_rate)
            print(message, file=sys.stderr)  # noqa: T201 - preserve the truthful process verdict
            raise SystemExit(returncode)
        counts = summary_counts("".join(lines))
        if counts is None:
            print("ADK evaluation produced no run summary.", file=sys.stderr)  # noqa: T201
            raise SystemExit(2)
        total_passed += counts[0]
        total_failed += counts[1]
        if eval_id in required_cases and counts != (1, 0):
            failed_required_cases.append(eval_id)

    returncode, message = verdict_counts(total_passed, total_failed, args.min_pass_rate)
    print(message, file=sys.stderr if returncode else sys.stdout)
    if failed_required_cases:
        names = ", ".join(repr(name) for name in failed_required_cases)
        print(  # noqa: T201 - preserve the truthful process verdict
            f"Required ADK cases failed their strict trajectory contracts: {names}.",
            file=sys.stderr,
        )
        if not returncode:
            returncode = 1
    elif required_cases and not returncode:
        names = ", ".join(sorted(required_cases))
        print(f"Required strict ADK cases passed: {names}.")  # noqa: T201
    raise SystemExit(returncode)


if __name__ == "__main__":
    main()
