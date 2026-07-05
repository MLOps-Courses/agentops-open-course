"""Agent Skills — progressive-disclosure instructions for the Ops Copilot (Chapter 3.2).

A *skill* is a folder with a ``SKILL.md`` (name + description front-matter, then instructions,
plus optional ``assets``/``references``). The model first sees only each skill's name and
description; it loads the full body on demand — progressive disclosure that keeps the context
small. Skills live in ``agents/data/skills`` so both tracks share them. This mirrors the
open **Agent Skills** standard used by AI coding assistants.
"""

from __future__ import annotations

from pathlib import Path

from google.adk.skills import list_skills_in_dir, load_skill_from_dir
from google.adk.tools.skill_toolset import SkillToolset

from .config import settings


def skills_dir() -> Path:
    """Return the directory holding the Ops Copilot skills."""
    return settings.data_dir / "skills"


def skill_toolset() -> SkillToolset:
    """Build a SkillToolset from every skill in the skills directory (progressive disclosure)."""
    base = skills_dir()
    skills = [load_skill_from_dir(base / name) for name in list_skills_in_dir(base)]
    return SkillToolset(skills=skills)
