# AGENTS.md

Guidance for coding agents working in the AgentOps Open Course. Humans should start with [README.md](./README.md). This repository dogfoods the [AGENTS.md](https://agents.md/) convention taught in Chapter 1.

## Repository purpose

The course teaches the complete lifecycle of one Go AgentOps Agent with Google ADK, agentgateway, kagent, and OpenTelemetry into Tempo, Loki, Prometheus, Alertmanager, and Grafana. `main` is a completed executable reference learners inspect and extend, not a collection of illustrative snippets.

- `agents/go/` is the Go reference agent, offline tests, state commands, protocol servers, and distroless image.
- `agents/data/` is immutable seed input: SQLite, logs, runbooks, and runtime Agent Skills.
- `evals/` is a standalone black-box Go evaluation module. It must not require or import the agent module.
- `tools/` is a standalone Go module for repository conventions, accessibility, release evidence, and local support commands.
- `content/` contains 84 Hugo pages: 74 course pages, nine chapter indexes, and the landing page.
- `layouts/`, `assets/`, `data/nav.yaml`, and `hugo.toml` own the Hextra site build and explicit learning path.
- `skills/` contains portable Agent Skills distilled from the course, distinct from runtime skills under `agents/data/skills`.
- `clients/web/` is a minimal dependency-free A2A client.
- `load/` contains k6 load tests and documented latency budgets.
- `infra/agentgateway/{host,k3d,gke}/` contains data-plane profiles.
- `infra/k8s/base` and `infra/k8s/overlays/{local,gke,scale}` contain shared deployment resources; `scale` layers a replicated MCP read plane on `local` for Chapter 6.9, and `check:infra` renders, validates, and lints all three.
- `infra/kagent/` declares the BYO Agent, ModelConfig, and governed RemoteMCPServer.
- `infra/observability/` contains host and in-cluster OpenTelemetry backends.
- `infra/gcp/` is a plan-first OpenTofu module for the optional GKE laboratory.

The root `go.mod` exists only for the Hextra Hugo Module. Never add agent, evaluator, or repository-tool dependencies to it.

## Course invariants

- **Docs mirror source.** Critical excerpts use `{{< include >}}` over exact named `--8<-- [start:name]` and end regions. Missing files, missing or duplicate regions, and empty excerpts fail the Hugo build.
- **Seed and state stay separate.** `agents/data/incidents.db` is never mutated. Host writes go to `agents/go/.state`; Kubernetes writers share `agentops-agent-state`.
- **Only write-owning boundaries prepare state.** A2A startup and direct state commands may copy or migrate runtime state. Probes and read tools remain read-only.
- **Restore is crash-recoverable.** Stop every writer first. State restore holds a process lock, fsyncs a three-phase journal, and recovers interrupted transactions before schema preflight or publication. Never bypass it with file copies or delete unexplained `.restore-*` evidence.
- **Reads and writes have different authority.** Only the conversational entrypoint can move its six read/runbook tools from local calls to MCP through `AGENT_MCP_URL`. Workflow and coordinator specialists keep local tools. Guarded writes remain in process.
- **MCP cannot widen the surface.** The client filters the server catalog through `MCPReadToolNames`; an advertised extra tool never joins the model request.
- **Writes require confirmation and attribution.** `restart_service` and `resolve_incident` require ADK confirmation, valid targets, approver/session/invocation identity, and a bounded redacted rationale. Mutation and audit insert share one transaction.
- **Action replay is idempotent.** The same invocation, action, and target returns the original audit result without another mutation.
- **Policy is attached once.** One ADK plugin at the app boundary covers every agent, sub-agent, workflow node, and coordinator specialist.
- **Guard order is load-bearing.** Before-model order is budget, compaction, redaction. The first non-nil callback result short-circuits later guards. After-model usage accounting runs before response redaction.
- **PII has two layers.** In-process deterministic Go redaction is always on. agentgateway adds central builtin masking and a private Go webhook that asks local Ollama for person, location, and organization spans. The webhook validates exact byte spans and fails closed.
- **Layer 1 remains necessary.** A gateway cannot see direct model calls, local logs, saved notes, audit writes, or pre-gateway chapters. It also cannot replace checksum-backed validation and credential tripwires.
- **Skills and retrieved data have different trust.** Only the locally constructed concrete skill loader can mark repository-reviewed instruction as trusted. Tool name strings cannot grant that status. Secret and PII redaction still applies.
- **Audit is append-only, not immutable.** SQLite triggers block ordinary update and delete, and every row carries its schema version. An administrator with file or schema authority can still alter it.
- **Telemetry content stays private by default.** ADK and GenAI content-capture settings default to literal `false`. Redaction covers model and tool boundaries, but raw session ingestion happens earlier.
- **Collection and evaluation are separate.** Runtime OTLP flows through the collector. `evals` exports only when `EVAL_OTEL_EXPORTER_OTLP_ENDPOINT` is set explicitly and forces child-agent export off.
- **Evaluation is black-box.** ADK REST and A2A events fold into one typed turn. Streaming partials never contribute duplicate usage. Expected domain values come from immutable seed data.
- **Evaluation evidence is sanitized.** Release artifacts and OTel attributes exclude prompts, answers, references, tool payloads, rationales, URLs, credentials, and provider errors.
- **Build identity has one authority.** Linker-owned mode, version, source identity, revision, tree digest, timestamp, and dirty state feed CLI output, AgentCard version, OTel resources, OCI labels, and backup manifests. Runtime environment variables cannot relabel a binary.
- **Dirty work never claims `HEAD`.** Release-bearing source resolution rejects tracked or untracked changes. Development may use `unknown+dirty.<digest>`, with revision empty and the deterministic tree digest recorded separately.
- **Planning is bounded.** The root agent plans multi-step investigations and verifies approved actions. The workflow is plan, investigate, evidence review, recommend; never introduce an unbounded reflection loop.
- **Cost-efficient by default.** Prefer deterministic offline tests, fakes, and the smallest model that materially proves the boundary. Do not start clusters, collectors, model servers, paid APIs, or cloud resources for an offline claim.
- **Go coverage has a floor.** `mise run test` fails when any package in `agents/go` or `evals` drops below 80% line coverage; `scripts/check-coverage.sh` measures it per package, because a repository total hides exactly the package worth worrying about. `cmd/` packages are excluded by kind — they are `package main` composition wiring (flag parsing, dependency construction, process lifecycle) this project has chosen not to hold to the floor, and their coverage is measured like every other package's and simply sits below it. `tools/` is maintainer scaffolding and sits outside the floor; only the accessibility driver is verified by execution rather than unit tests, because it needs a real Chrome, while `internal/conventions` is unit-tested as well as exercised by every `mise run check`, and the same script reports the module's real numbers on demand.

## Open-source boundary

The required path uses Google ADK for Go, agentgateway, kagent, OpenTelemetry, Tempo, Loki, Prometheus, Alertmanager, Grafana, Ollama, the Apache-2.0 Qwen3 open-weight model, and repository code. It requires no account, mandatory SaaS, or usage fee.

Gemini, Vertex AI, GKE, GCS, Artifact Registry, and hosted repository services are optional proprietary substrates. Never call the optional cloud environment fully open source.

Local Qwen3 through Ollama is the stable default:

```text
AGENT_MODEL_PROVIDER=openai-compatible
AGENT_MODEL=qwen3:4b-instruct
OPENAI_BASE_URL=http://127.0.0.1:11434/v1
OPENAI_API_KEY=local-ollama
```

Chapter 5 changes only `OPENAI_BASE_URL` to the agentgateway listener. Native Gemini and GKE/Vertex are comparisons.

The optional GKE path keeps its compatibility-pinned model until `mise run gke:smoke` passes both synthetic tool-result and stable-seed read-only A2A retrieval against a replacement gateway/model pair.

## Pinned contracts

Use locks and manifests as authority, never a number copied into prose:

- Agent dependencies: `agents/go/go.mod` and `agents/go/go.sum`.
- Evaluation dependencies: `evals/go.mod` and `evals/go.sum`.
- Repository-tool dependencies: `tools/go.mod` and `tools/go.sum`.
- Go and cross-repository CLI tools: root `mise.toml` and `mise.lock`.
- Agent build stage: `agents/go/Dockerfile`; its Go version must match every Go module and root mise.
- Hextra: root `go.mod` and `go.sum`; Hugo: root `mise.toml`.
- Self-hosted Mermaid and FlexSearch bundles: `assets/js/vendor/versions.json`. That file is regenerated wholesale by `scripts/vendor-assets.sh`, so the held line and the reason it is held live in that script's header, not in the manifest.
- kagent charts: `infra/helmfile.yaml`; API resources use the pinned version declared there.
- Container images: digest-pinned at their use sites under `infra/` and the agent Dockerfile.
- Workflow-only Buildx: explicit version inputs in the release workflow.
- GKE module provider: `infra/gcp/versions.tf` and `infra/gcp/.terraform.lock.hcl`. No Dependabot ecosystem and no freshness row watch them, so they move only from the quarterly docs-freshness checklist.
- Evaluation inputs: three `evals/*.evalset.json` files and `judge-calibration.json`; the run thresholds are the `eval` task command line in `evals/mise.toml`.

A `// compatibility hold:` comment records a pin that a newer version would break. It names the owner that decides the version, the constraint itself, and the validator that must pass before the pin moves; `check:freshness` walks every `go.mod` and renders the holds as a table, so a ceiling is reviewable rather than remembered.

One hold stands, in `tools/go.mod`: `github.com/chromedp/cdproto` is held at the revision `chromedp` v0.16.0 was built against, and only a real Chrome accessibility acceptance run qualifies a newer one, because nothing but driving a browser proves the CDP command set.

`agents/go/go.mod` carries none. ADK Go v2.3.0 migrated its own logs to the OpenTelemetry 1.45 and log 0.21 family (google/adk-go#1335), which retired the four holds it carried through v2.2.0 — the `openai-go`/`genai` client pair and OTel stable 1.44 with log 0.20 — and `agents/go/telemetry/export.go` moved off the removed `log.Value` and `log.KeyValue` with it. ADK still resolves both clients through minimal version selection, so that pair moves with ADK and never ahead of it.

The validator for an agent-module ceiling is `cd agents/go && mise run check && mise run test`; it must compile ADK and pass the focused telemetry, command, and full race suite before the constraint or prose changes. A newer resolved module is not supported evidence.

Generated result files are transient handoffs. The organization caps artifact and log retention at **7 days**; durable release evidence belongs on an owner-approved immutable release and in OCI attestations.

## Evaluation evidence contract

`evals/mise.toml` is the contract. `mise run eval` writes `results.json`, `mise run eval:judge-calibration` writes `judge-calibration-results.json`, and `mise run eval:ab` writes `prompt-comparison.json`. All three are gitignored, so the evidence a page shows is the evidence a page carries. There is no `release-policy.json` and no automatic release qualifier: the thresholds live on the `eval` task's command line, which is what `content/4. Quality/4.4. Evaluations.md` teaches learners to read, and `checkEvalThresholds` pins the page to that command line.

The judge is scored but never decides a safety case. `evals/score.go` tags a judged verdict `Stochastic`, and `summarizeCases` folds `required_cases_passed` over deterministic scores only — a judged failure costs the run its `pass_rate` and cannot on its own declare a required case failed.

Stable OTel names are:

- Spans: `agentops.eval.run`, `agentops.eval.case`, `agentops.eval.score`.
- Metrics: `agentops.eval.score`, `agentops.eval.case.passed`, `agentops.eval.tokens`, `agentops.eval.model_calls`, `agentops.eval.run.passed`.

Do not change these names or JSON schemas without a compatibility decision and coordinated release-qualifier update.

## Stable network inventory

This file owns the stable network inventory. Repository convention checks map MCP `:3000`, A2A `:3001`, model `:4000`, gateway metrics `:15020`, gateway readiness `:15021`, raw MCP `:8000`, raw A2A `:8080`, kagent control plane `:8083`, web client `:8001`, ADK web `:8002`, docs `:8003`, Ollama `:11434`, Tempo `:3200`, OTLP `:4317` and `:4318`, collector metrics `:8889`, collector health `:13133`, Prometheus `:9090`, Alertmanager `:9093`, Grafana `:3002`, Loki `:3100`, and registry `:5050` to executable owners.

Adding a port requires updating this inventory, the convention checker contract, the executable owner, and `content/0. Overview/0.4. Ecosystem.md`.

## Hugo documentation build

Hugo Extended builds the site with Hextra as a Hugo Module. `mise run serve` previews on `:8003`; `mise run build:docs` writes `site/`.

| Concern                              | Location                                                |
| ------------------------------------ | ------------------------------------------------------- |
| Site configuration and source mounts | `hugo.toml`                                             |
| Learning path                        | `data/nav.yaml` and sidebar partial                     |
| Source include                       | `layouts/_shortcodes/include.html` and include partials |
| Admonitions and collapsibles         | shortcode layouts and custom CSS                        |
| Self-hosted search and diagrams      | `assets/js/vendor/`                                     |
| Search accessibility                 | `assets/js/search-a11y.js`                              |
| Search route index                   | `assets/json/search-data.json`                          |

Four non-default contracts are easy to break:

1. Strict mode combines Hugo `--panicOnWarning`, reference-link errors, and the navigation checker.
1. Every non-home page has an explicit lowercase kebab-case slug; Hugo combines reviewed section slugs and regular-page slugs through the permalink configuration.
1. Includes read through `assets/source/**` mounts so Hugo watches quoted code.
1. The title lives in front matter; never add a second Markdown H1.

This repository publishes the site. `.github/workflows/docs.yml` builds `site/` and its `deploy` job pushes that output to GitHub Pages on every push to `main`, serving <https://agentops-open-course.fmind.dev/>; `static/CNAME` owns the hostname and `baseURL` must agree with it. Exactly one job may hold Pages authority, and `checkPagesDeployment` fails the build otherwise — a second deploying job would race the real one, and a rename that loses the `deploy` job would freeze the live site on its last good build with nothing failing. Treat `content/`, `hugo.toml`, `layouts/`, and `static/` as published surfaces rather than local build inputs.

**Chapter numbering was reviewed on 11 August 2026 and deliberately left as it is.** Renumbering — `7.2b`/`7.3b` into the sequence, `8.7 Capstone` to `8.0`, `1.2` and `1.3` into the chapters they render inside — was free only while nothing is published, and it was considered on that basis. It was declined because the cost is the largest mechanical change available here (83 slug fields, every `relref`, `data/nav.yaml`, and every hard-coded `content/` path in `tools/`) against a benefit that is presentational, and because the numbers that look wrong are load-bearing: `1.2` and `1.3` are cross-chapter prerequisites the navigation states explicitly, and `8.7` first is the correct pedagogical order. Three narrower fixes landed instead: `1.2. Container Engine` and `0.5. Provider Options` were retitled so neither collides with a later page in search, and the 48 never-linked heading anchors were deleted so a copied section link cannot contradict the heading it lands on. That window has closed: the course now serves published URLs, so slugs are frozen. Any route change must record the old address in `data/released-urls.json`, which `checkReleasedRoutes` validates for the permalinks the site serves today as well as for the historical ones — a rename either updates the ledger or fails the build. A page added after v0.7.0 has no old address to record, so it is listed as its own successor; that is what puts it under the ratchet, and a new page adds its permalink there the same way.

## Vocabulary decisions

Four words meant two things each across the corpus. Each was resolved on 13 August 2026 rather than left open, because an unresolved vocabulary conflict is where drift re-enters:

- **`token bucket` keeps its algorithmic meaning in both chapters, and every use names its unit.** Chapter 5's buckets hold requests; `7.3b. Cost Governance`'s holds model tokens. Renaming either would have been wrong — both are token buckets — so 7.3b states the relationship once and neither page says "token bucket" without saying what it counts.
- **`observation` belongs to `0.2. Evidence`**: a measurement that varies between runs. `7.4. Feedback` now says _report_ for the human sentence that starts a case, because the two are different objects and the reader meets the measurement first.
- **`audit row` is the term; `audit record` is retired.** Twenty-two pages already said row. The glossary entry was retitled and keeps its older `audit-record` anchor, because deleting an anchor orphans every deep link that points at it.
- **`artifact` keeps its A2A meaning; ADK's artifact service is declined, not deferred.** The A2A artifact is a task's typed output on the wire. ADK's `artifact.Service` — in-memory and GCS backends, `tool/loadartifactstool`, `{artifact.key}` templating — is file storage for agents, and nothing this course builds needs an agent to hold a file. It is not taught, and the glossary says so at the word rather than leaving the collision for a reader to hit.
- **Chapter 5 requires Chapters 1-4.** The index and `5.0` said 2-4 while `5.1` said 1-4; 5.1 was right, since the chapter's first live command needs the model pulled in `1.4. Providers`. All three now agree.

## Documentation page frame

`content/` holds 84 Markdown pages: 74 course pages, nine chapter `_index.md` files, and the landing `content/_index.md`. Every one of them follows this shape:

```markdown
---
title: "N.M. Title"
description: One sentence.
slug: "n-m-title"
---

{{% admonition abstract "In one glance" %}}

- **You will:** Outcome.
- **You need:** Checkable precondition.
- **Time:** about N minutes, kind. {{% /admonition %}}

## What this section teaches, named as its purpose

Prose that opens on the concept and the reason it exists, a runnable command, and the output that command produced.

## Your turn: do the thing

Predict first: one question the learner answers before running anything.

- **Mode**: `inspect`, `temporary experiment`, `keep`, or `capstone carry-forward`.
- **Goal**: what the learner ends up able to do.
- **Files to touch**: exact paths, or `none`.
- **Preflight**: the command that proves the starting state.
- **Steps**: the ordered work.
- **Gate that proves completion**: the command whose output decides it.
- **Final state**: what stays on disk afterwards.

## What you can do now

- Observable capability.

Continue to [Full page name](link), which does the next thing.
```

Rules:

- **Headings name their teaching purpose.** An H2 names its technical subject and what the section does with it — motivation, definition, mechanism, procedure, boundary, trade-off, or exercise — so it reads as a claim on its own in a sidebar, not a claim in an interview with the paragraph below it. Never a scene ("Ana gets paged at 02:14"), a riddle ("The tool that does not exist"), a quote, or a bare noun that could sit on any page ("Instructions", "Impact"). One interrogative H2 per page is allowed, for the tension the page actually resolves. `0.7. Troubleshooting` and `0.8. Glossary` are the two exceptions, because their headings are the symptoms and terms a reader scans for.
- **A page opens on the concept, not on the example.** The paragraph after the glance block names the term being taught — bolded at first use — says what breaks or cannot be proved without it, and says what the page will show. Only then may the reference agent's incident domain appear, framed explicitly as the worked example. Never open on a clock time, a persona, or a withheld subject meant to create suspense.
- **`You need` declares machine state, never reading history.** Two clauses, in order. Clause one is the machine state the page truly requires from a clean clone, named as the command that produces it (`mise run install` done, Docker running, Ollama serving), and it says what is _not_ needed when that is reassuring ("No model, cluster, or account."). Scope anything heavier to the part that needs it ("a model only for the optional live run"). Clause two is optional and is a back-pointer, never a gate: `Assumed, not required: <concept> from [N.M. Page](relref).` "Chapter N finished" is retired — it invented prerequisites on pages whose commands run from a clean clone, and the reader who arrives from a search reads it as two hours of backtracking. Verify against `mise.toml`, `scripts/doctor.sh`, and the shell scripts before loosening a tier; being wrong in the permissive direction strands a reader mid-command.
- **The incident domain illustrates; it is not the curriculum.** The committed seed (incidents, services, runbooks, logs) exists because no model can have memorised it, which makes a grounded answer checkable against a row — that is the whole reason this domain was chosen, not because incident response is what the course teaches. Where a human role carries the teaching (approval, escalation, paging), name the role rather than a character: "the approving engineer", "whoever is on call". Two checks hold the retired style out: `checkSceneHeadings` fails an H2 carrying a time of day, and `checkRetiredNarrative` fails prose that reintroduces the former persona or retailer brand. Both read outside code fences, so a capture may still print whatever it printed.
- **Reasoning sits beside the instruction, not after it.** Define a term at first use in one clause. Give the reason next to every command, flag, and design choice, rather than leaving the reader to infer it three paragraphs later. Number a sequence whose order is a constraint, not a preference.
- **Keep an anchor whenever a link depends on it.** Two mechanisms carry deep links, and both break silently. Dozens of H2s carry an explicit `{#kebab-slug}`, so rewording one of those headings without preserving its slug orphans every link that points at it. More than a hundred inline `<a id="…"></a>` anchors open a paragraph or a list item, so deleting or merging that paragraph destroys the target even though no heading changed. Check both before rewriting a section, including the links inside `content/` itself.
- **Close on the page's kind.** Course pages end with `## What you can do now`, chapter indexes with `## What this chapter proved`, and the four pure lookup pages — `0.4. Ecosystem`, `0.6. Resources`, `0.7. Troubleshooting`, `0.8. Glossary` — with `## How to use this page later`. The landing page has no closer. Every other page's final paragraph names the next page, usually as a `Continue to [Full page name](link)` line.
- **Closing bullets state capability, not attendance.** `## What you can do now` is the section a skimmer is most likely to read, and the course says plainly that exercises are optional — so a bullet whose truth depends on the reader having typed something tells the skipper they achieved nothing. Write what they can now do, say, name, predict, or point at, keeping the specific noun the page taught: a bullet that could sit on any page is worse than the past-tense one it replaced. Past tense belongs in the exercise's own `Gate that proves completion`, and a bullet reporting a measurement the reader took on their own hardware is honestly personal and stays.
- **An exercise declares all seven fields, in order.** A `## Your turn: …` section carries Mode, Goal, Files to touch, Preflight, Steps, Gate that proves completion, and Final state. A hands-on page states a prediction before its exercise, so the learner commits to an answer before the command supplies one.
- **An optional exercise stays out of the sidebar, and says so in bold.** Three openers start an exercise: a `## Your turn: …` H2 for the ones the page is built around, and the inline `**Exercise:**` / `**Optional exercise:**` labels for the ones a reader should feel free to skip. Inline keeps optional work out of the table of contents, which is the point; the bold label is what lets a skimmer see it is optional without reading the sentence. `exerciseSections` matches all three, bare and bolded, so emphasising a label can never silently drop its seven fields out of checking.
- **Mode is a promise about the working tree.** `inspect` changes nothing. `temporary experiment` requires a target-specific dirty preflight (`git diff --quiet -- <paths>` or `test ! -e <path>`) and target-specific cleanup (`git restore -- <paths>` or `rm -- <path>`); never restore a whole directory, which would discard unrelated learner work. `keep` leaves the change in the diff.
- **A ```text block is a verbatim capture.** Paste what the command printed. Trimming must be declared in the surrounding prose ("trimmed to seven of the twenty-two package lines"); reordering, re-spacing, and hand-edited figures are never allowed. When a capture carries a count or a percentage, that figure must be derivable from a file in this repository, and `tools/internal/conventions` must derive it.
- **Comment density is a deliverable.** Quoted Go carries the rationale comments that explain why a non-obvious choice was made. Remove such a comment only when its rationale stops being true, never to shorten an excerpt.
- **A hands-on page reaches a runnable command within its first two H2 sections.**
- **Use zero to three `{{% collapsible note "Deeper: …" %}}` blocks per page.** Never collapse definitions, commands, expected output, security bounds, cost, or destructive actions.
- **Open each H2 with a concrete sentence of 25 words or fewer.** `checkHeadingOpeners` enforces it, counting what a reader actually reads — shortcodes stripped, links reduced to their label, up to the first full stop — and skipping sections that open on a list, table, fence, or shortcode. The habit that breaks it is systematic: the definition, its appositive gloss, and the enumerated consequences all stack before the first full stop. Split the sentence in two so the definition lands first; a dash or colon only helps when it becomes a full stop. Keep sentences readable and cross-links sparse.
- **Every new or changed Mermaid diagram has adjacent `**Diagram in words:**` prose.** The Mermaid render hook derives each diagram's accessible name from that sentence, and `checkRenderedDiagramNames` fails a page whose diagrams end up sharing one. Override a derived name with a block attribute on the fence, `{ariaLabel="…"}` after the language token and separated from it by a space, rather than by rewriting the prose.
- **Use descriptive full-page link labels and define unfamiliar terms at first use.**
- **Include shortcodes stand alone outside code fences and quote the smallest stable source region.**
- **Never add a front-matter `url` override.** Hugo gives it precedence, so it shadows the reviewed slug and permalink route. Home alone omits `slug`; chapter sections and regular pages require one.
- **Distinguish offline, live model, container, Kubernetes, cloud, destructive, and paid commands** before asking a learner to run them.
- **Do not claim what the repository does not ship:** no dollar-denominated cost panel, no automatic live judge, no external paging integration, and no cryptographically immutable audit store.
- **Pages do not grow.** A rewrite that adds a definition or a reason pays for it by cutting tease, restatement, self-congratulation, and stacked em-dash asides — the corpus is long enough already that a page which only gains words costs every reader who follows.

## Development commands

Root task vocabulary:

```bash
mise run install
mise run install:platform
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
mise run build:docs
mise run serve
```

Agent module vocabulary:

```bash
cd agents/go
mise run check
mise run test
mise run coverage
mise run build
mise run run
mise run workflow
mise run coordinator
mise run web
mise run a2a
mise run mcp
mise run mcp:http
mise run config:check
mise run data:reset
```

Evaluation module vocabulary:

```bash
cd evals
mise run eval:validate
mise run check
mise run test
mise run build
mise run eval
mise run eval:judge-calibration
mise run eval:ab -- --baseline "$BASELINE_ARTIFACT" --candidate "$CANDIDATE_ARTIFACT"
```

Set `BASELINE_ARTIFACT` and `CANDIDATE_ARTIFACT` to reviewed sanitized run files before `eval:ab`. `eval:validate` and artifact-only `eval:ab` are offline; `eval` and `eval:judge-calibration` call a configured generative model and stay outside offline test gates. Every other capability the harness exposes — the A2A transport, the workflow and triage-report evalsets, streaming, schema checks, and groundedness — is a flag on `mise run eval` rather than a task of its own, and `evals/README.md` documents each recipe.

## Local and cloud safety

Host quickstarts use the digest-pinned gateway wrapper. Published listeners bind to loopback. On native Linux, the wrapper owns a bridge-address-only relay so the gateway container can reach host MCP, A2A, and Ollama without exposing those upstreams.

Do not run host observability while in-cluster observability is forwarded on the same ports. No profile creates an Ingress, LoadBalancer, or public application endpoint; clients use temporary port-forwards.

Local Kubernetes starts only from `infra/k3d.yaml`. `mise run platform:dev` resolves the working tree through the source-identity tool; a dirty tree receives `unknown+dirty.<digest>` and no revision. `mise run platform:run` and release workflows require a clean exact revision. Raw Skaffold builds must supply the complete mode/identity/revision/tree/dirty/version/timestamp tuple, and the Dockerfile rejects missing, templated, or inconsistent release inputs.

The GKE path stops at `tofu plan` unless the user explicitly approves deployment. It bills real money. `skaffold delete`, PVC deletion, cluster deletion, `tofu apply`, and `tofu destroy` require exact-context review; cloud apply and destroy require explicit approval.

## Maintainer recipes

- **Add a Go dependency:** change only the owning module, run `go mod tidy`, review both manifest and checksum diff, then run that module's check and test gates.
- **Add a network port:** update the stable inventory, convention contract, executable owner, and ecosystem table.
- **Add a course page:** preserve the page frame, explicit slug, chapter index entry, navigation entry, accessibility prose, and closing contract.
- **Bump a coordinated pin:** update its authority, regenerate lock or digest evidence, search for compatibility copies, and run every affected profile.
- **Change evaluation evidence:** coordinate harness schema, serialization tests, documentation, release qualifier, and workflow consumer in one change.
- **Change state schema:** add forward migration, unknown-future rejection, backup/restore evidence, and rollback notes before changing prose.
- **Cut a release:** bump `VERSION`, `CITATION.cff`, and the dated `CHANGELOG.md` heading together, then re-capture `mise run check:release-metadata` into `content/8. Community/8.2. Releases.md`, which quotes that line verbatim and which no checker derives.

Release evidence is commit-scoped. Freeze the candidate, dispatch evaluation and platform evidence at that exact SHA, then dispatch release with the same SHA and fresh handoffs. The release workflow checks the version and the commit, never whether the hosted gates passed on them, so that wait is yours to hold. Any push creates a new candidate.

## Definition of done

Re-read the request, inspect the scoped diff, and run:

```bash
mise run install:maintainer
mise run format
mise run check
mise run test
mise run scan
```

Also run `mise run check && mise run test` inside each changed Go module.

The complete offline gate must not call a model, collector, cluster, paid API, or cloud service. Local green evidence does not prove hosted CI, deployed runtime, immutable release, or public publication. Never suppress a real failure, weaken a scorer, invent a coverage threshold, or claim an external boundary you did not observe.
