"""Smoke test: the reference agent is importable and defined."""

from agent.agent import root_agent


def test_root_agent_defined() -> None:
    assert root_agent.name == "agentops_agent"
    assert root_agent.model


def test_instruction_defaults_to_the_committed_text(monkeypatch) -> None:
    from agent import agent as agent_module

    monkeypatch.setattr(agent_module.settings, "prompt_uri", None)
    assert agent_module._instruction() == agent_module.INSTRUCTION  # noqa: SLF001


def test_instruction_loads_a_pinned_registry_version(monkeypatch) -> None:
    import sys
    from types import SimpleNamespace

    from agent import agent as agent_module

    loaded: list[str] = []

    def fake_load_prompt(uri: str):
        loaded.append(uri)
        return SimpleNamespace(template="registry instruction v2")

    fake_genai = SimpleNamespace(load_prompt=fake_load_prompt)
    monkeypatch.setitem(sys.modules, "mlflow", SimpleNamespace(genai=fake_genai))
    monkeypatch.setitem(sys.modules, "mlflow.genai", fake_genai)
    monkeypatch.setattr(agent_module.settings, "prompt_uri", "prompts:/agentops-agent-instruction/2")
    assert agent_module._instruction() == "registry instruction v2"  # noqa: SLF001
    assert loaded == ["prompts:/agentops-agent-instruction/2"]
