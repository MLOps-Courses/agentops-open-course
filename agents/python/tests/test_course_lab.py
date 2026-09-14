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
    assert second.read_bytes() == expected


def test_joining_later_requires_an_explicit_reference_checkpoint(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(course_lab, "ROOT", tmp_path)
    monkeypatch.setattr(course_lab, "WORK", tmp_path / "learning")
    with pytest.raises(ValueError, match="Finish step 3"):
        course_lab.start(4, reference=False)
    fourth = course_lab.start(4, reference=True)
    assert fourth.read_bytes() == (course_lab.LABS / "solutions/step_3.py").read_bytes()


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
