"""The workshop preserves learner work and detects broken exercises offline."""

import asyncio

import pytest
from google.adk.models import BaseLlm, LlmResponse
from google.adk.runners import InMemoryRunner
from google.genai import types

from labs import course_lab

from .domain import REFERENCE_DOMAIN


def test_start_preserves_existing_work_and_carries_it_forward(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(course_lab, "ROOT", tmp_path)
    monkeypatch.setattr(course_lab, "WORK", tmp_path / "learning")
    first = course_lab.start(1, reference=False)
    first.write_text(first.read_text() + "\n# learner's explanation\n")
    expected = first.read_bytes()
    with pytest.raises(ValueError, match="already exists"):
        course_lab.start(1, reference=False)
    assert first.read_bytes() == expected
    second = course_lab.start(2, reference=False)
    # The learner's text carries forward verbatim; only the step's failing stubs follow it.
    assert second.read_text().startswith(expected.decode().rstrip("\n"))
    assert "Step 2:" in second.read_text().removeprefix(expected.decode().rstrip("\n"))


def test_joining_later_requires_an_explicit_reference_checkpoint(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(course_lab, "ROOT", tmp_path)
    monkeypatch.setattr(course_lab, "WORK", tmp_path / "learning")
    with pytest.raises(ValueError, match="Finish step 3"):
        course_lab.start(4, reference=False)
    fourth = course_lab.start(4, reference=True)
    reference = (course_lab.LABS / "solutions/step_3.py").read_text()
    assert fourth.read_text().startswith(reference.rstrip("\n"))
    assert "def propose_restart(" in fourth.read_text()


def test_check_rejects_invented_incidents(monkeypatch) -> None:
    module = course_lab.load(course_lab.LABS / "solutions/step_2.py")
    monkeypatch.setattr(module, "get_incident", lambda _incident_id: {"service": REFERENCE_DOMAIN.services.inventory})
    import unittest

    result = unittest.TestResult()
    course_lab.checks(module, 2).run(result)
    assert not result.wasSuccessful()
    assert len(result.failures) == 1


class RecordedToolLoop(BaseLlm):
    calls: int = 0

    async def generate_content_async(self, llm_request, stream=False):
        del llm_request, stream
        self.calls += 1
        if self.calls == 1:
            part = types.Part(function_call=types.FunctionCall(name="list_open_incidents", args={}))
        else:
            part = types.Part(text="Recorded tool response received.")
        yield LlmResponse(content=types.Content(role="model", parts=[part]))


def test_workshop_agent_executes_real_tool_through_adk_without_network() -> None:
    module = course_lab.load(course_lab.LABS / "solutions/step_6.py")
    model = RecordedToolLoop(model="recorded-test")

    async def run():
        runner = InMemoryRunner(agent=module.build_agent(model))
        try:
            session = await runner.session_service.create_session(app_name=runner.app_name, user_id="learner")
            return [
                event
                async for event in runner.run_async(
                    user_id="learner",
                    session_id=session.id,
                    new_message=types.Content(role="user", parts=[types.Part(text="List open incidents")]),
                )
            ]
        finally:
            await runner.close()

    events = asyncio.run(run())
    results = [
        part.function_response
        for event in events
        if event.content
        for part in event.content.parts or []
        if part.function_response
    ]
    assert results
    assert results[0].name == "list_open_incidents"
    assert REFERENCE_DOMAIN.incidents.inventory_down in str(results[0].response)
    assert model.calls == 2


def test_check_rejects_tools_that_are_not_attached_to_the_agent(monkeypatch) -> None:
    module = course_lab.load(course_lab.LABS / "solutions/step_2.py")
    monkeypatch.setattr(module, "TOOLS", [])
    import unittest

    result = unittest.TestResult()
    course_lab.checks(module, 2).run(result)
    assert not result.wasSuccessful()
    assert "Attach the step's tools" in result.failures[0][1]


def test_start_appends_failing_scaffolds_without_duplicating_learner_functions(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(course_lab, "ROOT", tmp_path)
    monkeypatch.setattr(course_lab, "WORK", tmp_path / "learning")
    course_lab.start(1, reference=False)
    second = course_lab.start(2, reference=False).read_text()
    assert second.count("def list_open_incidents(") == 1
    assert "# TOOLS = [*TOOLS, list_open_incidents, get_incident]" in second  # attaching stays the learner's job
    reference = (course_lab.LABS / "solutions/step_8.py").read_text()
    assert course_lab.scaffold(reference, 8) == reference


def test_status_names_the_single_next_command(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(course_lab, "ROOT", tmp_path)
    monkeypatch.setattr(course_lab, "WORK", tmp_path / "learning")
    course_lab.start(1, reference=False)
    course_lab.start(2, reference=False)  # the scaffold raises NotImplementedError until implemented
    states = course_lab.status()
    assert states[:3] == [(1, "passing"), (2, "failing"), (3, "not started")]
    assert course_lab.next_command(states).endswith("mise run lab -- check 2")
    assert course_lab.next_command([(step, "passing") for step in course_lab.STEPS]).startswith("all steps pass")


def test_guardrail_check_rejects_a_fence_that_retrieved_text_can_close(monkeypatch) -> None:
    import unittest

    module = course_lab.load(course_lab.LABS / "solutions/step_7.py")
    monkeypatch.setattr(module, "neutralize", lambda text: (text, 1 if "ignore" in text.lower() else 0))
    result = unittest.TestResult()
    course_lab.checks(module, 7).run(result)
    assert not result.wasSuccessful()


def test_mcp_check_rejects_a_server_that_exposes_the_guarded_write(monkeypatch) -> None:
    import unittest

    module = course_lab.load(course_lab.LABS / "solutions/step_8.py")
    build = module.build_mcp_server

    def widened():
        server = build()
        server.add_tool(module.propose_restart)
        return server

    monkeypatch.setattr(module, "build_mcp_server", widened)
    result = unittest.TestResult()
    course_lab.checks(module, 8).run(result)
    assert not result.wasSuccessful()
