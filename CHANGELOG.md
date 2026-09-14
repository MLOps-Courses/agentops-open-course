# Changelog

All notable changes to this project are documented in this file.

## [Unreleased]

- Split the course into laptop Python development and advanced Kubernetes platform engineering, with cumulative exercises and separate completion contracts.
- Make ADK with a Gemini API key the default; retain explicit Ollama profiles, offline checks, and an optional LangGraph/A2A comparison. See [provider migration instructions](./SUPPORT.md#migrating-to-the-python-developer-and-platform-course).
- Introduce MLflow after grader calibration, raise live trajectory acceptance to 80% plus critical cases, and attach feedback to the exact response trace.
- Add Gemini gateway and local Kubernetes profiles; update vulnerable dependency resolutions and validate the optional comparison dependency profile.

These are unreleased source changes. Offline evidence does not qualify live Gemini behavior, a deployed Kubernetes profile, or a published course release.

## [0.7.0] - 2026-08-06

### 🚀 Features

- _(operations)_ Add recoverable chaos drills
- _(evals)_ Expose workflow stage evidence

### 🐛 Bug Fixes

- _(observability)_ Unblock chapter 7 drills
- _(agent)_ Enforce runtime safety contracts
- _(infra)_ Harden host and cluster paths

### ♻️ Refactor

- _(tooling)_ Centralize Python source inventory
- _(ci)_ Extract workflow programs and pins

### 📚 Documentation

- Reconcile course claims with runtime
- Guard executable course contracts
- Sharpen course drills and tradeoffs

### 🧪 Testing

- _(agent)_ Strengthen evaluation evidence

## [0.6.0] - 2026-08-01

### 🐛 Bug Fixes

- _(docs)_ Validate rendered links hermetically
- _(docs)_ Bound anonymous publication checks
- Harden pre-v1 qualification evidence

## [0.5.0] - 2026-08-01

### 🚀 Features

- Prepare v0.5.0 release candidate

### 🐛 Bug Fixes

- _(ci)_ Align live workflow contracts
- _(release)_ Refresh upstream contracts (#116)
- _(ci)_ Harden release and evaluation acceptance (#117)
- _(docs)_ Repair online publication gate (#120)
- _(docs)_ Validate publication fragments (#121)
- _(docs)_ Align SBOM release sequence (#122)
- _(docs)_ Document release proof boundaries (#123)
- _(gke)_ Pin proven Vertex tool loop (#124)

### ⚙️ Build & CI

- _(release)_ Validate Docker manifest-list indexes

## [0.3.5] - 2026-07-30

### 🚀 Features

- _(release)_ Establish stable v1 course contract
- _(agent)_ [**breaking**] Attach policy at the App boundary and close four trust defects

### 🐛 Bug Fixes

- _(ci)_ Render overlays with kubectl
- _(scripts)_ Make GKE helpers hermetic
- _(tooling)_ Pin the lockfile platform matrix
- _(platform)_ Require cgroup v2 for k3d
- _(platform)_ Resolve teardown paths from infra
- _(platform)_ Make the cold-laptop path work and gate the claims the course makes
- _(ci)_ Validate canonical A2A interface

### 📚 Documentation

- _(course)_ Order pages by the learner's dependency graph and add a practice loop

## [0.2.0] - 2026-07-30

### 🚀 Features

- _(agent)_ Compact conversation history before each model call
- _(agent)_ Migrate to a2a-sdk 1.x
- _(evals)_ Size the eval gates for the local model, and add the missing exercises
- _(course)_ Complete the pre-v1 learning path
- _(platform)_ Prepare project-neutral GKE delivery

### 🐛 Bug Fixes

- _(ci)_ Lock all 7 platforms in mise.lock so CI's install leaves a clean tree
- _(course)_ Harden pre-v1 evidence contracts
- _(course)_ Constrain specialized evidence paths
- _(evals)_ Make model evidence attributable
- _(course)_ Harden pre-v1 runtime contracts
- _(evals)_ Preserve reproducible cost evidence
- _(evals)_ Require grounded model evidence
- _(evals)_ Gate critical approval trajectories
- _(course)_ Align staged learner navigation
- _(evals)_ Enforce evidence-backed confirmations

### 📚 Documentation

- _(course)_ Sync to v0.1.1, ADK 2.x wording, and fix cross-links
- Rework the course for newcomers and enforce the page contract
- Record the two red scheduled workflows in TODO.md
- Record what the Eval workflow actually measured
- _(course)_ Centralize repeated chapter guidance
- _(course)_ Clarify progressive entry points

### 🧪 Testing

- _(evals)_ Isolate skill discovery case
- _(agent)_ Isolate ADK CLI smoke
- _(evals)_ Record qwen cost baseline
- _(agent)_ Close readiness engine deterministically

### ⚙️ Build & CI

- Surface the offending file when the generated-files check fails
- Replace self-hosted Renovate with Dependabot

### 🧹 Miscellaneous

- _(mise)_ Bump uv 0.11.28 → 0.11.32

## [0.1.1] - 2026-07-24

### 🚀 Features

- _(course)_ Add prompt-lifecycle, cost-regression, incident-response, gateway-resilience, and context-compaction
- _(course)_ Add reliability, governance, evaluation, delivery, and installable skills

### 🐛 Bug Fixes

- _(agent)_ Harden retrieval, memory, evals, and config from pre-launch review
- _(mlflow)_ Pin GitPython >=3.1.54 to clear 8 HIGH advisories

### 📚 Documentation

- _(course)_ Fix reference drift and add targeted diagrams and checkpoints
- Fix reference drift and remove stray snippet artifacts
- _(course)_ Add diagrams, reference tables, and worked examples across chapters
- _(course)_ Add scannability aids, worked examples, and reference tables across all chapters
- _(course)_ Fix callback/eval drift, sharpen onboarding, add MCP/A2A exercises
- _(course)_ Fix pre-launch review findings in chapters and skills

### 🧪 Testing

- _(agent)_ Sync agent-card version assertion to 0.1.1

### 🧹 Miscellaneous

- _(course)_ Update agent, docs, and infra config

## [0.1.0] - 2026-07-16

### 🚀 Features

- Publish AgentOps Open Course

[unreleased]: https://github.com/MLOps-Courses/agentops-open-course/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.7.0
[0.6.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.6.0
[0.5.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.5.0
[0.3.5]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.3.5
[0.2.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.2.0
[0.1.1]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.1
[0.1.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.0
