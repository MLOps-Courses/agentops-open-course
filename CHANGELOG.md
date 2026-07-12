# Changelog

All notable changes to the AgentOps Open Course are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-07-12

### Added

- Initial AgentOps course structure, Python Ops Copilot, local dataset, documentation site, and infrastructure examples.
- Local Qwen3/Ollama and optional GKE/Vertex learning paths behind one agentgateway contract.
- Persistent A2A sessions, immutable seed data, disposable runtime state, and append-only action auditing.
- Self-hosted MLflow and OpenTelemetry observability for local and Kubernetes labs.
- Community health files, contribution templates, and end-to-end verification checkpoints.
- Release workflow publishing Trivy-scanned, cosign-signed, SBOM-attested images to GHCR on version tags, with in-workflow verification.
- Self-hosted Renovate dependency updates on a weekly schedule and a documented upgrade playbook for coordinated pins.

### Changed

- Course chapters now distinguish open-source software from optional proprietary model and cloud substrates.
- Gateway, platform, and observability material now tracks runnable repository resources.

[unreleased]: https://github.com/MLOps-Courses/agentops-open-course/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.0
