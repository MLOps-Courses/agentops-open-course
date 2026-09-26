"""Run ADK's evaluator with the same application-policy boundary as production.

ADK builds its evaluation Runner in one of two shapes. Before 2.10 it received a
bare ``root_agent`` plus two evaluator plugins, bypassing the repository policy
registered on ``agent.composition.app``. From 2.10 it receives the discovered
``App`` with those evaluator plugins appended after the application policy.
Keep the upstream evaluator and its metrics in both shapes, but guarantee the
policy is present exactly once and that evaluator evidence runs before a
short-circuiting policy hook. The guards fail fast if the seam changes again.
"""

from __future__ import annotations

from typing import Any, cast

from google.adk.agents import BaseAgent
from google.adk.apps import App
from google.adk.evaluation import evaluation_generator
from google.adk.plugins import BasePlugin
from google.adk.runners import Runner
from google.adk.workflow import Workflow

from agent.governance import build_app

_ADK_RUNNER = Runner


def _governed_runner(*, plugins: list[BasePlugin] | None = None, app: App | None = None, **kwargs: Any) -> Runner:
    """Restore production policy parity while retaining evaluator instrumentation."""
    if app is not None:
        return _ordered_app_runner(app, plugins, **kwargs)
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


def _ordered_app_runner(app: App, plugins: list[BasePlugin] | None, **kwargs: Any) -> Runner:
    """Keep ADK 2.10's App-based Runner, with evaluator plugins ahead of the policy."""
    if plugins:
        raise RuntimeError("ADK evaluation passed both an App and loose plugins; review the pinned seam")
    root = app.root_agent
    if not isinstance(root, BaseAgent | Workflow):
        raise RuntimeError("ADK evaluation App has no agent or workflow root")
    policy_types = tuple(type(plugin) for plugin in build_app(root).plugins)
    policy_plugins = [plugin for plugin in app.plugins if isinstance(plugin, policy_types)]
    if len(policy_plugins) != len(policy_types):
        raise RuntimeError("ADK evaluation App must carry the application policy exactly once")
    evaluator_plugins = [plugin for plugin in app.plugins if not isinstance(plugin, policy_types)]
    return _ADK_RUNNER(app=app.model_copy(update={"plugins": [*evaluator_plugins, *policy_plugins]}), **kwargs)


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
