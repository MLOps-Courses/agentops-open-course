# Accessibility

The AgentOps Open Course should be usable without a mouse, without color perception, and without relying on a diagram renderer. Accessibility defects are course defects.

## Current support

- The rendered site uses semantic headings, labeled navigation and search, visible keyboard focus from the Hextra theme, and a skip-to-content link.
- The dependency-free web client labels its endpoint, message, approval rationale, and cancellation controls; streaming and terminal task states use a polite live region.
- Commands, expected output, warnings, and completion criteria are written as text. Color is never the only intended signal.
- Contributor policy requires every new or changed Mermaid diagram to have adjacent prose that communicates the same actors, relationships, and sequence.
- Unfamiliar terms are defined at first use and linked to the course glossary on dense pages.

## What is checked automatically?

`mise run check:docs` enforces the structural floor before a change can publish:

- Every Mermaid block needs adjacent `**Diagram in words:**` prose, without exception. A hash allowlist used to exempt diagrams reviewed before the rule existed; every one of them now carries prose, so the allowlist and its file are gone and the rule holds for the whole corpus.
- The rendered site must give the document a language, exactly one main landmark and H1 per page, and accessible names to non-fragment links.
- The homepage must expose its existing description through Open Graph, Twitter, canonical URL, and Course structured metadata; the custom 404 must provide a named recovery route.
- The dependency-free client must retain native labels, one main landmark and H1, polite status announcements, visible focus, narrow-layout reflow, forced-colors behavior, and a reduced-motion fallback.

`mise run check:accessibility` adds the representative browser acceptance used by the CI workflow. `mise run install:accessibility` installs nothing — it only verifies that a system `google-chrome-stable`, `google-chrome`, or `chromium` is on `PATH`. `check:accessibility` then builds the site and drives that browser through `tools/bin/accessibility`, whose chromedp driver is pinned in `tools/go.mod`; the browser itself is the host's, resolved from `CHROME_PATH` then `PATH`. Because the browser version is not pinned, record the Chrome version alongside any accessibility result you report.

The browser acceptance covers the homepage, a dense diagram page, A2A, security, capstone, custom 404 recovery, search recovery, and `clients/web/index.html`. It verifies keyboard skip navigation and visible focus, named search and code-copy controls, search combobox semantics, document-level reflow at 320 CSS pixels, reduced-motion and forced-colors media behavior, representative computed AA contrast, table headers, and one main landmark plus a named H1 in each sampled accessibility tree. The web-client path also exercises polite status announcements, cancellation, and the required approval-rationale controls against deterministic browser-local A2A responses.

The browser acceptance samples load-bearing surfaces; it does not exhaustively test every page, keyboard sequence, color pair, browser, zoom implementation, or assistive technology. The deterministic source and rendered-HTML checks remain the whole-course structural floor.

## What was audited, and when?

On 30 July 2026, commit `5c8e083` was audited on Debian 12 x86_64 with Chrome 150 and Lighthouse 13.4.1. The rendered home page, A2A, security, and capstone pages plus the standalone web client each scored 100 in Lighthouse's accessibility category after every reported finding was corrected. That audit reviewed keyboard reachability, visible focus, skip navigation, search semantics, code-copy controls, task streaming, approval rationale, cancellation, 200% zoom/reflow, narrow mobile layout, reduced motion, forced-colors behavior, landmarks, table headers, form labels, status announcements, representative contrast, and the Markdown alternatives adjacent to diagrams. It was a WCAG-oriented product audit, not a formal conformance certification.

**That audit predates the current site.** Commit `5c8e083` rendered the Python course through a different static-site generator and a different theme; the course now builds with Hugo and Hextra, so the markup, the navigation, the search widget, and the color tokens are all new. What still covers the current render is the automated acceptance — `mise run check:accessibility` drives a real Chrome or Chromium over the built site on every CI run — plus the deterministic rendered-HTML checks. A fresh manual Lighthouse and accessibility-tree audit of the Hextra render has not been performed, and this file records its result when it happens. Treat the 100 scores above as history, not as a claim about the page you are reading.

## Known limits

The source and rendered-HTML checks above are release gates: they run inside `check:docs`, which the required `validate` job carries. The representative Chromium acceptance is weaker than that — it runs on every pull request as its own job, but it is not one of the three required status checks, so a red accessibility run reports a regression without blocking it. Manual Chrome and accessibility-tree evidence is not repeated for every candidate, and as noted above no manual audit of the Hextra render exists yet. Firefox, Safari, VoiceOver, NVDA, and Orca remain best-effort because the project does not have a repeatable test environment for those combinations. Report barriers with the exact combination so the support matrix can grow from evidence.

The Hextra theme and the repository's custom stylesheet supply the current color palette, but a theme or custom-style change still requires another contrast and keyboard audit. Mermaid support varies across screen readers, so diagrams never carry unique information.

PDF and offline ebook formats are not currently published. The repository Markdown remains the text-first fallback when the hosted interface creates a barrier.

## How to report a barrier

[Open an accessibility issue](https://github.com/MLOps-Courses/agentops-open-course/issues/new) with:

- the page URL or source path;
- the browser, operating system, and assistive technology involved;
- the action you attempted and what blocked it;
- a suggested correction, when you have one.

Do not include private or security-sensitive data. Report a vulnerability through [SECURITY.md](./SECURITY.md) instead of a public issue.
