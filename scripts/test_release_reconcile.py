"""Regression tests for fail-closed cleanup of reversible release indexes."""

# The repository executes this unittest module directly.
# ruff: noqa: PT027

from __future__ import annotations

import copy
import json
import pathlib
import tempfile
import unittest

from scripts import release_reconcile

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


def _index(*, media_type: str = "application/vnd.oci.image.index.v1+json") -> dict:
    index = {
        "mediaType": media_type,
        "manifests": [{"digest": _SOURCE_DIGEST}],
    }
    if media_type == "application/vnd.oci.image.index.v1+json":
        index["annotations"] = {
            "org.opencontainers.image.revision": _SHA,
            "org.opencontainers.image.version": _VERSION,
        }
    return index


def _source_image() -> dict:
    return {
        "config": {
            "Labels": {
                "org.opencontainers.image.revision": _SHA,
                "org.opencontainers.image.version": _VERSION,
            }
        }
    }


class ReleaseReconcileTests(unittest.TestCase):
    def _validate(
        self,
        package_versions: list[dict] | None = None,
        *,
        index: dict | None = None,
        source_image: dict | None = None,
        resolved_digest: str | None = None,
        registry_absent: bool = False,
    ) -> dict[str, str | int]:
        if source_image is None and index is not None:
            source_image = _source_image()
        return release_reconcile.validate_reconcile_target(
            package_versions if package_versions is not None else [_package_version()],
            version=_VERSION,
            sha=_SHA,
            source_digest=_SOURCE_DIGEST,
            index=index,
            source_image=source_image,
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

    def test_docker_manifest_list_uses_the_qualified_child_as_authority(self) -> None:
        docker_index = _index(media_type="application/vnd.docker.distribution.manifest.list.v2+json")
        assert self._validate(index=docker_index, resolved_digest=_INDEX_DIGEST) == {
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
        with self.assertRaisesRegex(ValueError, "complete registry evidence"):
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
        with self.assertRaisesRegex(ValueError, "OCI release index annotations"):
            self._validate(index=index, resolved_digest=_INDEX_DIGEST)

        index.pop("annotations")
        with self.assertRaisesRegex(ValueError, "OCI release index annotations"):
            self._validate(index=index, resolved_digest=_INDEX_DIGEST)

    def test_docker_manifest_list_rejects_unsupported_annotations(self) -> None:
        index = _index(media_type="application/vnd.docker.distribution.manifest.list.v2+json")
        index["annotations"] = {"org.opencontainers.image.revision": _SHA}
        with self.assertRaisesRegex(ValueError, "Docker release index contains unsupported annotations"):
            self._validate(index=index, resolved_digest=_INDEX_DIGEST)

    def test_source_image_labels_must_match_the_release_authority(self) -> None:
        source_image = _source_image()
        source_image["config"]["Labels"]["org.opencontainers.image.revision"] = "d" * 40
        with self.assertRaisesRegex(ValueError, "source image labels"):
            self._validate(index=_index(), source_image=source_image, resolved_digest=_INDEX_DIGEST)

        source_image = _source_image()
        source_image["config"]["Labels"]["org.opencontainers.image.version"] = "v9.9.9"
        with self.assertRaisesRegex(ValueError, "source image labels"):
            self._validate(index=_index(), source_image=source_image, resolved_digest=_INDEX_DIGEST)

        with self.assertRaisesRegex(ValueError, "source image labels"):
            self._validate(index=_index(), source_image={}, resolved_digest=_INDEX_DIGEST)


if __name__ == "__main__":
    unittest.main()
