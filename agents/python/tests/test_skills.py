"""Unit tests for Agent Skills (Ch. 3.2)."""

from google.adk.skills import list_skills_in_dir

from agent import skills


def test_skills_are_discovered() -> None:
    found = list_skills_in_dir(skills.skills_dir())
    assert {"incident-triage", "remediation"} <= set(found)


def test_skill_toolset_builds() -> None:
    toolset = skills.skill_toolset()
    assert toolset is not None
