"""Cumulative Python exercises with explicit starts, offline checks, and solutions."""

from __future__ import annotations

import argparse
import asyncio
import importlib.util
import io
import json
import subprocess
import sys
import unittest
from pathlib import Path
from types import ModuleType, SimpleNamespace

LABS = Path(__file__).resolve().parent
ROOT = LABS.parents[2]
WORK = ROOT / "learning"
STEPS = {
    1: "Build and inspect an ADK agent",
    2: "Add typed, read-only incident tools",
    3: "Keep conversation state isolated",
    4: "Require approval before a simulated action",
    5: "Build a bounded read-only workflow",
    6: "Evaluate missing and invented evidence",
    7: "Treat retrieved text as data",
    8: "Share read tools over MCP",
}
# The page that owns each step's concept; printed when a check fails.
PAGES = {
    1: "docs/2. Agents/2.1. First Agent.md",
    2: "docs/3. Capabilities/3.1. Tools.md",
    3: "docs/2. Agents/2.4. Sessions.md",
    4: "docs/3. Capabilities/3.1. Tools.md",
    5: "docs/3. Capabilities/3.5. Workflows.md",
    6: "docs/4. Quality/4.4. Evaluations.md",
    7: "docs/4. Quality/4.5. Guardrails.md",
    8: "docs/3. Capabilities/3.3. MCP.md",
}
READ_TOOLS = ("list_open_incidents", "get_incident", "read_runbook")

# `start N` appends these stubs when the carried-forward file lacks the step's functions.
# They fail loudly until implemented and never attach a tool on the learner's behalf.
SCAFFOLDS = {
    2: '''

# --- Step 2: typed, read-only incident tools ------------------------------------
# Read docs/3. Capabilities/3.1. Tools.md. The seed is immutable: open it with
# sqlite3.connect(f"file:{SEED_DB}?mode=ro", uri=True) and never write to it.
from pathlib import Path

SEED_ROOT = next(p for p in Path(__file__).resolve().parents if (p / "agents/data/incidents.db").is_file())
SEED_DB = SEED_ROOT / "agents/data/incidents.db"


def list_open_incidents() -> list[dict[str, str]]:
    """List open incidents (id, service, severity, status) from the immutable seed."""
    raise NotImplementedError("Step 2: return only the rows whose status is 'open'.")


def get_incident(incident_id: str) -> dict[str, str]:
    """Return one incident, or {"error": ...} for an unknown id. Never invent evidence."""
    raise NotImplementedError("Step 2: look the id up with a parameterized query; report a missing id as an error.")


# TODO(step 2): attach both tools once they work, then run `mise run lab -- check 2`.
# TOOLS = [*TOOLS, list_open_incidents, get_incident]
''',
    3: '''

# --- Step 3: conversation state ---------------------------------------------------
# Read docs/2. Agents/2.4. Sessions.md. ADK injects tool_context; its state belongs
# to one conversation only.
from google.adk.tools import ToolContext


def remember_incident(incident_id: str, tool_context: ToolContext) -> dict[str, str]:
    """Remember a real incident id in this conversation; refuse unknown ids."""
    raise NotImplementedError("Step 3: validate with get_incident, then set tool_context.state['incident_id'].")


# TODO(step 3): attach the tool once it works.
# TOOLS = [*TOOLS, remember_incident]
''',
    4: '''

# --- Step 4: human approval before a simulated action -----------------------------
# Read the confirmation section of docs/3. Capabilities/3.1. Tools.md.
from google.adk.tools import ToolContext


def propose_restart(service: str, tool_context: ToolContext) -> dict[str, str]:
    """Simulate a restart only after approval with a rationale; a replay records nothing new."""
    raise NotImplementedError(
        "Step 4: validate the service, request confirmation first, handle denial, "
        "require payload['rationale'], and record one simulated effect."
    )


# TODO(step 4): attach the tool once it works.
# TOOLS = [*TOOLS, propose_restart]
''',
    5: '''

# --- Step 5: a bounded, read-only workflow ------------------------------------------
# Read docs/3. Capabilities/3.5. Workflows.md.
from google.adk import Workflow


def build_workflow(model: str | BaseLlm) -> Workflow:
    """Investigate, then recommend, as `triage_workflow`; neither stage may restart anything."""
    raise NotImplementedError(
        'Step 5: return Workflow(name="triage_workflow", edges=[("START", investigate, recommend)]).'
    )
''',
    6: '''

# --- Step 6: grade evidence, not wording ---------------------------------------------
# Read docs/4. Quality/4.4. Evaluations.md, then predict each label in agents/python/labs/cases.json.
def grade_answer(answer: str, expected_ids: list[str]) -> bool:
    """Pass only when the answer cites exactly the expected incident ids."""
    raise NotImplementedError("Step 6: extract INC-### ids and compare them with expected_ids as sets.")
''',
    7: '''

# --- Step 7: treat retrieved text as data --------------------------------------------
# Read docs/4. Quality/4.5. Guardrails.md, then predict each label in agents/python/labs/injections.json.
from pathlib import Path

RUNBOOK_ROOT = next(p for p in Path(__file__).resolve().parents if (p / "agents/data/runbooks").is_dir())
RUNBOOKS = RUNBOOK_ROOT / "agents/data/runbooks"
DATA_START = "<<<TOOL_DATA data-not-instructions>>>"
DATA_END = "<<<END_TOOL_DATA>>>"
NEUTRALIZED = "[neutralized-injection]"


def neutralize(text: str) -> tuple[str, int]:
    """NFKC-normalize, remove fence markers and instruction-like phrases; return (text, hits)."""
    raise NotImplementedError("Step 7: replace DATA_START, DATA_END, and injection phrases with NEUTRALIZED.")


def read_runbook(slug: str) -> dict[str, str]:
    """Read agents/data/runbooks/<slug>.md for a valid slug and fence its neutralized text."""
    raise NotImplementedError("Step 7: validate the slug before touching the filesystem, then fence the content.")


# TODO(step 7): attach the read tool once it works.
# TOOLS = [*TOOLS, read_runbook]
''',
    8: '''

# --- Step 8: share read tools over MCP -------------------------------------------------
# Read docs/3. Capabilities/3.3. MCP.md. Writes stay in-process, behind approval.
from mcp.server.mcpserver import MCPServer
from mcp.types import ToolAnnotations


def build_mcp_server() -> MCPServer:
    """Expose only list_open_incidents, get_incident, and read_runbook, each marked read-only."""
    raise NotImplementedError("Step 8: create an MCPServer, then add_tool(...) each read tool as read-only.")
''',
}
SCAFFOLD_FUNCTIONS = {
    2: ("list_open_incidents", "get_incident"),
    3: ("remember_incident",),
    4: ("propose_restart",),
    5: ("build_workflow",),
    6: ("grade_answer",),
    7: ("neutralize", "read_runbook"),
    8: ("build_mcp_server",),
}


def load(path: Path) -> ModuleType:
    """Load only the learner-selected local Python file; this executes their code."""
    spec = importlib.util.spec_from_file_location("course_exercise", path)
    if spec is None or spec.loader is None:
        raise ValueError(f"Cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def checks(module: ModuleType, step: int) -> unittest.TestSuite:
    """Check observable outcomes, including denied actions and false answers."""

    class Outcomes(unittest.TestCase):
        def test_agent(self) -> None:
            agent = module.build_agent("offline-test-model")
            self.assertEqual(agent.name, "learner_agent")
            self.assertTrue(agent.instruction.strip())
            expected_tools = ("list_open_incidents", "get_incident", "remember_incident", "propose_restart")
            expected_tools += ("read_runbook",)  # step 7 adds the only new model-facing tool after step 4
            count = {1: 0, 2: 2, 3: 3, 4: 4, 5: 4, 6: 4}.get(step, 5)
            registered = {getattr(tool, "name", getattr(tool, "__name__", "")) for tool in agent.tools}
            self.assertTrue(set(expected_tools[:count]) <= registered, "Attach the step's tools to the ADK agent")

        def test_tools(self) -> None:
            if step < 2:
                return
            rows = module.list_open_incidents()
            self.assertEqual({row["id"] for row in rows}, {"INC-002", "INC-005", "INC-010"})
            self.assertTrue(all(row["status"] == "open" for row in rows))
            self.assertEqual(module.get_incident("INC-002")["service"], "inventory")
            self.assertIn("error", module.get_incident("INC-999"))
            self.assertIn("error", module.get_incident("' OR 1=1 --"))

        def test_state(self) -> None:
            if step < 3:
                return
            first, second = SimpleNamespace(state={}), SimpleNamespace(state={})
            module.remember_incident("INC-002", first)
            self.assertEqual(first.state["incident_id"], "INC-002")
            self.assertEqual(second.state, {})
            module.remember_incident("INC-999", first)
            self.assertEqual(first.state["incident_id"], "INC-002")

        def test_approval(self) -> None:
            if step < 4:
                return
            requests = []
            context = SimpleNamespace(
                state={},
                tool_confirmation=None,
                function_call_id="invocation-1",
                request_confirmation=lambda **kwargs: requests.append(kwargs),
            )
            self.assertIn("error", module.propose_restart("unknown", context))
            self.assertEqual(requests, [])
            self.assertEqual(module.propose_restart("inventory", context)["status"], "approval-required")
            self.assertEqual(len(requests), 1)
            self.assertEqual(context.state, {})
            context.tool_confirmation = SimpleNamespace(confirmed=False, payload={})
            self.assertEqual(module.propose_restart("inventory", context)["status"], "denied")
            self.assertEqual(context.state, {})
            context.tool_confirmation = SimpleNamespace(confirmed=True, payload={})
            self.assertIn("error", module.propose_restart("inventory", context))
            self.assertEqual(context.state, {})
            context.tool_confirmation.payload = {"rationale": "The fictional runbook supports a restart"}
            result = module.propose_restart("inventory", context)
            self.assertEqual(result["status"], "simulated")
            self.assertEqual(module.propose_restart("inventory", context), result)
            self.assertEqual(len(context.state), 1)

        def test_workflow(self) -> None:
            if step < 5:
                return
            workflow = module.build_workflow("offline-test-model")
            self.assertEqual(len(workflow.edges), 1, "Keep one bounded investigate → recommend path")
            source, *stages = workflow.edges[0]
            self.assertEqual(source, "START")
            self.assertEqual([agent.name for agent in stages], ["investigate", "recommend"])
            for agent in stages:
                self.assertNotIn(module.propose_restart, agent.tools)

        def test_evaluation(self) -> None:
            if step < 6:
                return
            cases = json.loads((LABS / "cases.json").read_text())
            for case in cases:
                with self.subTest(answer=case["answer"]):
                    self.assertEqual(module.grade_answer(case["answer"], case["expected_ids"]), case["passed"])

        def test_guardrail(self) -> None:
            if step < 7:
                return
            runbook = module.read_runbook("service-down")
            self.assertNotIn("error", runbook, "Read an allowlisted runbook slug")
            content = runbook["content"]
            self.assertTrue(content.startswith(module.DATA_START), "Open the fence before the retrieved text")
            self.assertTrue(content.rstrip().endswith(module.DATA_END), "Close the fence after the retrieved text")
            self.assertIn("Runbook: Service Down", content)
            for slug in ("../../.env", "service-down/../../../.env", "unknown-runbook", ""):
                with self.subTest(slug=slug):
                    self.assertIn("error", module.read_runbook(slug), "Reject the slug before any file access")
            for case in json.loads((LABS / "injections.json").read_text()):
                with self.subTest(text=case["text"]):
                    cleaned, hits = module.neutralize(case["text"])
                    self.assertEqual(hits > 0, case["injected"], "Flag attacks and leave benign text alone")
                    if not case["injected"]:
                        self.assertEqual(cleaned, case["text"], "Benign evidence must reach the model unchanged")
                    for phrase in case.get("must_remove", []):
                        self.assertNotIn(phrase.lower(), cleaned.lower())

        def test_mcp(self) -> None:
            if step < 8:
                return
            from mcp import Client

            async def exercise() -> tuple[list, object, object]:
                async with Client(module.build_mcp_server()) as client:
                    listed = await client.list_tools()
                    found = await client.call_tool("get_incident", {"incident_id": "INC-002"})
                    missing = await client.call_tool("get_incident", {"incident_id": "INC-999"})
                    return listed.tools, found, missing

            tools, found, missing = asyncio.run(exercise())
            self.assertEqual({tool.name for tool in tools}, set(READ_TOOLS), "Expose exactly the read tools")
            for tool in tools:
                with self.subTest(tool=tool.name):
                    self.assertTrue(tool.annotations and tool.annotations.read_only_hint, "Mark each tool read-only")
            self.assertFalse(getattr(found, "is_error", True))
            self.assertIn("inventory", str(getattr(found, "content", "")))
            self.assertIn("error", str(getattr(missing, "content", "")))

    names = (
        "test_agent",
        "test_tools",
        "test_state",
        "test_approval",
        "test_workflow",
        "test_evaluation",
        "test_guardrail",
        "test_mcp",
    )
    return unittest.TestSuite(Outcomes(name) for name in names[:step])


def check(path: Path, step: int, *, quiet: bool = False) -> bool:
    """Run checks with no provider call; preserve the learner's exception details locally."""
    stream = io.StringIO() if quiet else sys.stderr
    try:
        suite = checks(load(path), step)
    except Exception as error:  # a learner file that does not import is a failed check, not a crash
        if not quiet:
            print(f"{path.relative_to(ROOT)} does not load: {type(error).__name__}: {error}", file=sys.stderr)
        return False
    result = unittest.TextTestRunner(stream=stream, verbosity=0 if quiet else 2).run(suite)
    return result.wasSuccessful()


def scaffold(source: str, step: int) -> str:
    """Append the step's stubs unless the learner already defined those functions."""
    if step not in SCAFFOLDS or all(f"def {name}(" in source for name in SCAFFOLD_FUNCTIONS[step]):
        return source
    return source.rstrip("\n") + "\n" + SCAFFOLDS[step]


def start(step: int, *, reference: bool) -> Path:
    """Carry previous learner work forward; never overwrite an existing exercise."""
    destination = WORK / f"step-{step}" / "learner_agent"
    if destination.parent.exists():
        raise ValueError(
            f"{destination.parent.relative_to(ROOT)} already exists; continue editing it. No files changed."
        )
    previous = WORK / f"step-{step - 1}" / "learner_agent" / "agent.py"
    source = LABS / "solutions" / f"step_{max(1, step - 1)}.py" if reference or step == 1 else previous
    if not source.is_file():
        raise ValueError(
            f"Finish step {step - 1} first, or use start {step} --reference for its tested reference checkpoint."
        )
    destination.mkdir(parents=True)
    (destination / "agent.py").write_text(scaffold(source.read_text(), step))
    (destination / "__init__.py").write_text(
        '"""ADK discovery for this learner-owned exercise."""\n'
        "import os\n"
        "from google.adk.apps import App\n"
        "from agent.model import build_model\n"
        "from . import agent\n"
        "factory = agent.build_workflow if os.environ.get('COURSE_LAB_WORKFLOW') == '1' else agent.build_agent\n"
        "app = App(name='learner_agent', root_agent=factory(build_model()))\n"
    )
    return destination / "agent.py"


def status() -> list[tuple[int, str]]:
    """Return each step's offline state without printing learner tracebacks."""
    states = []
    for number in STEPS:
        path = WORK / f"step-{number}" / "learner_agent" / "agent.py"
        if not path.is_file():
            states.append((number, "not started"))
        else:
            states.append((number, "passing" if check(path, number, quiet=True) else "failing"))
    return states


def next_command(states: list[tuple[int, str]]) -> str:
    """Name the one command that moves the learner forward."""
    for number, state in states:
        if state == "failing":
            return f"edit learning/step-{number}/learner_agent/agent.py, then: mise run lab -- check {number}"
        if state == "not started":
            return f"mise run lab -- start {number}"
    return "all steps pass: continue to docs/4. Quality/4.8. Developer Handoff.md"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("list", "status", "start", "check", "run", "solution", "verify", "record"))
    parser.add_argument("step", type=int, choices=STEPS, nargs="?", default=1)
    parser.add_argument("--workflow", action="store_true", help="Run the bounded workflow from step 5 onward")
    parser.add_argument("--reference", action="store_true", help="Start from the preceding worked solution")
    args = parser.parse_args()
    path = WORK / f"step-{args.step}" / "learner_agent" / "agent.py"
    try:
        match args.command:
            case "list":
                for number, outcome in STEPS.items():
                    print(f"{number}: {outcome}")
            case "status":
                states = status()
                marks = {"passing": "[x]", "failing": "[!]", "not started": "[ ]"}
                for number, state in states:
                    print(f"{marks[state]} {number}. {STEPS[number]:<45} {state}")
                print(f"\nNext: {next_command(states)}")
            case "start":
                created = start(args.step, reference=args.reference).relative_to(ROOT)
                print(created)
                print(f"Next: edit {created}, then run `mise run lab -- check {args.step}`.")
            case "solution":
                print((LABS / "solutions" / f"step_{args.step}.py").read_text())
            case "check":
                if not path.is_file():
                    raise ValueError(f"Start step {args.step} before checking it.")
                if not check(path, args.step):
                    print(
                        f"\nStep {args.step} is not passing yet. Read {PAGES[args.step]}. "
                        f"After your own attempt, compare with `mise run lab -- solution {args.step}`."
                    )
                    return 1
                following = args.step + 1
                print(
                    f"\nStep {args.step} passes. Next: mise run lab -- start {following}"
                    if following in STEPS
                    else "\nAll steps pass. Continue to docs/4. Quality/4.8. Developer Handoff.md."
                )
            case "verify":
                results = [check(LABS / "solutions" / f"step_{step}.py", step) for step in STEPS]
                return 0 if all(results) else 1
            case "run":
                if not path.is_file():
                    raise ValueError(f"Start step {args.step} before running it.")
                # Only this explicit command loads provider credentials and starts a dev UI.
                import os

                from dotenv import load_dotenv

                if args.workflow and args.step < 5:
                    raise ValueError("The workflow is introduced in step 5.")
                load_dotenv(ROOT / ".env", override=False)
                return subprocess.run(  # noqa: S603 - fixed ADK executable and validated local exercise paths
                    [
                        str(Path(sys.executable).with_name("adk")),
                        "web",
                        str(path.parents[1]),
                        "--host",
                        "127.0.0.1",
                        "--port",
                        "8002",
                    ],
                    env={**os.environ, "COURSE_LAB_WORKFLOW": "1" if args.workflow else "0"},
                    check=False,
                ).returncode
            case "record":
                if args.step != 6:
                    raise ValueError("Use record 6 after completing the evaluation checkpoint.")
                if not path.is_file() or not check(path, 6):
                    raise ValueError("Complete and check step 6 before recording evaluation evidence.")
                try:
                    import mlflow
                except ImportError as error:
                    raise ValueError(
                        "Install the optional recorder: cd agents/python && mise run install:eval"
                    ) from error

                module = load(path)
                cases = json.loads((LABS / "cases.json").read_text())
                # A local file-backed database is enough here; no server or model is started.
                mlflow.set_tracking_uri(f"sqlite:///{WORK / 'evaluations.db'}")
                mlflow.set_experiment("course-workshop")
                with mlflow.start_run() as run:
                    matches = sum(module.grade_answer(c["answer"], c["expected_ids"]) == c["passed"] for c in cases)
                    mlflow.log_metric("grader_label_agreement", matches / len(cases))
                    mlflow.log_param("evidence_kind", "offline-grader-calibration")
                    mlflow.log_artifact(str(path))
                    mlflow.log_artifact(str(LABS / "cases.json"))
                    print(f"Recorded offline grader calibration: {run.info.run_id}")
    except (ValueError, FileNotFoundError) as error:
        parser.exit(2, f"{error}\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
