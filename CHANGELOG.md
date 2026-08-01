# Changelog

All notable changes to the AgentOps Open Course are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-08-01

### Added

- Added a versioned, hash-verified state snapshot format and one shared state CLI for host and Kubernetes backup, validation, process-serialized crash recovery, and failure-injection drills.
- Added semantic-index provenance for corpus, model artifact, vector dimensions, and chunking format, with serialized generation swaps and safe keyword fallback.
- Added a typed domain fixture and portability contract that exercises a second domain across tools, PII policy, A2A, ADK trajectories, and MLflow evaluation.
- Added deterministic learner-exercise contracts, route and source-drift checks, released-URL preservation, rendered accessibility checks, social metadata, and machine-readable course evidence.
- Added five hermetic dependency-profile audits plus repository, license, observability, and container-image checks shared by local tasks and CI.
- Added quarterly freshness evidence with compatibility-hold metadata, reviewed upstream provenance, and issue-based human follow-up.
- Added exact-candidate Platform and Eval workflows plus release qualification, digest-bound image promotion, attestation verification, and cleanup evidence.
- Added least-privilege state-backup resources, Kubernetes policy fixtures, and isolated GKE deployment and teardown guidance.

### Changed

- Upgraded the supported stack to current compatible stable releases, including Google ADK 2.6, MLflow 3.15, OpenAI 2.51, spaCy 3.8.14, PyArrow 25, SQLite 3.53.4, K3s 1.36.2, and kubeconform 1.36.
- Constrained Python to 3.13 until the locked ADK and GenAI dependency chain supports 3.14 without runtime warnings.
- Split the learner runtime from model-backed evaluation dependencies so the default installation remains smaller and account-free.
- Made course requirements, costs, dependency pins, task expansions, ports, support boundaries, and release procedures derive from or validate against one repository authority.
- Reworked the capstone around the shipped portability seam and an evidence matrix that distinguishes deterministic proof from learner-owned domain outcomes.
- Restricted GitHub Actions to SHA-pinned, explicitly allowed actions and strengthened branch, tag, environment, CodeQL, Dependabot, secret-scanning, and immutable-release controls.
- Made the optional GCP path use a clean committed source revision, isolated credentials and Kubernetes context, explicit resource inventories, and a plan-before-destroy teardown.
- Made model-backed release evidence retain a whitelisted six-signal verdict, exact run identity, model digest, and scorer versions without prompts, responses, or tool data.

### Fixed

- Compatibility-pinned the optional GKE backend to Gemini 3.5 Flash and added a live two-step tool/A2A smoke after the pinned gateway's Vertex function-response filler exposed a Gemini 3.6 failure.
- Rejected future audit schemas before readiness, reads, migrations, or writes and made failed migrations and restores preserve byte-identical input state.
- Prevented stale or partially rebuilt semantic vectors from being presented as current after corpus, model, dimension, or chunker changes.
- Rejected embedding-model alias changes around query or corpus generation before publishing or querying a mismatched vector generation.
- Replaced probabilistic required exercises and broad directory restores with deterministic offline checks, dirty-worktree preflights, and named-file cleanup.
- Closed documentation drift across dependency versions, tool profiles, CLI expansions, route ordering, setup tiers, retrieval behavior, and release evidence.
- Made CI concurrency, permissions, job scopes, diagnostics, and scheduled evidence explicit while keeping all required pull-request gates deterministic.
- Restored App-level policy parity in ADK and MLflow evaluation, made recall, skill loading, and both approval proposals individually required, and prevented failed critical transcripts from becoming cost baselines.
- Upgraded scheduled model evidence to Ollama 0.32.5 with its Linux runner fix, and made a real first inference fail fast while retaining the server diagnostic log.
- Hardened Platform acceptance around isolated DNS, MLflow's measured 2 GiB ceiling, PII-stable backup evidence, fail-fast restore Jobs, and sanitized container diagnostics.
- Made release lookup, run qualification, SPDX attestation matching, image cleanup, and package-index publication retry-safe and fail-closed.
- Made GKE delivery authenticate Docker explicitly, select the exact cluster context, publish only a clean source SHA, and document complete application and infrastructure cleanup.
- Made optional GKE cleanup capture exact CSI handles across both course namespaces, accept a valid empty partial-deploy inventory, and restore only APIs enabled by that lab.
- Made a failed host-observability readiness check tear down its project-scoped containers while preserving named volumes.
- Made the online publication gate resolve root-relative site links, check static anchors, bound request concurrency, isolate LinkedIn's documented bot-blocked profile, and use responsive upstream authorities.
- Corrected the container, release, and publication lessons to match the exact-archive handoff, pre-push SBOM, source-digest evidence, public-index sealing, proof boundaries, and residual-artifact boundary.

### Security

- Tightened Kubernetes network policies to exact callers and ports, plus backup-job privileges, non-root images, read-only filesystems, capabilities, seccomp, service-account tokens, and state access.
- Added complete lockfile, history, configuration, container, license, and vulnerability scanning without relying on mutable user-level tool configuration.
- Kept GitHub secret scanning and push protection enabled; repository-plan-dependent advanced validity and non-provider checks remain outside the claimed contract.

### Migration and rollback

- Run `mise run install:maintainer`, then the complete `format`, `check`, `test`, `scan`, and `build` gates after updating an existing checkout.
- Existing runtime state is migrated through the versioned state boundary. Take and validate a snapshot before upgrading; future schemas fail closed.
- Roll back source and images together to the previous supported release, then restore only a snapshot whose manifest and schema are accepted by that release.
- The GCP module remains plan-first and project-neutral. Review the saved destroy plan and exact resource inventory before removing an optional lab.

## [0.3.5] - 2026-07-30

### Added

- Attached every cross-cutting policy — token budget, history compaction, PII redaction, write validation, tool-output hardening, and safe errors — to one `AgentOpsPolicyPlugin` on an ADK `App`, replacing nine copy-pasted per-agent callback blocks so a new agent is governed by construction rather than by review.
- Added behavioral coverage for that policy chain through a real runner and a scripted model double, replacing identity assertions that proved the wiring existed but never that it ran.
- Added one required `## Your turn` drill per chapter, gated in the chapter checkpoint, plus cross-chapter recall bullets — the middle rung between reading a finished reference and the open-ended capstone.
- Added the missing concept units: the real tool-calling payload, ADK session state and `MemoryService`, MCP sampling/elicitation/roots and revision negotiation, delegated authority and the confused-deputy problem, structured output as a serving-layer constraint, and sandboxed code execution.
- Added a snippet gate: a hand-written Python block in Chapters 2-3 must now either include the real source or declare itself, with the existing debt recorded as a ceiling that may only go down.
- Added a separate `eval` dependency group and `install:eval`, cutting the first-run agent environment from 1.2 GB to 738 MB by deferring the MLflow evaluation stack to the chapter that uses it.
- Published the support, compatibility, deprecation, Linux x86_64 qualification, upgrade, rollback, maintenance, and explicit non-goal contracts for the software surfaces.
- Added first-class A2A task cancellation across the server and dependency-free web client, including persistent terminal state and deterministic protocol coverage.
- Documented the WCAG-oriented audit and repaired search, palette, code, table, form, landmark, live-region, focus, reflow, reduced-motion, and forced-color accessibility defects.

### Changed

- Split the stability contract by payload: the configuration, environment, port, state, audit, MCP, A2A, image, and Kubernetes interfaces stay versioned, while course pages may be reordered, renamed, split, or rewritten in any release.
- Ungated Chapter 7 from Kubernetes — seven of its eight pages need only Docker — and moved human confirmation and the write-plus-audit transaction into the tools chapter that introduces the write tools, removing the largest block of forward references in the course.
- Named one owner for dependency reliability, so a guardrail stays policy at a trust boundary rather than a mixture of policy and retry mechanics.
- Upgraded the pinned toolchain to current stable: uv 0.12.0, agentgateway 1.4.1, kagent charts 0.9.12, Zensical 0.0.52, OpenTelemetry Collector 0.157.0, Loki 3.7.4, Prometheus v3.13.2, Grafana 13.1.1, and the Google OpenTofu provider 7.42.0. The MCP client is deliberately held below 2.0 to match the locked ADK's declared compatibility bound.
- Replaced hand-copied version pins in prose with pointers to the lock or manifest that owns each one, and derived the A2A card version assertion from installed metadata instead of a literal.
- Moved the documentation preview to `:8003` and the ADK developer UI to `:8002`, ending a three-way collision on `:8000` that served a learner the course website when they expected the agent.
- Made the pre-commit gate globbed and offline-capable; a dependency audit is now a function of the lockfile and runs in the maintainer gate and CI, not on every commit.

### Fixed

- Closed a delimiter-forgery hole in the spotlight fence: attacker-controlled text could close the data block and reopen it, placing its own content outside the data-marked region. Forged markers are now neutralized and counted.
- Keyed the single trusted-instruction carve-out on the ADK skill-tool type rather than the tool's name, so a tool served by a remote MCP server can no longer inherit the injection-neutralization bypass by calling itself `load_skill`.
- Stopped PII redaction from corrupting internal hostnames mid-token, which fed the model plausible-looking but mangled evidence; the boundary and persisted policies now agree.
- Pinned the MCP tool allowlist on both transports, so a server cannot widen the agent's surface — or reach the model with new tool-description text — by registering a tool.
- Surfaced the repository's own typed failures (circuit open, tool deadline, data access) to the caller instead of collapsing every error into an opaque message.
- Set the local cluster to a single node: three workloads share one `ReadWriteOnce` volume, so a second schedulable node made the platform chapter an intermittent scheduling failure.
- Bound the host gateway's loopback relay to a dedicated Docker network instead of the shared default bridge, which had exposed the learner's MCP, A2A, and model ports to every other container on the machine.
- Gated the three-profile gateway contract, including the tool allowlist against the tools the MCP server actually registers, instead of asking the learner to compare three files by eye.
- Gave the in-cluster gateway a real HTTP readiness probe and corrected the reasoning that had defended a bare TCP check.
- Made the doctor name the install task for each missing tool and cover the binaries its own tier actually calls.
- Merged the two divergent `.env.example` files: the learner-facing copy was the ungated one and was missing the kill switch, the identity header, and the whole circuit breaker.
- Removed the redundant standalone Kustomize binary, rendered overlays through pinned kubectl, and made the tool lock and GKE scripts independent of user-level mise and ShellCheck configuration.
- Kept the maintainer installer scoped to repository tools instead of letting a bare `mise install` resolve every tool from the user's global configuration.
- Aligned the agent's `uv-build` backend range with the pinned uv 0.12 tool, removing a warning from wheel and container builds.
- Updated the host smoke and gateway lesson to validate the canonical A2A 1.x `supportedInterfaces` URL after agentgateway rewrites it, and print preserved diagnostics when the smoke fails.
- Made the platform doctor and cluster startup reject cgroup v1 before pinned Kubernetes 1.35 can leave a partial k3d cluster.

## [0.2.0] - 2026-07-30

### Added

- Added a bounded read-only `plan → investigate → evidence_review → recommend` workflow, a least-privilege coordinator path, and model-backed evaluation for the planning and reflection loop.
- Added explicit learner, platform, and maintainer install tiers plus stable aggregate CI and security-scan jobs that import-smoke every built image.
- Added explicit accessibility and single-maintainer governance contracts for diagrams, keyboard/contrast expectations, review authority, and the path to maintainership.
- Added a repository-wide `TODO.md` that defines the OSS, course, runtime, security, accessibility, maintenance, and release evidence required before v1.0.0.
- Added project-neutral GKE render and deployment helpers, a balanced persistent-disk storage class, and output-driven Workload Identity manifests for the optional GCP lab.

### Changed

- Reworked the learning path so setup stays read-only, the first model interaction happens in Chapter 2, Kubernetes deployment precedes inspection, and the capstone is the primary finish before optional project maintenance.
- Unified ADK discovery behind one validated `AGENT_ENTRYPOINT=agent|workflow|coordinator` package boundary while constructing only the selected composition.
- Made promotion a truthful offline preflight by default, with model-backed evidence required before it prints deploy and rollback commands.
- Bound scheduled model evidence to one provider, immutable prompt, model digest, evaluation contract, source revision, serving context, and sampling configuration while reusing the exact MLflow transcript for required cost and groundedness verdicts.
- Made skill loading and both guarded-action confirmation trajectories strict named Qwen gates, so an aggregate pass rate cannot hide a failed safety contract.
- Made the optional GCP module and GKE delivery path variable-driven, quota-aware, and cheaper by default while preserving explicit plan, verification, and teardown boundaries.

### Fixed

- Repaired the locked ADK 2.4 terminal entrypoint, which was shadowed by `agent.py`, and added real CLI, wheel, and container discovery coverage.
- Wrapped `adk eval` so metric failures cannot exit successfully; each trajectory case is strict and the measured local-model baseline uses an explicit aggregate case-pass floor.
- Strengthened host and load smoke tests to require successful A2A completion, made OpenTelemetry provider setup idempotent, preserved evidence across workflow nodes, and required fresh reads after approved actions.
- Made approved-write replays idempotent in SQLite, bounded model-controlled runbook retrieval, serialized per-session token accounting, and protected the optional circuit-breaker registry and generation-bound transitions across worker threads.

## [0.1.1] - 2026-07-24

### Changed

- Renamed the reference agent from "Ops Copilot" to "AgentOps Agent", aligning the application identity (`OTEL_SERVICE_NAME`, MLflow experiment and prompt registry, ADK `app_name`, MCP server, audit actor, gateway backend) with the `agentops-agent` name the container image, Kubernetes workload, and Python distribution already used.
- Promoted the default local model to `qwen3:4b-instruct` (Qwen3 4B Instruct 2507) for stronger tool calling at the same 2.5 GB footprint, and documented `gemma4:e4b` as an optional, heavier Apache-2.0 alternative.
- Workflow sub-agents now enforce the same per-session token budget as every other agent (`enforce_token_budget`/`record_token_usage`).

### Fixed

- Corrected the front matter on the Overview, Quality, and Observability chapter indexes, where an unquoted colon made the YAML invalid and published the page description as a heading. `scripts/check-docs.sh` now parses front matter instead of pattern-matching it.
- Made semantic runbook indexing atomic — a `BEGIN IMMEDIATE` rebuild — so two concurrent first-use turns can no longer race a parallel index drop/create.
- Long-term memory now surfaces the same `DataAccessError` boundary as the primary data layer instead of leaking a raw SQLite driver error.
- The prompt A/B evaluator reads a marked child-output line and re-raises the child's stderr on failure, instead of assuming its scores are the last line printed and swallowing the cause.
- `mise run config:check` now names the offending field on a validation error, and `eval:cost` reports an actionable message for a malformed `AGENT_COST_TOLERANCE` rather than an uncaught `ValueError`.
- Corrected the container build-stage count (three, not two), the `ObservabilityCollectorDown` runbook cross-reference, the Observability chapter description and incident loop, and several command-directory and cross-link notes across the course.
- Aligned the `agent-guardrails` skill's kill-switch variable name to `AGENT_WRITES_DISABLED`, converted the `agentops-course` skill's cross-links to GitHub-rendering Markdown, and clarified the source-path roots in the installable skills.

## [0.1.0] - 2026-07-16

### Added

- Source-synchronized course excerpts, staged prerequisite doctors, and a scored capstone for adapting the completed reference platform.
- A deterministic host smoke that proves the fake-model, MCP, A2A, CORS, readiness, host/container metrics, and cleanup contracts without a provider account.
- Machine-verifiable repository, Python dependency, and container-image license gates.
- A real streamed A2A approval round trip plus full-conversation MLflow scoring for exact write policy, response facts, terminal confirmation pauses, and isolated state.
- Initial AgentOps course structure, Python Ops Copilot, local dataset, documentation site, and infrastructure examples.
- Local Qwen3/Ollama and optional GKE/Vertex learning paths behind one agentgateway contract.
- Persistent A2A sessions, immutable seed data, disposable runtime state, and append-only action auditing.
- Self-hosted MLflow and OpenTelemetry observability for local and Kubernetes labs.
- Community health files, contribution templates, and end-to-end verification checkpoints.
- Release workflow publishing Trivy-scanned, cosign-signed, SBOM-attested images to GHCR on version tags, with in-workflow verification.
- Self-hosted Renovate dependency updates on a weekly schedule and a documented upgrade playbook for coordinated pins.

### Changed

- Local Qwen3/Ollama is now the default first model path; Gemini, Vertex AI, GKE, and hosted publication remain explicit optional integrations.
- Model-provider selection is independent from direct-versus-gateway topology, and live dotenv values are scoped away from offline gates.
- The Python runtime dependency set no longer installs the unused cloud-database extra.
- SQLite backups now publish atomically after complete integrity checks, and restore paths reject incomplete snapshots.
- Scheduled evaluation installs the exact checksum-verified Ollama release asset instead of a removed archive path.
- Required Helm plugin installation and both Dockerfile frontends now use immutable reviewed source/digest pins; helm-diff platform assets are checksum-verified.
- Release metadata and the pushed `v` tag must agree before any image build or publication.
- Course chapters distinguish open-source software from optional proprietary model and cloud substrates.
- Gateway, platform, and observability material tracks runnable repository resources.

### Security

- Guarded actions now fail closed without confirmed, attributable approval and a bounded rationale; persistence redacts PII/credentials and reads current context inside the write transaction.
- Host gateway tasks use a digest-pinned, non-root, loopback-published container with a bridge-only relay for loopback upstreams.
- Kubernetes denies direct A2A ingress except from agentgateway, mounts shared state read-only in read/backup workloads, and disables unused service-account tokens.
- OTLP log export uses one trace-correlated handler that redacts and bounds copied records without mutating local console logs.
- Untrusted tool-output sanitization is enabled by default.
- Release publishing now pushes and signs the exact local image that passed the pre-push scan instead of rebuilding it.

[unreleased]: https://github.com/MLOps-Courses/agentops-open-course/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.5.0
[0.3.5]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.3.5
[0.2.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.2.0
[0.1.1]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.1
[0.1.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.0
