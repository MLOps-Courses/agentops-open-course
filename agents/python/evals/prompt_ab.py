"""Side-by-side prompt A/B evaluation for the AgentOps Agent (Chapter 4.4).

Version-pinning and rollback (Chapter 7.0) let you *choose* a prompt version;
this tool tells you which one to choose. It runs the committed eval set through
the four deterministic scorers under two prompt versions and prints a per-scorer
pass-rate table with the delta, so a prompt change is a measured decision, not a
vibe. It is the runnable form of the "compare prompt versions" workflow that
Chapter 7.0 (Reproducibility) describes.

Each version runs in its own subprocess with ``AGENT_PROMPT_URI`` set, because
the agent binds its instruction once at import — a fresh interpreter is the clean
way to evaluate a different pinned version. It is model-backed, so like the other
live evals it belongs in the weekly ``eval.yml`` workflow, not ``ci.yml``.

    uv run python evals/prompt_ab.py \
      prompts:/agentops-agent-instruction/2 prompts:/agentops-agent-instruction/1
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

try:  # pytest imports this as ``evals.prompt_ab``; the CLI runs it with ``evals/`` on sys.path[0]
    from evals.mlflow_eval import (
        _load_cases,
        ask,
        complete_conversation,
        response_facts,
        tool_policy,
        tool_trajectory,
    )
except ModuleNotFoundError:  # pragma: no cover - script-invocation fallback
    from mlflow_eval import (  # ty: ignore[unresolved-import]
        _load_cases,
        ask,
        complete_conversation,
        response_facts,
        tool_policy,
        tool_trajectory,
    )

DETERMINISTIC_SCORERS = {
    "tool_trajectory": tool_trajectory,
    "complete_conversation": complete_conversation,
    "response_facts": response_facts,
    "tool_policy": tool_policy,
}


def score_configured_prompt() -> dict[str, float]:  # pragma: no cover - model-backed, weekly lane
    """Run the eval set under the currently-configured prompt and return pass rates."""
    cases = _load_cases()
    totals = dict.fromkeys(DETERMINISTIC_SCORERS, 0.0)
    for case in cases:
        outputs = ask(case["inputs"]["turns"], case["inputs"]["eval_id"])
        for name, score in DETERMINISTIC_SCORERS.items():
            totals[name] += 1.0 if score(outputs=outputs, expectations=case["expectations"]) else 0.0
    return {name: total / len(cases) for name, total in totals.items()}


def format_comparison(label_a: str, scores_a: dict[str, float], label_b: str, scores_b: dict[str, float]) -> str:
    """Render a deterministic per-scorer pass-rate table with the A→B delta."""
    lines = [f"{'scorer':<22} {label_a:>12} {label_b:>12} {'delta':>8}"]
    for name in DETERMINISTIC_SCORERS:
        a = scores_a.get(name, 0.0)
        b = scores_b.get(name, 0.0)
        lines.append(f"{name:<22} {a:>12.2f} {b:>12.2f} {b - a:>+8.2f}")
    return "\n".join(lines)


def _score_pinned_prompt(prompt_uri: str) -> dict[str, float]:  # pragma: no cover - spawns a model-backed child
    """Score one prompt version in a fresh interpreter with it pinned."""
    environment = {**os.environ, "AGENT_PROMPT_URI": prompt_uri}
    completed = subprocess.run(  # noqa: S603 - fixed argv, no shell
        [sys.executable, __file__, "--score"],
        env=environment,
        capture_output=True,
        text=True,
        check=True,
    )
    return json.loads(completed.stdout.strip().splitlines()[-1])


def main() -> None:  # pragma: no cover - CLI entrypoint (model-backed)
    """Score the current prompt (``--score``) or compare two pinned versions."""
    args = sys.argv[1:]
    if args == ["--score"]:
        print(json.dumps(score_configured_prompt()))  # noqa: T201 - machine-readable child output
        return
    if len(args) != 2:
        raise SystemExit("Usage: prompt_ab.py <prompt-uri-a> <prompt-uri-b>")
    scores_a = _score_pinned_prompt(args[0])
    scores_b = _score_pinned_prompt(args[1])
    print(format_comparison(args[0], scores_a, args[1], scores_b))  # noqa: T201 - CLI output


if __name__ == "__main__":
    main()
