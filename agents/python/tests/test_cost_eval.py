"""Offline tests for the token/cost regression comparison logic."""

import io
import json

import pytest

from evals import cost_eval


@pytest.fixture(autouse=True)
def ignore_retained_workflow_transcripts(monkeypatch) -> None:
    monkeypatch.delenv("AGENT_EVAL_OBSERVED_PATH", raising=False)
    monkeypatch.delenv("EVAL_MODEL_METADATA_PATH", raising=False)


def _identity(
    *,
    model: str = "qwen3:4b-instruct",
    model_digest: str | None = "sha256:canonical",
    source_revision: str | None = "abc123",
) -> dict:
    return {
        "model_provider": str(cost_eval.settings.model_provider),
        "model": model,
        "model_digest": model_digest,
        "source_revision": source_revision,
        "prompt_selection": "committed:sha256:prompt",
        "evaluation_contract_digest": "sha256:evalset",
        "context_length": 8192,
        "ollama_version": "ollama version 0.31.2",
        "temperature": 0,
    }


def _baseline(cases: dict, **identity_overrides) -> dict:
    return {
        "schema_version": 2,
        **_identity(**identity_overrides),
        "cases": cases,
    }


def test_no_regression_within_tolerance() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1200, "model_calls": 3}}  # +20%, under the 25% default
    assert cost_eval.regressions(observed, baseline) == []


def test_flags_token_growth_beyond_tolerance() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1400, "model_calls": 3}}  # +40%
    problems = cost_eval.regressions(observed, baseline)
    assert len(problems) == 1
    assert "lookup total_tokens" in problems[0]


def test_flags_extra_model_call() -> None:
    baseline = {"triage": {"total_tokens": 500, "model_calls": 2}}
    observed = {"triage": {"total_tokens": 500, "model_calls": 4}}  # doubled calls
    problems = cost_eval.regressions(observed, baseline)
    assert any("model_calls" in line for line in problems)
    assert "4 > 2.5" in problems[0]


def test_case_set_mismatch_requires_a_reviewed_baseline() -> None:
    baseline = {"removed": {"total_tokens": 900, "model_calls": 2}}
    observed = {"added": {"total_tokens": 500, "model_calls": 1}}
    problems = cost_eval.regressions(observed, baseline)
    assert problems == [
        "removed: missing from the observation; regenerate and review the baseline",
        "added: no reviewed baseline; regenerate and review the baseline",
    ]


def test_zero_baseline_requires_replacement() -> None:
    # Zero means the provider omitted usage metadata, not unlimited headroom.
    baseline = {"new": {"total_tokens": 0, "model_calls": 0}}
    observed = {"new": {"total_tokens": 5000, "model_calls": 9}}
    problems = cost_eval.regressions(observed, baseline)
    assert len(problems) == 2
    assert all("not comparable" in problem for problem in problems)


def test_tolerance_is_configurable() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1050, "model_calls": 3}}  # +5%
    assert cost_eval.regressions(observed, baseline, tolerance=0.0)  # zero tolerance flags it
    assert cost_eval.regressions(observed, baseline, tolerance=0.10) == []  # 10% absorbs it


def test_baseline_identity_matches_model_and_optional_digest(monkeypatch) -> None:
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")
    cases = {"lookup": {"total_tokens": 1000, "model_calls": 2}}
    identity = _identity()
    document = _baseline(cases)
    assert (
        cost_eval._baseline_cases(  # noqa: SLF001 - baseline boundary
            document,
            identity=identity,
        )
        == cases
    )


def test_baseline_identity_rejects_a_different_model_or_digest(monkeypatch) -> None:
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")
    cases = {"lookup": {"total_tokens": 1000, "model_calls": 2}}
    with pytest.raises(SystemExit, match="not 'qwen3:4b-instruct'"):
        cost_eval._baseline_cases(  # noqa: SLF001
            _baseline(cases, model="qwen3:1.7b", model_digest=None),
            identity=_identity(model_digest=None),
        )

    with pytest.raises(SystemExit, match="does not match"):
        cost_eval._baseline_cases(  # noqa: SLF001
            _baseline(cases, model_digest="sha256:old"),
            identity=_identity(model_digest="sha256:new"),
        )
    with pytest.raises(SystemExit, match="does not match"):
        cost_eval._baseline_cases(  # noqa: SLF001
            _baseline(cases, model_digest=None),
            identity=_identity(model_digest="sha256:new"),
        )


def test_baseline_identity_rejects_a_different_provider(monkeypatch) -> None:
    monkeypatch.setattr(cost_eval.settings, "model_provider", "openai-compatible")
    document = _baseline({"lookup": {"total_tokens": 1000, "model_calls": 2}})
    document["model_provider"] = "gemini"
    with pytest.raises(SystemExit, match="provider 'gemini'"):
        cost_eval._baseline_cases(  # noqa: SLF001
            document,
            identity=_identity(),
        )


def test_baseline_shape_must_be_current() -> None:
    with pytest.raises(SystemExit, match="unsupported shape"):
        cost_eval._baseline_cases(  # noqa: SLF001
            {"lookup": {"total_tokens": 1000}},
            identity=_identity(),
        )
    document = _baseline({"lookup": {"total_tokens": 1000, "model_calls": 2}})
    document["schema_version"] = True
    with pytest.raises(SystemExit, match="unsupported shape"):
        cost_eval._baseline_cases(  # noqa: SLF001
            document,
            identity=_identity(),
        )


@pytest.mark.parametrize(
    ("field", "baseline_value", "current_value"),
    [
        ("prompt_selection", "committed:sha256:old", "committed:sha256:new"),
        ("evaluation_contract_digest", "sha256:old-evalset", "sha256:new-evalset"),
        ("context_length", 4096, 8192),
        ("ollama_version", "ollama version 0.30.0", "ollama version 0.31.2"),
        ("temperature", 0.8, 0),
    ],
)
def test_baseline_rejects_incomparable_prompt_eval_or_runtime_identity(
    field: str,
    baseline_value,
    current_value,
) -> None:
    cases = {"lookup": {"total_tokens": 1000, "model_calls": 2}}
    document = _baseline(cases)
    identity = _identity()
    document[field] = baseline_value
    identity[field] = current_value

    with pytest.raises(SystemExit, match=field):
        cost_eval._baseline_cases(document, identity=identity)  # noqa: SLF001


def test_baseline_retains_but_does_not_compare_source_revision() -> None:
    cases = {"lookup": {"total_tokens": 1000, "model_calls": 2}}
    document = _baseline(cases, source_revision="baseline-sha")
    assert (
        cost_eval._baseline_cases(  # noqa: SLF001
            document,
            identity=_identity(source_revision="candidate-sha"),
        )
        == cases
    )
    document["source_revision"] = ""
    with pytest.raises(SystemExit, match="unsupported shape"):
        cost_eval._baseline_cases(document, identity=_identity())  # noqa: SLF001


def test_scheduled_model_metadata_is_validated_and_retained(monkeypatch, tmp_path) -> None:
    metadata = tmp_path / "model.json"
    metadata.write_text(
        json.dumps(
            {
                "model": "qwen3:4b-instruct",
                "digest": "sha256:canonical",
                "ollama_version": "ollama version 0.31.2",
                "context_length": 8192,
                "temperature": 0,
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("EVAL_MODEL_METADATA_PATH", str(metadata))
    monkeypatch.setenv("GITHUB_SHA", "abc123")
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(cost_eval.settings, "model_temperature", 0.0)
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}],
    )
    monkeypatch.setattr(cost_eval, "_prompt_selection", lambda: "committed:sha256:prompt")
    monkeypatch.setattr(cost_eval, "_evaluation_contract_digest", lambda _cases: "sha256:evalset")

    identity = cost_eval._current_identity("sha256:canonical")  # noqa: SLF001

    assert identity == _identity()
    measurement = cost_eval._measurement(  # noqa: SLF001
        {"lookup": {"total_tokens": 1000, "model_calls": 2}},
        identity,
    )
    assert measurement["schema_version"] == 2
    assert measurement["source_revision"] == "abc123"
    assert measurement["context_length"] == 8192
    assert measurement["ollama_version"] == "ollama version 0.31.2"
    assert measurement["temperature"] == 0


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("model", "qwen3:1.7b"),
        ("digest", "sha256:different"),
        ("context_length", 0),
        ("ollama_version", ""),
        ("temperature", 0.7),
    ],
)
def test_scheduled_model_metadata_rejects_mismatched_provenance(monkeypatch, tmp_path, field, value) -> None:
    metadata = tmp_path / "model.json"
    document = {
        "model": "qwen3:4b-instruct",
        "digest": "sha256:canonical",
        "ollama_version": "ollama version 0.31.2",
        "context_length": 8192,
        "temperature": 0,
    }
    document[field] = value
    metadata.write_text(json.dumps(document), encoding="utf-8")
    monkeypatch.setenv("EVAL_MODEL_METADATA_PATH", str(metadata))
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(cost_eval.settings, "model_temperature", 0.0)

    with pytest.raises(SystemExit, match="does not match"):
        cost_eval._model_metadata("sha256:canonical")  # noqa: SLF001


@pytest.mark.parametrize(
    "cases",
    [
        {},
        {"lookup": "broken"},
        {"lookup": {"total_tokens": 1000}},
        {"lookup": {"total_tokens": True, "model_calls": 2}},
        {"lookup": {"total_tokens": 0, "model_calls": 2}},
    ],
)
def test_baseline_usage_must_be_comparable(cases) -> None:
    document = _baseline(cases)
    with pytest.raises(SystemExit, match=r"cost_baseline\.json"):
        cost_eval._baseline_cases(document, identity=_identity())  # noqa: SLF001


def test_model_digest_prefers_explicit_evidence(monkeypatch) -> None:
    monkeypatch.setenv("EVAL_MODEL_DIGEST", "sha256:explicit")
    monkeypatch.setattr(cost_eval, "HTTPConnection", lambda *_args, **_kwargs: pytest.fail("must not probe Ollama"))
    assert cost_eval._model_digest() == "sha256:explicit"  # noqa: SLF001


def test_model_digest_resolves_direct_loopback_ollama(monkeypatch) -> None:
    monkeypatch.delenv("EVAL_MODEL_DIGEST", raising=False)
    monkeypatch.setattr(cost_eval.settings, "model_provider", "openai-compatible")
    monkeypatch.setattr(cost_eval.settings, "openai_base_url", "http://localhost:11434/v1")
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")

    class Response(io.BytesIO):
        status = 200

    class Connection:
        def __init__(self, host, port, *, timeout):
            assert (host, port, timeout) == ("localhost", 11434, 2)

        def request(self, method, path, *, headers):
            assert (method, path, headers) == ("GET", "/api/tags", {"Accept": "application/json"})

        def getresponse(self):
            return Response(json.dumps({"models": [{"name": "qwen3:4b-instruct", "digest": "sha256:local"}]}).encode())

        def close(self):
            return None

    monkeypatch.setenv("HTTP_PROXY", "http://untrusted.example:8080")
    monkeypatch.setattr(cost_eval, "HTTPConnection", Connection)
    assert cost_eval._model_digest() == "sha256:local"  # noqa: SLF001


@pytest.mark.parametrize(
    ("provider", "base_url"),
    [
        ("gemini", None),
        ("openai-compatible", "https://example.com/v1"),
        ("openai-compatible", "http://127.0.0.1:4000/v1"),
    ],
)
def test_model_digest_never_probes_non_ollama_topologies(monkeypatch, provider, base_url) -> None:
    monkeypatch.delenv("EVAL_MODEL_DIGEST", raising=False)
    monkeypatch.setattr(cost_eval.settings, "model_provider", provider)
    monkeypatch.setattr(cost_eval.settings, "openai_base_url", base_url)
    monkeypatch.setattr(cost_eval, "HTTPConnection", lambda *_args, **_kwargs: pytest.fail("must not probe"))
    assert cost_eval._model_digest() is None  # noqa: SLF001


def test_model_digest_treats_unavailable_or_malformed_local_metadata_as_unknown(monkeypatch) -> None:
    monkeypatch.delenv("EVAL_MODEL_DIGEST", raising=False)
    monkeypatch.setattr(cost_eval.settings, "model_provider", "openai-compatible")
    monkeypatch.setattr(cost_eval.settings, "openai_base_url", "http://127.0.0.1:11434/v1")

    class Response(io.BytesIO):
        status = 200

    class Connection:
        def request(self, *_args, **_kwargs):
            return None

        def getresponse(self):
            return Response(b"not-json")

        def close(self):
            return None

    monkeypatch.setattr(cost_eval, "HTTPConnection", lambda *_args, **_kwargs: Connection())
    assert cost_eval._model_digest() is None  # noqa: SLF001


@pytest.mark.parametrize(
    "bad_usage", [{"total_tokens": True, "model_calls": 1}, {"total_tokens": 1.9, "model_calls": 1}]
)
def test_measure_rejects_malformed_provider_usage_without_coercion(monkeypatch, bad_usage) -> None:
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}],
    )
    monkeypatch.setattr(cost_eval, "ask", lambda *_args: {"usage": bad_usage, "provider_errors": [[]]})
    with pytest.raises(SystemExit, match="positive integer total_tokens"):
        cost_eval.measure()


def test_measure_rejects_provider_fallback_before_recording_a_baseline(monkeypatch) -> None:
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}],
    )
    monkeypatch.setattr(
        cost_eval,
        "ask",
        lambda *_args: {
            "provider_errors": [[{"code": "MODEL_UNAVAILABLE", "message": "Model request failed safely."}]],
            "usage": {"total_tokens": 10, "model_calls": 1},
        },
    )

    with pytest.raises(SystemExit, match="contains provider errors"):
        cost_eval.measure()


def test_measure_rejects_a_failed_required_trajectory(monkeypatch) -> None:
    required_case = cost_eval.REQUIRED_LIVE_CASES[0]
    expected_call = {"name": "recall_incident_context", "args": {"incident_id": "case-id"}}
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [
            {
                "inputs": {"eval_id": required_case, "turns": ["investigate"]},
                "expectations": {"expected_tools": [[expected_call]]},
            }
        ],
    )
    monkeypatch.setattr(
        cost_eval,
        "ask",
        lambda *_args: {
            "provider_errors": [[]],
            "tools": [[]],
            "usage": {"total_tokens": 10, "model_calls": 1},
        },
    )

    with pytest.raises(SystemExit, match="missed its tool trajectory"):
        cost_eval.measure()


def test_measure_accepts_a_passing_required_trajectory(monkeypatch) -> None:
    required_case = cost_eval.REQUIRED_LIVE_CASES[0]
    expected_call = {"name": "recall_incident_context", "args": {"incident_id": "case-id"}}
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [
            {
                "inputs": {"eval_id": required_case, "turns": ["investigate"]},
                "expectations": {"expected_tools": [[expected_call]]},
            }
        ],
    )
    monkeypatch.setattr(
        cost_eval,
        "ask",
        lambda *_args: {
            "provider_errors": [[]],
            "tools": [[expected_call]],
            "usage": {"total_tokens": 10, "model_calls": 1},
        },
    )

    assert cost_eval.measure() == {required_case: {"total_tokens": 10, "model_calls": 1}}


def test_measure_reuses_the_exact_mlflow_transcript_when_configured(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_EVAL_OBSERVED_PATH", "evals/model-observed.json")
    monkeypatch.setattr(
        cost_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}],
    )
    monkeypatch.setattr(cost_eval, "_model_digest", lambda: "sha256:canonical")

    def load(path, *, expected_cases, model_digest):
        assert str(path) == "evals/model-observed.json"
        assert expected_cases == [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}]
        assert model_digest == "sha256:canonical"
        return {
            "lookup": {
                "provider_errors": [[]],
                "usage": {"total_tokens": 120, "model_calls": 2},
            }
        }

    monkeypatch.setattr(cost_eval, "load_model_observations", load)
    monkeypatch.setattr(cost_eval, "ask", lambda *_args: pytest.fail("retained evidence must avoid a new model call"))

    assert cost_eval.measure() == {"lookup": {"total_tokens": 120, "model_calls": 2}}


@pytest.mark.parametrize("value", ["nan", "inf", "-0.1", "not-a-number"])
def test_tolerance_must_be_finite_and_non_negative(value: str) -> None:
    with pytest.raises(SystemExit, match="finite non-negative"):
        cost_eval._tolerance(value)  # noqa: SLF001


def test_tolerance_accepts_zero_and_the_default() -> None:
    assert cost_eval._tolerance("0") == 0.0  # noqa: SLF001
    assert cost_eval._tolerance(None) == 0.25  # noqa: SLF001


def test_main_rejects_an_incompatible_baseline_before_model_measurement(monkeypatch, tmp_path) -> None:
    baseline = tmp_path / "cost_baseline.json"
    baseline.write_text(
        json.dumps(_baseline({"lookup": {"total_tokens": 1, "model_calls": 1}}, model="qwen3:1.7b")),
        encoding="utf-8",
    )
    monkeypatch.setattr(cost_eval, "_BASELINE", baseline)
    monkeypatch.setattr(cost_eval.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setattr(cost_eval, "_model_digest", lambda: None)
    monkeypatch.setattr(cost_eval, "_current_identity", lambda _digest: _identity(model_digest=None))
    monkeypatch.setattr(cost_eval, "measure", lambda: pytest.fail("measurement must not start"))
    monkeypatch.setattr(cost_eval.sys, "argv", ["cost_eval.py"])
    with pytest.raises(SystemExit, match="not 'qwen3:4b-instruct'"):
        cost_eval.main()


def test_main_retains_scheduled_observation_before_rejecting_an_incompatible_baseline(
    monkeypatch,
    tmp_path,
) -> None:
    baseline = tmp_path / "cost_baseline.json"
    observed = tmp_path / "cost-observed.json"
    baseline.write_text(
        json.dumps(
            _baseline(
                {"lookup": {"total_tokens": 1, "model_calls": 1}},
                model="qwen3:1.7b",
                model_digest=None,
            )
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("AGENT_EVAL_OBSERVED_PATH", "evals/model-observed.json")
    monkeypatch.setattr(cost_eval, "_BASELINE", baseline)
    monkeypatch.setattr(cost_eval, "_OBSERVED", observed)
    monkeypatch.setattr(cost_eval, "_model_digest", lambda: None)
    monkeypatch.setattr(cost_eval, "_current_identity", lambda _digest: _identity(model_digest=None))
    monkeypatch.setattr(cost_eval, "measure", lambda: {"lookup": {"total_tokens": 2, "model_calls": 1}})
    monkeypatch.setattr(cost_eval.sys, "argv", ["cost_eval.py"])

    with pytest.raises(SystemExit, match="not 'qwen3:4b-instruct'"):
        cost_eval.main()

    document = json.loads(observed.read_text(encoding="utf-8"))
    assert document["cases"] == {"lookup": {"total_tokens": 2, "model_calls": 1}}


def test_main_rejects_invalid_baseline_json_before_model_measurement(monkeypatch, tmp_path) -> None:
    baseline = tmp_path / "cost_baseline.json"
    baseline.write_text("{broken", encoding="utf-8")
    monkeypatch.setattr(cost_eval, "_BASELINE", baseline)
    monkeypatch.setattr(cost_eval, "_model_digest", lambda: None)
    monkeypatch.setattr(cost_eval, "_current_identity", lambda _digest: _identity(model_digest=None))
    monkeypatch.setattr(cost_eval, "measure", lambda: pytest.fail("measurement must not start"))
    monkeypatch.setattr(cost_eval.sys, "argv", ["cost_eval.py"])
    with pytest.raises(SystemExit, match=r"cost_baseline\.json is unreadable or invalid JSON"):
        cost_eval.main()
