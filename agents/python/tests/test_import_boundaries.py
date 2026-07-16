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
        "import sys; import agent; assert 'agent.agent' not in sys.modules; "
        "import agent.mcp_server; assert 'agent.agent' not in sys.modules",
        warnings_as_errors=True,
    )
    assert result.returncode == 0, result.stderr
    assert result.stderr == ""


@pytest.mark.parametrize(
    "script",
    [
        (
            "import sys; import agent; assert 'agent.agent' not in sys.modules; "
            "from google.adk.cli.utils.agent_loader import AgentLoader; "
            "loaded = AgentLoader('src').load_agent('agent'); assert loaded.name == 'agentops_agent'"
        ),
        (
            "from google.adk.cli.cli_eval import get_root_agent; "
            "loaded = get_root_agent('src/agent'); assert loaded.name == 'agentops_agent'"
        ),
    ],
)
def test_adk_cli_discovery_resolves_lazy_root_agent(script: str) -> None:
    result = _run_python(script)
    assert result.returncode == 0, result.stderr
