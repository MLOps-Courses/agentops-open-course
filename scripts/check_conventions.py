"""Validate the repository's authoring conventions.

One checker for the three text-shaped gates: course pages, installable skills, and release
metadata. They were three shell scripts plus a Python helper; the helper existed because a regex
cannot safely parse YAML front matter, which is the whole argument for doing this in Python. Rules
here are about *text conventions*, not about running processes — anything that starts a container,
binds a port, or probes PATH stays in shell.

Usage:

    uv run python scripts/check_conventions.py docs
    uv run python scripts/check_conventions.py rendered [site-directory]
    uv run python scripts/check_conventions.py skills
    uv run python scripts/check_conventions.py release-metadata [expected-tag]

Every rule prints ``path: what is wrong`` on stderr and the command exits non-zero, so a failure
names the file and the fix without needing the source.
"""

import ast
import hashlib
import json
import pathlib
import re
import sys
import tomllib
from collections.abc import Iterator
from html.parser import HTMLParser
from typing import Final
from urllib.parse import unquote

ROOT: Final = pathlib.Path(__file__).resolve().parent.parent

# --- course pages -------------------------------------------------------------------------------

# The page frame. Every page opens with this block so a learner can decide in ten seconds whether
# to read it now, skim it, or come back later.
GLANCE_OPEN: Final = '!!! abstract "In one glance"'
GLANCE_FIELDS: Final = ("**You will:**", "**You need:**", "**Time:**")

# One closing landmark, spelled the same way everywhere, so "am I done?" is answered in the same
# place on every page. `docs/index.md` is the site landing page and carries neither frame rule.
CLOSING_HEADINGS: Final = (
    "## What proves this page worked?",
    "## What proves this chapter worked?",
    "## How should you use this page later?",
)
LANDING_PAGE: Final = "docs/index.md"

# Material's footer follows the flattened nav order. These adjacent pairs keep the
# rendered "Next" link aligned with the course's staged prerequisite handoffs.
NAV_SUCCESSORS: Final = {
    "1. Setup/1.1. Python.md": "1. Setup/1.4. Providers.md",
    "1. Setup/1.5. Workspace.md": "2. Agents/index.md",
    "5. Gateway/5.0. Gateway.md": "1. Setup/1.2. Containers.md",
    "1. Setup/1.2. Containers.md": "5. Gateway/5.1. Gateway Setup.md",
    "6. Platform/6.0. Platform.md": "1. Setup/1.3. Kubernetes.md",
    "1. Setup/1.3. Kubernetes.md": "6. Platform/6.1. Containers.md",
}

# The kind word in the Time line, which the chapter index repeats next to the page link.
KINDS: Final = ("concept", "hands-on", "reference", "orientation", "lookup")

# Depth a first-time reader can skip lives behind a collapsible. Three is the
# documented ceiling; more turns the page into a wall of triangles.
MAX_COLLAPSIBLES: Final = 3

# Chapters 2 and 3 quote the reference agent on nearly every page, so a hand-written Python block
# there is the excerpt that rots. Each one must either include the real file or open with a comment
# telling the reader it is deliberately not the source.
SNIPPET_CHAPTERS: Final = ("docs/2. ", "docs/3. ")
SNIPPET_DECLARATIONS: Final = ("# simplified", "# illustrative", "# pseudocode")
SNIPPET_INCLUDE: Final = re.compile(r'--8<--\s+"([^"]+)"')
SOURCE_SNIPPET_SURFACES: Final = {
    "TOML": ("docs/1. Setup/1.1. Python.md", "agents/python/pyproject.toml:runtime-dependencies"),
    "YAML": ("docs/5. Gateway/5.0. Gateway.md", "infra/agentgateway/host/config.yaml:host-mcp-bind"),
    "shell": ("docs/5. Gateway/5.1. Gateway Setup.md", "infra/scripts/gateway-host.sh:hardened-container-args"),
    "Dockerfile": ("docs/6. Platform/6.1. Containers.md", "agents/python/Dockerfile:runtime-base"),
    "Helm": ("docs/6. Platform/6.2. Platform Install.md", "infra/helmfile.yaml:kagent-crds-release"),
    "workflow": (
        "docs/8. Community/8.2. Releases.md",
        ".github/workflows/release.yml:release-dispatch-authority",
    ),
}

FRONT_MATTER: Final = re.compile(r"\A---\n(.*?)\n---\n", re.DOTALL)
FENCE: Final = re.compile(r"^\s*(`{3,}|~{3,})\s*(\S*)")
MACHINE_PATH: Final = re.compile(r"/home/[^ /]+|file:///|k3d-registry\.localhost")
COLLAPSIBLE: Final = re.compile(r'^\s*\?\?\? \w+ "([^"]*)"')
BARE_NUMBER_LABEL: Final = re.compile(r"\[(\d+(?:\.\d+)*)\]\(")
INDEX_KIND_LABEL: Final = re.compile(r"\[(\d+\.\d+\.[^\]]+)\][^\n]*?_\(([a-z-]+)[^)]*\)_")
TIME_LINE: Final = re.compile(r"^ {4}- \*\*Time:\*\* (.+)$", re.MULTILINE)
ADMONITION: Final = re.compile(r'^\s*(?:!!!|\?\?\?)\s+([a-z-]+)(?:\s+"[^"]*")?\s*$')
ORDERED_ITEM: Final = re.compile(r"^(\s*)(\d+)\.\s")
FULL_PAGE_LINK: Final = re.compile(r"\[([0-8]\.\d+\.\s+[^\]]+)\]\(([^)#]+)")
DOCUMENTED_TASK: Final = re.compile(r"\bmise run ([A-Za-z0-9][A-Za-z0-9:_*-]*(?:<[^>]+>)?)")
TASK_TABLE_ROW: Final = re.compile(r"^\| `mise run ([^`]+)`\s+\| `([^`]+)`\s+\|", re.MULTILINE)
TOOL_TABLE_ROW: Final = re.compile(r"^\|\s*([^|]+?)\s*\|\s*([v\d][^|]*?)\s*\|", re.MULTILINE)
EXERCISE_MODE: Final = re.compile(r"\*\*Mode\*\*:\s*`(inspect|temporary experiment|keep|capstone carry-forward)`")
ALLOWED_ADMONITIONS: Final = {
    "abstract",
    "danger",
    "info",
    "note",
    "success",
    "tip",
    "warning",
}
EXERCISE_FIELDS: Final = (
    "**Mode**:",
    "**Goal**:",
    "**Files to touch**:",
    "**Preflight**:",
    ("**Gate that proves completion**:", "**Proof of completion**:"),
    "**Final state**:",
)
DIAGRAM_LEGACY: Final = ROOT / "docs/diagram-legacy.txt"
ROUTE_MANIFEST: Final = ROOT / "docs/released-urls.json"
PORT_CONTRACT: Final = {
    3000: ("infra/agentgateway/host/config.yaml", r"(?m)^\s*-\s+port:\s+3000$"),
    3001: ("infra/agentgateway/host/config.yaml", r"(?m)^\s*-\s+port:\s+3001$"),
    4000: ("infra/agentgateway/host/config.yaml", r"(?m)^\s*-\s+port:\s+4000$"),
    15020: ("infra/scripts/gateway-host.sh", r"AGENTOPS_GATEWAY_METRICS_PORT:-15020"),
    15021: ("infra/scripts/gateway-host.sh", r"AGENTOPS_GATEWAY_READINESS_PORT:-15021"),
    8000: ("agents/python/mise.toml", r'MCP_PORT = "8000"'),
    8080: ("agents/python/src/agent/config.py", r"a2a_port: int = Field\(default=8080,"),
    8001: ("mise.toml", r"python3 -m http\.server 8001"),
    8002: ("agents/python/mise.toml", r"adk web src --port 8002"),
    8003: ("mise.toml", r"zensical serve --dev-addr 127\.0\.0\.1:8003"),
    11434: ("agents/python/src/agent/config.py", r'default="http://127\.0\.0\.1:11434/v1"'),
    5000: ("infra/observability/compose.yaml", r"MLFLOW_PORT:-5000"),
    4317: ("infra/observability/compose.yaml", r"OTEL_GRPC_PORT:-4317"),
    4318: ("infra/observability/compose.yaml", r"OTEL_HTTP_PORT:-4318"),
    8889: ("infra/observability/otel-collector.yaml", r"endpoint: 0\.0\.0\.0:8889"),
    9090: ("infra/observability/compose.yaml", r"PROMETHEUS_PORT:-9090"),
    9093: ("infra/observability/compose.yaml", r"ALERTMANAGER_PORT:-9093"),
    3002: ("infra/observability/compose.yaml", r"GRAFANA_PORT:-3002"),
    3100: ("infra/observability/compose.yaml", r"LOKI_PORT:-3100"),
    5050: ("infra/k3d.yaml", r'hostPort: "5050"'),
}

Problem = tuple[str, str]


def outside_fences(text: str) -> Iterator[tuple[int, str]]:
    """Yield (line number, line) for every line that is not inside a fenced code block."""
    fenced = False
    for number, line in enumerate(text.splitlines(), start=1):
        if line.lstrip().startswith(("```", "~~~")):
            fenced = not fenced
            continue
        if not fenced:
            yield number, line


def fenced_blocks(text: str, language: str) -> Iterator[tuple[int, list[str]]]:
    """Yield (opening line number, body lines) for every fenced block in one language.

    A fence can be indented inside an admonition and can use either delimiter, so the close is
    matched against the run that opened it — a shorter fence inside a block does not end it.
    """
    delimiter = language_of = ""
    start, body = 0, []
    for number, line in enumerate(text.splitlines(), start=1):
        match = FENCE.match(line)
        if not delimiter:
            if match:
                delimiter, language_of, start, body = match.group(1), match.group(2), number, []
            continue
        if match and not match.group(2) and match.group(1)[0] == delimiter[0] and len(match.group(1)) >= len(delimiter):
            if language_of == language:
                yield start, body
            delimiter = ""
            continue
        body.append(line)


def fenced_block_ranges(text: str, language: str) -> Iterator[tuple[int, int, list[str]]]:
    """Yield opening line, closing line, and body for each fence in one language."""
    delimiter = language_of = ""
    start, body = 0, []
    for number, line in enumerate(text.splitlines(), start=1):
        match = FENCE.match(line)
        if not delimiter:
            if match:
                delimiter, language_of, start, body = match.group(1), match.group(2), number, []
            continue
        if match and not match.group(2) and match.group(1)[0] == delimiter[0] and len(match.group(1)) >= len(delimiter):
            if language_of == language:
                yield start, number, body
            delimiter = ""
            continue
        body.append(line)


def section_after(lines: list[str], start: int) -> list[str]:
    """Return one exercise section, stopping before the next H2."""
    end = next((index for index in range(start + 1, len(lines)) if lines[index].startswith("## ")), len(lines))
    return lines[start:end]


def page_url(page: pathlib.Path) -> str:
    """Return the deployed path emitted by use_directory_urls=false."""
    relative = page.relative_to(ROOT / "docs")
    return relative.with_suffix(".html").as_posix()


def sha256_lines(lines: list[str]) -> str:
    """Hash a normalized fenced block for a small, reviewable legacy ratchet."""
    return hashlib.sha256(("\n".join(lines).strip() + "\n").encode()).hexdigest()


def declared_kind(text: str) -> str | None:
    """Return the single page-kind word in the Time line, or None when it is absent or ambiguous."""
    match = TIME_LINE.search(text)
    if match is None:
        return None
    found = [kind for kind in KINDS if re.search(rf"\b{re.escape(kind)}\b", match.group(1))]
    return found[0] if len(found) == 1 else None


def parse_front_matter(text: str) -> tuple[dict[str, object] | None, str | None]:
    """Return (mapping, error) for a file's YAML front matter.

    PyYAML is imported here rather than at module scope so the release-metadata check stays
    dependency-free: it runs in a CI job that has only a checkout and a system Python.
    """
    import yaml

    match = FRONT_MATTER.match(text)
    if match is None:
        return None, "expected YAML front matter at the start of the file"
    try:
        meta = yaml.safe_load(match.group(1))
    except yaml.YAMLError as error:
        return None, f"front matter is not valid YAML ({str(error).splitlines()[0]}); quote values containing ': '"
    if not isinstance(meta, dict):
        return None, "front matter must be a YAML mapping"
    return meta, None


def check_front_matter(page: pathlib.Path, text: str) -> list[Problem]:
    """Front matter must be real YAML with a non-empty description.

    An unquoted ``key: value`` containing a colon-space looks fine but is invalid YAML. The
    renderer then leaves the block unparsed, Markdown reads ``text`` + ``---`` as a setext heading,
    and the description is published as the page's <h2>. Only a parser catches that.
    """
    name = page.as_posix()
    meta, error = parse_front_matter(text)
    if meta is None:
        return [(name, error or "unreadable front matter")]
    description = meta.get("description")
    if not isinstance(description, str) or not description.strip():
        return [(name, "front matter must define a non-empty description")]
    return []


def check_headings(page: pathlib.Path, text: str) -> list[Problem]:
    """Every page is an FAQ: at least one H2, and every H2 phrased as a question."""
    name = page.as_posix()
    problems = [
        (name, f"FAQ heading must end with ?: {line}")
        for _, line in outside_fences(text)
        if line.startswith("## ") and not line.endswith("?")
    ]
    if not any(line.startswith("## ") for _, line in outside_fences(text)):
        problems.append((name, "expected at least one FAQ question heading"))
    return problems


def check_glance(page: pathlib.Path, text: str) -> list[Problem]:
    """The orientation block sits between the H1 and the first H2, with all three fields."""
    name = page.as_posix()
    inside = False
    seen: set[str] = set()
    for _, line in outside_fences(text):
        if line.startswith("## "):
            break
        if line == GLANCE_OPEN:
            inside = True
            continue
        if inside:
            seen.update(field for field in GLANCE_FIELDS if line.startswith(f"    - {field}"))
    if not inside or len(seen) != len(GLANCE_FIELDS):
        missing = ", ".join(field for field in GLANCE_FIELDS if field not in seen)
        return [
            (name, f'expected an "In one glance" abstract block between the H1 and the first H2 (missing: {missing})')
        ]
    return []


def check_closing(page: pathlib.Path, text: str) -> list[Problem]:
    """The last H2 is the page's exit, spelled one of three fixed ways."""
    name = page.as_posix()
    headings = [line for _, line in outside_fences(text) if line.startswith("## ")]
    if headings and headings[-1] not in CLOSING_HEADINGS:
        allowed = " | ".join(f'"{heading.removeprefix("## ")}"' for heading in CLOSING_HEADINGS)
        return [(name, f"last H2 must be one of {allowed}, not: {headings[-1]}")]
    return []


def check_kind(page: pathlib.Path, text: str) -> list[Problem]:
    """The Time line names exactly one page kind, so the chapter index can repeat it."""
    if declared_kind(text) is not None:
        return []
    return [(page.as_posix(), f"the Time line must name exactly one page kind: {', '.join(KINDS)}")]


def check_collapsibles(page: pathlib.Path, text: str) -> list[Problem]:
    """Collapsibles are titled consistently and stay rare enough to read past."""
    name = page.as_posix()
    summaries = [match.group(1) for line in text.splitlines() if (match := COLLAPSIBLE.match(line))]
    problems = [
        (name, f'collapsible summary must start with "Deeper: ": {summary}')
        for summary in summaries
        if not summary.startswith("Deeper: ")
    ]
    if len(summaries) > MAX_COLLAPSIBLES:
        problems.append((name, f"{len(summaries)} collapsibles; at most {MAX_COLLAPSIBLES} keep a page readable"))
    return problems


def check_link_labels(page: pathlib.Path, text: str) -> list[Problem]:
    """A link label is a page name. The audience is exactly the reader who does not know what 5.2 is."""
    return [
        (page.as_posix(), f"link label must be the page name, not a bare number: [{label}]")
        for label in BARE_NUMBER_LABEL.findall(text)
    ]


def check_snippets(page: pathlib.Path, text: str) -> list[Problem]:
    """A snippet include belongs inside a fence.

    Outside one it is rendered as Markdown, so a leading ``#`` comment in the included region
    becomes an <h1> on the published page.
    """
    return [
        (page.as_posix(), f"line {number}: snippet include must sit inside a fenced code block")
        for number, line in outside_fences(text)
        if line.strip().startswith("--8<--")
    ]


def check_snippet_targets(
    page: pathlib.Path,
    text: str,
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Every checked include resolves to one bounded repository source region."""
    problems: list[Problem] = []
    for number, line in enumerate(text.splitlines(), start=1):
        for specification in SNIPPET_INCLUDE.findall(line):
            relative, separator, region = specification.partition(":")
            path = pathlib.PurePosixPath(relative)
            where = page.as_posix()
            if path.is_absolute() or ".." in path.parts:
                problems.append((where, f"line {number}: snippet source is outside the repository: {relative!r}"))
                continue
            source = root.joinpath(*path.parts)
            if not source.is_file():
                problems.append((where, f"line {number}: snippet source does not exist: {relative!r}"))
                continue
            if not separator or not region:
                problems.append((where, f"line {number}: trusted snippet must name a source region: {specification!r}"))
                continue
            source_text = source.read_text(encoding="utf-8")
            start = f"--8<-- [start:{region}]"
            end = f"--8<-- [end:{region}]"
            if source_text.count(start) != 1 or source_text.count(end) != 1:
                message = (
                    f"line {number}: snippet region {region!r} needs exactly one start and end marker in {relative}"
                )
                problems.append(
                    (
                        where,
                        message,
                    )
                )
            elif source_text.index(start) >= source_text.index(end):
                problems.append((where, f"line {number}: snippet region {region!r} ends before it starts"))
    return problems


def check_source_snippet_coverage(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Ratchet the non-Python source formats learners are invited to trust."""
    problems: list[Problem] = []
    for format_name, (relative, specification) in SOURCE_SNIPPET_SURFACES.items():
        text = pages.get(ROOT / relative, "")
        if f'--8<-- "{specification}"' not in text:
            problems.append((relative, f"source-backed {format_name} example must include {specification!r}"))
    return problems


def sourced(body: list[str]) -> bool:
    """True when a code block pulls its source in, or admits it is not the source."""
    lines = [line.strip() for line in body if line.strip()]
    if not lines:
        return False
    return lines[0].startswith(SNIPPET_DECLARATIONS) or any(line.startswith("--8<--") for line in lines)


def check_python_blocks(page: pathlib.Path, text: str) -> list[Problem]:
    """A Python block in the chapters that quote the agent is the source, or says it is not.

    ``pymdownx.snippets`` re-reads an include on every build, so an include cannot drift; a pasted
    block can, and four already had. Nothing here compares text to source — that is the include's
    whole job. This rule removes the third option: quoting source by hand without saying so.
    """
    name = page.as_posix()
    if not name.startswith(SNIPPET_CHAPTERS):
        return []
    undeclared = [number for number, body in fenced_blocks(text, "python") if not sourced(body)]
    if not undeclared:
        return []
    lines = ", ".join(str(number) for number in undeclared)
    remedy = (
        f'make it a "--8<--" include of the real source, or open the block with a `{SNIPPET_DECLARATIONS[0]}` comment'
    )
    return [(name, f"{len(undeclared)} unowned python blocks (lines {lines}): {remedy}")]


def check_admonitions(page: pathlib.Path, text: str) -> list[Problem]:
    """Use the small semantic admonition vocabulary documented in AGENTS.md."""
    return [
        (page.as_posix(), f"line {number}: unsupported admonition type {kind!r}")
        for number, line in outside_fences(text)
        if (match := ADMONITION.match(line)) and (kind := match.group(1)) not in ALLOWED_ADMONITIONS
    ]


def check_ordered_lists(page: pathlib.Path, text: str) -> list[Problem]:
    """Ordered Markdown always uses `1.` so inserts do not renumber a whole diff."""
    return [
        (page.as_posix(), f"line {number}: ordered Markdown items must use `1.`, not `{index}.`")
        for number, line in outside_fences(text)
        if (match := ORDERED_ITEM.match(line)) and (index := match.group(2)) != "1"
    ]


def check_page_link_targets(page: pathlib.Path, text: str) -> list[Problem]:
    """A numbered page label must agree with the page file it targets."""
    problems: list[Problem] = []
    for label, target in FULL_PAGE_LINK.findall(text):
        stem = pathlib.PurePosixPath(unquote(target)).stem
        if label != stem and not label.startswith(f"{stem} —"):
            problems.append(
                (
                    page.as_posix(),
                    f"link label {label!r} targets page {stem!r}; use the target's full page name",
                )
            )
    return problems


def check_hands_on_action(page: pathlib.Path, text: str) -> list[Problem]:
    """A hands-on page reaches a runnable command within its first two H2 sections."""
    if declared_kind(text) != "hands-on":
        return []
    h2_lines = [number for number, line in outside_fences(text) if line.startswith("## ")]
    limit = h2_lines[2] if len(h2_lines) > 2 else len(text.splitlines()) + 1
    actionable = any(
        start < limit and language in {"bash", "console", "shell", "sh"}
        for language in ("bash", "console", "shell", "sh")
        for start, _ in fenced_blocks(text, language)
    )
    if actionable:
        return []
    return [
        (
            page.as_posix(),
            "hands-on page must reach a bash/shell/console command within its first two H2 sections",
        )
    ]


def exercise_sections(text: str) -> Iterator[tuple[int, list[str]]]:
    """Yield every modifying/assessment exercise as an isolated section."""
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.startswith(("## Your turn:", "Exercise:", "Optional exercise:")):
            yield index + 1, section_after(lines, index)


def check_exercises(page: pathlib.Path, text: str) -> list[Problem]:
    """Every exercise declares authority, dirty-tree safety, evidence, and final state."""
    name = page.as_posix()
    problems: list[Problem] = []
    for line_number, section_lines in exercise_sections(text):
        section = "\n".join(section_lines)
        for field in EXERCISE_FIELDS:
            alternatives = (field,) if isinstance(field, str) else field
            if not any(candidate in section for candidate in alternatives):
                problems.append(
                    (
                        name,
                        f"line {line_number}: exercise is missing {' or '.join(alternatives)}",
                    )
                )
        mode = EXERCISE_MODE.search(section)
        if mode is None:
            continue
        if mode.group(1) == "temporary experiment":
            if "git diff --quiet --" not in section and "test ! -e " not in section:
                problems.append(
                    (
                        name,
                        f"line {line_number}: temporary experiment needs a target-specific dirty preflight",
                    )
                )
            if "git restore --" not in section and "rm --" not in section:
                problems.append(
                    (
                        name,
                        f"line {line_number}: temporary experiment needs target-specific cleanup",
                    )
                )
        if "git checkout --" in section:
            problems.append(
                (
                    name,
                    f"line {line_number}: use scoped `git restore -- <files>` instead of deprecated checkout cleanup",
                )
            )
        if re.search(r"git restore -- (?:agents/data|infra/k8s)(?:\s|`|$)", section):
            problems.append(
                (
                    name,
                    f"line {line_number}: cleanup restores a directory and can discard unrelated learner work",
                )
            )
        if re.search(r"\bmay pass\b", section, re.IGNORECASE) and re.search(
            r"\b(?:must fail|fails? without|exits? non-zero)\b", section, re.IGNORECASE
        ):
            problems.append(
                (
                    name,
                    f"line {line_number}: probabilistic evidence cannot satisfy a mandatory red-state claim",
                )
            )
    return problems


def check_diagram_alternatives(page: pathlib.Path, text: str, legacy: set[str]) -> list[Problem]:
    """A Mermaid diagram has adjacent prose, or an exact reviewed legacy hash."""
    name = page.as_posix()
    lines = text.splitlines()
    problems: list[Problem] = []
    for start, end, body in fenced_block_ranges(text, "mermaid"):
        nearby = "\n".join(lines[max(0, start - 5) : min(len(lines), end + 5)])
        if "**Diagram in words:**" in nearby:
            continue
        digest = sha256_lines(body)
        if digest not in legacy:
            problems.append(
                (
                    name,
                    f"line {start}: Mermaid diagram needs adjacent `**Diagram in words:**` prose",
                )
            )
    return problems


def check_machine_paths(page: pathlib.Path, text: str) -> list[Problem]:
    """No machine-specific path, local file URL, or obsolete registry hostname."""
    return [
        (page.as_posix(), f"line {number}: found a machine-specific path or obsolete registry hostname")
        for number, line in enumerate(text.splitlines(), start=1)
        if MACHINE_PATH.search(line)
    ]


def check_index_kinds(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """A chapter index labels each sub-page with the kind that page declares for itself."""
    kinds = {page: declared_kind(text) for page, text in pages.items()}
    problems: list[Problem] = []
    for index, text in pages.items():
        if index.name != "index.md":
            continue
        for label, labelled in INDEX_KIND_LABEL.findall(text):
            target = index.parent / f"{label}.md"
            declared = kinds.get(target)
            if labelled in KINDS and declared is not None and labelled != declared:
                problems.append(
                    (index.as_posix(), f"labels {label} as ({labelled}) but that page declares ({declared})")
                )
    return problems


def nav_paths(value: object) -> Iterator[str]:
    """Yield Markdown targets in Material's flattened footer order."""
    if isinstance(value, str):
        if value.endswith(".md"):
            yield value
        return
    if isinstance(value, list):
        for item in value:
            yield from nav_paths(item)
        return
    if isinstance(value, dict):
        for item in value.values():
            yield from nav_paths(item)


def check_navigation(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Every page appears once, and staged handoffs match the rendered footer."""
    import yaml

    config_path = ROOT / "mkdocs.yml"

    class _NavigationLoader(yaml.SafeLoader):
        """Keep Python-name extension tags inert while reading only the nav."""

    _NavigationLoader.add_multi_constructor(
        "tag:yaml.org,2002:python/name:",
        lambda _loader, suffix, _node: suffix,
    )
    try:
        loader = _NavigationLoader(config_path.read_text(encoding="utf-8"))
        try:
            config = loader.get_single_data()
        finally:
            loader.dispose()
    except (OSError, yaml.YAMLError) as error:
        return [("mkdocs.yml", f"could not read navigation: {error}")]
    if not isinstance(config, dict) or "nav" not in config:
        return [("mkdocs.yml", "expected an explicit nav list")]

    targets = list(nav_paths(config["nav"]))
    duplicates = sorted(target for target in set(targets) if targets.count(target) > 1)
    expected = {page.relative_to(ROOT / "docs").as_posix() for page in pages}
    missing = sorted(expected - set(targets))
    extra = sorted(set(targets) - expected)
    problems: list[Problem] = []
    if duplicates:
        problems.append(("mkdocs.yml", f"navigation repeats pages: {', '.join(duplicates)}"))
    if missing:
        problems.append(("mkdocs.yml", f"navigation omits pages: {', '.join(missing)}"))
    if extra:
        problems.append(("mkdocs.yml", f"navigation references unknown pages: {', '.join(extra)}"))

    positions = {target: index for index, target in enumerate(targets)}
    for current, successor in NAV_SUCCESSORS.items():
        position = positions.get(current)
        if position is None or position + 1 >= len(targets) or targets[position + 1] != successor:
            problems.append(("mkdocs.yml", f"rendered footer after {current} must continue to {successor}"))
    return problems


def read_toml(path: pathlib.Path) -> dict[str, object]:
    """Read one repository TOML authority."""
    with path.open("rb") as stream:
        return tomllib.load(stream)


def project_dependencies(path: pathlib.Path) -> list[str]:
    """Return the project dependencies from one pyproject."""
    project = read_toml(path).get("project")
    if not isinstance(project, dict):
        return []
    dependencies = project.get("dependencies")
    if not isinstance(dependencies, list):
        return []
    return [dependency for dependency in dependencies if isinstance(dependency, str)]


def dependency_spec(path: pathlib.Path, name: str) -> str | None:
    """Return the requirement suffix for one named project dependency."""
    normalized = name.lower().replace("_", "-")
    for dependency in project_dependencies(path):
        requirement_name = re.split(r"[\[<>=!~ ]", dependency, maxsplit=1)[0].lower().replace("_", "-")
        if requirement_name == normalized:
            return dependency[len(re.split(r"[\[<>=!~ ]", dependency, maxsplit=1)[0]) :]
    return None


def exact_dependency(path: pathlib.Path, name: str) -> str | None:
    """Return an exact `==` dependency pin."""
    spec = dependency_spec(path, name)
    if spec and (match := re.fullmatch(r"==([^; ]+)(?:\s*;.*)?", spec)):
        return match.group(1)
    return None


def compare_contract(where: str, label: str, expected: str | None, actual: str | None) -> list[Problem]:
    """Compare one copied value with the repository authority that owns it."""
    if expected is None:
        return [(where, f"could not resolve authoritative {label}")]
    if actual != expected:
        return [(where, f"{label} drifted: expected {expected!r}, found {actual!r}")]
    return []


def task_names(manifest: pathlib.Path) -> set[str]:
    """Return public and hidden mise task names from one manifest."""
    tasks = read_toml(manifest).get("tasks")
    return {name for name in tasks if isinstance(name, str)} if isinstance(tasks, dict) else set()


def task_expansion(manifest: pathlib.Path, name: str) -> str | None:
    """Return the public scalar environment plus command for one mise task."""
    tasks = read_toml(manifest).get("tasks")
    if not isinstance(tasks, dict) or not isinstance(task := tasks.get(name), dict):
        return None
    run = task.get("run")
    if not isinstance(run, str):
        return None
    environment = task.get("env")
    prefixes: list[str] = []
    if isinstance(environment, dict):
        prefixes = [
            f"{key}={value}"
            for key, value in environment.items()
            if isinstance(key, str) and key.isupper() and isinstance(value, (str, int, float, bool))
        ]
    return " ".join((*prefixes, run))


def check_documented_tasks(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Every documented `mise run` name resolves in one repository project."""
    known = (
        task_names(ROOT / "mise.toml")
        | task_names(ROOT / "agents/python/mise.toml")
        | task_names(ROOT / "agents/data/mise.toml")
        | {"install:core", "watch", "scan"}
    )
    ignored = {"doctor:PROFILE", "tasks"}
    problems: list[Problem] = []
    for page, text in pages.items():
        problems.extend(
            (page.relative_to(ROOT).as_posix(), f"documents unknown mise task {name!r}")
            for name in DOCUMENTED_TASK.findall(text)
            if name not in known and name not in ignored and "<" not in name and "*" not in name
        )
    return problems


def check_task_expansions(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """The packaging task table must show the exact command mise executes."""
    page = ROOT / "docs/3. Capabilities/3.0. Packaging.md"
    text = pages.get(page, "")
    documented = dict(TASK_TABLE_ROW.findall(text))
    public = ("run", "workflow", "coordinator", "web", "mcp", "mcp:http", "a2a", "data:reset")
    problems: list[Problem] = []
    for name in public:
        problems += compare_contract(
            page.relative_to(ROOT).as_posix(),
            f"mise run {name} expansion",
            task_expansion(ROOT / "agents/python/mise.toml", name),
            documented.get(name),
        )
    return problems


def check_copied_version_inventory(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path,
    label: str,
    expected: str,
    surfaces: dict[str, int],
    pattern: re.Pattern[str],
) -> list[Problem]:
    """Compare every declared prose copy with one parsed repository owner."""
    if not expected:
        return []
    problems: list[Problem] = []
    for relative, expected_count in surfaces.items():
        text = contract_page(pages, root, relative)
        actual_count = text.count(expected)
        if actual_count != expected_count:
            problems.append(
                (
                    relative,
                    (
                        f"{label} copy inventory drifted: expected {expected_count} occurrence(s) "
                        f"of {expected!r}, found {actual_count}"
                    ),
                )
            )
        for found in pattern.findall(text):
            problems += compare_contract(relative, f"{label} version", expected, found)

    declared = set(surfaces)
    for page, text in pages.items():
        try:
            relative = page.relative_to(root).as_posix()
        except ValueError:
            continue
        if relative not in declared and expected in text:
            problems.append((relative, f"{label} copied outside its explicit version inventory"))
    return problems


def check_source_versions(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Copied volatile versions agree with their manifest owners."""
    problems: list[Problem] = []
    root_manifest = root / "pyproject.toml"
    agent_manifest = root / "agents/python/pyproject.toml"
    root_mise_path = root / "mise.toml"
    root_mise = read_toml(root_mise_path)
    root_mise_text = root_mise_path.read_text(encoding="utf-8")
    tools = root_mise.get("tools")
    tool_versions = tools if isinstance(tools, dict) else {}

    shim = root / "docs/javascripts/accessibility.js"
    shim_text = shim.read_text(encoding="utf-8")
    shim_match = re.search(r'const SEARCH_SHIM_ZENSICAL = "([^"]+)";', shim_text)
    problems += compare_contract(
        shim.relative_to(root).as_posix(),
        "Zensical search-shim version",
        exact_dependency(root_manifest, "zensical"),
        shim_match.group(1) if shim_match else None,
    )

    table = root / "docs/1. Setup/1.3. Kubernetes.md"
    rows = {name.strip().lower(): version.strip() for name, version in TOOL_TABLE_ROW.findall(pages.get(table, ""))}
    table_names = {
        "k3d": "k3d",
        "kubectl": "kubectl",
        "helm": "helm",
        "helmfile": "helmfile",
        "skaffold": "skaffold",
        "kubeconform": "kubeconform",
        "kube-linter": "kube-linter",
        "agentgateway": "github:agentgateway/agentgateway",
    }
    for label, key in table_names.items():
        expected = tool_versions.get(key)
        problems += compare_contract(
            table.relative_to(root).as_posix(),
            f"{label} table version",
            str(expected) if expected is not None else None,
            rows.get(label),
        )

    packaging = pages.get(root / "docs/3. Capabilities/3.0. Packaging.md", "")
    security = pages.get(root / "docs/4. Quality/4.6. Security.md", "")
    for component, authority, surfaces in (
        ("ty", dependency_spec(agent_manifest, "ty"), (root / "docs/4. Quality/4.0. Typing.md",)),
        (
            "presidio-analyzer",
            exact_dependency(agent_manifest, "presidio-analyzer"),
            (root / "docs/3. Capabilities/3.0. Packaging.md", root / "docs/4. Quality/4.6. Security.md"),
        ),
    ):
        for surface in surfaces:
            copied = re.search(
                rf"{re.escape(component)}[^\\n]*?(?:==|>=)([0-9][0-9A-Za-z.,<>=-]*)",
                pages.get(surface, ""),
                re.IGNORECASE,
            )
            if copied:
                problems += compare_contract(
                    surface.relative_to(root).as_posix(),
                    f"{component} requirement",
                    authority,
                    copied.group(1) if component == "presidio-analyzer" else f">={copied.group(1)}",
                )

    # These pages intentionally describe the rationale without owning a stale point release.
    if re.search(r"\bPresidio\b[^\n]*\b2\.\d+\.\d+\b", packaging + "\n" + security):
        problems.append(
            (
                "docs",
                "Presidio rationale must link to the manifest instead of copying a point release",
            )
        )

    feedback_path = root / "docs/7. Observability/7.4. Feedback.md"
    for number, line in enumerate(pages.get(feedback_path, "").splitlines(), start=1):
        if "agentops-mlflow" in line and re.search(r"\b\d+\.\d+\.\d+\b", line):
            problems.append(
                (
                    feedback_path.relative_to(root).as_posix(),
                    f"line {number}: MLflow version belongs to infra/mlflow/pyproject.toml; link to it instead",
                )
            )
    adk_copy = re.compile(r"google-adk(?:\[[^]]+\])?[^\n`]*(?:==|>=|<=|~=)\s*\d", re.IGNORECASE)
    for page, text in pages.items():
        if adk_copy.search(text):
            problems.append(
                (
                    page.relative_to(root).as_posix(),
                    "ADK requirements must link to agents/python/pyproject.toml instead of copying a volatile range",
                )
            )

    helmfile = (root / "infra/helmfile.yaml").read_text(encoding="utf-8")
    kagent_match = re.search(r"(?m)^# kagent-chart-version: (\d+\.\d+\.\d+)$", helmfile)
    helm_diff_source = (root / "scripts/install-helm-diff.sh").read_text(encoding="utf-8")
    helm_diff_match = re.search(r"(?m)^readonly expected=(\d+\.\d+\.\d+)$", helm_diff_source)
    k6_match = re.search(r"mise x k6@(\d+\.\d+\.\d+) -- k6 run", root_mise_text)
    agent_dockerfile = (root / "agents/python/Dockerfile").read_text(encoding="utf-8")
    python_base_match = re.search(r"(?m)^FROM python:([^@\s]+)@sha256:", agent_dockerfile)
    python_apk_match = re.search(r"(?m)^\s*(python-3\.13=[^\s\\]+)\s*\\?$", agent_dockerfile)
    libstdcpp_match = re.search(r"(?m)^\s*(libstdc\+\+=[^\s\\]+)\s*\\?$", agent_dockerfile)
    parsed_owners = {
        "agentgateway": str(tool_versions.get("github:agentgateway/agentgateway", "")),
        "k6": k6_match.group(1) if k6_match else "",
        "kagent": kagent_match.group(1) if kagent_match else "",
        "helm-diff": helm_diff_match.group(1) if helm_diff_match else "",
        "Python build image": python_base_match.group(1) if python_base_match else "",
        "Wolfi Python": python_apk_match.group(1) if python_apk_match else "",
        "Wolfi libstdc++": libstdcpp_match.group(1) if libstdcpp_match else "",
    }
    for label, value in parsed_owners.items():
        if not value:
            problems.append(("source", f"could not parse the {label} version authority"))

    copied_versions = (
        (
            "agentgateway",
            parsed_owners["agentgateway"],
            {
                "docs/1. Setup/1.3. Kubernetes.md": 1,
                "docs/5. Gateway/5.1. Gateway Setup.md": 1,
                "docs/5. Gateway/5.2. MCP Gateway.md": 1,
                "docs/5. Gateway/5.6. Gateway Observability.md": 1,
                "docs/6. Platform/6.5. Platform Gateway.md": 1,
                "docs/8. Community/8.3. Templates.md": 1,
                "docs/8. Community/8.6. AAIF.md": 1,
            },
            re.compile(r"(?:agentgateway|pinned gateway)[^\n]{0,160}?v?(\d+\.\d+\.\d+)", re.IGNORECASE),
        ),
        (
            "kagent chart",
            parsed_owners["kagent"],
            {
                "docs/0. Overview/0.3. Ecosystem.md": 1,
                "docs/6. Platform/6.0. Platform.md": 1,
                "docs/6. Platform/6.2. Platform Install.md": 3,
                "docs/7. Observability/7.0. Reproducibility.md": 1,
                "docs/8. Community/8.3. Templates.md": 1,
                "docs/8. Community/8.6. AAIF.md": 1,
            },
            re.compile(
                r"(?:pinned to|pinned stable chart|chart version|at exactly)\s+`?v?(\d+\.\d+\.\d+)",
                re.IGNORECASE,
            ),
        ),
        (
            "helm-diff",
            parsed_owners["helm-diff"],
            {
                "docs/1. Setup/1.0. System.md": 2,
                "docs/1. Setup/1.3. Kubernetes.md": 2,
                "docs/6. Platform/6.2. Platform Install.md": 1,
            },
            re.compile(r"helm-diff[^\n]{0,80}?(\d+\.\d+\.\d+)", re.IGNORECASE),
        ),
        (
            "k6",
            parsed_owners["k6"],
            {"docs/7. Observability/7.2. Monitoring.md": 4},
            re.compile(r"k6@(\d+\.\d+\.\d+)", re.IGNORECASE),
        ),
        (
            "Python build image",
            parsed_owners["Python build image"],
            {
                "docs/1. Setup/1.1. Python.md": 1,
                "docs/3. Capabilities/3.0. Packaging.md": 1,
                "docs/6. Platform/6.1. Containers.md": 4,
            },
            re.compile(r"python:([0-9]+\.[0-9]+\.[0-9]+-slim-[a-z]+)", re.IGNORECASE),
        ),
        (
            "Wolfi Python",
            parsed_owners["Wolfi Python"],
            {
                "docs/3. Capabilities/3.0. Packaging.md": 1,
                "docs/6. Platform/6.1. Containers.md": 2,
            },
            re.compile(r"(python-3\.13=\d+\.\d+\.\d+-r\d+)", re.IGNORECASE),
        ),
        (
            "Wolfi libstdc++",
            parsed_owners["Wolfi libstdc++"],
            {"docs/3. Capabilities/3.0. Packaging.md": 1},
            re.compile(r"(libstdc\+\+=\d+\.\d+\.\d+-r\d+)", re.IGNORECASE),
        ),
    )
    for label, expected, surfaces, pattern in copied_versions:
        problems += check_copied_version_inventory(
            pages,
            root=root,
            label=label,
            expected=expected,
            surfaces=surfaces,
            pattern=pattern,
        )

    for dockerfile in (root / "infra/mlflow/Dockerfile",):
        text = dockerfile.read_text(encoding="utf-8")
        for label, expected, pattern in (
            ("Python build image", parsed_owners["Python build image"], r"(?m)^FROM python:([^@\s]+)@sha256:"),
            ("Wolfi Python", parsed_owners["Wolfi Python"], r"(?m)^\s*(python-3\.13=[^\s\\]+)\s*\\?$"),
            ("Wolfi libstdc++", parsed_owners["Wolfi libstdc++"], r"(?m)^\s*(libstdc\+\+=[^\s\\]+)\s*\\?$"),
        ):
            match = re.search(pattern, text)
            problems += compare_contract(
                dockerfile.relative_to(root).as_posix(),
                f"{label} source pin",
                expected,
                match.group(1) if match else None,
            )

    external_image = re.compile(
        r"(?:--image=|^\s*image:\s+)"
        r"((?:docker\.io|ghcr\.io|cgr\.dev|cr\.[^/\s]+\.dev)/[^\s`]+)",
        re.MULTILINE,
    )
    for page, text in pages.items():
        for image_match in external_image.finditer(text):
            image = image_match.group(1).rstrip("\"')],")
            if "@sha256:" not in image:
                problems.append(
                    (
                        page.relative_to(root).as_posix(),
                        f"external image {image!r} must include an immutable sha256 digest",
                    )
                )
    return problems


def contract_page(pages: dict[pathlib.Path, str], root: pathlib.Path, relative: str) -> str:
    """Return one learner-facing contract page from a production or test root."""
    return pages.get(root / relative, "")


def require_contract_tokens(relative: str, text: str, tokens: tuple[str, ...]) -> list[Problem]:
    """Require source-owned terms that make a copied contract machine-checkable."""
    return [(relative, f"source contract is missing {token!r}") for token in tokens if token not in text]


def check_python_profile_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Python support and dependency-profile prose agrees with its two manifests."""
    manifest_path = root / "agents/python/pyproject.toml"
    manifest = read_toml(manifest_path)
    project = manifest.get("project")
    requires_python = project.get("requires-python") if isinstance(project, dict) else None
    groups = manifest.get("dependency-groups")
    tool = manifest.get("tool")
    uv_config = tool.get("uv") if isinstance(tool, dict) else None
    defaults = uv_config.get("default-groups") if isinstance(uv_config, dict) else None
    default_groups = set(defaults) if isinstance(defaults, list) else set()
    dependency_groups = set(groups) if isinstance(groups, dict) else set()
    problems: list[Problem] = []

    python_page = "docs/1. Setup/1.1. Python.md"
    python_text = contract_page(pages, root, python_page)
    if not isinstance(requires_python, str):
        problems.append((manifest_path.relative_to(root).as_posix(), "project.requires-python must be a string"))
    else:
        problems += require_contract_tokens(
            python_page,
            python_text,
            (f'`requires-python = "{requires_python}"`',),
        )

    profile_pages = (
        "docs/1. Setup/1.0. System.md",
        python_page,
        "docs/1. Setup/index.md",
        "docs/4. Quality/4.4. Evaluations.md",
    )
    if "eval" in dependency_groups and "eval" not in default_groups:
        for relative in profile_pages:
            problems += require_contract_tokens(
                relative,
                contract_page(pages, root, relative),
                ("mise run install:eval",),
            )
        combined = "\n".join(contract_page(pages, root, relative) for relative in profile_pages)
        if re.search(r"one complete development/evaluation|one dev/eval group", combined, re.IGNORECASE):
            problems.append(
                ("docs/1. Setup", "optional eval group is described as part of the base development install")
            )
        expansion = task_expansion(root / "agents/python/mise.toml", "install:eval")
        if expansion is None or "--group eval" not in expansion:
            problems.append(("agents/python/mise.toml", "install:eval must synchronize the eval dependency group"))
    elif "eval" in default_groups:
        for relative in profile_pages:
            if "mise run install:eval" in contract_page(pages, root, relative):
                problems.append((relative, "documents an opt-in eval install although eval is a default group"))
    else:
        problems.append(("agents/python/pyproject.toml", "dependency-groups.eval must be explicitly declared"))
    return problems


def check_state_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """The restore lesson follows the shared manifest-bound state CLI."""
    relative = "docs/6. Platform/6.6. Platform Delivery.md"
    text = contract_page(pages, root, relative)
    state_source = (root / "agents/python/src/agent/state.py").read_text(encoding="utf-8")
    manifest_match = re.search(r'^_MANIFEST_NAME\s*=\s*"([^"]+)"$', state_source, re.MULTILINE)
    complete_match = re.search(r'^_COMPLETE_NAME\s*=\s*"([^"]+)"$', state_source, re.MULTILINE)
    problems: list[Problem] = []
    for label, match in (("manifest", manifest_match), ("completion marker", complete_match)):
        if match is None:
            problems.append(("agents/python/src/agent/state.py", f"could not resolve snapshot {label} filename"))
        else:
            problems += require_contract_tokens(relative, text, (match.group(1),))

    problems += require_contract_tokens(
        relative,
        text,
        (
            "manifest_sha256",
            "format_version",
            'command: ["python", "-m", "agent.state", "restore"]',
        ),
    )
    if "databases=" in text:
        problems.append((relative, "documents the retired databases=N completion-marker format"))

    cronjob = (root / "infra/k8s/base/state-backup.yaml").read_text(encoding="utf-8")
    if 'command: ["python", "-m", "agent.state", "backup"]' not in cronjob:
        problems.append(("infra/k8s/base/state-backup.yaml", "backup CronJob must use the shared agent.state CLI"))

    drill = (root / "infra/scripts/backup-drill.sh").read_text(encoding="utf-8")
    final_line = re.search(r'^echo "([^"]+)"$', drill, re.MULTILINE)
    if final_line is None:
        problems.append(("infra/scripts/backup-drill.sh", "could not resolve the drill completion line"))
    else:
        problems += compare_contract(
            relative,
            "state drill completion line",
            final_line.group(1),
            final_line.group(1) if final_line.group(1) in text else None,
        )
    return problems


def profile_labels(text: str, function: str) -> tuple[str, ...]:
    """Extract the human profile labels passed to a shell audit function."""
    return tuple(re.findall(rf'^\s*{re.escape(function)} "([^"]+)"', text, re.MULTILINE))


def check_dependency_audit_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """License and vulnerability teaching names every lock-owned audit profile."""
    license_script = (root / "scripts/check-licenses.sh").read_text(encoding="utf-8")
    vulnerability_script = (root / "scripts/check-vulnerabilities.sh").read_text(encoding="utf-8")
    license_profiles = profile_labels(license_script, "sync_inventory")
    vulnerability_profiles = profile_labels(vulnerability_script, "audit_profile")
    problems: list[Problem] = []
    if set(license_profiles) != set(vulnerability_profiles):
        problems.append(
            (
                "scripts/check-licenses.sh",
                "license and vulnerability audit profile labels must describe the same lock-owned set",
            )
        )

    license_page = "docs/8. Community/8.1. License.md"
    license_text = contract_page(pages, root, license_page)
    problems += require_contract_tokens(license_page, license_text, tuple(dict.fromkeys(license_profiles)))
    problems += require_contract_tokens(
        license_page,
        license_text,
        ("agentops-ambient-contaminant", "clean and pre-populated inventory verdicts"),
    )
    problems += require_contract_tokens(
        "scripts/check-licenses.sh",
        license_script,
        ("agentops-ambient-contaminant", "cmp -s"),
    )
    if re.search(r"\b(?:all )?three locked environments\b|\binventory 3 locked envs\b", license_text, re.IGNORECASE):
        problems.append((license_page, "documents the retired three-environment audit instead of five profiles"))

    linting_page = "docs/4. Quality/4.1. Linting.md"
    linting_text = contract_page(pages, root, linting_page)
    problems += require_contract_tokens(
        linting_page,
        linting_text,
        (
            "check-vulnerabilities.sh",
            "five hash-pinned profiles",
            "pip-audit --strict",
            "clean and pre-populated lock-export verdicts",
        ),
    )
    problems += require_contract_tokens(
        "scripts/check-vulnerabilities.sh",
        vulnerability_script,
        ("agentops-ambient-contaminant", "cmp -s"),
    )
    if "plain `pip-audit`" in linting_text:
        problems.append((linting_page, "documents the retired ambient plain pip-audit command"))
    return problems


def assigned_string_tuple(path: pathlib.Path, function: str, variable: str) -> tuple[str, ...]:
    """Read one tuple of string constants assigned inside a named function."""
    module = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    for node in module.body:
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) or node.name != function:
            continue
        for child in ast.walk(node):
            if (
                isinstance(child, ast.Assign)
                and any(isinstance(target, ast.Name) and target.id == variable for target in child.targets)
                and isinstance(child.value, (ast.Tuple, ast.List))
                and all(isinstance(item, ast.Constant) and isinstance(item.value, str) for item in child.value.elts)
            ):
                return tuple(
                    item.value
                    for item in child.value.elts
                    if isinstance(item, ast.Constant) and isinstance(item.value, str)
                )
    return ()


def class_annotation_names(path: pathlib.Path, class_name: str) -> tuple[str, ...]:
    """Read the annotated field names declared directly on one class."""
    module = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    for node in module.body:
        if isinstance(node, ast.ClassDef) and node.name == class_name:
            return tuple(
                item.target.id
                for item in node.body
                if isinstance(item, ast.AnnAssign) and isinstance(item.target, ast.Name)
            )
    return ()


def check_retrieval_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """The memory lesson inventories every stored semantic-index provenance field."""
    source = root / "agents/python/src/agent/retrieval.py"
    metadata = assigned_string_tuple(source, "_metadata", "keys")
    relative = "docs/3. Capabilities/3.4. Memory.md"
    text = contract_page(pages, root, relative)
    problems: list[Problem] = []
    if not metadata:
        problems.append((source.relative_to(root).as_posix(), "could not resolve semantic index metadata fields"))
    else:
        problems += require_contract_tokens(relative, text, tuple(f"`{field}`" for field in metadata))
    if "Historical checkpoint (" not in text or "\nMeasured checkpoint (" in text:
        problems.append((relative, "the superseded retrieval result must be labelled as a historical checkpoint"))
    return problems


def check_domain_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """The Capstone describes the production vocabulary and its test adapter."""
    source = root / "agents/python/src/agent/domain.py"
    fields = class_annotation_names(source, "DomainVocabulary")
    relative = "docs/8. Community/8.7. Capstone.md"
    text = contract_page(pages, root, relative)
    problems: list[Problem] = []
    if not fields:
        problems.append((source.relative_to(root).as_posix(), "could not resolve DomainVocabulary fields"))
    else:
        problems += require_contract_tokens(relative, text, tuple(f"`{field}`" for field in fields))
    problems += require_contract_tokens(
        relative,
        text,
        (
            "src/agent/domain.py",
            "tests/domain.py",
            "`REFERENCE_DOMAIN`",
            "`PIVOT_DOMAIN`",
            "`adapt_dataset`",
        ),
    )
    if re.search(r"reference does not ship (?:that|the) seam", text, re.IGNORECASE):
        problems.append((relative, "claims the shipped portability seam is absent"))
    if "no `INC-*` literal survives" in text:
        problems.append((relative, "requires zero domain literals although explicit contract assertions are ratcheted"))
    return problems


def check_outcome_evidence_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """The outcome matrix cannot promise assessment actions absent from the exercise."""
    overview_relative = "docs/0. Overview/0.0. Course.md"
    evaluation_relative = "docs/4. Quality/4.4. Evaluations.md"
    overview = contract_page(pages, root, overview_relative)
    evaluation = contract_page(pages, root, evaluation_relative)
    matrix_row = next(
        (line for line in overview.splitlines() if line.startswith("| Evaluate a behavioral boundary")), ""
    )
    exercise = evaluation.partition("## How would you add an evaluation case?")[2].partition("\n## ")[0]
    return [
        (
            overview_relative,
            f"evaluation outcome promises {action!r} but the linked exercise does not require it",
        )
        for action in ("diagnose", "repair")
        if action in matrix_row.lower() and action not in exercise.lower()
    ]


def check_capacity_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """SUPPORT owns the local-platform planning values consumed by doctor."""
    support_path = root / "SUPPORT.md"
    doctor_path = root / "scripts/doctor.sh"
    support = support_path.read_text(encoding="utf-8")
    doctor = doctor_path.read_text(encoding="utf-8")
    matches = re.findall(
        r"^<!-- local-platform-capacity: total-ram-gib=(\d+) free-disk-gib=(\d+) -->$",
        support,
        re.MULTILINE,
    )
    if len(matches) != 1:
        return [("SUPPORT.md", "expected exactly one machine-readable local-platform-capacity contract")]
    ram, disk = matches[0]
    problems = require_contract_tokens(
        "SUPPORT.md",
        support,
        (f"**{ram} GiB total RAM**", f"**{disk} GiB free disk**"),
    )
    problems += require_contract_tokens(
        "scripts/doctor.sh",
        doctor,
        (
            "local-platform-capacity:",
            "platform_total_ram_gib",
            "platform_free_disk_gib",
        ),
    )
    copied = (f"{ram} GiB total RAM", f"{disk} GiB free disk")
    for page, text in pages.items():
        for value in copied:
            if value in text:
                problems.append(
                    (
                        page.relative_to(root).as_posix(),
                        f"capacity literal {value!r} belongs only in SUPPORT.md",
                    )
                )
    return problems


def check_project_neutral_provider_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Learner provider examples never target the maintainer-owned GCP project."""
    env_relative = ".env.example"
    provider_page = "docs/1. Setup/1.4. Providers.md"
    env = (root / env_relative).read_text(encoding="utf-8")
    page = contract_page(pages, root, provider_page)
    problems = require_contract_tokens(
        env_relative,
        env,
        ("GOOGLE_CLOUD_PROJECT=your-gcp-project-id",),
    )
    problems += require_contract_tokens(
        provider_page,
        page,
        (
            "GOOGLE_CLOUD_PROJECT=your-gcp-project-id",
            "GCP_PROJECT_ID=your-gcp-project-id mise run doctor:gcp",
            "does not load `GOOGLE_CLOUD_PROJECT` from `.env`",
        ),
    )
    assignment = re.compile(r"(?m)^\s*#?\s*(?:GOOGLE_CLOUD_PROJECT|GCP_PROJECT_ID)\s*=\s*agentops-open-course\s*$")
    for relative, text in ((env_relative, env), (provider_page, page)):
        if assignment.search(text):
            problems.append((relative, "learner GCP example targets the maintainer-owned project"))
    return problems


def check_release_freshness_course_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Every release carries one recent audit or protected reviewed waiver."""
    workflow_relative = ".github/workflows/release.yml"
    workflow = (root / workflow_relative).read_text(encoding="utf-8")
    release_page = "docs/8. Community/8.2. Releases.md"
    text = contract_page(pages, root, release_page)
    problems = require_contract_tokens(
        workflow_relative,
        workflow,
        (
            "freshness_evidence:",
            "issues: read",
            "application/vnd.github.full+json",
            "gh api --method POST markdown --input -",
            "--checklist-template-html freshness-template.html",
            "scripts/release_freshness.py",
            "freshness: $freshness",
        ),
    )
    problems += require_contract_tokens(
        release_page,
        text,
        (
            "issue:<number>",
            "waiver:<reviewed reason>",
            "120 days",
            "every checklist box checked",
            "reject tag updates and deletion",
            "do not restrict tag creation",
            "break-glass bypass",
        ),
    )
    return problems


def check_actions_artifact_retention_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Keep transient workflow handoffs within the repository's live retention policy."""
    agents_relative = "AGENTS.md"
    agents = (root / agents_relative).read_text(encoding="utf-8")
    policy = re.search(r"caps artifact and log retention at \*\*(\d+) days\*\*", agents)
    if policy is None:
        return [(agents_relative, "Actions artifact retention policy is missing")]
    limit = int(policy.group(1))
    problems: list[Problem] = []
    for workflow in sorted((root / ".github/workflows").glob("*.yml")):
        text = workflow.read_text(encoding="utf-8")
        relative = workflow.relative_to(root).as_posix()
        uploads = text.count("uses: actions/upload-artifact@")
        retention_values = [int(value) for value in re.findall(r"(?m)^\s*retention-days:\s*(\d+)\s*$", text)]
        if uploads != len(retention_values):
            problems.append((relative, "every upload-artifact step must declare one retention-days value"))
        problems.extend(
            (relative, f"Actions artifact retention {days} days exceeds the {limit}-day policy")
            for days in retention_values
            if days > limit
        )

    release_page = "docs/8. Community/8.2. Releases.md"
    problems += require_contract_tokens(
        release_page,
        contract_page(pages, root, release_page),
        (f"**{limit} days**", "immutable GitHub release", "OCI image"),
    )
    return problems


def check_course_source_contracts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Run the cross-file facts whose learner-facing copies must not drift."""
    problems: list[Problem] = []
    for check in (
        check_python_profile_contracts,
        check_state_course_contracts,
        check_dependency_audit_course_contracts,
        check_retrieval_course_contracts,
        check_domain_course_contracts,
        check_outcome_evidence_contracts,
        check_capacity_course_contracts,
        check_project_neutral_provider_contracts,
        check_release_freshness_course_contracts,
        check_actions_artifact_retention_contracts,
    ):
        problems += check(pages, root=root)
    return problems


def check_port_contracts(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Every documented stable port resolves at one executable owner."""
    problems: list[Problem] = []
    expected = set(PORT_CONTRACT)
    for port, (relative, pattern) in PORT_CONTRACT.items():
        path = ROOT / relative
        try:
            source = path.read_text(encoding="utf-8")
        except OSError as error:
            problems.append((relative, f"could not read port owner for :{port}: {error}"))
            continue
        if not re.search(pattern, source):
            problems.append((relative, f"authoritative pattern for stable port :{port} is missing"))

    ecosystem_path = ROOT / "docs/0. Overview/0.3. Ecosystem.md"
    ecosystem = pages.get(ecosystem_path, "")
    heading = '??? note "Deeper: every port the course binds"'
    section = ecosystem.partition(heading)[2].partition("\n## ")[0]
    documented = {int(value) for value in re.findall(r"`:(\d+)`", section)}
    if documented != expected:
        missing = ", ".join(f":{port}" for port in sorted(expected - documented)) or "none"
        extra = ", ".join(f":{port}" for port in sorted(documented - expected)) or "none"
        problems.append(
            (
                ecosystem_path.relative_to(ROOT).as_posix(),
                f"stable port inventory drifted (missing: {missing}; extra: {extra})",
            )
        )

    agents_text = ROOT.joinpath("AGENTS.md").read_text(encoding="utf-8")
    paragraph = next(
        (part for part in agents_text.split("\n\n") if part.startswith("This file owns the stable network inventory")),
        "",
    )
    agents_ports = {int(value) for value in re.findall(r":(\d+)", paragraph)}
    if agents_ports != expected:
        problems.append(("AGENTS.md", "stable network paragraph must match the executable port inventory"))
    return problems


def check_cost_owner(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Only the canonical cost page may own a dated GKE currency estimate."""
    owner = ROOT / "docs/7. Observability/7.3. Costs.md"
    problems: list[Problem] = []
    for page, text in pages.items():
        if page == owner:
            continue
        if re.search(r"\bUSD\s+\d|\$\s*\d+(?:\.\d+)?\s*(?:per|/)\s*(?:hour|month)", text):
            problems.append(
                (
                    page.relative_to(ROOT).as_posix(),
                    "GKE currency literals belong only in 7.3. Costs; link to its dated estimate",
                )
            )
        if re.search(r"\bprices? checked on \d{1,2} [A-Z][a-z]+ \d{4}\b", text):
            problems.append(
                (
                    page.relative_to(ROOT).as_posix(),
                    "dated price review belongs only in 7.3. Costs",
                )
            )
    return problems


def contains_in_order(text: str, values: tuple[str, ...]) -> bool:
    """Return whether every value appears after its predecessor."""
    position = 0
    for value in values:
        found = text.find(value, position)
        if found < 0:
            return False
        position = found + len(value)
    return True


def check_quickstarts(
    pages: dict[pathlib.Path, str],
    *,
    root: pathlib.Path = ROOT,
) -> list[Problem]:
    """Guarded quickstarts share one ordered command contract; previews say what they skip."""
    guarded = (
        "git clone https://github.com/MLOps-Courses/agentops-open-course.git",
        "cd agentops-open-course",
        "mise run install",
        "mise run doctor",
        "mise run check:core",
        "mise run test",
    )
    surfaces = {
        "README.md": root.joinpath("README.md").read_text(encoding="utf-8"),
        "docs/1. Setup/1.0. System.md": pages.get(root / "docs/1. Setup/1.0. System.md", ""),
    }
    problems = [
        (name, "guarded quickstart must contain the canonical install → doctor → check → test sequence")
        for name, text in surfaces.items()
        if not contains_in_order(text, guarded)
    ]
    landing = pages.get(root / "docs/index.md", "")
    if "<!-- quickstart: unverified-preview -->" not in landing or "unverified preview" not in landing.lower():
        problems.append(
            (
                "docs/index.md",
                "shorter model-first quickstart must be marked as an unverified preview",
            )
        )
    ci = root.joinpath(".github/workflows/ci.yml").read_text(encoding="utf-8")
    ci_sequence = (
        "run: mise run install:validation",
        "run: mise run doctor",
        "run: mise run check",
        "run: mise run test",
    )
    if not contains_in_order(ci, ci_sequence):
        problems.append(
            (
                ".github/workflows/ci.yml",
                "CI must execute install, the learner base doctor, check, and test in order",
            )
        )
    linting_page = pages.get(root / "docs/4. Quality/4.1. Linting.md", "")
    if linting_page.count("install:validation") != 2 or "install:maintainer" in linting_page:
        problems.append(
            (
                "docs/4. Quality/4.1. Linting.md",
                "CI prose must name the narrow install:validation profile at both workflow descriptions",
            )
        )
    return problems


def check_gcp_runbook(*, root: pathlib.Path = ROOT) -> list[Problem]:
    """Root-scoped OpenTofu commands must select the module directory explicitly."""
    path = root / "infra/gcp/README.md"
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as error:
        return [(path.relative_to(root).as_posix(), f"could not read GCP runbook: {error}")]
    return [
        (
            path.relative_to(root).as_posix(),
            f"line {line_number}: root-scoped OpenTofu command needs `-chdir=infra/gcp`",
        )
        for line_number, line in enumerate(lines, start=1)
        if re.search(r"\btofu\s+", line) and "-chdir=infra/gcp" not in line
    ]


def check_skaffold_runbooks(*, root: pathlib.Path = ROOT) -> list[Problem]:
    """Reject root-relative configs and working-directory-dependent profile shorthand."""
    paths = sorted(path for base in (root / "docs", root / "infra") if base.exists() for path in base.rglob("*.md"))
    return [
        (
            path.relative_to(root).as_posix(),
            f"line {line_number}: run from `infra/` with `--filename skaffold.yaml`",
        )
        for path in paths
        for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1)
        if "--filename infra/skaffold.yaml" in line
        or re.search(r"\bskaffold\s+(?:run|delete)\s+-p\s+(?:local|gke)\b", line)
    ]


def check_eval_runtime_baseline(*, root: pathlib.Path = ROOT) -> list[Problem]:
    """Keep the measured cost baseline bound to the scheduled Ollama runtime."""
    workflow_path = root / ".github/workflows/eval.yml"
    baseline_path = root / "agents/python/evals/cost_baseline.json"
    try:
        workflow = workflow_path.read_text(encoding="utf-8")
        baseline = json.loads(baseline_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        return [(baseline_path.relative_to(root).as_posix(), f"could not read eval runtime evidence: {error}")]
    release = re.search(r"/ollama/ollama/releases/download/(v\d+\.\d+\.\d+)/ollama-", workflow)
    if release is None:
        return [(workflow_path.relative_to(root).as_posix(), "could not identify the pinned Ollama release")]
    expected = f"ollama version is {release.group(1).removeprefix('v')}"
    actual = baseline.get("ollama_version") if isinstance(baseline, dict) else None
    if actual == expected:
        return []
    return [
        (
            baseline_path.relative_to(root).as_posix(),
            f"reviewed Ollama runtime is {actual!r}; scheduled Eval pins {expected!r}",
        )
    ]


def safe_course_route(route: str) -> bool:
    """Return whether a route is a normalized relative HTML path."""
    path = pathlib.PurePosixPath(route)
    return (
        bool(route)
        and route == route.strip()
        and route.endswith(".html")
        and "\\" not in route
        and ":" not in route
        and "?" not in route
        and "#" not in route
        and not path.is_absolute()
        and ".." not in path.parts
        and path.as_posix() == route
    )


def changelog_release_versions(changelog: str) -> set[str]:
    """Return every dated stable release recorded by the changelog."""
    return set(re.findall(r"^## \[(\d+\.\d+\.\d+)\] - \d{4}-\d{2}-\d{2}$", changelog, re.MULTILINE))


def validate_route_manifest(
    current: set[str],
    manifest: object,
    *,
    expected_releases: set[str] | None = None,
) -> list[str]:
    """Return route-manifest errors; exposed for seeded rename/loop tests."""
    if not isinstance(manifest, dict):
        return ["manifest must be a JSON object"]
    releases = manifest.get("releases")
    redirects = manifest.get("redirects")
    if manifest.get("format") != 1 or not isinstance(releases, dict) or not isinstance(redirects, dict):
        return ["manifest needs format=1 plus releases and redirects objects"]
    errors: list[str] = []
    if expected_releases is not None:
        actual_releases = {version for version in releases if isinstance(version, str)}
        missing_releases = sorted(expected_releases - actual_releases)
        unexpected_releases = sorted(actual_releases - expected_releases)
        if missing_releases:
            errors.append("release inventory is missing changelog versions: " + ", ".join(missing_releases))
        if unexpected_releases:
            errors.append("release inventory has versions absent from the changelog: " + ", ".join(unexpected_releases))
    historical: set[str] = set()
    for version, values in releases.items():
        if (
            not isinstance(version, str)
            or not isinstance(values, list)
            or not all(isinstance(value, str) for value in values)
        ):
            errors.append(f"release {version!r} must contain a list of URL strings")
            continue
        string_values = [value for value in values if isinstance(value, str)]
        if string_values != sorted(set(string_values)):
            errors.append(f"release {version} URLs must be sorted and unique")
        for route in string_values:
            if safe_course_route(route):
                historical.add(route)
            else:
                errors.append(f"release {version} contains unsafe course route {route!r}")
    valid_redirects: dict[str, str] = {}
    for old, target in redirects.items():
        if not isinstance(old, str) or not isinstance(target, str):
            errors.append("redirect keys and targets must be strings")
            continue
        if not safe_course_route(old):
            errors.append(f"redirect source is an unsafe course route: {old!r}")
            continue
        if not safe_course_route(target):
            errors.append(f"redirect target is an unsafe course route: {target!r}")
            continue
        valid_redirects[old] = target
    errors.extend(
        f"released URL {old!r} is missing a redirect" for old in historical - current if old not in valid_redirects
    )
    for old, initial_target in valid_redirects.items():
        seen = {old}
        target = initial_target
        while target in valid_redirects:
            if target in seen:
                errors.append(f"redirect loop includes {target!r}")
                break
            seen.add(target)
            target = valid_redirects[target]
        else:
            if target not in current:
                errors.append(f"redirect {old!r} terminates at missing URL {target!r}")
    return sorted(set(errors))


def check_routes(pages: dict[pathlib.Path, str]) -> list[Problem]:
    """Every released URL is still current or has a loop-free terminal redirect."""
    try:
        manifest = json.loads(ROUTE_MANIFEST.read_text(encoding="utf-8"))
        changelog = ROOT.joinpath("CHANGELOG.md").read_text(encoding="utf-8")
    except (OSError, json.JSONDecodeError) as error:
        return [(ROUTE_MANIFEST.relative_to(ROOT).as_posix(), f"could not read route manifest: {error}")]
    expected_releases = changelog_release_versions(changelog)
    if not expected_releases:
        return [("CHANGELOG.md", "expected at least one dated stable release for the URL inventory")]
    current = {page_url(page) for page in pages}
    return [
        (ROUTE_MANIFEST.relative_to(ROOT).as_posix(), error)
        for error in validate_route_manifest(current, manifest, expected_releases=expected_releases)
    ]


def check_docs() -> list[Problem]:
    """Run every page rule over docs/."""
    pages = {page: page.read_text(encoding="utf-8") for page in sorted(ROOT.joinpath("docs").rglob("*.md"))}
    if not pages:
        return [("docs", "no Markdown pages found")]
    try:
        legacy = {
            line
            for raw in DIAGRAM_LEGACY.read_text(encoding="utf-8").splitlines()
            if (line := raw.strip()) and not line.startswith("#")
        }
    except OSError as error:
        return [(DIAGRAM_LEGACY.relative_to(ROOT).as_posix(), f"could not read diagram legacy hashes: {error}")]
    problems: list[Problem] = []
    used_legacy: set[str] = set()
    for page, text in pages.items():
        relative = page.relative_to(ROOT)
        problems += check_front_matter(relative, text)
        problems += check_headings(relative, text)
        problems += check_glance(relative, text)
        problems += check_collapsibles(relative, text)
        problems += check_link_labels(relative, text)
        problems += check_snippets(relative, text)
        problems += check_snippet_targets(relative, text)
        problems += check_python_blocks(relative, text)
        problems += check_machine_paths(relative, text)
        problems += check_admonitions(relative, text)
        problems += check_ordered_lists(relative, text)
        problems += check_page_link_targets(relative, text)
        problems += check_hands_on_action(relative, text)
        problems += check_exercises(relative, text)
        problems += check_diagram_alternatives(relative, text, legacy)
        used_legacy.update(
            sha256_lines(body) for _, _, body in fenced_block_ranges(text, "mermaid") if sha256_lines(body) in legacy
        )
        if relative.as_posix() != LANDING_PAGE:
            problems += check_closing(relative, text)
            problems += check_kind(relative, text)
    stale_legacy = sorted(legacy - used_legacy)
    if stale_legacy:
        problems.append(
            (
                DIAGRAM_LEGACY.relative_to(ROOT).as_posix(),
                f"remove {len(stale_legacy)} stale diagram hashes after adding alternatives or deleting diagrams",
            )
        )
    problems += [
        (page.relative_to(ROOT).as_posix(), message)
        for page, message in [(ROOT / path, message) for path, message in check_index_kinds(pages)]
    ]
    problems += check_navigation(pages)
    problems += check_source_snippet_coverage(pages)
    problems += check_documented_tasks(pages)
    problems += check_task_expansions(pages)
    problems += check_source_versions(pages)
    problems += check_course_source_contracts(pages)
    problems += check_port_contracts(pages)
    problems += check_cost_owner(pages)
    problems += check_quickstarts(pages)
    problems += check_gcp_runbook()
    problems += check_skaffold_runbooks()
    problems += check_eval_runtime_baseline()
    problems += check_routes(pages)
    return problems


class RenderedPage(HTMLParser):
    """Collect the small semantic surface the static accessibility gate owns."""

    def __init__(self) -> None:
        super().__init__()
        self.h1 = 0
        self.main = 0
        self.html_language = ""
        self.links: list[tuple[str, str]] = []
        self.meta: dict[str, str] = {}
        self.json_ld = 0
        self._link_href = ""
        self._link_text: list[str] = []
        self._script_json_ld = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        values = {name: value or "" for name, value in attrs}
        if tag == "html":
            self.html_language = values.get("lang", "")
        elif tag == "h1":
            self.h1 += 1
        elif tag == "main":
            self.main += 1
        elif tag == "a":
            self._link_href = values.get("href", "")
            self._link_text = [values.get("aria-label") or values.get("title") or ""]
        elif tag == "img" and self._link_href:
            self._link_text.append(values.get("alt", ""))
        elif tag == "meta":
            key = values.get("property") or values.get("name")
            if key:
                self.meta[key] = values.get("content", "")
        elif tag == "script" and values.get("type") == "application/ld+json":
            self._script_json_ld = True
            self.json_ld += 1

    def handle_data(self, data: str) -> None:
        if self._link_href:
            self._link_text.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "a" and self._link_href:
            self.links.append((self._link_href, " ".join("".join(self._link_text).split())))
            self._link_href = ""
            self._link_text = []
        elif tag == "script":
            self._script_json_ld = False


def parse_rendered(path: pathlib.Path) -> RenderedPage:
    """Parse one generated HTML page."""
    parsed = RenderedPage()
    parsed.feed(path.read_text(encoding="utf-8"))
    return parsed


def png_dimensions(path: pathlib.Path) -> tuple[int, int] | None:
    """Read PNG dimensions from its fixed signature and IHDR header."""
    try:
        header = path.read_bytes()[:24]
    except OSError:
        return None
    if len(header) != 24 or header[:8] != b"\x89PNG\r\n\x1a\n" or header[12:16] != b"IHDR":
        return None
    return int.from_bytes(header[16:20], "big"), int.from_bytes(header[20:24], "big")


def check_web_client() -> list[Problem]:
    """Keep the dependency-free client on the same structural accessibility floor."""
    path = ROOT / "clients/web/index.html"
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as error:
        return [(path.relative_to(ROOT).as_posix(), f"could not read web client: {error}")]
    requirements = {
        "<main": "a main landmark",
        "<h1": "one visible H1",
        "<label": "native form labels",
        'aria-live="polite"': "a polite status announcement",
        ":focus-visible": "visible keyboard focus",
        "@media (max-width:": "a narrow-layout reflow rule",
        "@media (forced-colors: active)": "a forced-colors fallback",
        "@media (prefers-reduced-motion: reduce)": "a reduced-motion fallback",
    }
    problems = [
        (path.relative_to(ROOT).as_posix(), f"web client needs {description}")
        for token, description in requirements.items()
        if token not in text
    ]
    parsed = RenderedPage()
    parsed.feed(text)
    if parsed.main != 1 or parsed.h1 != 1 or not parsed.html_language:
        problems.append(
            (
                path.relative_to(ROOT).as_posix(),
                "web client needs one <main>, one <h1>, and a document language",
            )
        )
    return problems


def check_rendered(site_dir: pathlib.Path) -> list[Problem]:
    """Check rendered semantics, recovery, and discoverability without a browser."""
    if not site_dir.is_dir():
        return [(site_dir.as_posix(), "rendered site directory does not exist")]
    problems: list[Problem] = []
    pages = sorted(site_dir.rglob("*.html"))
    if not pages:
        return [(site_dir.as_posix(), "rendered site contains no HTML pages")]
    for page in pages:
        relative = page.relative_to(site_dir).as_posix()
        parsed = parse_rendered(page)
        if not parsed.html_language:
            problems.append((relative, "rendered <html> needs a language"))
        if parsed.main != 1:
            problems.append((relative, f"rendered page needs exactly one <main>, found {parsed.main}"))
        if parsed.h1 != 1:
            problems.append((relative, f"rendered page needs exactly one <h1>, found {parsed.h1}"))
        unnamed = [href for href, text in parsed.links if href and not text and not href.startswith("#")]
        if unnamed:
            problems.append((relative, f"rendered page has {len(unnamed)} links without accessible text"))

    homepage_path = site_dir / "index.html"
    if homepage_path.is_file():
        homepage = parse_rendered(homepage_path)
        problems.extend(
            ("index.html", f"homepage is missing {name!r} metadata")
            for name in (
                "description",
                "og:title",
                "og:description",
                "og:url",
                "og:image",
                "twitter:card",
                "twitter:image",
            )
            if not homepage.meta.get(name)
        )
        if homepage.json_ld != 1:
            problems.append(("index.html", "homepage needs exactly one Course JSON-LD record"))
    else:
        problems.append(("index.html", "rendered homepage is missing"))

    not_found_path = site_dir / "404.html"
    if not_found_path.is_file():
        not_found = parse_rendered(not_found_path)
        recoveries = [
            (href, text)
            for href, text in not_found.links
            if href.endswith(("index.html", "/")) or "search" in text.lower()
        ]
        if not recoveries:
            problems.append(("404.html", "404 page needs a named route back home or to search"))
    else:
        problems.append(("404.html", "custom accessible 404 page is missing"))
    social_card = site_dir / "assets/social-card.png"
    if (dimensions := png_dimensions(social_card)) != (1200, 630):
        problems.append(
            (
                social_card.relative_to(site_dir).as_posix(),
                f"social preview must be a 1200x630 PNG, found {dimensions!r}",
            )
        )
    problems += check_web_client()
    return problems


# --- installable skills -------------------------------------------------------------------------


def check_skills() -> list[Problem]:
    """Every skill under skills/ is portable and resolvable by `npx skills add --skill <dir>`."""
    files = sorted(ROOT.joinpath("skills").glob("*/SKILL.md"))
    if not files:
        return [("skills", "no SKILL.md found under skills/")]
    problems: list[Problem] = []
    for skill in files:
        name = skill.relative_to(ROOT).as_posix()
        text = skill.read_text(encoding="utf-8")
        meta, error = parse_front_matter(text)
        if meta is None:
            problems.append((name, error or "must start with YAML front matter (---)"))
            continue
        declared, directory = meta.get("name"), skill.parent.name
        if not declared:
            problems.append((name, "front matter is missing a name"))
        elif declared != directory:
            problems.append((name, f"name {declared!r} must match its directory {directory!r}"))
        if not meta.get("description"):
            problems.append((name, "front matter is missing a description"))
        if not any(line.startswith("# ") for line in text.splitlines()):
            problems.append((name, "expected an H1 title"))
        if MACHINE_PATH.search(text):
            problems.append((name, "found a machine-specific path (skills must be portable)"))
    return problems


# --- release metadata ---------------------------------------------------------------------------

SEMVER: Final = re.compile(r"^\d+\.\d+\.\d+$")
ISO_DATE: Final = re.compile(r"^\d{4}-\d{2}-\d{2}$")
RELEASE_HEADING: Final = re.compile(r"^## \[\d+\.\d+\.\d+\] - ", re.MULTILINE)


def project_version(manifest: pathlib.Path) -> str | None:
    """Return the [project] version of a pyproject.toml, without importing a TOML parser."""
    in_project = False
    for line in manifest.read_text(encoding="utf-8").splitlines():
        if line.strip() == "[project]":
            in_project = True
        elif in_project and line.startswith("["):
            break
        elif in_project and (match := re.fullmatch(r'version = "([^"]+)"', line.strip())):
            return match.group(1)
    return None


def citation_value(key: str) -> str | None:
    """Return a top-level scalar from CITATION.cff, without importing a YAML parser.

    This check runs in the release workflow, which has a checkout and nothing else, so it must not
    depend on the project's virtualenv.
    """
    for line in ROOT.joinpath("CITATION.cff").read_text(encoding="utf-8").splitlines():
        if line.startswith(f"{key}:"):
            return line.removeprefix(f"{key}:").strip().strip('"').strip("'")
    return None


def check_release_metadata(expected_tag: str = "") -> list[Problem]:
    """Package, citation, changelog, and optional tag versions must agree."""
    values = {
        "root version": project_version(ROOT / "pyproject.toml"),
        "agent version": project_version(ROOT / "agents/python/pyproject.toml"),
        "citation version": citation_value("version"),
        "citation date": citation_value("date-released"),
    }
    missing = [key for key, value in values.items() if not value]
    if missing:
        return [("release metadata", f"could not read {', '.join(missing)}")]

    root, agent, cited, dated = (
        values["root version"],
        values["agent version"],
        values["citation version"],
        values["citation date"],
    )
    problems: list[Problem] = []
    if not SEMVER.match(root or ""):
        return [("release metadata", f"root version is not stable SemVer: {root}")]
    if agent != root or cited != root:
        problems.append(("release metadata", f"version mismatch (root={root} agent={agent} citation={cited})"))
    if not ISO_DATE.match(dated or ""):
        problems.append(("release metadata", f"CITATION.cff date-released is not YYYY-MM-DD: {dated}"))

    changelog = ROOT.joinpath("CHANGELOG.md").read_text(encoding="utf-8")
    expected_heading = f"## [{root}] - {dated}"
    newest = next((line for line in changelog.splitlines() if RELEASE_HEADING.match(line)), "")
    if newest != expected_heading:
        problems.append(("release metadata", f"newest changelog heading must be {expected_heading!r}, got {newest!r}"))
    expected_link = f"[{root}]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v{root}"
    if expected_link not in changelog.splitlines():
        problems.append(("release metadata", f"CHANGELOG.md is missing {expected_link}"))
    if expected_tag and expected_tag != f"v{root}":
        problems.append(("release metadata", f"tag {expected_tag} does not match source version v{root}"))

    if not problems:
        sys.stdout.write(f"release metadata: v{root} ({dated}) is consistent\n")
    return problems


# --- entry point --------------------------------------------------------------------------------


def main(argv: list[str]) -> int:
    """Run one named check and report every problem it found."""
    check = argv[1] if len(argv) > 1 else ""
    if check == "docs":
        problems = check_docs()
    elif check == "rendered":
        problems = check_rendered(ROOT / (argv[2] if len(argv) > 2 else "site"))
    elif check == "skills":
        problems = check_skills()
    elif check == "release-metadata":
        problems = check_release_metadata(argv[2] if len(argv) > 2 else "")
    else:
        sys.stderr.write("usage: check_conventions.py <docs|rendered|skills|release-metadata> [argument]\n")
        return 2
    for where, message in problems:
        sys.stderr.write(f"{where}: {message}\n")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
