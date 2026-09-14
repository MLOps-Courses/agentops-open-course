"""Cumulative Python exercises with explicit starts, offline checks, and solutions."""

from __future__ import annotations

import argparse
import importlib.util
import json
import shutil
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
            count = {1: 0, 2: 2, 3: 3}.get(step, 4)
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

    names = ("test_agent", "test_tools", "test_state", "test_approval", "test_workflow", "test_evaluation")
    return unittest.TestSuite(Outcomes(name) for name in names[:step])


def check(path: Path, step: int) -> bool:
    """Run checks with no provider call; preserve the learner's exception details locally."""
    result = unittest.TextTestRunner(verbosity=2).run(checks(load(path), step))
    return result.wasSuccessful()


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
    shutil.copyfile(source, destination / "agent.py")
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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("list", "start", "check", "run", "solution", "verify", "record"))
    parser.add_argument("step", type=int, choices=STEPS, nargs="?", default=1)
    parser.add_argument("--workflow", action="store_true", help="Run the bounded workflow from step 5 or 6")
    parser.add_argument("--reference", action="store_true", help="Start from the preceding worked solution")
    args = parser.parse_args()
    path = WORK / f"step-{args.step}" / "learner_agent" / "agent.py"
    try:
        match args.command:
            case "list":
                for number, outcome in STEPS.items():
                    print(f"{number}: {outcome}")
            case "start":
                print(start(args.step, reference=args.reference).relative_to(ROOT))
            case "solution":
                print((LABS / "solutions" / f"step_{args.step}.py").read_text())
            case "check":
                if not path.is_file():
                    raise ValueError(f"Start step {args.step} before checking it.")
                return 0 if check(path, args.step) else 1
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
