#!/usr/bin/env -S uv run --quiet --script
# /// script
# requires-python = ">=3.13,<3.14"
# dependencies = [
#     "playwright==1.61.0",
# ]
# ///
# Version authority: https://pypi.org/project/playwright/
"""Run the representative browser accessibility acceptance for docs and web client."""

from __future__ import annotations

import argparse
import contextlib
import functools
import http.server
import pathlib
import subprocess
import sys
import threading
from collections.abc import Iterator, Sequence
from typing import Any
from urllib.parse import quote, urlparse

from playwright.sync_api import BrowserContext, Page, sync_playwright  # ty: ignore[unresolved-import]
from playwright.sync_api import Error as PlaywrightError  # ty: ignore[unresolved-import]

ROOT = pathlib.Path(__file__).resolve().parent.parent
SITE = ROOT / "site"
CLIENT = ROOT / "clients/web"
CONTRAST_COLORS = """
selector => {
  const element = document.querySelector(selector);
  if (!element) throw new Error(`missing contrast target: ${selector}`);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 1;
  const context = canvas.getContext("2d", { willReadFrequently: true });
  const rgba = value => {
    context.clearRect(0, 0, 1, 1);
    context.fillStyle = value;
    context.fillRect(0, 0, 1, 1);
    return [...context.getImageData(0, 0, 1, 1).data];
  };
  const over = (top, bottom) => {
    const alpha = top[3] / 255;
    return [
      top[0] * alpha + bottom[0] * (1 - alpha),
      top[1] * alpha + bottom[1] * (1 - alpha),
      top[2] * alpha + bottom[2] * (1 - alpha),
      255,
    ];
  };
  const backgrounds = [];
  for (let node = element; node; node = node.parentElement) {
    backgrounds.push(rgba(getComputedStyle(node).backgroundColor));
  }
  const background = backgrounds.reverse().reduce((color, layer) => over(layer, color), [255, 255, 255, 255]);
  const foreground = over(rgba(getComputedStyle(element).color), background);
  return { foreground: foreground.slice(0, 3), background: background.slice(0, 3) };
}
"""


class AcceptanceError(RuntimeError):
    """A browser-observed accessibility contract failed."""


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    """Serve fixtures without polluting acceptance output with request logs."""

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
        """Suppress one local request log line."""


@contextlib.contextmanager
def serve(directory: pathlib.Path) -> Iterator[str]:
    """Serve one static directory on an ephemeral loopback port."""
    handler = functools.partial(QuietHandler, directory=str(directory))
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def require(condition: bool, message: str) -> None:
    """Raise a concise acceptance failure."""
    if not condition:
        raise AcceptanceError(message)


def open_page(page: Page, url: str) -> None:
    """Open a local surface and require a successful main response."""
    response = page.goto(url, wait_until="load")
    require(response is not None and response.ok, f"{url}: expected a successful document response")


def keyboard_smoke(page: Page, url: str, skip_selector: str, next_selector: str) -> None:
    """Prove the skip route and next control are reachable and visibly focused."""
    open_page(page, url)
    skip = page.locator(skip_selector)
    before = skip.bounding_box()
    before_opacity = skip.evaluate("element => Number(getComputedStyle(element).opacity)")
    page.keyboard.press("Tab")
    page.wait_for_function(
        """selector => {
          const element = document.querySelector(selector);
          const box = element.getBoundingClientRect();
          return element === document.activeElement && Number(getComputedStyle(element).opacity) > 0.9
            && box.right > 0 && box.bottom > 0 && box.left < innerWidth && box.top < innerHeight;
        }""",
        arg=skip_selector,
    )
    after = skip.bounding_box()
    require(
        skip.evaluate("element => element === document.activeElement"), f"{url}: first Tab must focus the skip link"
    )
    require(
        after is not None
        and 0 <= after["x"] < page.viewport_size["width"]
        and 0 <= after["y"] < page.viewport_size["height"]
        and (before is None or before["x"] < 0 or before_opacity < 0.1),
        f"{url}: focused skip link must move visibly into the viewport",
    )
    target = skip.get_attribute("href")
    require(bool(target and target.startswith("#")), f"{url}: skip link needs a same-page target")
    page.keyboard.press("Enter")
    page.wait_for_function("target => location.hash === target", arg=target)
    require(page.locator(target).count() == 1, f"{url}: skip target {target!r} must exist")

    open_page(page, url)
    page.keyboard.press("Tab")
    page.keyboard.press("Tab")
    next_control = page.locator(next_selector)
    require(
        next_control.evaluate("element => element === document.activeElement"),
        f"{url}: second Tab must reach {next_selector!r}",
    )
    require(page.evaluate("document.activeElement.matches(':focus-visible')"), f"{url}: keyboard focus must be visible")


def reflow_smoke(page: Page, url: str) -> None:
    """Reject document-level horizontal overflow at the WCAG reflow width."""
    page.set_viewport_size({"width": 320, "height": 800})
    open_page(page, url)
    widths = page.evaluate(
        """() => ({
          viewport: document.documentElement.clientWidth,
          content: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
        })"""
    )
    require(
        widths["content"] <= widths["viewport"] + 1,
        f"{url}: 320px viewport overflows horizontally ({widths['content']}px > {widths['viewport']}px)",
    )


def duration_seconds(value: str) -> float:
    """Return the longest duration in a computed CSS duration list."""
    durations = []
    for item in value.split(","):
        item = item.strip()
        durations.append(float(item[:-2]) / 1000 if item.endswith("ms") else float(item[:-1]))
    return max(durations, default=0)


def reduced_motion_smoke(page: Page, url: str, *, stops_animation: bool) -> None:
    """Exercise each surface's reduced-motion media rule against a two-second probe."""
    open_page(page, url)
    values = page.evaluate(
        """() => {
          const probe = document.createElement("div");
          probe.style.cssText = "transition-duration: 2s; animation-duration: 2s";
          document.body.append(probe);
          const style = getComputedStyle(probe);
          return { transition: style.transitionDuration, animation: style.animationDuration };
        }"""
    )
    require(
        page.evaluate("matchMedia('(prefers-reduced-motion: reduce)').matches"), f"{url}: reduced motion is not active"
    )
    require(duration_seconds(values["transition"]) <= 0.001, f"{url}: reduced motion did not stop transitions")
    if stops_animation:
        require(duration_seconds(values["animation"]) <= 0.001, f"{url}: reduced motion did not stop animations")


def luminance(rgb: Sequence[float]) -> float:
    """Return WCAG relative luminance for one sRGB triplet."""
    channels = [value / 255 for value in rgb]
    linear = [value / 12.92 if value <= 0.04045 else ((value + 0.055) / 1.055) ** 2.4 for value in channels]
    return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2]


def contrast_ratio(foreground: Sequence[float], background: Sequence[float]) -> float:
    """Return the WCAG contrast ratio between two opaque sRGB triplets."""
    lighter, darker = sorted((luminance(foreground), luminance(background)), reverse=True)
    return (lighter + 0.05) / (darker + 0.05)


def contrast_smoke(page: Page, selector: str, label: str) -> None:
    """Require AA normal-text contrast for one computed browser surface."""
    colors = page.evaluate(CONTRAST_COLORS, selector)
    ratio = contrast_ratio(colors["foreground"], colors["background"])
    require(ratio >= 4.5, f"{label}: computed contrast is {ratio:.2f}:1, expected at least 4.5:1")


def block_external_requests(context: BrowserContext) -> None:
    """Keep the acceptance deterministic and independent of optional web fonts."""
    context.route(
        "**/*",
        lambda route: (
            route.continue_() if urlparse(route.request.url).hostname in {"127.0.0.1", "localhost"} else route.abort()
        ),
    )


def run_acceptance() -> None:
    """Exercise landing/course templates and the dependency-free client."""
    require(SITE.joinpath("index.html").is_file(), "site/index.html is missing; run `mise run build:docs`")
    require(CLIENT.joinpath("index.html").is_file(), "clients/web/index.html is missing")
    with serve(SITE) as docs, serve(CLIENT) as client, sync_playwright() as playwright:
        try:
            browser = playwright.chromium.launch(headless=True)
        except PlaywrightError as error:
            raise AcceptanceError("Chromium is unavailable; run `mise run install:accessibility`") from error
        with browser:
            context = browser.new_context(viewport={"width": 1280, "height": 900}, color_scheme="light")
            block_external_requests(context)
            page = context.new_page()
            keyboard_smoke(page, f"{docs}/index.html", ".md-skip", ".md-header__button.md-logo")
            reflow_smoke(page, f"{docs}/index.html")
            reflow_smoke(page, f"{docs}/{quote('4. Quality/4.6. Security.html')}")
            open_page(page, f"{docs}/index.html")
            contrast_smoke(page, ".md-content__inner > p", "documentation body text")
            contrast_smoke(page, ".md-content__inner p a", "documentation content link")

            keyboard_smoke(page, f"{client}/index.html", ".skip-link", "#base-url")
            reflow_smoke(page, f"{client}/index.html")
            open_page(page, f"{client}/index.html")
            contrast_smoke(page, "header h1", "web client heading")
            page.evaluate(
                """() => ["working", "input-required", "completed"].forEach(state => {
                  const badge = document.createElement("span");
                  badge.id = `contrast-${state}`;
                  badge.className = `state state-${state}`;
                  badge.textContent = state;
                  document.body.append(badge);
                })"""
            )
            for state in ("working", "input-required", "completed"):
                contrast_smoke(page, f"#contrast-{state}", f"web client {state} state")
            context.close()

            reduced = browser.new_context(reduced_motion="reduce", color_scheme="light")
            block_external_requests(reduced)
            reduced_page = reduced.new_page()
            reduced_motion_smoke(reduced_page, f"{docs}/index.html", stops_animation=False)
            reduced_motion_smoke(reduced_page, f"{client}/index.html", stops_animation=True)
            reduced.close()

    require(abs(contrast_ratio((0, 0, 0), (255, 255, 255)) - 21) < 0.001, "contrast calculation self-test failed")
    sys.stdout.write("browser accessibility: documentation and web client passed\n")


def main(argv: Sequence[str] | None = None) -> int:
    """Install the pinned browser or run its acceptance contract."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--install-browser", action="store_true", help="install the Chromium build pinned by Playwright"
    )
    parser.add_argument("--with-deps", action="store_true", help="also install Linux browser system dependencies")
    arguments = parser.parse_args(argv)
    if arguments.install_browser:
        command = [sys.executable, "-m", "playwright", "install"]
        if arguments.with_deps:
            command.append("--with-deps")
        command.append("chromium")
        # Every argv value is a constant selected by the two boolean CLI flags above.
        subprocess.run(command, check=True)  # noqa: S603
        return 0
    if arguments.with_deps:
        parser.error("--with-deps requires --install-browser")
    run_acceptance()
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AcceptanceError as error:
        sys.stderr.write(f"browser accessibility: {error}\n")
        raise SystemExit(1) from error
