"""Regression tests for minor-release freshness handoffs."""

# ruff: noqa: PT027

from __future__ import annotations

import copy
import datetime as dt
import unittest

from scripts import release_freshness  # ty: ignore[unresolved-import]

_NOW = dt.datetime(2026, 7, 31, 12, tzinfo=dt.UTC)
_REPOSITORY = "MLOps-Courses/agentops-open-course"


def _issue() -> dict:
    return {
        "number": 113,
        "state": "closed",
        "title": "docs: freshness audit for 2026-Q3",
        "closed_at": "2026-07-30T10:00:00Z",
        "closed_by": {"login": "maintainer"},
        "html_url": f"https://github.com/{_REPOSITORY}/issues/113",
        "labels": [{"name": "documentation"}],
    }


class ReleaseFreshnessTests(unittest.TestCase):
    def _validate(self, evidence: str, issue: dict | None = None) -> dict:
        return release_freshness.validate_freshness_handoff(
            evidence,
            actor="release-operator",
            repository=_REPOSITORY,
            now=_NOW,
            issue=issue,
        )

    def test_recent_closed_documentation_issue_is_sanitized(self) -> None:
        result = self._validate("issue:113", _issue())
        assert result == {
            "schema_version": 1,
            "kind": "issue",
            "issue_number": 113,
            "url": f"https://github.com/{_REPOSITORY}/issues/113",
            "closed_at": "2026-07-30T10:00:00Z",
            "reviewed_by": "maintainer",
            "selected_by": "release-operator",
        }

    def test_open_stale_mismatched_or_unlabelled_issue_is_rejected(self) -> None:
        cases = []
        opened = copy.deepcopy(_issue())
        opened["state"] = "open"
        cases.append(opened)
        stale = copy.deepcopy(_issue())
        stale["closed_at"] = "2026-01-01T00:00:00Z"
        cases.append(stale)
        unlabelled = copy.deepcopy(_issue())
        unlabelled["labels"] = []
        cases.append(unlabelled)
        mismatched = copy.deepcopy(_issue())
        mismatched["number"] = 114
        cases.append(mismatched)
        for issue in cases:
            with self.subTest(issue=issue), self.assertRaises(ValueError):
                self._validate("issue:113", issue)

    def test_explicit_waiver_is_recorded_for_protected_review(self) -> None:
        result = self._validate("waiver:Provider source was unavailable after two reviewed attempts.")
        assert result["kind"] == "waiver"
        assert result["review_gate"] == "protected release environment"

    def test_ambiguous_or_empty_waiver_is_rejected(self) -> None:
        for evidence in ("113", "issue:0", "waiver:too short", "waiver:line one\nline two is long enough"):
            with self.subTest(evidence=evidence), self.assertRaises(ValueError):
                self._validate(evidence)


if __name__ == "__main__":
    unittest.main()
