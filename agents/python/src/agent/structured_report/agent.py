"""Expose the structured report agent through ADK's ``root_agent`` contract."""

from agent.report import triage_report_agent as root_agent

__all__ = ["root_agent"]
