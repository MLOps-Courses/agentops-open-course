# Contributing

Contributions that make the course more accurate, runnable, or useful are welcome. Small fixes can go straight to a pull request. For a new dependency, architectural change, or substantial chapter rewrite, open an issue first so the approach can be reviewed before implementation.

[GOVERNANCE.md](./GOVERNANCE.md) explains how decisions are made, what “reviewed” means in this single-maintainer project, and how contributor access can grow.

## How do I set up the repository?

Install [mise](https://mise.jdx.dev/), clone your fork, and run:

```bash
mise run install:maintainer
```

This installs every pinned tool and locked environment used by the complete contributor gate, then enables the Git hooks. Learners use the smaller `mise run install` tier.

## What should a contribution preserve?

- Keep documentation examples synchronized with the implementation in `agents/` and `infra/`.
- Keep every `docs/**/*.md` page FAQ-oriented and start it with `description:` front matter.
- Keep the page frame: an `!!! abstract "In one glance"` block (**You will** / **You need** / **Time**) directly under the H1, and a closing `## What proves this page worked?` section with a **You are done when:** list and a `Continue to …` line. `mise run check:docs` enforces it; `AGENTS.md` documents the full contract.
- Put depth a first-time reader can skip in a `??? note "Deeper: …"` collapsible rather than deleting it.
- Give every new or changed Mermaid diagram adjacent prose that communicates the same actors, relationships, and sequence. Never rely on color alone, and link dense unfamiliar terms to glossary anchors; see [ACCESSIBILITY.md](./ACCESSIBILITY.md).
- Use only open-source software dependencies without paid feature gates. A hosted model or cloud may be documented as an optional substrate, never as part of the open-source software claim.
- Keep the local path usable without a Kubernetes or cloud account.
- Never commit credentials, generated reports, runtime state, or a populated `.env`.
- Use `1.` for every item in an ordered Markdown list.

## Which checks must pass?

Run the same tasks as the hooks and CI:

```bash
mise run format
mise run check
mise run test
mise run scan
```

`format` updates Python, Markdown, shell, and configuration files. `check` validates/builds docs, Python, infrastructure, shell, workflows, and dependency licenses. `test` is offline and must not call a model or cloud service. `scan` runs full-history gitleaks plus Trivy vulnerability, secret, license, and misconfiguration checks.

Live-model evaluations are optional and require a configured model. The optional local Ollama path uses the non-secret `local-ollama` marker and needs no provider credential; default Gemini and other hosted paths require their documented authentication:

```bash
cd agents/python
mise run eval
mise run eval:mlflow
```

Do not include live-model output or secrets in a pull request.

## How should I change course examples?

Treat an executable snippet as a public API. Before changing it:

1. Read the source file it mirrors.
1. Run the exact command from the documented working directory.
1. Include the expected observable result and a cleanup command where the exercise creates state.
1. Rebuild the site with `mise run build:docs` and follow the rendered links around the changed page.

## How should I submit a pull request?

- Keep one pull request focused on one outcome.
- Explain what changed, why it was needed, how it was implemented, and how it was tested.
- Use a [Conventional Commits](https://www.conventionalcommits.org/) subject such as `docs: clarify the local gateway setup`.
- Do not add generated-by or co-author attribution.

By participating, you agree to follow the [Code of Conduct](./CODE_OF_CONDUCT.md) and [accessibility contract](./ACCESSIBILITY.md). Report security issues through [SECURITY.md](./SECURITY.md), not a public issue.
