"""Regression tests for minor-release freshness handoffs."""

# ruff: noqa: PT027

from __future__ import annotations

import copy
import datetime as dt
import html
import unittest

from scripts import release_freshness  # ty: ignore[unresolved-import]

_NOW = dt.datetime(2026, 7, 31, 12, tzinfo=dt.UTC)
_REPOSITORY = "MLOps-Courses/agentops-open-course"


def _task_html(label: str, *, checked: bool) -> str:
    state = "Completed" if checked else "Incomplete"
    checked_attribute = ' checked=""' if checked else ""
    return (
        '<li class="task-list-item"><input type="checkbox" disabled="" '
        f'class="task-list-item-checkbox" aria-label="{state} task"{checked_attribute}> '
        f"{html.escape(label)}</li>"
    )


_LABELS = ["Provider names", "Prices and cost inputs"]
_TEMPLATE_HTML = "<ul>" + "".join(_task_html(label, checked=False) for label in _LABELS) + "</ul>"
_REVIEWED_HTML = "<ul>" + "".join(_task_html(label, checked=True) for label in _LABELS) + "</ul>"


def _issue() -> dict:
    return {
        "number": 113,
        "state": "closed",
        "title": "docs: freshness audit for 2026-Q3",
        "body": "## Reviewed\n\n- [x] Provider names\n- [x] Prices and cost inputs\n",
        "body_html": _REVIEWED_HTML,
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
            checklist_template_html=_TEMPLATE_HTML if issue is not None else None,
        )

    def test_recent_closed_documentation_issue_is_sanitized(self) -> None:
        result = self._validate("issue:113", _issue())
        assert result == {
            "schema_version": 2,
            "kind": "issue",
            "issue_number": 113,
            "checklist_items": 2,
            "checklist_sha256": release_freshness._checklist_digest(  # noqa: SLF001
                ["Provider names", "Prices and cost inputs"]
            ),
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
        missing_body = copy.deepcopy(_issue())
        missing_body["body"] = None
        cases.append(missing_body)
        no_checklist = copy.deepcopy(_issue())
        no_checklist["body"] = "Reviewed without a machine-checkable task list."
        no_checklist["body_html"] = "<p>Reviewed without a machine-checkable task list.</p>"
        cases.append(no_checklist)
        unchecked = copy.deepcopy(_issue())
        unchecked["body"] = "- [x] Provider names\n* [ ] Prices and cost inputs\n"
        unchecked["body_html"] = (
            "<ul>" + _task_html(_LABELS[0], checked=True) + _task_html(_LABELS[1], checked=False) + "</ul>"
        )
        cases.append(unchecked)
        incomplete = copy.deepcopy(_issue())
        incomplete["body"] = "- [x] Provider names\n"
        incomplete["body_html"] = "<ul>" + _task_html(_LABELS[0], checked=True) + "</ul>"
        cases.append(incomplete)
        for hidden_markdown in (
            "```markdown\n- [x] Provider names\n- [x] Prices and cost inputs\n```\n",
            "~~~markdown\n- [x] Provider names\n- [x] Prices and cost inputs\n~~~\n",
            "<!--\n- [x] Provider names\n- [x] Prices and cost inputs\n-->\n",
            "<!--\n- [x] Provider names\n- [x] Prices and cost inputs\n",
            "<PRE class=hidden>\n- [x] Provider names\n- [x] Prices and cost inputs\n</PRE>\n",
            "<code>\n- [x] Provider names\n- [x] Prices and cost inputs\n</code>\n",
            "- <pre>\n  - [x] Provider names\n  - [x] Prices and cost inputs\n  </pre>\n",
        ):
            hidden = copy.deepcopy(_issue())
            hidden["body"] = hidden_markdown
            hidden["body_html"] = "<pre>GitHub rendered no task-list items here.</pre>"
            cases.append(hidden)
        extra = copy.deepcopy(_issue())
        extra["body"] += "- [x] Unreviewed extra claim\n"
        extra["body_html"] += _task_html("Unreviewed extra claim", checked=True)
        cases.append(extra)
        mismatched = copy.deepcopy(_issue())
        mismatched["number"] = 114
        cases.append(mismatched)
        for issue in cases:
            with self.subTest(issue=issue), self.assertRaises(ValueError):
                self._validate("issue:113", issue)

    def test_rendered_labels_preserve_visible_code_links_and_entities(self) -> None:
        rendered = (
            '<ul><li class="task-list-item"><input type="checkbox" disabled="" '
            'class="task-list-item-checkbox" aria-label="Completed task" checked=""> '
            'Review <code>qwen3</code> &amp; <a href="https://example.com">source</a></li></ul>'
        )
        assert release_freshness._rendered_task_items(rendered) == [(True, "Review qwen3 & source")]  # noqa: SLF001

    def test_rendered_task_signature_and_full_issue_media_type_fail_closed(self) -> None:
        missing_html = _issue()
        del missing_html["body_html"]
        with self.assertRaisesRegex(ValueError, "GitHub-rendered HTML"):
            self._validate("issue:113", missing_html)

        for body_html in (
            '<li class="task-list-item">Provider names</li>',
            '<li class="task-list-item"><input type="checkbox"> Provider names</li>',
            '<input type="checkbox" disabled="" class="task-list-item-checkbox" checked="">',
        ):
            with self.subTest(body_html=body_html), self.assertRaises(ValueError):
                issue = _issue()
                issue["body_html"] = body_html
                self._validate("issue:113", issue)

    def test_checked_template_is_rejected(self) -> None:
        with self.assertRaisesRegex(ValueError, "template must contain only unchecked"):
            release_freshness.validate_freshness_handoff(
                "issue:113",
                actor="release-operator",
                repository=_REPOSITORY,
                now=_NOW,
                issue=_issue(),
                checklist_template_html=_REVIEWED_HTML,
            )

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
