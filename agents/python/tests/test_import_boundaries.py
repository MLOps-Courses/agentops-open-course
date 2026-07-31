"""Fresh-interpreter tests for lazy ADK discovery and pure MCP startup."""

import os
import subprocess
import sys
from pathlib import Path

import pytest

_PROJECT_DIR = Path(__file__).parents[1]
_SOURCE_DIR = _PROJECT_DIR / "src"


def _run_python(script: str, *, warnings_as_errors: bool = False) -> subprocess.CompletedProcess[str]:
    command = [sys.executable]
    if warnings_as_errors:
        command.extend(["-W", "error"])
    command.extend(["-c", script])
    env = {**os.environ, "PYTHONPATH": str(_SOURCE_DIR)}
    return subprocess.run(  # noqa: S603 - fixed interpreter and test-owned script
        command,
        cwd=_PROJECT_DIR,
        env=env,
        check=False,
        capture_output=True,
        text=True,
    )


def test_mcp_import_does_not_initialize_adk_or_emit_warnings() -> None:
    result = _run_python(
        "import sys; import agent; assert 'agent.composition' not in sys.modules; "
        "import agent.mcp_server; assert 'agent.composition' not in sys.modules",
        warnings_as_errors=True,
    )
    assert result.returncode == 0, result.stderr
    assert result.stderr == ""


@pytest.mark.parametrize(
    "script",
    [
        (
            # ADK discovery prefers a module-level ``App`` over a bare ``root_agent``, so the
            # loader must hand back the governed application — not an agent with no policy.
            "import sys; import agent; assert 'agent.composition' not in sys.modules; "
            "from google.adk.apps import App; "
            "from google.adk.cli.utils.agent_loader import AgentLoader; "
            "loaded = AgentLoader('src').load_agent('agent'); "
            "assert isinstance(loaded, App); assert loaded.name == 'agentops-agent'; "
            "assert loaded.root_agent.name == 'agentops_agent'; "
            "assert [p.name for p in loaded.plugins] == ['agentops_policy']"
        ),
        (
            "import asyncio; from google.adk.cli.cli_eval import get_root_agent; "
            "loaded = asyncio.run(get_root_agent('src/agent')); assert loaded.name == 'agentops_agent'"
        ),
        (
            "import os; from agent.structured_report.agent import root_agent; "
            "assert root_agent.name == 'triage_report_agent'; "
            "assert os.environ['ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS'] == 'false'; "
            "assert os.environ['OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT'] == 'false'"
        ),
        (
            "import asyncio; import os; os.environ['AGENT_ENTRYPOINT'] = 'workflow'; "
            "from google.adk.cli.cli_eval import get_root_agent; "
            "loaded = asyncio.run(get_root_agent('src/agent')); "
            "assert loaded.name == 'triage_workflow'; "
            "assert os.environ['ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS'] == 'false'; "
            "assert os.environ['OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT'] == 'false'"
        ),
        (
            "import asyncio; import os; os.environ['AGENT_ENTRYPOINT'] = 'coordinator'; "
            "from google.adk.cli.cli_eval import get_root_agent; "
            "loaded = asyncio.run(get_root_agent('src/agent')); "
            "assert loaded.name == 'coordinator_agent'; "
            "assert os.environ['ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS'] == 'false'; "
            "assert os.environ['OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT'] == 'false'"
        ),
    ],
)
def test_adk_cli_discovery_resolves_lazy_root_agent(script: str) -> None:
    result = _run_python(script)
    assert result.returncode == 0, result.stderr


@pytest.mark.parametrize("entrypoint", ["agent", "workflow", "coordinator"])
def test_adk_terminal_cli_loads_each_entrypoint_without_a_model_call(entrypoint: str) -> None:
    adk = Path(sys.executable).with_name("adk")
    # Stay offline and independent of a developer's ignored .env, which may
    # select a hosted provider and make a pure import nondeterministic.
    env = {**os.environ, "ADK_DISABLE_LOAD_DOTENV": "true", "AGENT_ENTRYPOINT": entrypoint}
    result = subprocess.run(  # noqa: S603 - venv-owned CLI and fixed arguments
        [adk, "run", "--in_memory", "src/agent"],
        cwd=_PROJECT_DIR,
        env=env,
        input="exit\n",
        check=False,
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 0, result.stderr
