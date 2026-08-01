# AGENTS.md

Guidance for coding agents working in the AgentOps Open Course. Humans should start with [README.md](./README.md). This repository dogfoods the [AGENTS.md](https://agents.md/) convention taught in Chapter 1.

## Repository purpose

The course teaches the complete lifecycle of one **AgentOps Agent** with Google ADK, agentgateway, kagent, MLflow, and OpenTelemetry. `main` is a completed, executable reference that learners inspect and extend; it must not drift into a collection of illustrative snippets. Chapter 8.7 turns that reference into a capstone contract for a learner-owned domain.

- `docs/` contains FAQ-based course pages published by Zensical.
- `agents/python/` is the locked Python reference agent, offline tests, and model-backed evaluations.
- `agents/data/` is immutable seed input: SQLite, logs, runbooks, and the agent's runtime Agent Skills.
- `skills/` holds installable, portable Agent Skills (`npx skills add …`) that distil the course's patterns for reuse in other projects — distinct from the runtime skills under `agents/data/skills`. `scripts/check_conventions.py skills` (via `mise run check:skills`) validates them.
- `clients/web/` is a minimal, offline, dependency-free A2A web client for the AgentOps Agent.
- `load/` holds k6 load tests and the documented latency budgets for the platform.
- `infra/agentgateway/{host,k3d,gke}/` contains the three data-plane profiles.
- `infra/k8s/base` plus `infra/k8s/overlays/{local,gke}` contains the shared Kubernetes deployment.
- `infra/kagent/` declares the BYO Agent, gateway ModelConfig, and governed RemoteMCPServer.
- `infra/mlflow/` builds the locked non-root MLflow server.
- `infra/observability/` contains host Compose and in-cluster OTel/Prometheus/Grafana resources.
- `infra/gcp/` is a plan-first OpenTofu module for the optional GKE lab.

## Course invariants

- **Docs mirror source.** Critical Python excerpts use checked `pymdownx.snippets` regions from `agents/python`; commands/manifests match `infra`. Prefer a short exact excerpt plus a source link over a second pseudo-implementation.
- **Every course page is an FAQ.** It starts with YAML `description` front matter, contains at least one H2, and every H2 ends in `?`. `scripts/check_conventions.py` enforces this and the page frame below.
- **Seed and state stay separate.** `agents/data/incidents.db` is never mutated. Host writes go to `agents/python/.state`; Kubernetes agent/MCP processes share `agentops-agent-state` so reads remain coherent with approved writes. Only the A2A startup and direct write boundary may prepare or migrate runtime state; probes and read tools stay read-only.
- **Restore is crash-recoverable, not an instantaneous multi-file rename.** Stop every writer first. `agent.state` serializes backup/restore with a process lock and fsyncs a three-phase journal; A2A startup recovers an interrupted transaction before schema preflight or publication. Never bypass that boundary with direct file copies or delete unexplained `.restore-*` evidence.
- **Reads and writes have different authority.** Six read/runbook tools can be direct locally or MCP through `AGENT_MCP_URL`. The MCP toolset passes `tool_filter=MCP_READ_TOOL_NAMES` (`mcp_client.py`), so a server cannot widen the surface by advertising more tools. `restart_service` and `resolve_incident` remain in-process, require ADK confirmation, validate targets, and append audit evidence in the same transaction. Replays with the same invocation, action, and target return the original audit row without mutating state again.
- **Policy is attached once, at the app boundary.** `src/agent/governance.py` holds `AgentOpsPolicyPlugin`, an ADK `BasePlugin` registered on the `App` that `composition.py` exports as `app` (also re-exported by `src/agent/__init__.py`, which ADK discovery prefers over a bare `root_agent`). Its hooks fire for every agent, sub-agent, and workflow node, so adding an agent cannot lose the policy. Two properties are load-bearing: the before-model order is budget → compaction → redaction, and the first non-`None` return short-circuits the rest. Never reintroduce a per-agent callback list — that is what let nine copies of the same six callbacks accumulate. ADK 2.6's stock evaluator rebuilds a bare-agent runner, so ADK evals must enter through `evals/governed_adk_eval.py`, while MLflow must use `InMemoryRunner(app=build_app(...))`.
- **Skills and retrieved data have different trust.** The carve-out is keyed on the ADK `LoadSkillTool` **type**, which only the locally built `skill_toolset()` constructs — not on the tool's name, which any MCP server could claim. That result is reviewed repository instruction, so it bypasses injection neutralization and spotlighting while retaining recursive PII/credential redaction. Every other tool result stays data-hardened by default.
- **Audit is append-only, not immutable.** Every row carries its audit schema version. SQLite triggers block row update/delete through the schema; administrators can still alter the file/schema. Do not overclaim.
- **Telemetry content stays private by default.** Both ADK/GenAI content-capture variables default to literal `false`. PII callbacks cover outbound model requests, inbound model responses, and tool output, but raw session ingestion occurs earlier.
- **No LiteLLM or garak contract.** Runtime/evaluation uses ADK's OpenAI-compatible client for Ollama/agentgateway or native Gemini when selected explicitly. `mise run redteam` is deterministic offline adversarial regression, not live-model penetration testing.
- **Planning is bounded.** `root_agent` plans only multi-step investigations and verifies approved actions afterward. `triage_workflow` is the runnable, read-only plan → investigate → evidence review → recommend path; do not replace it with an unbounded reflection loop.
- **Cost-efficient by default.** Prefer deterministic offline tests and fakes, the smallest model that can validate the behavior, and single-replica resource-bounded local services. Measure before increasing model size, context, RAM, CPU, storage, replicas, or load-test concurrency. Do not start a cluster, observability stack, model server, paid API, or cloud resource unless it materially validates the current boundary; stop temporary processes and tear down disposable resources when the check is complete.

## Open-source boundary

The required software path is OSS: ADK, agentgateway, kagent, MLflow, OpenTelemetry, Prometheus, Grafana, Ollama, the Apache-2.0 open-weight Qwen3 model, and repository code. It requires no account, no mandatory SaaS, and no usage fee. Gemini, Vertex AI, GKE, GCS, Artifact Registry, and GitHub hosting are optional proprietary services. Never blur that distinction or call an optional cloud environment fully OSS.

Local Qwen3/Ollama is the default model path from the first Chapter 2 interaction. `AGENT_MODEL_PROVIDER=openai-compatible`, `AGENT_MODEL=qwen3:4b-instruct`, `OPENAI_BASE_URL=http://127.0.0.1:11434/v1`, and the non-secret `local-ollama` marker are the stable defaults. Chapter 5 changes only `OPENAI_BASE_URL` to the agentgateway listener. Native Gemini and the GKE/Vertex path are optional comparisons; the GKE overlay uses Workload Identity Federation and mounts no cloud key.

The optional GKE path compatibility-pins `gemini-3.5-flash`. Do not move that pin because a newer model exists: the pinned agentgateway release's Vertex conversion adds a blank text part beside a function response, which Gemini 3.6 rejects. A replacement model and stable gateway pair is supported only after `mise run gke:smoke` completes both its synthetic tool-result turn and its stable-seed, read-only A2A retrieval.

## Pinned contracts

`SUPPORT.md` defines which surfaces are stable, plus compatibility, deprecation, supported platforms, upgrade, rollback, and explicit non-goals. Course prose is deliberately not frozen; the software contracts are.

Use the repository files and locks as version authority — never a number copied into prose. The authoritative pin for each component lives in:

- Google ADK, MLflow, and every Python dependency: `agents/python/pyproject.toml` for the range, `uv.lock` for the exact resolution. The MLflow server image has its own `infra/mlflow/pyproject.toml`.
- Zensical and the documentation toolchain: root `pyproject.toml` and `uv.lock`.
- CLI tools (agentgateway, k3d, kubectl, helm, helmfile, skaffold, k6, gcloud, …): `mise.toml` `[tools]`, with checksums and provenance in `mise.lock`.
- kagent Helm charts: `infra/helmfile.yaml`. API resources are `v1alpha2`.
- Container images (agentgateway, OpenTelemetry Collector, Loki, Prometheus, …): digest-pinned at their use site under `infra/k8s/` and `infra/observability/`.
- Python interpreter: `.python-version`.

GitHub Actions artifacts are transient handoffs. The organization caps artifact and log retention at **7 days**, so every `upload-artifact` step stays at or below that limit; durable release evidence belongs on the immutable GitHub release and in OCI attestations.

This file owns the stable network inventory, while `scripts/check_conventions.py` maps every entry to its executable owner: MCP `:3000`, A2A `:3001`, OpenAI-compatible model `:4000`, gateway metrics `:15020`, host gateway readiness `:15021`, raw MCP `:8000`, raw A2A `:8080`, web client `:8001`, ADK web UI `:8002`, documentation preview `:8003`, Ollama `:11434`, MLflow `:5000`, OTLP `:4317/:4318`, collector metrics `:8889`, Prometheus `:9090`, Alertmanager `:9093`, host Grafana `:3002`, Loki `:3100`, and the local registry `:5050`.

## Development commands

Root tasks:

```bash
mise run install
mise run install:platform
mise run install:gcp
mise run install:maintainer
mise run doctor
mise run doctor:model
mise run doctor:gateway
mise run doctor:platform
mise run doctor:gcp
mise run format
mise run check:core
mise run check
mise run test
mise run scan
mise run build
mise run build:docs
mise run serve
mise run gateway:host
mise run gateway:host:start
mise run gateway:host:stop
mise run gateway:host:status
mise run gateway:host:logs
mise run gateway:host:auth
mise run smoke:host
mise run observability:up
mise run observability:down
mise run cluster:start
mise run platform:install
mise run platform:dev
mise run promote
mise run gke:smoke
```

`mise run install` bootstraps the learner-facing core tools and environments. The platform and maintainer tiers are explicit so a first checkout does not install Kubernetes, cloud, and security tooling it does not yet need.

Aggregate tasks run their children: `install`, `format`, `check`, and `build` each fan out, so `mise run build` builds the site **and** both container images and therefore needs Docker. Use `mise run build:docs` for the container-free documentation build. `install:core`, `doctor:base`, `watch`, and `scan` are aliases of `install`, `doctor`, `serve`, and `secure`; `install:tools:*` are hidden implementation details.

Agent tasks from `agents/python/`:

```bash
mise run check
mise run test
mise run redteam
mise run mcp
mise run mcp:http
mise run a2a
mise run data:reset
mise run workflow
mise run coordinator
```

`AGENT_ENTRYPOINT=agent|workflow|coordinator` selects the composition behind the single lazy `src/agent` package boundary. Use the task aliases above rather than raw `adk run` commands so model configuration and the repository `.env` are loaded consistently.

The `eval:*` tasks (`eval`, `eval:workflow`, `eval:report`, `eval:mlflow`, `eval:cost`, `eval:ground`, `eval:ab`, `eval:retrieval`) call a configured model and stay outside the offline test gate — they are scheduled evidence in `eval.yml`, not CI gates. `eval:validate` is the only offline eval and runs in CI. The MLflow judge is optional and must use the configured agentgateway URL.

## Local and cloud safety

The host gateway is `infra/agentgateway/host/config.yaml`. Host quickstarts use the digest-pinned container wrapper exposed by the `gateway:host*` tasks; every published listener binds to `127.0.0.1`. On native Linux, the wrapper owns a bridge-address-only relay so its container can reach MCP, A2A, and Ollama while those upstream processes remain bound to host loopback. The raw agentgateway binary currently listens on all interfaces and is an advanced/manual path, not a learner quickstart.

Kubernetes begins in Chapter 6. Local Kubernetes is created only from `infra/k3d.yaml`, uses `registry.localhost:5050`, and is deployed from the repository root with:

```bash
mise run platform:dev
```

That task derives `AGENT_SOURCE_COMMIT` from `HEAD`; raw Skaffold commands must provide the same exact-source value because `infra/skaffold.yaml` refuses an untraceable image build.

Do not start host Compose observability while the in-cluster stack is forwarded on the same ports. No profile creates an Ingress, LoadBalancer, or public application endpoint; clients use temporary port-forwards through agentgateway.

The GKE path stops at `tofu plan` unless the user explicitly approves deployment. The required `project_id` variable selects the project; the rendered GKE bundle derives its Workload Identity accounts, GCS bucket, DNS service IP, and Vertex project from OpenTofu outputs. The single zonal Spot-node design is production-shaped but interruptible and non-HA. It bills real money: `docs/7. Observability/7.3. Costs.md` owns the estimate and the date it was checked. Do not copy that figure anywhere else, and refresh variable prices before apply. `skaffold delete`, PVC deletion, `k3d cluster delete`, `tofu apply`, and `tofu destroy` require careful context/review; cloud apply/destroy requires explicit approval.

## Documentation workflow

Every course page follows the same frame. `scripts/check_conventions.py` (via `mise run check:docs`) enforces the front matter, the FAQ headings, the opening block, the closing heading, the page kind, and the collapsible and link-label rules below, so a page cannot silently drift out of shape.

```markdown
---
description: <one sentence>
---

# N.M. Title

!!! abstract "In one glance"

    - **You will:** <outcome, verb first, second person, plain words>
    - **You need:** <a checkable precondition, or "Nothing beyond a terminal">
    - **Time:** about <N> minutes, <concept | hands-on | reference | orientation>.

## <question ending in ?>

…

## What proves this page worked?

<the verification commands>

**You are done when:**

- <observable state>

Continue to [<next page>](link) when <the condition that matters>.
```

- The closing H2 is exactly `What proves this page worked?`, or `What proves this chapter worked?` on a `docs/*/index.md`, or `How should you use this page later?` on a pure lookup page (0.5, 0.6, 0.7). Nothing links to those anchors, so the wording stays uniform on purpose.
- Depth that is valuable but not needed on a first pass goes in a `??? note "Deeper: …"` collapsible, relocated word for word. Every summary starts with `Deeper:`. Zero to three per page.
- Never collapse the subject's definition, the reason it matters, the command to run, the expected output, or anything that costs money, destroys data, or bounds a security claim. The arithmetic behind a cost may be collapsed; the sentence saying "this can be billed" or "this is not production" stays visible above the triangle.
- On a hands-on page the learner must reach a runnable command within the first two H2 sections. `docs/2. Agents/2.1. First Agent.md` is the reference for that shape.
- Admonition vocabulary is fixed: `abstract` for the page frame, `success` for end-of-page takeaways, `warning` for common mistakes, `danger` for destructive/costly/security actions, `tip` for an optional shortcut, `info` for skippable background, `note` for a neutral aside. The same message must use the same type everywhere it appears.
- Prose rules: open each H2 with a concrete sentence of 25 words or fewer; keep sentences under ~35 words and at most one em-dash pair per paragraph; cap inline cross-links at two per paragraph and push the rest to a closing "Owned by …" line; define an unfamiliar term at first use in 15 words or fewer; use full page names as link labels, never a bare `[5.2]`.
- Accessibility is content, not decoration: adjacent `**Diagram in words:**` prose must communicate every new or changed Mermaid diagram's actors, relationships, and sequence; never rely on color alone; link dense unfamiliar terms to glossary anchors. `ACCESSIBILITY.md` is the public contract; `docs/diagram-legacy.txt` is an exact-hash ratchet for previously reviewed diagrams, not permission for new exemptions.
- Keep prose practical and question-led; finish technical pages with verification and, where relevant, teardown.
- Use only `1.` for ordered Markdown list items.
- A `--8<--` snippet include must sit inside a fenced code block. A bare include is rendered as Markdown, so a leading `#` comment in the region becomes an `<h1>`.
- Never add machine-specific paths, credentials, floating image tags, stale registry names, or commands that depend on private dotfiles.
- Distinguish offline tests, local model calls, hosted model calls, Kubernetes changes, and cloud changes before asking a learner to run anything.
- Do not claim alerts, feedback endpoints, online scorers, public auth/TLS, HA, backups, or cost metrics unless the repository implements and validates them.
- The public course is hosted at `https://agentops-open-course.fmind.dev/`. When changing repository, Pages, DNS, or source-link contracts, re-run the anonymous publication gate before claiming the surface still works.
- Update `README.md`, public component READMEs, course prose, and this file together when a public contract changes.

## Definition of done

Re-read the original request, inspect the final diff, and run:

```bash
mise run install:maintainer
mise run format
mise run check
mise run test
mise run scan
```

The Python suite enforces at least 95% branch coverage. The complete gate renders both overlays and scans the repository; no model, cluster, or cloud call is part of it. Never suppress a real failure to force green. Do not call a live model, deploy Kubernetes/cloud resources, or commit unless the user explicitly asks.
