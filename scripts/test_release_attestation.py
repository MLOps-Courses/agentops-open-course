"""Unit tests for exact release-SBOM attestation matching."""

from __future__ import annotations

import base64
import json
import unittest

from scripts import release_attestation


def envelope(predicate: object) -> dict[str, str]:
    """Build the verified-cosign JSON shape consumed by the checker."""
    statement = json.dumps({"predicate": predicate}, separators=(",", ":")).encode()
    return {"payload": base64.b64encode(statement).decode()}


class ReleaseAttestationTests(unittest.TestCase):
    def test_one_of_multiple_attestations_may_match(self) -> None:
        sbom = {"name": "release", "packages": []}
        assert release_attestation.verify([envelope({"name": "other"}), envelope(sbom)], sbom) == 2

    def test_mismatched_attestation_is_rejected(self) -> None:
        caught = ""
        try:
            release_attestation.verify(envelope({"name": "other"}), {"name": "release"})
        except ValueError as error:
            caught = str(error)
        assert "matches the release SBOM" in caught

    def test_malformed_envelope_is_rejected(self) -> None:
        caught = ""
        try:
            release_attestation.verify({}, {})
        except ValueError as error:
            caught = str(error)
        assert "base64 payload" in caught
