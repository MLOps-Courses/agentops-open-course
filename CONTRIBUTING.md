# Contributing

Contributions that make the course more accurate, runnable, or useful are welcome. Small fixes can go straight to a pull request. For a new dependency, architectural change, or substantial chapter rewrite, open an issue first so the approach can be reviewed before implementation.

## How do I set up the repository?

Install [mise](https://mise.jdx.dev/), clone your fork, and run:

```bash
mise install
mise run install
```

The first command installs the repository's pinned tools. The second installs the documentation and agent dependencies and enables the Git hooks.

## What should a contribution preserve?

- Keep documentation examples synchronized with the implementation in `agents/` and `infra/`.
- Keep every `docs/**/*.md` page FAQ-oriented and start it with `description:` front matter.
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

`format` updates Python, Markdown, shell, and configuration files. `check` validates/builds docs, Python, infrastructure, shell, and workflows. `test` is offline and must not call a model or cloud service. `scan` runs full-history gitleaks plus Trivy vulnerability, secret, and misconfiguration checks.

Live-model evaluations are optional and require credentials:

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
1. Rebuild the site with `mise run build` and follow the rendered links around the changed page.

## How should I submit a pull request?

- Keep one pull request focused on one outcome.
- Explain what changed, why it was needed, how it was implemented, and how it was tested.
- Use a [Conventional Commits](https://www.conventionalcommits.org/) subject such as `docs: clarify the local gateway setup`.
- Do not add generated-by or co-author attribution.

By participating, you agree to follow the [Code of Conduct](./CODE_OF_CONDUCT.md). Report security issues through [SECURITY.md](./SECURITY.md), not a public issue.
