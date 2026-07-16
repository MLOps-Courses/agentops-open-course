# Changelog

All notable changes to the AgentOps Open Course are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[unreleased]: https://github.com/MLOps-Courses/agentops-open-course/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.0
