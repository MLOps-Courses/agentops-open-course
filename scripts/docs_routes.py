"""Generate static redirect pages from the released course URL contract.

Zensical owns current pages. This script runs after its build and materializes only
historical paths listed in ``docs/released-urls.json``. It refuses loops, missing
targets, and overwriting a current page.
"""

from __future__ import annotations

import html
import json
import pathlib
import sys
from typing import Final
from urllib.parse import quote

import check_conventions  # ty: ignore[unresolved-import]

ROOT: Final = pathlib.Path(__file__).resolve().parent.parent
SITE_URL: Final = "https://agentops-open-course.fmind.dev/"


def redirect_document(old: str, target: str) -> str:
    """Return one accessible redirect document with a canonical terminal target."""
    encoded = quote(target, safe="/")
    absolute = f"{SITE_URL}{encoded}"
    label = html.escape(target)
    return f"""<!doctype html>
<html lang="en" class="no-js">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width,initial-scale=1">
    <meta http-equiv="refresh" content="0; url=/{encoded}">
    <link rel="canonical" href="{absolute}">
    <meta name="robots" content="noindex">
    <title>Page moved - AgentOps Open Course</title>
  </head>
  <body data-course-redirect="{html.escape(old)}">
    <main>
      <h1>This course page moved</h1>
      <p>Continue to <a href="/{encoded}">{label}</a>.</p>
    </main>
  </body>
</html>
"""


def main(argv: list[str]) -> int:
    """Validate the manifest against the build, then write missing historical pages."""
    site = ROOT / (argv[1] if len(argv) > 1 else "site")
    manifest = json.loads(check_conventions.ROUTE_MANIFEST.read_text(encoding="utf-8"))
    changelog = ROOT.joinpath("CHANGELOG.md").read_text(encoding="utf-8")
    expected_releases = check_conventions.changelog_release_versions(changelog)
    current = {path.relative_to(site).as_posix() for path in site.rglob("*.html") if path.name != "404.html"}
    errors = check_conventions.validate_route_manifest(
        current,
        manifest,
        expected_releases=expected_releases,
    )
    if errors:
        for error in errors:
            sys.stderr.write(f"docs/released-urls.json: {error}\n")
        return 1

    redirects = manifest["redirects"]
    site_root = site.resolve()
    for old, target in sorted(redirects.items()):
        destination = site / old
        if not destination.resolve().is_relative_to(site_root):
            sys.stderr.write(f"{old}: refusing to write outside the rendered site\n")
            return 1
        if destination.exists():
            sys.stderr.write(f"{old}: refusing to overwrite a current rendered page\n")
            return 1
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(redirect_document(old, target), encoding="utf-8")
    sys.stdout.write(f"course routes: {len(current)} current, {len(redirects)} redirects\n")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
