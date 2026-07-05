"""Unit tests for the runbook knowledge tools (Ch. 3.4)."""

from agent import memory


def test_get_runbook_known() -> None:
    result = memory.get_runbook("high-latency")
    assert result["slug"] == "high-latency"
    assert "# Runbook: High Latency" in result["content"]


def test_get_runbook_unknown_lists_available() -> None:
    result = memory.get_runbook("nope")
    assert "error" in result
    assert "high-latency" in result["error"]


def test_search_runbooks_ranks_relevant_first() -> None:
    result = memory.search_runbooks("service is completely down and returning 503")
    assert result["count"] >= 1
    assert result["runbooks"][0]["slug"] == "service-down"


def test_search_runbooks_respects_limit() -> None:
    result = memory.search_runbooks("latency errors disk deploy service", limit=2)
    assert result["count"] <= 2


def test_search_runbooks_no_match_is_empty() -> None:
    result = memory.search_runbooks("zzzznomatchzzzz")
    assert result["count"] == 0
    assert result["runbooks"] == []
