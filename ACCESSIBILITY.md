# Accessibility

The AgentOps Open Course should be usable without a mouse, without color perception, and without relying on a diagram renderer. Accessibility defects are course defects.

## Current support

- The rendered site uses semantic headings, labeled navigation and search, visible keyboard focus from the Material theme, and a skip-to-content link.
- The dependency-free web client labels its endpoint, message, approval rationale, and cancellation controls; streaming and terminal task states use a polite live region.
- Commands, expected output, warnings, and completion criteria are written as text. Color is never the only intended signal.
- Contributor policy requires every new or changed Mermaid diagram to have adjacent prose that communicates the same actors, relationships, and sequence.
- Unfamiliar terms are defined at first use and linked to the course glossary on dense pages.

## What is checked automatically?

`mise run check:docs` enforces the structural floor before a change can publish:

- Every new or changed Mermaid block needs adjacent `**Diagram in words:**` prose. `docs/diagram-legacy.txt` stores exact hashes for previously reviewed diagrams, so changing one cannot inherit a broad exemption and deleting one removes its hash.
- The rendered site must give the document a language, exactly one main landmark and H1 per page, and accessible names to non-fragment links.
- The homepage must expose its existing description through Open Graph, Twitter, canonical URL, and Course structured metadata; the custom 404 must provide a named recovery route.
- The dependency-free client must retain native labels, one main landmark and H1, polite status announcements, visible focus, narrow-layout reflow, forced-colors behavior, and a reduced-motion fallback.

`mise run check:accessibility` adds the representative browser acceptance used by the Docs workflow. After the one-time `mise run install:accessibility`, it builds the site and uses the Chromium release paired with the exactly pinned Playwright dependency to verify keyboard skip navigation and visible focus, document-level reflow at 320 CSS pixels, reduced-motion media behavior, and computed AA contrast. The smoke covers both the landing and technical course templates plus `clients/web/index.html`.

The browser smoke samples load-bearing surfaces; it does not exhaustively test every page, keyboard sequence, color pair, browser, or assistive technology. The deterministic source and rendered-HTML checks remain the whole-course structural floor.

## What was audited, and when?

On 30 July 2026, commit `5c8e083` was audited on Debian 12 x86_64 with Chrome 150 and Lighthouse 13.4.1. The rendered home page, A2A, security, and capstone pages plus the standalone web client each scored 100 in Lighthouse's accessibility category after every reported finding was corrected. This is a historical baseline, not manual evidence for every later commit.

The audit also reviewed keyboard reachability, visible focus, skip navigation, search semantics, code-copy controls, task streaming, approval rationale, cancellation, 200% zoom/reflow, narrow mobile layout, reduced motion, forced-colors behavior, landmarks, table headers, form labels, status announcements, representative contrast, and the Markdown alternatives adjacent to diagrams. It is a WCAG-oriented product audit, not a formal conformance certification.

## Known limits

The source, rendered-HTML, and representative Chromium checks above are release gates. Manual Chrome and accessibility-tree evidence is not yet repeated for every candidate; [issue #112](https://github.com/MLOps-Courses/agentops-open-course/issues/112) owns a fresh v1-candidate audit. Firefox, Safari, VoiceOver, NVDA, and Orca remain best-effort because the project does not have a repeatable test environment for those combinations. Report barriers with the exact combination so the support matrix can grow from evidence.

The default indigo theme supplies the current color palette, but a theme or custom-style change still requires another contrast and keyboard audit. Mermaid support varies across screen readers, so diagrams never carry unique information.

PDF and offline ebook formats are not currently published. The repository Markdown remains the text-first fallback when the hosted interface creates a barrier.

## How to report a barrier

[Open an accessibility issue](https://github.com/MLOps-Courses/agentops-open-course/issues/new) with:

- the page URL or source path;
- the browser, operating system, and assistive technology involved;
- the action you attempted and what blocked it;
- a suggested correction, when you have one.

Do not include private or security-sensitive data. Report a vulnerability through [SECURITY.md](./SECURITY.md) instead of a public issue.
