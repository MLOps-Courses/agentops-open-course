"""Regression tests for fail-closed cleanup of reversible release indexes."""

# The repository executes this unittest module directly.
# ruff: noqa: PT027

from __future__ import annotations

import copy
import json
import pathlib
import tempfile
import unittest

from scripts import release_reconcile  # ty: ignore[unresolved-import]

_VERSION = "v0.5.0"
_SHA = "a" * 40
_SOURCE_DIGEST = "sha256:" + "b" * 64
_INDEX_DIGEST = "sha256:" + "c" * 64


def _package_version(*, digest: str = _INDEX_DIGEST, tags: list[str] | None = None) -> dict:
    return {
        "id": 12345,
        "name": digest,
        "metadata": {"container": {"tags": tags if tags is not None else [_VERSION]}},
    }


def _index() -> dict:
    return {
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [{"digest": _SOURCE_DIGEST}],
        "annotations": {
            "org.opencontainers.image.revision": _SHA,
            "org.opencontainers.image.version": _VERSION,
        },
    }


class ReleaseReconcileTests(unittest.TestCase):
    def _validate(
        self,
        package_versions: list[dict] | None = None,
        *,
        index: dict | None = None,
        resolved_digest: str | None = None,
        registry_absent: bool = False,
    ) -> dict[str, str | int]:
        return release_reconcile.validate_reconcile_target(
            package_versions if package_versions is not None else [_package_version()],
            version=_VERSION,
            sha=_SHA,
            source_digest=_SOURCE_DIGEST,
            index=index,
            resolved_digest=resolved_digest,
            registry_absent=registry_absent,
        )

    def test_absent_version_is_a_safe_noop(self) -> None:
        assert self._validate([], registry_absent=True) == {"state": "absent"}
        with self.assertRaisesRegex(ValueError, "absence was not proven"):
            self._validate([])

    def test_paginated_package_response_is_flattened_without_losing_identity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "versions.json"
            path.write_text(json.dumps([[_package_version()]]), encoding="utf-8")
            assert release_reconcile._package_versions(path) == [_package_version()]  # noqa: SLF001

    def test_exact_owned_index_returns_only_its_delete_identity(self) -> None:
        assert self._validate(index=_index(), resolved_digest=_INDEX_DIGEST) == {
            "state": "owned",
            "version_id": 12345,
            "digest": _INDEX_DIGEST,
        }

    def test_package_tag_ownership_must_be_unique_and_exclusive(self) -> None:
        with self.assertRaisesRegex(ValueError, "more than one"):
            self._validate([_package_version(), _package_version(digest="sha256:" + "d" * 64)])
        with self.assertRaisesRegex(ValueError, "another tag"):
            self._validate([_package_version(tags=[_VERSION, "latest"])])
        with self.assertRaisesRegex(ValueError, "no uniquely owned"):
            self._validate([], index=_index(), resolved_digest=_INDEX_DIGEST)
        with self.assertRaisesRegex(ValueError, "both present and absent"):
            self._validate(index=_index(), resolved_digest=_INDEX_DIGEST, registry_absent=True)

    def test_registry_and_package_digests_must_match(self) -> None:
        with self.assertRaisesRegex(ValueError, "disagree"):
            self._validate(index=_index(), resolved_digest="sha256:" + "d" * 64)
        with self.assertRaisesRegex(ValueError, "readable registry index"):
            self._validate()

    def test_index_must_contain_only_the_qualified_source(self) -> None:
        wrong_child = _index()
        wrong_child["manifests"] = [{"digest": "sha256:" + "d" * 64}]
        with self.assertRaisesRegex(ValueError, "qualified source digest"):
            self._validate(index=wrong_child, resolved_digest=_INDEX_DIGEST)

        multiple = _index()
        multiple["manifests"].append({"digest": "sha256:" + "d" * 64})
        with self.assertRaisesRegex(ValueError, "exactly one"):
            self._validate(index=multiple, resolved_digest=_INDEX_DIGEST)

    def test_index_annotations_must_match_the_release_authority(self) -> None:
        index = copy.deepcopy(_index())
        index["annotations"]["org.opencontainers.image.revision"] = "d" * 40
        with self.assertRaisesRegex(ValueError, "annotations"):
            self._validate(index=index, resolved_digest=_INDEX_DIGEST)


if __name__ == "__main__":
    unittest.main()
