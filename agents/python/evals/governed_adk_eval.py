"""Run ADK's evaluator with the same application-policy boundary as production.

ADK 2.6's evaluation generator accepts only a bare ``root_agent`` and builds a
Runner with its two evaluator plugins. That bypasses a repository policy
registered on ``agent.composition.app``. Keep the upstream evaluator and its
metrics, but inject the application policy into the Runner it creates. The
guard below fails fast if the pinned ADK implementation changes this seam.
"""

from __future__ import annotations

from typing import Any, cast

from google.adk.evaluation import evaluation_generator
from google.adk.plugins import BasePlugin
from google.adk.runners import Runner

from agent.governance import build_app

_ADK_RUNNER = Runner


def _governed_runner(*, plugins: list[BasePlugin] | None = None, **kwargs: Any) -> Runner:
    """Restore production policy parity while retaining evaluator instrumentation."""
    evaluator_plugins = list(plugins or [])
    selected_root = kwargs.pop("agent", None)
    if selected_root is None:
        raise RuntimeError("ADK evaluation did not provide the root agent")
    policy_app = build_app(selected_root)
    policy_plugins = policy_app.plugins
    policy_types = tuple(type(plugin) for plugin in policy_plugins)
    if any(isinstance(plugin, policy_types) for plugin in evaluator_plugins):
        raise RuntimeError("ADK evaluation already carries an application policy plugin")
    evaluation_app = policy_app.model_copy(
        # ADK plugins historically ran before agent callbacks. Keep its request
        # evidence and retry instrumentation ahead of the short-circuiting policy.
        update={"plugins": [*evaluator_plugins, *policy_plugins]},
    )
    return _ADK_RUNNER(
        app=evaluation_app,
        **kwargs,
    )


def install_app_policy() -> None:
    """Install the narrow, version-pinned ADK evaluation Runner adapter."""
    current = evaluation_generator.Runner
    if current is _governed_runner:
        return
    if current is not _ADK_RUNNER:
        raise RuntimeError("ADK's evaluation Runner seam changed; review the pinned implementation")
    evaluation_generator.Runner = cast(Any, _governed_runner)


def main() -> None:
    """Delegate to the stock CLI after restoring application-policy parity."""
    install_app_policy()
    from google.adk.cli.cli_tools_click import main as adk_main

    adk_main()


if __name__ == "__main__":
    main()
