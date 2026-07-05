# AgentOps Open Course

**Welcome to the [AgentOps Open Course](https://agentops-open-course.fmind.dev/)!**

Learn to build, secure, deploy, and operate AI agents — the **AgentOps lifecycle** — with **100% open-source** technology. You will build a real agent end to end with **Google ADK 2.0** in **Python and Go**, give it tools, memory, and multi-agent workflows, evaluate and guard it, secure and connect it with **agentgateway** (AAIF), deploy it to **Kubernetes with kagent** (CNCF), and observe its behavior and cost — all runnable **locally**, on the model provider of your choice.

This is the agent-focused sibling of the [MLOps Coding Course](https://mlops-coding-course.fmind.dev/).

## Key Features

- **Hands-on, two languages**: every capability is shown in **Python** and **Go**, side by side.
- **100% open source**: Google ADK, agentgateway, kagent, MCP, A2A, and AGENTS.md — no proprietary lock-in.
- **Local-first**: run everything on your machine, from a first agent to a Kubernetes deployment.
- **Provider-neutral**: start on native Gemini, then reach any provider (incl. local Ollama) via the gateway.
- **AgentOps end to end**: capabilities, quality, gateway, platform, observability, and community.

## Course Content

1. **Overview**: what agents and AgentOps are, the ecosystem, languages, and providers.
1. **Setup**: a professional local environment (Python/Go, containers, k3d, providers).
1. **Agents**: your first ADK 2.0 agent, models, instructions, sessions, and the dev loop.
1. **Capabilities**: tools, skills, MCP, memory/RAG, workflows, and A2A.
1. **Quality**: typing, linting, testing, metrics, evaluations, guardrails, and security.
1. **Gateway**: secure and connect the agent with agentgateway (AAIF).
1. **Platform**: run agents on Kubernetes with kagent (CNCF).
1. **Observability**: reproducibility, tracing, monitoring, cost, feedback, and governance.
1. **Community**: repository, license, releases, templates, documentation, and the AAIF.

## Repository Layout

- `docs/` — the course content (one folder per lifecycle phase), published with [Zensical](https://zensical.org).
- `agents/python` and `agents/go` — the reference agent, one self-contained project per language track (MIT).
- `agents/data` — the shared, local dataset the agent runs on (SQLite incidents, runbooks, skills).
- `infra/` — agentgateway, kagent, and Kubernetes manifests (MIT).

## Installation

This project uses [mise](https://mise.jdx.dev) as its task runner and [uv](https://docs.astral.sh/uv/) for Python.

```bash
mise run install   # sync docs deps, install git hooks, and set up both agent tracks
mise run serve     # serve the documentation locally at http://localhost:8000/
```

Common tasks: `mise run build` (build the site), `mise run format`, `mise run check`, `mise run test`.

## Contributions

This course is open source under a **dual license**: [CC-BY-4.0](./LICENSE.txt) for the course content and [MIT](./agents/LICENSE) for the code (`agents/`, `infra/`). Contributions are welcome — [open an issue](https://github.com/MLOps-Courses/agentops-open-course/issues) or [submit a pull request](https://github.com/MLOps-Courses/agentops-open-course/pulls).

**Join us in advancing the field of AgentOps by sharing your expertise and learning from others!**
