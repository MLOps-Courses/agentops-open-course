# AGENTS.md

Guidance for AI coding agents working in this repository. This course dogfoods the [AGENTS.md](https://agents.md) standard it teaches. Humans should read [README.md](./README.md).

## What this repository is

The **AgentOps Open Course**: a hands-on course teaching the agent lifecycle with Google ADK 2.0 (Python), agentgateway (AAIF), and kagent (CNCF). It has three parts:

- `docs/` — course content (Markdown), published with **Zensical** (config in `mkdocs.yml`). One folder per lifecycle phase (`0. Overview` … `8. Community`), each with an `index.md` and `N.M` section pages.
- `agents/python` — the reference agent (the "Ops Copilot"), a self-contained project.
- `agents/data` — the local dataset the agent reads (SQLite `incidents.db`, `runbooks/`, `skills/`, logs).
- `infra/` — `agentgateway/` config, `kagent/` custom resources, `k8s/` manifests, `helmfile.yaml`/`skaffold.yaml` (deploy loop), and `observability/` (an optional OTel→Jaeger/Prometheus/Grafana `compose.yaml` for Ch. 7).
- `.github/workflows/` — CI (`ci.yml`: `check`/`test`), docs deploy (`docs.yml`: Zensical → GitHub Pages), and security (`scan.yml`: gitleaks + Trivy). All call the same `mise run` tasks as the local hooks.

## Golden rules

- **Docs mirror the reference agent.** Prose is followed by runnable **Python** code fences; keep every snippet consistent with the real source in `agents/python`. The Chapter 2 hero build is a single standalone page (`2.1. First Agent`); the rest of the course refines it one page at a time. Never let a doc example drift from the code it teaches.
- **Every doc page** starts with a `description:` front-matter and is written as an FAQ (`## question?`).
- **Pin versions and set `model=` explicitly** — the agent ecosystem churns. Verified July 2026: ADK Python `2.3.0`, agentgateway `v1.3.1` (image `cr.agentgateway.dev/agentgateway`), kagent `v0.10.0-beta3`, Zensical `0.0.x` (alpha).
- **Provider path**: native Gemini (API key/ADC) in Ch. 2–4; all other providers (incl. local Ollama) arrive via agentgateway's LLM backend in Ch. 5. **No LiteLLM.**
- **The container entrypoint is `agents/python/Dockerfile`** — a slim, non-root image whose `server.py` serves the agent over A2A on `:8080` (the contract kagent's `type: BYO` Agent expects). `skaffold.yaml` builds it (context `../agents` so the shared dataset is included). Keep any container/launcher command in the docs consistent with `agents/python/README.md`.

## Commands (delegated to `mise run` tasks)

- `mise run install` — sync docs deps, install git hooks, set up the Python agent.
- `mise run serve` / `mise run build` — serve / build the documentation site (Zensical).
- `mise run format` — dprint (docs/config) + ruff (Python).
- `mise run check` — dprint check, docs build, and the Python agent's lint/type/vuln checks.
- `mise run test` — run the Python agent test suite.
- `mise run scan` — scan git history for leaked secrets (gitleaks). CI (`scan.yml`) additionally runs Trivy (deps/secrets/misconfig); accepted, documented suppressions live in `.trivyignore` and the agent's `pip-audit`/lint ignores.

Agent work: `cd agents/python && mise run <task>`. Live-model gates (need a provider key, kept outside the fast offline `mise run test`): `mise run eval` (`adk eval`, tool trajectory), `mise run eval:mlflow` (MLflow GenAI judges + prompt registry, Ch. 4.4/7.0), `mise run redteam` (garak red-teaming via `uvx`, Ch. 4.6). PII redaction (Presidio, `src/agent/pii.py`) is a runtime `before_model_callback` and is unit-tested offline.

## Conventions

- **Formatting**: dprint for JSON/Markdown/TOML/YAML; ruff for Python.
- **Python**: uv, ruff, ty, pytest — see the `python-stack` standard.
- **Tooling bar**: any added tool must be fully OSS with **no paywall/feature-gating** (current adds: MLflow eval, Presidio PII, garak red-team). Fix a transitive-dep CVE with a version floor in `pyproject.toml` (see the `cryptography`/`pyarrow` comments); use a documented `--ignore-vuln` only when no fix exists.
- **Licensing**: docs are CC-BY-4.0 (`LICENSE.txt`); code is MIT (`agents/LICENSE`, `infra/LICENSE`).
- **Local Kubernetes**: k3d shared `local` cluster; access via `*.localhost` ingress.

## Definition of done

Before committing: `mise run format && mise run check && mise run test` must pass warning-free. Do not commit unless explicitly asked. Use Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`).
