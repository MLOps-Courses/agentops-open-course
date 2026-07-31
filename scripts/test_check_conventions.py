"""Regression fixtures for the course authoring contracts."""

# The repository executes this unittest module directly; assertions keep fixtures concise.

from __future__ import annotations

import hashlib
import json
import pathlib
import shutil
import tempfile
import unittest
from unittest import mock

from scripts import check_conventions, course_evidence  # ty: ignore[unresolved-import]


def copy_contract_files(root: pathlib.Path, relative_paths: tuple[str, ...]) -> None:
    """Copy current repository authorities so tests mutate realistic fixtures."""
    for relative in relative_paths:
        source = check_conventions.ROOT / relative
        target = root / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)


def contract_pages(root: pathlib.Path, relative_paths: tuple[str, ...]) -> dict[pathlib.Path, str]:
    """Load copied Markdown pages using the same absolute-key shape as the gate."""
    return {root / relative: (root / relative).read_text(encoding="utf-8") for relative in relative_paths}


class SourceContractTests(unittest.TestCase):
    def test_checked_snippet_requires_one_existing_bounded_source_region(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source = root / "infra/example.yaml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "# --8<-- [start:trusted]\nvalue: 1\n# --8<-- [end:trusted]\n",
                encoding="utf-8",
            )
            text = '```yaml\n--8<-- "infra/example.yaml:trusted"\n```\n'
            assert check_conventions.check_snippet_targets(pathlib.Path("docs/example.md"), text, root=root) == []

            source.write_text("# --8<-- [start:trusted]\nvalue: 1\n", encoding="utf-8")
            problems = check_conventions.check_snippet_targets(pathlib.Path("docs/example.md"), text, root=root)
        assert any("exactly one start and end marker" in message for _, message in problems)

    def test_trusted_snippet_ratchet_covers_every_non_python_source_format(self) -> None:
        pages = {
            check_conventions.ROOT / relative: check_conventions.ROOT.joinpath(relative).read_text(encoding="utf-8")
            for relative, _ in check_conventions.TRUSTED_SNIPPET_SURFACES.values()
        }
        assert check_conventions.check_trusted_snippet_coverage(pages) == []

    def test_mlflow_point_version_copy_is_rejected_from_feedback_prose(self) -> None:
        feedback = check_conventions.ROOT / "docs/7. Observability/7.4. Feedback.md"
        pages = {feedback: "The `agentops-mlflow` image (`99.99.99`) stores assessments.\n"}
        problems = check_conventions.check_source_versions(pages)
        assert any("MLflow version belongs" in message for _, message in problems)

    def test_changed_authoritative_pin_rejects_stale_copy(self) -> None:
        problems = check_conventions.compare_contract(
            "docs/example.md",
            "tool pin",
            "2.0.0",
            "1.9.0",
        )
        assert problems == [("docs/example.md", "tool pin drifted: expected '2.0.0', found '1.9.0'")]

    def test_changed_task_expansion_rejects_stale_command(self) -> None:
        problems = check_conventions.compare_contract(
            "docs/example.md",
            "mise run web expansion",
            "uv run adk web src --port 8002",
            "uv run adk web src",
        )
        assert problems

    def test_changed_port_rejects_stale_documented_port(self) -> None:
        problems = check_conventions.compare_contract(
            "docs/example.md",
            "ADK web port",
            "8002",
            "8000",
        )
        assert problems

    def test_changed_manifest_resource_rejects_stale_documented_name(self) -> None:
        problems = check_conventions.compare_contract(
            "docs/example.md",
            "NetworkPolicy resource name",
            "otel-collector-ingress-v2",
            "otel-collector-ingress",
        )
        assert problems

    def test_real_python_owner_change_rejects_stale_support_prose(self) -> None:
        docs = (
            "docs/1. Setup/1.0. System.md",
            "docs/1. Setup/1.1. Python.md",
            "docs/1. Setup/index.md",
            "docs/4. Quality/4.4. Evaluations.md",
        )
        owners = ("agents/python/pyproject.toml", "agents/python/mise.toml")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            manifest = root / "agents/python/pyproject.toml"
            text = manifest.read_text(encoding="utf-8")
            document = check_conventions.read_toml(manifest)
            project = document.get("project")
            assert isinstance(project, dict)
            requires_python = project.get("requires-python")
            assert isinstance(requires_python, str)
            owner_line = f'requires-python = "{requires_python}"'
            assert owner_line in text
            manifest.write_text(
                text.replace(owner_line, f'requires-python = "{requires_python},!=99.99.99"', 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_python_profile_contracts(contract_pages(root, docs), root=root)
        assert any("requires-python" in message for _, message in problems)

    def test_real_agentgateway_owner_change_rejects_every_declared_prose_copy(self) -> None:
        docs = tuple(
            path.relative_to(check_conventions.ROOT).as_posix()
            for path in check_conventions.ROOT.joinpath("docs").rglob("*.md")
        )
        owners = (
            "pyproject.toml",
            "agents/python/pyproject.toml",
            "agents/python/Dockerfile",
            "docs/javascripts/accessibility.js",
            "infra/helmfile.yaml",
            "infra/mlflow/Dockerfile",
            "mise.toml",
            "scripts/install-helm-diff.sh",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            mise = root / "mise.toml"
            text = mise.read_text(encoding="utf-8")
            owner = '"github:agentgateway/agentgateway" = "1.4.1"'
            assert owner in text
            mise.write_text(text.replace(owner, '"github:agentgateway/agentgateway" = "9.9.9"', 1), encoding="utf-8")
            problems = check_conventions.check_source_versions(contract_pages(root, docs), root=root)
        assert any("agentgateway copy inventory drifted" in message for _, message in problems)

    def test_external_course_image_requires_a_digest(self) -> None:
        docs = tuple(
            path.relative_to(check_conventions.ROOT).as_posix()
            for path in check_conventions.ROOT.joinpath("docs").rglob("*.md")
        )
        owners = (
            "pyproject.toml",
            "agents/python/pyproject.toml",
            "agents/python/Dockerfile",
            "docs/javascripts/accessibility.js",
            "infra/helmfile.yaml",
            "infra/mlflow/Dockerfile",
            "mise.toml",
            "scripts/install-helm-diff.sh",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            platform = root / "docs/6. Platform/6.2. Platform Install.md"
            text = platform.read_text(encoding="utf-8")
            digest = "@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
            assert digest in text
            platform.write_text(text.replace(digest, "", 1), encoding="utf-8")
            problems = check_conventions.check_source_versions(contract_pages(root, docs), root=root)
        assert any("must include an immutable sha256 digest" in message for _, message in problems)

    def test_real_state_owner_change_rejects_stale_drill_result(self) -> None:
        docs = ("docs/6. Platform/6.6. Platform Delivery.md",)
        owners = (
            "agents/python/src/agent/state.py",
            "infra/k8s/base/state-backup.yaml",
            "infra/scripts/backup-drill.sh",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            drill = root / "infra/scripts/backup-drill.sh"
            text = drill.read_text(encoding="utf-8")
            assert 'echo "drill passed:' in text
            drill.write_text(
                text.replace('echo "drill passed:', 'echo "replacement drill passed:', 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_state_course_contracts(contract_pages(root, docs), root=root)
        assert any("completion line drifted" in message for _, message in problems)

    def test_real_audit_owner_change_rejects_profile_mismatch(self) -> None:
        docs = (
            "docs/8. Community/8.1. License.md",
            "docs/4. Quality/4.1. Linting.md",
        )
        owners = ("scripts/check-licenses.sh", "scripts/check-vulnerabilities.sh")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            audit = root / "scripts/check-vulnerabilities.sh"
            text = audit.read_text(encoding="utf-8")
            assert 'audit_profile "agent evaluation"' in text
            audit.write_text(
                text.replace('audit_profile "agent evaluation"', 'audit_profile "agent extended evaluation"', 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_dependency_audit_course_contracts(
                contract_pages(root, docs),
                root=root,
            )
        assert any("same lock-owned set" in message for _, message in problems)

    def test_real_retrieval_owner_change_requires_new_provenance_field(self) -> None:
        docs = ("docs/3. Capabilities/3.4. Memory.md",)
        owners = ("agents/python/src/agent/retrieval.py",)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            source = root / "agents/python/src/agent/retrieval.py"
            text = source.read_text(encoding="utf-8")
            needle = '        "chunk_count",\n    )'
            assert needle in text
            source.write_text(
                text.replace(needle, '        "chunk_count",\n        "runtime_version",\n    )', 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_retrieval_course_contracts(contract_pages(root, docs), root=root)
        assert any("`runtime_version`" in message for _, message in problems)

    def test_real_domain_owner_change_requires_capstone_review(self) -> None:
        docs = ("docs/8. Community/8.7. Capstone.md",)
        owners = ("agents/python/src/agent/domain.py",)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            source = root / "agents/python/src/agent/domain.py"
            text = source.read_text(encoding="utf-8")
            needle = "    dependency_edges: tuple[tuple[str, str], ...]\n"
            assert needle in text
            source.write_text(
                text.replace(needle, needle + "    ownership_labels: tuple[str, ...]\n", 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_domain_course_contracts(contract_pages(root, docs), root=root)
        assert any("`ownership_labels`" in message for _, message in problems)

    def test_capacity_marker_change_rejects_a_stale_support_table(self) -> None:
        owners = ("SUPPORT.md", "scripts/doctor.sh")
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, owners)
            support = root / "SUPPORT.md"
            text = support.read_text(encoding="utf-8")
            support.write_text(
                text.replace("total-ram-gib=14", "total-ram-gib=99", 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_capacity_course_contracts({}, root=root)
        assert any("99 GiB total RAM" in message for _, message in problems)

    def test_release_workflow_cannot_drop_the_freshness_validator(self) -> None:
        docs = ("docs/8. Community/8.2. Releases.md",)
        owners = (".github/workflows/release.yml",)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (*docs, *owners))
            workflow = root / owners[0]
            text = workflow.read_text(encoding="utf-8")
            workflow.write_text(
                text.replace("scripts/release_freshness.py", "scripts/removed_validator.py", 1),
                encoding="utf-8",
            )
            problems = check_conventions.check_release_freshness_course_contracts(
                contract_pages(root, docs),
                root=root,
            )
        assert any("release_freshness.py" in message for _, message in problems)

    def test_gcp_runbook_rejects_a_root_scoped_tofu_command_without_chdir(self) -> None:
        relative = "infra/gcp/README.md"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, (relative,))
            readme = root / relative
            text = readme.read_text(encoding="utf-8")
            needle = "tofu -chdir=infra/gcp output -raw get_credentials_command"
            assert needle in text
            readme.write_text(text.replace(needle, "tofu output -raw get_credentials_command", 1), encoding="utf-8")
            problems = check_conventions.check_gcp_runbook(root=root)
        assert any("needs `-chdir=infra/gcp`" in message for _, message in problems)

    def test_ci_install_profile_cannot_drift_from_linting_page(self) -> None:
        files = (
            ".github/workflows/ci.yml",
            "README.md",
            "docs/index.md",
            "docs/1. Setup/1.0. System.md",
            "docs/4. Quality/4.1. Linting.md",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, files)
            linting = root / "docs/4. Quality/4.1. Linting.md"
            text = linting.read_text(encoding="utf-8")
            assert "install:validation" in text
            linting.write_text(text.replace("install:validation", "install:maintainer"), encoding="utf-8")
            problems = check_conventions.check_quickstarts(
                contract_pages(root, files[2:]),
                root=root,
            )
        assert any("CI prose must name" in message for _, message in problems)

    def test_outcome_matrix_cannot_exceed_the_linked_exercise(self) -> None:
        docs = (
            "docs/0. Overview/0.0. Course.md",
            "docs/4. Quality/4.4. Evaluations.md",
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            copy_contract_files(root, docs)
            overview = root / docs[0]
            text = overview.read_text(encoding="utf-8")
            overview.write_text(
                text.replace(
                    "add one adversarial case and its deterministic validator",
                    "add a failing case, diagnose it, and repair the behavior",
                    1,
                ),
                encoding="utf-8",
            )
            problems = check_conventions.check_outcome_evidence_contracts(contract_pages(root, docs), root=root)
        assert {message for _, message in problems} == {
            "evaluation outcome promises 'diagnose' but the linked exercise does not require it",
            "evaluation outcome promises 'repair' but the linked exercise does not require it",
        }

    def test_course_evidence_rejects_untracked_source(self) -> None:
        with mock.patch.object(
            course_evidence,
            "git",
            return_value="?? agents/python/src/agent/local_only.py",
        ):
            caught = ""
            try:
                course_evidence.require_clean_revision()
            except ValueError as error:
                caught = str(error)
        assert "tracked or untracked source is dirty" in caught


class ExerciseContractTests(unittest.TestCase):
    def test_temporary_experiment_requires_dirty_preflight_and_cleanup(self) -> None:
        text = """## Your turn: what changes?

- **Mode**: `temporary experiment`
- **Goal**: Change one thing.
- **Files to touch**: `example.py`.
- **Preflight**: Start from a clean checkout.
- **Gate that proves completion**: The test is red.
- **Final state**: Restore the file.
"""
        problems = check_conventions.check_exercises(pathlib.Path("docs/example.md"), text)
        assert len(problems) == 2

    def test_probabilistic_result_cannot_be_a_mandatory_red_state(self) -> None:
        text = """## Your turn: what changes?

- **Mode**: `inspect`
- **Goal**: Observe.
- **Files to touch**: None.
- **Preflight**: None.
- **Gate that proves completion**: It fails without the rule, but may pass.
- **Final state**: Clean.
"""
        problems = check_conventions.check_exercises(pathlib.Path("docs/example.md"), text)
        assert any("probabilistic evidence" in message for _, message in problems)


class DiagramContractTests(unittest.TestCase):
    def test_changed_legacy_diagram_needs_adjacent_words(self) -> None:
        original = ["flowchart LR", "A --> B"]
        changed = """```mermaid
flowchart LR
A --> C
```
"""
        problems = check_conventions.check_diagram_alternatives(
            pathlib.Path("docs/example.md"),
            changed,
            {check_conventions.sha256_lines(original)},
        )
        assert len(problems) == 1


class RouteContractTests(unittest.TestCase):
    def test_manifest_covers_every_dated_changelog_release(self) -> None:
        manifest = json.loads(check_conventions.ROUTE_MANIFEST.read_text(encoding="utf-8"))
        changelog = check_conventions.ROOT.joinpath("CHANGELOG.md").read_text(encoding="utf-8")
        expected = check_conventions.changelog_release_versions(changelog)
        current = set(manifest["releases"]["0.5.0"])

        assert (
            check_conventions.validate_route_manifest(
                current,
                manifest,
                expected_releases=expected,
            )
            == []
        )

    def test_missing_changelog_release_inventory_fails(self) -> None:
        manifest = {
            "format": 1,
            "releases": {"0.2.0": ["index.html"]},
            "redirects": {},
        }
        errors = check_conventions.validate_route_manifest(
            {"index.html"},
            manifest,
            expected_releases={"0.1.0", "0.2.0"},
        )

        assert errors == ["release inventory is missing changelog versions: 0.1.0"]

    def test_historical_route_inventories_match_published_tag_evidence(self) -> None:
        manifest = json.loads(check_conventions.ROUTE_MANIFEST.read_text(encoding="utf-8"))
        expected = {
            "0.1.0": (74, "58fa85e6214c70502068bb6c545527790fc04ab8e87ccb5175e70e7579946c3b"),
            "0.1.1": (76, "660266511809d7f0a19a23764d40dc2d6bfb834de9fafb9c194f5ea9e40a5826"),
            "0.2.0": (76, "660266511809d7f0a19a23764d40dc2d6bfb834de9fafb9c194f5ea9e40a5826"),
            "0.3.5": (76, "660266511809d7f0a19a23764d40dc2d6bfb834de9fafb9c194f5ea9e40a5826"),
        }

        for version, (count, digest) in expected.items():
            routes = manifest["releases"][version]
            encoded = ("\n".join(routes) + "\n").encode()
            assert len(routes) == count
            assert hashlib.sha256(encoded).hexdigest() == digest

    def test_simulated_rename_requires_redirect(self) -> None:
        manifest = {
            "format": 1,
            "releases": {"0.3.5": ["old.html"]},
            "redirects": {},
        }
        assert check_conventions.validate_route_manifest({"new.html"}, manifest) == [
            "released URL 'old.html' is missing a redirect"
        ]

    def test_redirect_loop_fails(self) -> None:
        manifest = {
            "format": 1,
            "releases": {"0.3.5": ["old.html"]},
            "redirects": {"old.html": "older.html", "older.html": "old.html"},
        }
        errors = check_conventions.validate_route_manifest({"new.html"}, manifest)
        assert any("loop" in error for error in errors)

    def test_redirect_traversal_fails_before_generation(self) -> None:
        manifest = {
            "format": 1,
            "releases": {"0.3.5": ["index.html"]},
            "redirects": {"../escape.html": "index.html"},
        }
        errors = check_conventions.validate_route_manifest({"index.html"}, manifest)
        assert errors == ["redirect source is an unsafe course route: '../escape.html'"]


class RenderedContractTests(unittest.TestCase):
    def test_homepage_missing_metadata_and_404_recovery_fails(self) -> None:
        document = """<!doctype html><html lang="en"><head><meta name="description" content="x"></head>
<body><main><h1>Page</h1></main></body></html>"""
        with tempfile.TemporaryDirectory() as directory:
            site = pathlib.Path(directory)
            (site / "index.html").write_text(document, encoding="utf-8")
            (site / "404.html").write_text(document, encoding="utf-8")
            problems = check_conventions.check_rendered(site)
        messages = "\n".join(message for _, message in problems)
        assert "og:title" in messages
        assert "route back home" in messages


if __name__ == "__main__":
    unittest.main()
