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


def test_get_runbook_rejects_path_traversal() -> None:
    # The slug is model-controlled: a traversal payload must be refused, not read off disk.
    result = memory.get_runbook("../../../../etc/passwd")
    assert "error" in result
    assert "root:" not in result["error"]


def test_search_runbooks_ranks_relevant_first() -> None:
    result = memory.search_runbooks("service is completely down and returning 503")
    assert result["count"] >= 1
    assert result["runbooks"][0]["slug"] == "service-down"


def test_search_runbooks_reports_keyword_mode() -> None:
    # The default (offline) path must label its retrieval mode so a downstream
    # reader can tell a keyword result from a semantic one without inspecting logs.
    result = memory.search_runbooks("service down")
    assert result["retrieval"] == "keyword"


def test_search_runbooks_respects_limit() -> None:
    result = memory.search_runbooks("latency errors disk deploy service", limit=2)
    assert result["count"] <= 2


def test_search_runbooks_no_match_is_empty() -> None:
    result = memory.search_runbooks("zzzznomatchzzzz")
    assert result["count"] == 0
    assert result["runbooks"] == []


def test_search_runbooks_non_positive_limit_falls_back_to_default() -> None:
    # A non-positive limit is treated as the default (3), not "return nothing".
    query = "service down latency errors disk deploy"
    default = memory.search_runbooks(query)
    assert memory.search_runbooks(query, limit=0) == default
    assert memory.search_runbooks(query, limit=-5) == default
