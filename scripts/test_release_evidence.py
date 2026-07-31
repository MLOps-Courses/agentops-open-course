"""Regression tests for the sanitized release-evidence boundary."""

# The repository executes this unittest module directly.
# ruff: noqa: PT027

from __future__ import annotations

import copy
import json
import pathlib
import tempfile
import unittest

from scripts import release_evidence  # ty: ignore[unresolved-import]

_REPOSITORY = "MLOps-Courses/agentops-open-course"
_SHA = "a" * 40
_RUN_ID = 123456
_DIGEST = "b" * 64


def _model() -> dict:
    return {
        "model": "qwen3:4b-instruct",
        "digest": _DIGEST,
        "ollama_version": "ollama version 0.32.5",
        "context_length": 8192,
        "temperature": 0,
    }


def _verdict() -> dict:
    return {
        "schema_version": 1,
        "repository": _REPOSITORY,
        "sha": _SHA,
        "run": {
            "id": _RUN_ID,
            "attempt": 1,
            "url": f"https://github.com/{_REPOSITORY}/actions/runs/{_RUN_ID}",
        },
        "model": {"name": "qwen3:4b-instruct", "digest": _DIGEST},
        "signals": {
            "adk_trajectory": "success",
            "structured_report": "success",
            "bounded_workflow": "success",
            "mlflow_scorers": "success",
            "cost_regression": "success",
            "groundedness": "success",
        },
        "overall": "success",
        "privacy": "sanitized verdict only; no prompts, responses, tool data, or model transcripts",
    }


def _lock(*, omit: str = "") -> str:
    packages = {
        "google-adk": "2.6.0",
        "mlflow": "3.15.0",
        "mlflow-skinny": "3.15.0",
        "openai": "2.51.0",
        "rouge-score": "0.1.2",
    }
    return "\n".join(
        f'[[package]]\nname = "{name}"\nversion = "{version}"\n' for name, version in packages.items() if name != omit
    )


class ReleaseEvidenceTests(unittest.TestCase):
    def _validate(
        self,
        *,
        model: dict | None = None,
        verdict: dict | None = None,
        lock: str | None = None,
        sha: str = _SHA,
        run_id: int = _RUN_ID,
        run_attempt: int = 1,
    ) -> dict:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            model_path = root / "model.json"
            verdict_path = root / "verdict.json"
            lock_path = root / "uv.lock"
            model_path.write_text(json.dumps(model or _model()), encoding="utf-8")
            verdict_path.write_text(json.dumps(verdict or _verdict()), encoding="utf-8")
            lock_path.write_text(lock or _lock(), encoding="utf-8")
            return release_evidence.validate_release_evidence(
                model_path,
                verdict_path,
                lock_path,
                repository=_REPOSITORY,
                sha=sha,
                run_id=run_id,
                run_attempt=run_attempt,
            )

    def test_success_returns_only_the_sanitized_release_shape(self) -> None:
        evidence = self._validate()
        assert set(evidence) == {
            "schema_version",
            "sha",
            "eval_run",
            "model",
            "signals",
            "scorer_runtime_versions",
            "privacy",
        }
        assert evidence["sha"] == _SHA
        assert evidence["eval_run"]["id"] == _RUN_ID
        assert set(evidence["signals"]) == release_evidence._SIGNAL_KEYS  # noqa: SLF001
        assert "do not publish" not in json.dumps(evidence).lower()

    def test_wrong_candidate_or_run_identity_is_rejected(self) -> None:
        with self.assertRaisesRegex(ValueError, "full lowercase commit"):
            self._validate(sha="short")
        with self.assertRaisesRegex(ValueError, "exact run and source"):
            self._validate(run_id=_RUN_ID + 1)
        with self.assertRaisesRegex(ValueError, "exact run and source"):
            self._validate(run_attempt=2)
        boolean_attempt = _verdict()
        boolean_attempt["run"]["attempt"] = True
        with self.assertRaisesRegex(ValueError, "exact run and source"):
            self._validate(verdict=boolean_attempt)
        verdict = _verdict()
        verdict["sha"] = "c" * 40
        with self.assertRaisesRegex(ValueError, "exact run and source"):
            self._validate(verdict=verdict)

    def test_signal_inventory_and_outcomes_fail_closed(self) -> None:
        missing = _verdict()
        del missing["signals"]["groundedness"]
        with self.assertRaisesRegex(ValueError, "incomplete signal inventory"):
            self._validate(verdict=missing)

        extra = _verdict()
        extra["signals"]["raw_case"] = "success"
        with self.assertRaisesRegex(ValueError, "incomplete signal inventory"):
            self._validate(verdict=extra)

        failed = _verdict()
        failed["signals"]["groundedness"] = "failure"
        with self.assertRaisesRegex(ValueError, "non-success signals"):
            self._validate(verdict=failed)

    def test_unexpected_or_sensitive_fields_are_rejected(self) -> None:
        verdict = _verdict()
        verdict["raw_prompt"] = "do not publish"
        with self.assertRaisesRegex(ValueError, "unexpected fields"):
            self._validate(verdict=verdict)

        model = _model()
        model["response"] = "do not publish"
        with self.assertRaisesRegex(ValueError, "unexpected fields"):
            self._validate(model=model)

    def test_model_lineage_and_scorer_versions_must_match(self) -> None:
        model = _model()
        model["digest"] = "not-a-digest"
        with self.assertRaisesRegex(ValueError, "immutable SHA-256"):
            self._validate(model=model)

        verdict = copy.deepcopy(_verdict())
        verdict["model"]["digest"] = "c" * 64
        with self.assertRaisesRegex(ValueError, "model lineage disagree"):
            self._validate(verdict=verdict)

        with self.assertRaisesRegex(ValueError, "scorer versions are incomplete"):
            self._validate(lock=_lock(omit="rouge-score"))


if __name__ == "__main__":
    unittest.main()
