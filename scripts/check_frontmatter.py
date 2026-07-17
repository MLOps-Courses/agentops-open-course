"""Validate that every documentation page starts with parseable YAML front matter.

Guards the failure mode where a description containing an unquoted colon-space
(``description: The why and what: ...``) is invalid YAML. The renderer leaves the
block unparsed, Markdown reads ``text`` + ``---`` as a setext heading, and the
description is published as the page's <h2>. A textual regex cannot see this;
only a YAML parse can.
"""

import pathlib
import re
import sys

import yaml

FRONT_MATTER = re.compile(r"\A---\n(.*?)\n---\n", re.DOTALL)


def check(page: pathlib.Path) -> str | None:
    """Return an error message for `page`, or None when its front matter is valid."""
    match = FRONT_MATTER.match(page.read_text(encoding="utf-8"))
    if match is None:
        return "expected YAML front matter at the start of the file"
    try:
        meta = yaml.safe_load(match.group(1))
    except yaml.YAMLError as error:
        detail = str(error).splitlines()[0]
        return f"front matter is not valid YAML ({detail}); quote values containing ': '"
    if not isinstance(meta, dict):
        return "front matter must be a YAML mapping"
    description = meta.get("description")
    if not isinstance(description, str) or not description.strip():
        return "front matter must define a non-empty description"
    return None


def main() -> int:
    # splitlines(), not split(): page paths contain spaces ("docs/0. Overview/index.md").
    pages = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines()
    failed = 0
    for name in pages:
        page = pathlib.Path(name)
        if error := check(page):
            print(f"{page}: {error}", file=sys.stderr)
            failed = 1
    return failed


if __name__ == "__main__":
    sys.exit(main())
