"""Validate the external-fact review handed to a protected release."""

from __future__ import annotations

import argparse
import collections
import datetime as dt
import hashlib
import json
import re
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any, Final

_ISSUE_TITLE_PREFIX: Final = "docs: freshness audit"
_MAX_AGE: Final = dt.timedelta(days=120)
_MIN_WAIVER_LENGTH: Final = 24
_TASK: Final = re.compile(r"^\s{0,3}[-*+]\s+\[([ xX])\]\s+(.+?)\s*$")
_FENCE: Final = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
_HTML_COMMENT: Final = re.compile(r"<!--.*?-->", re.DOTALL)
_DEFAULT_TEMPLATE: Final = Path(".github/ISSUE_TEMPLATE/docs-freshness.md")


def _task_items(markdown: str) -> list[tuple[bool, str]]:
    """Return real GFM task items, excluding fenced examples and comments."""
    items: list[tuple[bool, str]] = []
    fence_character: str | None = None
    fence_length = 0
    for line in _HTML_COMMENT.sub("", markdown).splitlines():
        if fence_character is not None:
            closing = re.fullmatch(rf"\s{{0,3}}{re.escape(fence_character)}{{{fence_length},}}\s*", line)
            if closing:
                fence_character = None
                fence_length = 0
            continue
        if opening := _FENCE.match(line):
            marker = opening.group(1)
            fence_character = marker[0]
            fence_length = len(marker)
            continue
        if task := _TASK.match(line):
            label = re.sub(r"\s+", " ", task.group(2)).strip()
            if label:
                items.append((task.group(1).lower() == "x", label))
    return items


def _checklist_digest(labels: list[str]) -> str:
    """Return a stable, content-bound checklist identifier without issue prose."""
    payload = json.dumps(labels, ensure_ascii=True, separators=(",", ":"))
    return hashlib.sha256(payload.encode()).hexdigest()


def _closed_at(value: object) -> dt.datetime:
    if not isinstance(value, str):
        raise ValueError("freshness issue has no close timestamp")
    try:
        parsed = dt.datetime.fromisoformat(value)
    except ValueError as error:
        raise ValueError("freshness issue close timestamp is invalid") from error
    if parsed.tzinfo is None:
        raise ValueError("freshness issue close timestamp needs a timezone")
    return parsed.astimezone(dt.UTC)


def _label_names(issue: dict[str, Any]) -> set[str]:
    labels = issue.get("labels")
    if not isinstance(labels, list):
        return set()
    return {name for label in labels if isinstance(label, dict) and isinstance(name := label.get("name"), str)}


def validate_freshness_handoff(
    evidence: str,
    *,
    actor: str,
    repository: str,
    now: dt.datetime,
    issue: dict[str, Any] | None = None,
    checklist_template: str | None = None,
) -> dict[str, Any]:
    """Return sanitized review evidence or reject a stale/ambiguous handoff."""
    if not actor.strip() or not re.fullmatch(r"[^/]+/[^/]+", repository):
        raise ValueError("release actor and owner/repository are required")
    if now.tzinfo is None:
        raise ValueError("release qualification time needs a timezone")
    now = now.astimezone(dt.UTC)

    if match := re.fullmatch(r"issue:([1-9][0-9]*)", evidence):
        number = int(match.group(1))
        if issue is None or issue.get("number") != number:
            raise ValueError("freshness handoff does not match the fetched issue")
        if issue.get("state") != "closed" or issue.get("pull_request") is not None:
            raise ValueError("freshness evidence must be a closed issue, not a pull request")
        title = issue.get("title")
        if not isinstance(title, str) or not title.startswith(_ISSUE_TITLE_PREFIX):
            raise ValueError(f"freshness issue title must start with {_ISSUE_TITLE_PREFIX!r}")
        if "documentation" not in _label_names(issue):
            raise ValueError("freshness issue must carry the documentation label")
        body = issue.get("body")
        if not isinstance(body, str) or not body.strip():
            raise ValueError("freshness issue must contain its reviewed checklist")
        if not isinstance(checklist_template, str) or not checklist_template.strip():
            raise ValueError("freshness checklist template is required for issue evidence")
        expected = [label for _, label in _task_items(checklist_template)]
        reviewed = _task_items(body)
        if not expected:
            raise ValueError("freshness checklist template contains no task items")
        if not reviewed:
            raise ValueError("freshness issue must contain checked checklist items outside code examples")
        if any(not checked for checked, _ in reviewed):
            raise ValueError("freshness issue still contains unchecked checklist items")
        reviewed_labels = [label for _, label in reviewed]
        if collections.Counter(reviewed_labels) != collections.Counter(expected):
            raise ValueError("freshness issue does not match the exact current checklist inventory")
        closed_at = _closed_at(issue.get("closed_at"))
        age = now - closed_at
        if age < dt.timedelta(0) or age > _MAX_AGE:
            raise ValueError("freshness issue must have been reviewed and closed within 120 days")
        url = issue.get("html_url")
        closed_by = issue.get("closed_by")
        reviewer = closed_by.get("login") if isinstance(closed_by, dict) else None
        if not isinstance(url, str) or not url.startswith(f"https://github.com/{repository}/issues/"):
            raise ValueError("freshness issue URL does not belong to this repository")
        if not isinstance(reviewer, str) or not reviewer:
            raise ValueError("freshness issue does not record who closed the review")
        return {
            "schema_version": 2,
            "kind": "issue",
            "issue_number": number,
            "checklist_items": len(expected),
            "checklist_sha256": _checklist_digest(expected),
            "url": url,
            "closed_at": closed_at.isoformat().replace("+00:00", "Z"),
            "reviewed_by": reviewer,
            "selected_by": actor,
        }

    if evidence.startswith("waiver:"):
        reason = evidence.removeprefix("waiver:").strip()
        if "\n" in reason or len(reason) < _MIN_WAIVER_LENGTH or len(reason) > 500:
            raise ValueError("freshness waiver needs a single-line reason between 24 and 500 characters")
        return {
            "schema_version": 2,
            "kind": "waiver",
            "reason": reason,
            "selected_by": actor,
            "review_gate": "protected release environment",
        }

    raise ValueError("freshness evidence must be issue:<number> or waiver:<reviewed reason>")


def main(argv: Sequence[str] | None = None) -> None:
    """Validate command-line handoff data and print minimized JSON evidence."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", required=True)
    parser.add_argument("--actor", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--issue-json", type=Path)
    parser.add_argument("--checklist-template", type=Path, default=_DEFAULT_TEMPLATE)
    arguments = parser.parse_args(argv)
    issue = None
    checklist_template = None
    try:
        if arguments.issue_json is not None:
            loaded = json.loads(arguments.issue_json.read_text(encoding="utf-8"))
            if not isinstance(loaded, dict):
                raise ValueError("freshness issue response must be one JSON object")
            issue = loaded
            checklist_template = arguments.checklist_template.read_text(encoding="utf-8")
        result = validate_freshness_handoff(
            arguments.evidence,
            actor=arguments.actor,
            repository=arguments.repository,
            now=dt.datetime.now(dt.UTC),
            issue=issue,
            checklist_template=checklist_template,
        )
    except (OSError, ValueError, json.JSONDecodeError) as error:
        raise SystemExit(f"error: {error}") from error
    sys.stdout.write(json.dumps(result, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
