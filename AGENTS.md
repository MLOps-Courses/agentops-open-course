# AGENTS.md

Guidance for AI coding agents working in this repository. This course dogfoods the [AGENTS.md](https://agents.md) standard it teaches. Humans should read [README.md](./README.md).

## What this repository is

The **AgentOps Open Course**: a hands-on course teaching the agent lifecycle with Google ADK 2.0 (Python **and** Go), agentgateway (AAIF), and kagent (CNCF). It has three parts:

- `docs/` — course content (Markdown), published with **Zensical** (config in `mkdocs.yml`). One folder per lifecycle phase (`0. Overview` … `8. Community`), each with an `index.md` and `N.M` section pages.
- `agents/python`, `agents/go` — the reference agent (the "Ops Copilot"), one self-contained project per language **track**.
- `agents/data` — the shared, local dataset both tracks read (SQLite `incidents.db`, `runbooks/`, `skills/`, logs).
- `infra/` — `agentgateway/` config, `kagent/` custom resources, and `k8s/` manifests.

## Golden rules

- **Two tracks stay in lockstep.** Prose is language-neutral; code lives in `=== "Python"` / `=== "Go"` content tabs (`pymdownx.tabbed`, synced site-wide). Only the Chapter 2 hero build uses paired standalone pages (`2.1`/`2.2`). Never update one language's example without the other.
- **Every doc page** starts with a `description:` front-matter and is written as an FAQ (`## question?`).
- **Pin versions and set `model=` explicitly** — the agent ecosystem churns. Verified July 2026: ADK Python `2.3.0`, ADK Go `v2.0.0` (`google.golang.org/adk/v2`, Go `1.26`), agentgateway `v1.3.1` (image `cr.agentgateway.dev/agentgateway`), kagent `v0.10.0-beta3`, Zensical `0.0.x` (alpha).
- **Provider path**: native Gemini (API key/ADC) in Ch. 2–4; all other providers (incl. local Ollama) arrive via agentgateway's LLM backend in Ch. 5. **No LiteLLM.**

## Commands (delegated to `mise run` tasks)

- `mise run install` — sync docs deps, install git hooks, set up both agent tracks.
- `mise run serve` / `mise run build` — serve / build the documentation site (Zensical).
- `mise run format` — dprint (docs/config) + ruff (Python) + gofumpt/goimports (Go).
- `mise run check` — dprint check, docs build, and per-track lint/type/vuln checks.
- `mise run test` — run both agent test suites.
- `mise run scan` — scan for leaked secrets.

Per-track work: `cd agents/python && mise run <task>` or `cd agents/go && mise run <task>`.

## Conventions

- **Formatting**: dprint for JSON/Markdown/TOML/YAML; ruff for Python; gofumpt/goimports for Go.
- **Python**: uv, ruff, ty, pytest — see the `python-stack` standard.
- **Go**: `go tool` dependencies, golangci-lint v2, gotestsum — see the `go-stack` standard.
- **Licensing**: docs are CC-BY-4.0 (`LICENSE.txt`); code is MIT (`agents/LICENSE`, `infra/LICENSE`).
- **Local Kubernetes**: k3d shared `local` cluster; access via `*.localhost` ingress.

## Definition of done

Before committing: `mise run format && mise run check && mise run test` must pass warning-free. Do not commit unless explicitly asked. Use Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`).
