"""Generate a read-only freshness report for the recurring documentation audit.

The report is advisory. It compares repository-owned pins with upstream release
and package indexes, runs the copied-prose checks that do not need the docs
environment, and writes Markdown without changing project files.
"""

from __future__ import annotations

import argparse
import datetime
import io
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tarfile
import tomllib
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Mapping
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from typing import Final

import check_conventions  # ty: ignore[unresolved-import]

ROOT: Final = pathlib.Path(__file__).resolve().parent.parent
HTTP_TIMEOUT_SECONDS: Final = 10
GITHUB_RELEASES: Final = {
    "k3s": "k3s-io/k3s",
    "Ollama": "ollama/ollama",
    "kagent": "kagent-dev/kagent",
    "mise": "jdx/mise",
}
RELEASE_VALIDATION: Final = {
    "k3s": "check:infra + Platform",
    "Ollama": "doctor:model + Eval",
    "kagent": "check:infra + Platform",
    "mise": "install:maintainer + check",
}
UNSTABLE_TAG: Final = re.compile(
    r"(?:^|[._+-])(?:alpha|beta|rc|pre|preview|dev|nightly)\d*(?:$|[._+-])",
    re.IGNORECASE,
)
VERSION_TAG: Final = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:[+-]k3s(\d+))?$")
HELM_CHART: Final = re.compile(r"^\s*chart:\s+oci://(?P<source>ghcr\.io/[^@\s]+)@(?P<digest>sha256:[0-9a-f]{64})\s*$")
HELM_VERSION: Final = re.compile(r"(?m)^# kagent-chart-version: (\d+\.\d+\.\d+)$")
APK_PIN: Final = re.compile(r"^\s*(?P<package>libstdc\+\+|python-3\.13)=(?P<version>[^\s\\]+)", re.MULTILINE)
REGISTRY_ACCEPT: Final = ", ".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)


@dataclass(frozen=True)
class ReleaseAsset:
    """One named release artifact with an upstream-published digest."""

    name: str
    url: str
    digest: str


@dataclass(frozen=True)
class StableRelease:
    """One stable semantic release selected from an upstream feed."""

    tag: str
    url: str
    assets: tuple[ReleaseAsset, ...] = ()


@dataclass(frozen=True)
class HelmChart:
    """One immutable OCI chart reference from Helmfile."""

    name: str
    source: str
    digest: str


@dataclass(frozen=True)
class MiseUpdate:
    """The actionable subset of one `mise outdated --json` record."""

    requested: str
    latest: str


@dataclass(frozen=True)
class CompatibilityHold:
    """One direct dependency held below upstream latest by a locked owner."""

    package: str
    constraint: str
    owner: str
    owner_constraints: tuple[str, ...]
    validation: str


@dataclass(frozen=True)
class OllamaAssetPin:
    """The exact evaluation archive and checksum committed in the workflow."""

    tag: str
    name: str
    url: str
    digest: str


@dataclass(frozen=True)
class ImageReference:
    """A parsed OCI image name, optional tag, and required digest."""

    registry: str
    repository: str
    tag: str | None
    digest: str


class Fetcher:
    """Small authenticated HTTP client with a fixed timeout and no writes."""

    def __init__(self, github_token: str = "") -> None:
        self.github_token = github_token

    def get(self, url: str, headers: Mapping[str, str] | None = None) -> tuple[bytes, dict[str, str]]:
        """Fetch bytes and response headers from an HTTPS endpoint."""
        if not url.startswith("https://"):
            raise ValueError(f"freshness source must use HTTPS: {url!r}")
        request_headers = {
            "Accept": "application/vnd.github+json",
            "User-Agent": "agentops-open-course-freshness",
            **(headers or {}),
        }
        if self.github_token and url.startswith("https://api.github.com/"):
            request_headers["Authorization"] = f"Bearer {self.github_token}"
            request_headers["X-GitHub-Api-Version"] = "2022-11-28"
        request = urllib.request.Request(url, headers=request_headers)  # noqa: S310 - HTTPS is required above.
        try:
            with urllib.request.urlopen(  # noqa: S310 - the validated request is HTTPS-only.
                request,
                timeout=HTTP_TIMEOUT_SECONDS,
            ) as response:
                return response.read(), dict(response.headers.items())
        except (TimeoutError, urllib.error.URLError) as error:
            raise RuntimeError(f"{url}: {clean_error(error)}") from error

    def json(self, url: str, headers: Mapping[str, str] | None = None) -> object:
        """Fetch and decode one JSON document."""
        body, _ = self.get(url, headers)
        try:
            return json.loads(body)
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise RuntimeError(f"{url}: invalid JSON: {clean_error(error)}") from error


def clean_error(error: object) -> str:
    """Return one bounded, single-line error safe for a Markdown issue."""
    return " ".join(str(error).split())[:300]


def code(value: object) -> str:
    """Render an external or local value as safe inline Markdown code."""
    cleaned = " ".join(str(value).replace("`", "'").split())
    return f"`{cleaned}`"


def version_key(tag: str) -> tuple[int, int, int, int] | None:
    """Return a sortable stable semver key, including k3s build revision."""
    match = VERSION_TAG.fullmatch(tag)
    if match is None:
        return None
    major, minor, patch, k3s_revision = match.groups()
    return int(major), int(minor), int(patch), int(k3s_revision or 0)


def latest_stable_release(document: object) -> StableRelease | None:
    """Select the highest stable semver, even when upstream mislabels RCs."""
    if not isinstance(document, list):
        return None
    candidates: list[tuple[tuple[int, int, int, int], StableRelease]] = []
    for item in document:
        if not isinstance(item, dict) or item.get("draft") is True or item.get("prerelease") is True:
            continue
        tag = item.get("tag_name")
        url = item.get("html_url")
        if not isinstance(tag, str) or not isinstance(url, str) or UNSTABLE_TAG.search(tag):
            continue
        if (key := version_key(tag)) is not None:
            assets: list[ReleaseAsset] = []
            raw_assets = item.get("assets")
            if isinstance(raw_assets, list):
                for raw_asset in raw_assets:
                    if not isinstance(raw_asset, dict):
                        continue
                    name = raw_asset.get("name")
                    asset_url = raw_asset.get("browser_download_url")
                    digest = raw_asset.get("digest")
                    if isinstance(name, str) and isinstance(asset_url, str) and isinstance(digest, str):
                        assets.append(ReleaseAsset(name=name, url=asset_url, digest=digest))
            candidates.append((key, StableRelease(tag=tag, url=url, assets=tuple(assets))))
    return max(candidates, key=lambda candidate: candidate[0])[1] if candidates else None


def parse_mise_outdated(document: object) -> dict[str, MiseUpdate]:
    """Parse the stable, useful fields from `mise outdated --json`."""
    if not isinstance(document, dict):
        raise ValueError("mise outdated output must be a JSON object")
    updates: dict[str, MiseUpdate] = {}
    for name, item in document.items():
        if not isinstance(name, str) or not isinstance(item, dict):
            raise ValueError("mise outdated entries must be named JSON objects")
        requested = item.get("requested")
        latest = item.get("latest")
        if isinstance(requested, str) and isinstance(latest, str):
            updates[name] = MiseUpdate(requested=requested, latest=latest)
    return updates


def mise_result(pinned: str, update: MiseUpdate | None, *, available: bool) -> tuple[str, str]:
    """Return the displayed latest version and triage status for one mise pin."""
    if not available:
        return "unavailable", "UNAVAILABLE"
    latest = update.latest if update else pinned
    return latest, "CURRENT" if latest == pinned else "REVIEW"


def run_mise_outdated() -> tuple[dict[str, MiseUpdate] | None, str | None]:
    """Ask the pinned mise binary for updates without installing any tool."""
    executable = shutil.which("mise")
    if executable is None:
        return None, "mise is not available"
    try:
        result = subprocess.run(  # noqa: S603 - the resolved executable and arguments are fixed.
            (executable, "outdated", "--json"),
            cwd=ROOT,
            check=False,
            capture_output=True,
            text=True,
            timeout=120,
        )
    except (OSError, subprocess.SubprocessError) as error:
        return None, clean_error(error)
    if result.returncode:
        return None, clean_error(result.stderr or f"mise exited {result.returncode}")
    try:
        return parse_mise_outdated(json.loads(result.stdout)), None
    except (ValueError, json.JSONDecodeError) as error:
        return None, clean_error(error)


def mise_pins() -> dict[str, str]:
    """Read exact tool requests from the repository authority."""
    document = tomllib.loads((ROOT / "mise.toml").read_text(encoding="utf-8"))
    tools = document.get("tools")
    if not isinstance(tools, dict):
        raise ValueError("mise.toml has no [tools] table")
    return {name: value for name, value in tools.items() if isinstance(name, str) and isinstance(value, str)}


def parse_helm_charts(text: str) -> tuple[str | None, list[HelmChart]]:
    """Extract the reviewed version and immutable chart sources from Helmfile."""
    version_match = HELM_VERSION.search(text)
    version = version_match.group(1) if version_match else None
    release_name = ""
    charts: list[HelmChart] = []
    for line in text.splitlines():
        if match := re.match(r"^\s*-\s+name:\s+([a-z0-9-]+)\s*$", line):
            release_name = match.group(1)
        elif match := HELM_CHART.match(line):
            charts.append(
                HelmChart(
                    name=release_name or "unnamed",
                    source=match.group("source"),
                    digest=match.group("digest"),
                )
            )
    return version, charts


def oci_digest(fetcher: Fetcher, source: str, tag: str) -> str:
    """Resolve one public GHCR OCI tag to its registry manifest digest."""
    repository = source.removeprefix("ghcr.io/")
    scope = urllib.parse.quote(f"repository:{repository}:pull", safe=":")
    token_document = fetcher.json(f"https://ghcr.io/token?scope={scope}")
    if not isinstance(token_document, dict) or not isinstance(token := token_document.get("token"), str):
        raise RuntimeError(f"GHCR did not issue a pull token for {source}")
    _, headers = fetcher.get(
        f"https://ghcr.io/v2/{repository}/manifests/{urllib.parse.quote(tag, safe='')}",
        {
            "Accept": ", ".join(
                (
                    "application/vnd.oci.image.manifest.v1+json",
                    "application/vnd.oci.image.index.v1+json",
                    "application/vnd.docker.distribution.manifest.v2+json",
                )
            ),
            "Authorization": f"Bearer {token}",
        },
    )
    digest = headers.get("Docker-Content-Digest") or headers.get("docker-content-digest")
    if not digest or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise RuntimeError(f"GHCR returned no immutable digest for {source}:{tag}")
    return digest


def parse_apk_index(text: str, packages: set[str]) -> dict[str, set[str]]:
    """Collect available versions for selected packages from a Wolfi APKINDEX."""
    versions = {package: set() for package in packages}
    for block in text.split("\n\n"):
        fields = dict(line.split(":", 1) for line in block.splitlines() if ":" in line)
        package = fields.get("P")
        version = fields.get("V")
        if package in versions and version:
            versions[package].add(version)
    return versions


def apk_version_key(version: str) -> tuple[int, ...]:
    """Return a sufficient numeric ordering for the course's Wolfi pins."""
    upstream, _, revision = version.partition("-r")
    return (*[int(part) for part in re.findall(r"\d+", upstream)], int(revision or 0))


def wolfi_index(fetcher: Fetcher) -> dict[str, set[str]]:
    """Fetch and parse the current public x86_64 Wolfi package index."""
    body, _ = fetcher.get("https://packages.wolfi.dev/os/x86_64/APKINDEX.tar.gz")
    try:
        with tarfile.open(fileobj=io.BytesIO(body), mode="r:gz") as archive:
            stream = archive.extractfile("APKINDEX")
            if stream is None:
                raise RuntimeError("Wolfi archive has no APKINDEX")
            text = stream.read().decode()
    except (tarfile.TarError, UnicodeDecodeError, KeyError) as error:
        raise RuntimeError(f"invalid Wolfi APKINDEX archive: {clean_error(error)}") from error
    return parse_apk_index(text, {"libstdc++", "python-3.13"})


def local_wolfi_pins() -> list[tuple[str, str, str]]:
    """Return source, package, and exact version from both runtime images."""
    pins: list[tuple[str, str, str]] = []
    for relative in ("agents/python/Dockerfile", "infra/mlflow/Dockerfile"):
        text = (ROOT / relative).read_text(encoding="utf-8")
        pins.extend((relative, match.group("package"), match.group("version")) for match in APK_PIN.finditer(text))
    return pins


def ollama_asset_pin(text: str) -> OllamaAssetPin | None:
    """Extract the exact Ollama evaluation archive and its checked digest."""
    url_match = re.search(
        r'"(?P<url>https://github\.com/ollama/ollama/releases/download/'
        r'(?P<tag>v\d+\.\d+\.\d+)/(?P<name>ollama-[^"]+))"',
        text,
    )
    digest_match = re.search(r'echo\s+"(?P<digest>[0-9a-f]{64})\s+\$\{archive\}"', text)
    if url_match is None or digest_match is None:
        return None
    return OllamaAssetPin(
        tag=url_match.group("tag"),
        name=url_match.group("name"),
        url=url_match.group("url"),
        digest=f"sha256:{digest_match.group('digest')}",
    )


def release_pins(helm_version: str | None) -> dict[str, str]:
    """Extract the local tags compared with the upstream release feeds."""
    k3s_text = (ROOT / "infra/k3d.yaml").read_text(encoding="utf-8")
    ollama_text = (ROOT / ".github/workflows/eval.yml").read_text(encoding="utf-8")
    freshness_text = (ROOT / ".github/workflows/freshness.yml").read_text(encoding="utf-8")
    k3s_match = re.search(r"rancher/k3s:(v[^@\s]+)@sha256:", k3s_text)
    ollama_match = re.search(r"/releases/download/(v\d+\.\d+\.\d+)/ollama-", ollama_text)
    mise_match = re.search(r"^\s*version:\s*(\d{4}\.\d+\.\d+)\s*$", freshness_text, re.MULTILINE)
    return {
        "k3s": k3s_match.group(1) if k3s_match else "not found",
        "Ollama": ollama_match.group(1) if ollama_match else "not found",
        "kagent": f"v{helm_version}" if helm_version else "not found",
        "mise": f"v{mise_match.group(1)}" if mise_match else "not found",
    }


def normalized_release_tag(component: str, tag: str) -> str:
    """Normalize the Docker-safe k3s tag spelling to its GitHub release tag."""
    return tag.replace("-k3s", "+k3s") if component == "k3s" else tag


def release_result(component: str, pinned: str, latest: str) -> str:
    """Return whether one local release pin equals the filtered stable release."""
    return (
        "CURRENT"
        if normalized_release_tag(component, pinned) == normalized_release_tag(component, latest)
        else "REVIEW"
    )


def major_minor(version: str) -> tuple[int, int] | None:
    """Extract a Kubernetes-compatible major/minor pair from a version string."""
    match = re.match(r"^v?(\d+)\.(\d+)", version)
    return (int(match.group(1)), int(match.group(2))) if match else None


def kubernetes_skew_result(k3s_version: str, kubectl_version: str) -> tuple[str, str]:
    """Check kubectl's supported one-minor distance from kube-apiserver."""
    server = major_minor(k3s_version)
    client = major_minor(kubectl_version)
    if server is None or client is None or server[0] != client[0]:
        return "unknown", "REVIEW"
    distance = client[1] - server[1]
    return f"{distance:+d} minor", "CURRENT" if abs(distance) <= 1 else "REVIEW"


def static_image_references() -> dict[str, list[str]]:
    """Inventory static external runtime image references and their source files."""
    candidates = [
        ROOT / "agents/python/Dockerfile",
        ROOT / "infra/mlflow/Dockerfile",
        ROOT / "infra/k3d.yaml",
        ROOT / "infra/observability/compose.yaml",
        ROOT / "scripts/smoke-host.sh",
        *sorted((ROOT / "infra/k8s").rglob("*.yaml")),
        *sorted((ROOT / "infra/k8s").rglob("*.yml")),
    ]
    references: dict[str, list[str]] = {}
    patterns = (
        re.compile(r"^\s*FROM\s+([^\s]+)", re.MULTILINE),
        re.compile(r"^\s*image:\s*[\"']?([^\"'\s#]+)", re.MULTILINE),
        re.compile(r'^\s*readonly\s+\w*image="([^"]+)"', re.MULTILINE),
    )
    for path in candidates:
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8")
        for pattern in patterns:
            for reference in pattern.findall(text):
                if is_external_image(reference):
                    references.setdefault(reference, []).append(path.relative_to(ROOT).as_posix())
    return references


def is_external_image(reference: str) -> bool:
    """Exclude locally built and templated workload references."""
    if any(token in reference for token in ("$", "[", "]")):
        return False
    image_name = reference.split("@", 1)[0].rsplit("/", 1)[-1].split(":", 1)[0]
    return not image_name.startswith("agentops-")


def parse_image_reference(reference: str) -> ImageReference | None:
    """Parse a static digest-pinned image using Docker's default registry rules."""
    if "@" not in reference:
        return None
    name, digest = reference.rsplit("@", 1)
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        return None
    first, separator, remainder = name.partition("/")
    if separator and ("." in first or ":" in first or first == "localhost"):
        registry = first
        repository_and_tag = remainder
    else:
        registry = "docker.io"
        repository_and_tag = name
    last_component = repository_and_tag.rsplit("/", 1)[-1]
    if ":" in last_component:
        repository, tag = repository_and_tag.rsplit(":", 1)
    else:
        repository, tag = repository_and_tag, None
    if registry == "docker.io" and "/" not in repository:
        repository = f"library/{repository}"
    return ImageReference(registry=registry, repository=repository, tag=tag, digest=digest)


def registry_manifest_digest(fetcher: Fetcher, image: ImageReference, reference: str) -> str:
    """Resolve a tag or digest through the registry v2 bearer-auth flow."""
    registry_host = "registry-1.docker.io" if image.registry == "docker.io" else image.registry
    url = f"https://{registry_host}/v2/{image.repository}/manifests/{urllib.parse.quote(reference, safe=':')}"
    base_headers = {
        "Accept": REGISTRY_ACCEPT,
        "User-Agent": "docker/28.0.0 agentops-open-course-freshness",
    }

    def request_manifest(headers: Mapping[str, str]) -> tuple[bytes, dict[str, str]]:
        request = urllib.request.Request(url, headers=dict(headers))
        with urllib.request.urlopen(  # noqa: S310 - URL is HTTPS and parsed locally.
            request,
            timeout=HTTP_TIMEOUT_SECONDS,
        ) as response:
            return response.read(), dict(response.headers.items())

    try:
        _, headers = request_manifest(base_headers)
    except urllib.error.HTTPError as error:
        if error.code != 401:
            raise RuntimeError(f"{url}: HTTP {error.code}") from error
        challenge = error.headers.get("WWW-Authenticate", "")
        if not challenge.lower().startswith("bearer "):
            raise RuntimeError(f"{url}: registry supplied no bearer challenge") from error
        parameters = dict(re.findall(r'([a-z]+)="([^"]*)"', challenge, re.IGNORECASE))
        realm = parameters.pop("realm", "")
        if not realm.startswith("https://"):
            raise RuntimeError(f"{url}: registry supplied an unsafe token realm") from error
        token_url = f"{realm}{'&' if '?' in realm else '?'}{urllib.parse.urlencode(parameters)}"
        token_document = fetcher.json(token_url, {"User-Agent": base_headers["User-Agent"]})
        if not isinstance(token_document, dict):
            raise RuntimeError(f"{url}: registry token response is not an object") from error
        token = token_document.get("token") or token_document.get("access_token")
        if not isinstance(token, str):
            raise RuntimeError(f"{url}: registry supplied no pull token") from error
        try:
            _, headers = request_manifest({**base_headers, "Authorization": f"Bearer {token}"})
        except (TimeoutError, urllib.error.URLError) as retry_error:
            raise RuntimeError(f"{url}: {clean_error(retry_error)}") from retry_error
    except (TimeoutError, urllib.error.URLError) as error:
        raise RuntimeError(f"{url}: {clean_error(error)}") from error
    digest = headers.get("Docker-Content-Digest") or headers.get("docker-content-digest")
    if not digest or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise RuntimeError(f"{url}: registry returned no immutable manifest digest")
    return digest


def image_validation_tier(reference: str, sources: list[str]) -> str:
    """Name the narrow validation required before accepting an image proposal."""
    joined = " ".join(sources)
    if "rancher/k3s" in reference or "infra/k8s" in joined:
        return "check:infra + Platform"
    if "agentgateway" in reference or "smoke-host" in joined:
        return "smoke:host + check:infra"
    if "observability" in joined:
        return "check:infra + observability smoke"
    return "build + scan"


def resolve_image_freshness(fetcher: Fetcher, reference: str) -> tuple[str, str, str, str]:
    """Verify a pinned manifest and compare its tag, when present."""
    image = parse_image_reference(reference)
    if image is None:
        return "unavailable", "unavailable", "REVIEW — mutable", "unavailable"
    host = "registry-1.docker.io" if image.registry == "docker.io" else image.registry
    authority = f"https://{host}/v2/{image.repository}/manifests/"
    pinned = registry_manifest_digest(fetcher, image, image.digest)
    if pinned != image.digest:
        return pinned, "unavailable", "MISMATCH", authority
    if image.tag is None:
        return pinned, "no tag", "RESOLVES", authority
    current = registry_manifest_digest(fetcher, image, image.tag)
    return pinned, current, "CURRENT" if current == image.digest else "REVIEW", authority


def mise_validation_tier(name: str) -> str:
    """Map a tool proposal to the narrowest repository validation tier."""
    if name in {
        "k3d",
        "kubectl",
        "helm",
        "helmfile",
        "skaffold",
        "kubeconform",
        "kube-linter",
        "opentofu",
        "tflint",
        "sops",
        "age",
        "gcloud",
        "github:mikefarah/yq",
        "github:agentgateway/agentgateway",
    }:
        return "install:platform + check:infra"
    if name == "gh":
        return "install:maintainer + check:workflows"
    return "install + check:core"


def canonical_package_name(name: str) -> str:
    """Normalize a Python distribution name using the packaging convention."""
    return re.sub(r"[-_.]+", "-", name).lower()


def compatibility_holds() -> list[CompatibilityHold]:
    """Read the machine-owned compatibility holds from the agent manifest."""
    document = tomllib.loads((ROOT / "agents/python/pyproject.toml").read_text(encoding="utf-8"))
    raw_holds = document.get("tool", {}).get("agentops", {}).get("compatibility-holds", [])
    if not isinstance(raw_holds, list):
        raise ValueError("tool.agentops.compatibility-holds must be an array")
    holds: list[CompatibilityHold] = []
    for item in raw_holds:
        if not isinstance(item, dict):
            raise ValueError("compatibility hold entries must be tables")
        package = item.get("package")
        constraint = item.get("constraint")
        owner = item.get("owner")
        owner_constraints = item.get("owner-constraints")
        validation = item.get("validation")
        if not all(isinstance(value, str) and value for value in (package, constraint, owner, validation)):
            raise ValueError("compatibility holds require package, constraint, owner, and validation strings")
        if (
            not isinstance(owner_constraints, list)
            or not owner_constraints
            or not all(isinstance(value, str) and value for value in owner_constraints)
        ):
            raise ValueError("compatibility holds require non-empty owner-constraints")
        holds.append(
            CompatibilityHold(
                package=package,
                constraint=constraint,
                owner=owner,
                owner_constraints=tuple(owner_constraints),
                validation=validation,
            )
        )
    names = [canonical_package_name(hold.package) for hold in holds]
    if len(names) != len(set(names)):
        raise ValueError("compatibility hold packages must be unique")
    return holds


def agent_requirements() -> dict[str, str]:
    """Return direct agent requirements across runtime and optional groups."""
    document = tomllib.loads((ROOT / "agents/python/pyproject.toml").read_text(encoding="utf-8"))
    requirements = list(document.get("project", {}).get("dependencies", []))
    for group in document.get("dependency-groups", {}).values():
        if isinstance(group, list):
            requirements.extend(group)
    parsed: dict[str, str] = {}
    for requirement in requirements:
        if not isinstance(requirement, str):
            continue
        match = re.match(r"^[A-Za-z0-9_.-]+", requirement)
        if match is not None:
            parsed[canonical_package_name(match.group())] = requirement
    return parsed


def agent_locked_versions() -> dict[str, str]:
    """Return the one resolved version for every agent lock distribution."""
    document = tomllib.loads((ROOT / "agents/python/uv.lock").read_text(encoding="utf-8"))
    versions: dict[str, set[str]] = {}
    for package in document.get("package", []):
        if not isinstance(package, dict):
            continue
        name = package.get("name")
        version = package.get("version")
        if isinstance(name, str) and isinstance(version, str):
            versions.setdefault(canonical_package_name(name), set()).add(version)
    ambiguous = sorted(name for name, resolved in versions.items() if len(resolved) != 1)
    if ambiguous:
        raise ValueError(f"agent lock has multiple versions for: {', '.join(ambiguous)}")
    return {name: next(iter(resolved)) for name, resolved in versions.items()}


def pypi_project(fetcher: Fetcher, package: str) -> tuple[str, tuple[str, ...]]:
    """Read the latest stable version and requirements from PyPI JSON."""
    document = fetcher.json(f"https://pypi.org/pypi/{urllib.parse.quote(package, safe='')}/json")
    if not isinstance(document, dict) or not isinstance(info := document.get("info"), dict):
        raise RuntimeError(f"PyPI returned no project metadata for {package}")
    version = info.get("version")
    requirements = info.get("requires_dist")
    if not isinstance(version, str) or UNSTABLE_TAG.search(version):
        raise RuntimeError(f"PyPI returned no stable latest version for {package}")
    if requirements is None:
        return version, ()
    if not isinstance(requirements, list) or not all(isinstance(requirement, str) for requirement in requirements):
        raise RuntimeError(f"PyPI returned invalid requirements for {package}")
    return version, tuple(requirement for requirement in requirements if isinstance(requirement, str))


def requirement_matches(requirement: str, expected: str) -> bool:
    """Compare the package/specifier part of two requirements."""

    def normalize(value: str) -> str:
        return re.sub(r"\s+", "", value.split(";", 1)[0]).lower()

    return normalize(requirement).startswith(normalize(expected))


def hold_result(
    hold: CompatibilityHold,
    *,
    requirement: str,
    resolved: str,
    latest: str,
    owner_resolved: str,
    owner_latest: str,
    owner_requirements: tuple[str, ...],
) -> str:
    """Classify whether a compatibility hold is current or needs review."""
    if hold.constraint not in requirement:
        return "MISMATCH"
    if owner_resolved != owner_latest:
        return "REVIEW"
    if not all(
        any(requirement_matches(requirement, expected) for requirement in owner_requirements)
        for expected in hold.owner_constraints
    ):
        return "REVIEW"
    return "CURRENT" if resolved == latest else "HELD"


def copied_prose_problems() -> list[tuple[str, str]]:
    """Run the repository-owned checks for values copied from executable sources."""
    pages = {path: path.read_text(encoding="utf-8") for path in sorted((ROOT / "docs").rglob("*.md")) if path.is_file()}
    problems: list[tuple[str, str]] = []
    for check in (
        check_conventions.check_documented_tasks,
        check_conventions.check_task_expansions,
        check_conventions.check_source_versions,
        check_conventions.check_port_contracts,
    ):
        problems.extend(check(pages))
    return sorted(set(problems))


def report(fetcher: Fetcher, *, generated_at: datetime.datetime, run_id: str) -> str:
    """Collect local and upstream evidence into one Markdown issue comment."""
    warnings: list[str] = []
    lines = [
        f"<!-- freshness-report:{run_id} -->",
        f"## Automated freshness snapshot — {generated_at.date().isoformat()}",
        "",
        (
            "The reporter changed no pin, branch, pull request, or release; "
            "the workflow only creates or comments on this audit issue."
        ),
        "",
    ]

    prose_problems = copied_prose_problems()
    lines.extend(
        (
            "### Copied-prose source gate",
            "",
            (
                f"Authority: executable repository sources through {code('scripts/check_conventions.py')}. "
                f"Required validation: {code('mise run check:docs')}."
            ),
            "",
            (
                "PASS — task expansions, documented task names, source versions, and stable ports match their owners."
                if not prose_problems
                else f"REVIEW — {len(prose_problems)} copied-source mismatch(es):"
            ),
        )
    )
    lines.extend(f"- {code(path)}: {message}" for path, message in prose_problems)

    local_mise = mise_pins()
    updates, mise_error = run_mise_outdated()
    if mise_error:
        warnings.append(f"mise outdated: {mise_error}")
    lines.extend(
        (
            "",
            "### mise tool pins",
            "",
            f"{len(local_mise)} exact requests are owned by {code('mise.toml')}; this run installed none of them.",
            "",
            "| Tool | Pinned | Latest reported | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- | --- |",
        )
    )
    for name, pinned in sorted(local_mise.items()):
        update = updates.get(name) if updates is not None else None
        latest, status = mise_result(pinned, update, available=updates is not None)
        authority = "[mise registry](https://mise.jdx.dev/registry.html)"
        lines.append(
            f"| {code(name)} | {code(pinned)} | {code(latest)} | {authority} | "
            f"{code(mise_validation_tier(name))} | {status} |"
        )

    direct_requirements = agent_requirements()
    locked_versions = agent_locked_versions()
    holds = compatibility_holds()
    lines.extend(
        (
            "",
            "### Python compatibility holds",
            "",
            (
                f"{len(holds)} holds are machine-owned by "
                f"{code('agents/python/pyproject.toml')} and rechecked against each owner's latest PyPI metadata."
            ),
            "",
            (
                "| Package | Resolved / latest | Constraint owner | Owner resolved / latest | "
                "Required validation | Result |"
            ),
            "| --- | --- | --- | --- | --- | --- |",
        )
    )
    for hold in holds:
        package_name = canonical_package_name(hold.package)
        owner_name = canonical_package_name(hold.owner)
        requirement = direct_requirements.get(package_name, "")
        resolved = locked_versions.get(package_name, "not found")
        owner_resolved = locked_versions.get(owner_name, "not found")
        latest = "unavailable"
        owner_latest = "unavailable"
        status = "UNAVAILABLE"
        try:
            latest, _ = pypi_project(fetcher, hold.package)
            owner_latest, owner_requirements = pypi_project(fetcher, hold.owner)
            status = hold_result(
                hold,
                requirement=requirement,
                resolved=resolved,
                latest=latest,
                owner_resolved=owner_resolved,
                owner_latest=owner_latest,
                owner_requirements=owner_requirements,
            )
        except (RuntimeError, ValueError) as error:
            warnings.append(f"Python hold {hold.package}: {clean_error(error)}")
        lines.append(
            f"| [{code(hold.package)}](https://pypi.org/project/{hold.package}/) | "
            f"{code(resolved)} / {code(latest)} | {code(hold.owner)} {code(hold.constraint)} | "
            f"{code(owner_resolved)} / {code(owner_latest)} | {code(hold.validation)} | {status} |"
        )

    helm_text = (ROOT / "infra/helmfile.yaml").read_text(encoding="utf-8")
    helm_version, charts = parse_helm_charts(helm_text)
    local_releases = release_pins(helm_version)
    stable_releases: dict[str, StableRelease] = {}
    lines.extend(
        (
            "",
            "### Latest stable upstream releases",
            "",
            "Drafts and tags containing alpha, beta, RC, preview, dev, or nightly markers are excluded explicitly.",
            "",
            "| Component | Repository pin | Latest stable | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- | --- |",
        )
    )
    for component, repository in GITHUB_RELEASES.items():
        try:
            document = fetcher.json(f"https://api.github.com/repos/{repository}/releases?per_page=100")
            latest = latest_stable_release(document)
            if latest is None:
                raise RuntimeError("release feed contained no stable semantic version")
            stable_releases[component] = latest
            local = local_releases[component]
            status = release_result(component, local, latest.tag)
            lines.append(
                f"| {component} | {code(local)} | [{code(latest.tag)}]({latest.url}) | "
                f"[GitHub releases](https://github.com/{repository}/releases) | "
                f"{code(RELEASE_VALIDATION[component])} | {status} |"
            )
        except (RuntimeError, ValueError) as error:
            warnings.append(f"{component} releases: {clean_error(error)}")
            lines.append(
                f"| {component} | {code(local_releases[component])} | unavailable | "
                f"[GitHub releases](https://github.com/{repository}/releases) | "
                f"{code(RELEASE_VALIDATION[component])} | UNAVAILABLE |"
            )

    skew, skew_status = kubernetes_skew_result(local_releases["k3s"], local_mise.get("kubectl", "not found"))
    lines.extend(
        (
            "",
            "### Kubernetes client/server skew",
            "",
            "| k3s API-server pin | kubectl pin | Difference | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- | --- |",
            (
                f"| {code(local_releases['k3s'])} | {code(local_mise.get('kubectl', 'not found'))} | {code(skew)} | "
                "[Kubernetes version-skew policy](https://kubernetes.io/releases/version-skew-policy/) | "
                f"{code('check:infra + Platform')} | {skew_status} |"
            ),
        )
    )

    ollama_pin = ollama_asset_pin((ROOT / ".github/workflows/eval.yml").read_text(encoding="utf-8"))
    latest_ollama = stable_releases.get("Ollama")
    asset_status = "UNAVAILABLE"
    upstream_asset: ReleaseAsset | None = None
    if ollama_pin is None:
        warnings.append("Ollama evaluation workflow has no parseable asset and SHA-256")
    elif latest_ollama is None:
        warnings.append("latest stable Ollama release is unavailable for asset comparison")
    else:
        upstream_asset = next((asset for asset in latest_ollama.assets if asset.name == ollama_pin.name), None)
        if upstream_asset is None:
            warnings.append(f"Ollama release {latest_ollama.tag} has no {ollama_pin.name} digest")
        elif (
            ollama_pin.tag == latest_ollama.tag
            and ollama_pin.url == upstream_asset.url
            and ollama_pin.digest == upstream_asset.digest
        ):
            asset_status = "CURRENT"
        else:
            asset_status = "REVIEW"
    lines.extend(
        (
            "",
            "### Ollama evaluation asset",
            "",
            "| Repository asset pin | Latest stable asset digest | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- |",
            (
                f"| {code(ollama_pin.url if ollama_pin else 'not found')} "
                f"{code(ollama_pin.digest if ollama_pin else 'not found')} | "
                f"{code(upstream_asset.digest if upstream_asset else 'unavailable')} | "
                "[Ollama release assets](https://github.com/ollama/ollama/releases) | "
                f"{code('doctor:model + Eval')} | {asset_status} |"
            ),
        )
    )

    lines.extend(
        (
            "",
            "### kagent Helm chart sources",
            "",
            "| Chart | Immutable source | Digest at reviewed tag | Digest at latest stable tag | Validation | Result |",
            "| --- | --- | --- | --- | --- | --- |",
        )
    )
    for chart in charts:
        reviewed_digest = "unavailable"
        latest_digest = "unavailable"
        status = "UNAVAILABLE"
        try:
            if helm_version is None:
                raise RuntimeError("Helmfile has no reviewed chart version")
            reviewed_digest = oci_digest(fetcher, chart.source, helm_version)
            latest_release = stable_releases.get("kagent")
            if latest_release is None:
                raise RuntimeError("latest stable kagent release is unavailable")
            latest_digest = oci_digest(fetcher, chart.source, latest_release.tag.removeprefix("v"))
            if reviewed_digest != chart.digest:
                status = "MISMATCH"
            elif latest_release.tag == f"v{helm_version}":
                status = "CURRENT"
            else:
                status = "REVIEW"
        except (RuntimeError, ValueError) as error:
            warnings.append(f"{chart.name} OCI source: {clean_error(error)}")
        source = f"oci://{chart.source}@{chart.digest}"
        lines.append(
            f"| {code(chart.name)} | {code(source)} | {code(reviewed_digest)} | {code(latest_digest)} | "
            f"{code('check:infra + Platform')} | {status} |"
        )
    if not charts:
        warnings.append("Helmfile contains no immutable kagent chart references")
        lines.append("| unavailable | unavailable | unavailable | unavailable | `check:infra + Platform` | REVIEW |")

    python_version = (ROOT / ".python-version").read_text(encoding="utf-8").strip()
    wolfi_pins = local_wolfi_pins()
    try:
        available_wolfi = wolfi_index(fetcher)
    except RuntimeError as error:
        available_wolfi = {}
        warnings.append(f"Wolfi APKINDEX: {clean_error(error)}")
    lines.extend(
        (
            "",
            "### Python and Wolfi pins",
            "",
            (
                f"The repository interpreter line is {code(python_version)}. "
                "Wolfi is rolling, so both availability and newest indexed revision matter."
            ),
            "",
            "| Source | Package | Pinned | Latest indexed | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- | --- | --- |",
        )
    )
    for source, package, pinned in wolfi_pins:
        versions = available_wolfi.get(package, set())
        latest = max(versions, key=apk_version_key) if versions else "unavailable"
        if pinned not in versions:
            status = "MISSING" if versions else "UNAVAILABLE"
        else:
            status = "CURRENT" if pinned == latest else "REVIEW"
        lines.append(
            f"| {code(source)} | {code(package)} | {code(pinned)} | {code(latest)} | "
            "[Wolfi APKINDEX](https://packages.wolfi.dev/os/x86_64/) | "
            f"{code('build + scan')} | {status} |"
        )

    images = static_image_references()
    image_results: dict[str, tuple[str, str, str, str]] = {}
    with ThreadPoolExecutor(max_workers=4) as executor:
        pending = {executor.submit(resolve_image_freshness, fetcher, reference): reference for reference in images}
        for future in as_completed(pending):
            reference = pending[future]
            try:
                image_results[reference] = future.result()
            except (RuntimeError, ValueError) as error:
                warnings.append(f"image {reference}: {clean_error(error)}")
                image_results[reference] = ("unavailable", "unavailable", "UNAVAILABLE", "registry v2 API")
    lines.extend(
        (
            "",
            "### Static external image pins",
            "",
            "| Reference | Resolved pin | Current tag digest | Authority | Required validation | Result |",
            "| --- | --- | --- | --- | --- | --- |",
        )
    )
    for reference, sources in sorted(images.items()):
        pinned, current, status, authority = image_results[reference]
        source_note = ", ".join(sorted(set(sources)))
        lines.append(
            f"| {code(reference)} | {code(pinned)} | {code(current)} | "
            f"[registry manifest]({authority}) from {code(source_note)} | "
            f"{code(image_validation_tier(reference, sources))} | {status} |"
        )

    lines.extend(("", "### Collection warnings", ""))
    if warnings:
        lines.extend(f"- REVIEW — {warning}" for warning in warnings)
    else:
        lines.append("- None.")
    lines.extend(
        (
            "",
            (
                "Treat REVIEW, MISMATCH, MISSING, and UNAVAILABLE as triage signals. "
                "This reporter never updates a pin or opens a pull request."
            ),
            "",
        )
    )
    return "\n".join(lines)


def parser() -> argparse.ArgumentParser:
    """Build the dependency-free reporter CLI."""
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("--output", type=pathlib.Path, help="write Markdown here instead of stdout")
    command.add_argument("--run-id", default=os.getenv("GITHUB_RUN_ID", "local"), help="idempotency marker")
    return command


def main(argv: list[str]) -> int:
    """Generate the report, failing only when the local reporter itself is broken."""
    arguments = parser().parse_args(argv[1:])
    try:
        document = report(
            Fetcher(os.getenv("GITHUB_TOKEN", "")),
            generated_at=datetime.datetime.now(datetime.UTC).replace(microsecond=0),
            run_id=arguments.run_id,
        )
        if arguments.output:
            arguments.output.write_text(document, encoding="utf-8")
        else:
            sys.stdout.write(document)
    except (OSError, ValueError, RuntimeError, tomllib.TOMLDecodeError) as error:
        sys.stderr.write(f"freshness report: {clean_error(error)}\n")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
