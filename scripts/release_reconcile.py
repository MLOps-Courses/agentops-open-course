"""Validate that a reversible release index belongs to one qualified source digest."""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import Any, Final

_DIGEST: Final = re.compile(r"sha256:[0-9a-f]{64}")
_SHA: Final = re.compile(r"[0-9a-f]{40}")
_VERSION: Final = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+")
_INDEX_MEDIA_TYPES: Final = {
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.index.v1+json",
}


def _object(path: Path) -> dict[str, Any]:
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict):
        raise ValueError(f"{path} must contain one JSON object")
    return document


def _package_versions(path: Path) -> list[dict[str, Any]]:
    """Return the package records from ``gh api --paginate --slurp`` output."""
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, list):
        raise ValueError(f"{path} must contain a JSON array of package pages")
    records: list[dict[str, Any]] = []
    for page in document:
        if not isinstance(page, list):
            raise ValueError(f"{path} contains a non-array package page")
        for record in page:
            if not isinstance(record, dict):
                raise ValueError(f"{path} contains a non-object package version")
            records.append(record)
    return records


def _tags(record: dict[str, Any]) -> list[str]:
    metadata = record.get("metadata")
    container = metadata.get("container") if isinstance(metadata, dict) else None
    tags = container.get("tags") if isinstance(container, dict) else None
    if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
        raise ValueError("package version has an invalid container tag inventory")
    return tags


def validate_reconcile_target(
    package_versions: list[dict[str, Any]],
    *,
    version: str,
    sha: str,
    source_digest: str,
    index: dict[str, Any] | None = None,
    resolved_digest: str | None = None,
    registry_absent: bool = False,
) -> dict[str, str | int]:
    """Return one exact package version to delete, or prove that no version tag exists."""
    if not _VERSION.fullmatch(version):
        raise ValueError("release version must be a v-prefixed three-part version")
    if not _SHA.fullmatch(sha):
        raise ValueError("release source must be a full lowercase commit SHA")
    if not _DIGEST.fullmatch(source_digest):
        raise ValueError("release source digest must be an immutable SHA-256")

    if (index is None) != (resolved_digest is None):
        raise ValueError("registry index and resolved digest must be supplied together")
    registry_present = index is not None or resolved_digest is not None
    if registry_present and registry_absent:
        raise ValueError("registry tag cannot be both present and absent")

    matching = [record for record in package_versions if version in _tags(record)]
    if not matching:
        if registry_present:
            raise ValueError("registry tag has no uniquely owned package version")
        if not registry_absent:
            raise ValueError("registry tag absence was not proven")
        return {"state": "absent"}
    if len(matching) != 1:
        raise ValueError("more than one package version carries the release tag")

    record = matching[0]
    if _tags(record) != [version]:
        raise ValueError("release package version carries another tag")
    version_id = record.get("id")
    if isinstance(version_id, bool) or not isinstance(version_id, int) or version_id < 1:
        raise ValueError("release package version has no positive numeric id")
    package_digest = record.get("name")
    if not isinstance(package_digest, str) or not _DIGEST.fullmatch(package_digest):
        raise ValueError("release package version has no immutable index digest")
    if index is None or resolved_digest is None:
        raise ValueError("owned package version does not resolve to a readable registry index")
    if not _DIGEST.fullmatch(resolved_digest) or resolved_digest != package_digest:
        raise ValueError("registry and package API disagree on the release index digest")

    manifests = index.get("manifests")
    annotations = index.get("annotations")
    if index.get("mediaType") not in _INDEX_MEDIA_TYPES:
        raise ValueError("release tag does not resolve to an OCI or Docker index")
    if not isinstance(manifests, list) or len(manifests) != 1 or not isinstance(manifests[0], dict):
        raise ValueError("release index must contain exactly one source manifest")
    if manifests[0].get("digest") != source_digest:
        raise ValueError("release index does not contain the qualified source digest")
    if (
        not isinstance(annotations, dict)
        or annotations.get("org.opencontainers.image.revision") != sha
        or annotations.get("org.opencontainers.image.version") != version
    ):
        raise ValueError("release index annotations do not identify the qualified source")

    return {"state": "owned", "version_id": version_id, "digest": package_digest}


def main(argv: Sequence[str] | None = None) -> None:
    """Validate downloaded package/index documents and print minimized ownership evidence."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--versions", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--source-digest", required=True)
    parser.add_argument("--index", type=Path)
    parser.add_argument("--resolved-digest")
    parser.add_argument("--registry-absent", action="store_true")
    arguments = parser.parse_args(argv)
    try:
        result = validate_reconcile_target(
            _package_versions(arguments.versions),
            version=arguments.version,
            sha=arguments.sha,
            source_digest=arguments.source_digest,
            index=_object(arguments.index) if arguments.index is not None else None,
            resolved_digest=arguments.resolved_digest,
            registry_absent=arguments.registry_absent,
        )
    except (OSError, ValueError, json.JSONDecodeError) as error:
        raise SystemExit(f"error: {error}") from error
    sys.stdout.write(json.dumps(result, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
