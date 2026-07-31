"""Offline fixtures for the read-only freshness reporter."""

# unittest's named assertions produce better fixture failures than bare asserts.
# ruff: noqa: PT009

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import freshness_report  # ty: ignore[unresolved-import]


class ReleaseFixtureTests(unittest.TestCase):
    def test_latest_stable_rejects_mislabeled_beta_and_rc_tags(self) -> None:
        fixture = [
            {
                "tag_name": "v0.10.0-rc1",
                "html_url": "https://example.test/rc",
                "draft": False,
                "prerelease": False,
            },
            {
                "tag_name": "v0.10.0-beta11",
                "html_url": "https://example.test/beta",
                "draft": False,
                "prerelease": False,
            },
            {
                "tag_name": "v0.9.12",
                "html_url": "https://example.test/stable",
                "draft": False,
                "prerelease": False,
            },
            {
                "tag_name": "v0.9.13",
                "html_url": "https://example.test/draft",
                "draft": True,
                "prerelease": False,
            },
        ]

        self.assertEqual(
            freshness_report.latest_stable_release(fixture),
            freshness_report.StableRelease("v0.9.12", "https://example.test/stable"),
        )

    def test_k3s_stable_build_revision_sorts_after_lower_revision(self) -> None:
        fixture = [
            {
                "tag_name": "v1.35.6+k3s1",
                "html_url": "https://example.test/one",
                "draft": False,
                "prerelease": False,
            },
            {
                "tag_name": "v1.35.6+k3s2",
                "html_url": "https://example.test/two",
                "draft": False,
                "prerelease": False,
            },
        ]

        release = freshness_report.latest_stable_release(fixture)

        self.assertIsNotNone(release)
        self.assertEqual(release.tag, "v1.35.6+k3s2")

    def test_review_status_detects_k3s_and_ollama_drift(self) -> None:
        self.assertEqual(freshness_report.release_result("k3s", "v1.35.6-k3s1", "v1.36.2+k3s1"), "REVIEW")
        self.assertEqual(freshness_report.release_result("Ollama", "v0.31.2", "v0.32.5"), "REVIEW")


class ParserFixtureTests(unittest.TestCase):
    def test_mise_outdated_keeps_only_actionable_string_versions(self) -> None:
        fixture = {
            "uv": {"requested": "0.11.0", "latest": "0.12.0", "current": "0.11.0"},
            "broken": {"requested": None, "latest": []},
        }

        self.assertEqual(
            freshness_report.parse_mise_outdated(fixture),
            {"uv": freshness_report.MiseUpdate(requested="0.11.0", latest="0.12.0")},
        )

    def test_mise_result_detects_github_cli_drift(self) -> None:
        latest, status = freshness_report.mise_result(
            "2.96.0",
            freshness_report.MiseUpdate(requested="2.96.0", latest="2.97.0"),
            available=True,
        )

        self.assertEqual((latest, status), ("2.97.0", "REVIEW"))

    def test_compatibility_hold_requires_current_owner_and_matching_constraint(self) -> None:
        hold = freshness_report.CompatibilityHold(
            package="mcp",
            constraint="<2",
            owner="google-adk",
            owner_constraints=("mcp<2",),
            validation="MCP tests",
        )

        self.assertEqual(
            freshness_report.hold_result(
                hold,
                requirement="mcp>=1.28,<2",
                resolved="1.29.0",
                latest="2.0.0",
                owner_resolved="2.6.0",
                owner_latest="2.6.0",
                owner_requirements=('mcp<2,>=1.24; extra == "mcp"',),
            ),
            "HELD",
        )
        self.assertEqual(
            freshness_report.hold_result(
                hold,
                requirement="mcp>=1.28,<2",
                resolved="1.29.0",
                latest="2.0.0",
                owner_resolved="2.6.0",
                owner_latest="2.7.0",
                owner_requirements=("mcp>=2",),
            ),
            "REVIEW",
        )

    def test_repository_compatibility_holds_are_unique_and_declared(self) -> None:
        holds = freshness_report.compatibility_holds()
        requirements = freshness_report.agent_requirements()

        self.assertEqual(
            {hold.package for hold in holds},
            {"cryptography", "mcp", "opentelemetry-exporter-otlp-proto-http", "pandas"},
        )
        self.assertTrue(
            all(
                hold.constraint in requirements[freshness_report.canonical_package_name(hold.package)] for hold in holds
            )
        )

    def test_helm_parser_retains_names_sources_and_digests(self) -> None:
        first = "a" * 64
        second = "b" * 64
        fixture = f"""# kagent-chart-version: 0.9.12
releases:
  - name: kagent-crds
    chart: oci://ghcr.io/kagent-dev/kagent/helm/kagent-crds@sha256:{first}
  - name: kagent
    chart: oci://ghcr.io/kagent-dev/kagent/helm/kagent@sha256:{second}
"""

        version, charts = freshness_report.parse_helm_charts(fixture)

        self.assertEqual(version, "0.9.12")
        self.assertEqual(
            charts,
            [
                freshness_report.HelmChart(
                    "kagent-crds",
                    "ghcr.io/kagent-dev/kagent/helm/kagent-crds",
                    f"sha256:{first}",
                ),
                freshness_report.HelmChart(
                    "kagent",
                    "ghcr.io/kagent-dev/kagent/helm/kagent",
                    f"sha256:{second}",
                ),
            ],
        )

    def test_wolfi_index_tracks_all_versions_and_numeric_latest(self) -> None:
        fixture = """P:python-3.13
V:3.13.14-r2

P:python-3.13
V:3.13.14-r10

P:libstdc++
V:16.1.0-r4
"""

        versions = freshness_report.parse_apk_index(fixture, {"python-3.13", "libstdc++"})

        self.assertEqual(versions["python-3.13"], {"3.13.14-r2", "3.13.14-r10"})
        self.assertEqual(max(versions["python-3.13"], key=freshness_report.apk_version_key), "3.13.14-r10")

    def test_ollama_asset_parser_binds_release_url_and_checksum(self) -> None:
        checksum = "f" * 64
        fixture = f"""archive="${{RUNNER_TEMP}}/ollama-linux-amd64.tar.zst"
curl "https://github.com/ollama/ollama/releases/download/v0.32.5/ollama-linux-amd64.tar.zst"
echo "{checksum}  ${{archive}}" | sha256sum --check -
"""

        self.assertEqual(
            freshness_report.ollama_asset_pin(fixture),
            freshness_report.OllamaAssetPin(
                tag="v0.32.5",
                name="ollama-linux-amd64.tar.zst",
                url="https://github.com/ollama/ollama/releases/download/v0.32.5/ollama-linux-amd64.tar.zst",
                digest=f"sha256:{checksum}",
            ),
        )

    def test_image_parser_applies_docker_hub_library_default(self) -> None:
        digest = "a" * 64

        self.assertEqual(
            freshness_report.parse_image_reference(f"python:3.13-slim@sha256:{digest}"),
            freshness_report.ImageReference(
                registry="docker.io",
                repository="library/python",
                tag="3.13-slim",
                digest=f"sha256:{digest}",
            ),
        )


if __name__ == "__main__":
    unittest.main()
