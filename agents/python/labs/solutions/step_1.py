"""A learner-owned incident assistant; this lab changes only synthetic state."""

from __future__ import annotations

from google.adk.agents import Agent
from google.adk.models import BaseLlm

INSTRUCTION = "Investigate fictional incidents using evidence. Ask before taking action."

TOOLS = []


def build_agent(model: str | BaseLlm) -> Agent:
    """Build without making a model request; the caller owns the model."""
    return Agent(name="learner_agent", model=model, instruction=INSTRUCTION, tools=TOOLS)
