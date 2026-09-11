# Changelog

All notable changes to this project are documented in this file.

## [0.10.0] - 2026-09-11

A currency release. The reference agent moves to ADK Go v2.3.0, which retires the four compatibility ceilings the course had been teaching around; five dependency advisories leave the shipped modules; and the freshness reporter that was supposed to catch all of this stops answering its own question with the pin it was asked about.

Six pins are deliberately not moved, each with its reason recorded beside it: the Go toolchain stays on the 1.26 line, `kubectl` on 1.36.3, agentgateway on 1.4.1, k3s and the Ollama evaluation archive on their existing evidence holds, and the vendored Mermaid bundle on 11.x. The first five surface as `REVIEW` rows in the quarterly freshness report on purpose; Mermaid is outside that report and its reason lives in `scripts/vendor-assets.sh`.

### 🚀 Features

- _(agent)_ Upgrade Google ADK for Go to v2.3.0 and `a2a-go` to v2.5.0, retiring all four transitive compatibility holds
- _(platform)_ Take the kagent chart to v0.10.1, the GA release the previous pin was waiting on
- _(site)_ Give every Mermaid diagram its own accessible name, derived from the prose beside it
- _(site)_ Publish the ten RSS feeds with items in them, and `llms.txt` with each page's own description

### 🐛 Bug Fixes

- _(tools)_ Ask mise for the newest version instead of whether the pin is satisfied, which made all 30 tool rows report CURRENT against themselves
- _(scripts)_ Stop `test-lib.sh` committing its throwaway repository into the contributor's checkout when a git hook exports `GIT_DIR`
- _(tools)_ Resolve source identity against the directory it is given rather than the repository a hook exported, which also unbreaks `mise run test` on pre-push
- _(evals)_ Wait out `ETXTBSY` when executing a binary the harness has just written, instead of failing a correct run
- _(site)_ Serve `6. Platform/6.7. Promotion and Rollback.html`, a published URL that had been a live 404, and forward the fragment through every historical alias
- _(agent)_ Move the MCP read allowlist onto `tool.FilterToolset`, the spelling ADK v2.3.0 deprecated `mcptoolset.Config.ToolFilter` in favor of
- _(platform)_ Declare both security contexts on the declarative kagent exercise, which restricted Pod Security rejected
- _(platform)_ Stop the `scale` overlay pinning `spec.replicas` beside its own autoscaler, which snapped the read plane back to two on every apply; `minReplicas` is now the only floor, at the cost of one pod until the first reconcile

### 🔒 Security

- _(deps)_ Upgrade `golang.org/x/mod` past CVE-2026-56864 and CVE-2026-56865, and `google.golang.org/grpc` past CVE-2026-84304 and CVE-2026-84445
- _(deps)_ Carry `golang.org/x/crypto` past CVE-2026-56854 into the shipped image
- _(ci)_ Take both container bases and fourteen pinned tools to their current releases, and the Go toolchain to 1.26.8, the newest patch of the line the four modules and the build stage share

### 🔒 Gates

- _(infra)_ Render, schema-validate and lint the `scale` overlay, which Chapter 6.9 claimed as proof and no gate ran
- _(tools)_ Hold `static/CNAME`, `baseURL` and the served `robots.txt` to one hostname
- _(tools)_ Hold both sides of a pre-Hugo rename to the published-route ratchet, not only the spelling it replaced — which is how 6.7's live 404 passed every gate
- _(tools)_ Prove the `ETXTBSY` retry and the mise freshness resolution with tests that fail without them

### 📚 Documentation

- Correct three product claims the course taught as impossible: per-key gateway budgets, MCP server elicitation, and A2A push notifications
- Rewrite `8.8. From Python` around the ADK-Python course this repository actually shipped, and record that MLflow was removed rather than renamed
- Regenerate every drifted capture: test counts, coverage figures, release metadata, and the ADK and chart versions quoted in prose
- Say what a learner's machine truly needs on the pages whose prerequisites over- or under-stated it
- Give the signing chain a verification command, and say what verifying it does not prove
- Say plainly that the release workflow checks the version and the commit but never whether the hosted gates passed on them, so that wait stays a human step

## [0.9.1] - 2026-08-16

A full-repository review, and the corrections it earned. The course now says what the code does, the learner gate installs what it needs, and eight checks that could not fail can fail again.

### 🐛 Bug Fixes

- _(tooling)_ Install ripgrep in the learner tier, which `check:core` required and no install step supplied
- _(agent)_ Mask the uncovered tail of a staggered PII span instead of forwarding it to the model
- _(agent)_ Honor the per-tool deadline in the filesystem-backed reads, and say what it cannot preempt
- _(evals)_ Score a declined confirmation as a failed case instead of aborting the run and discarding its evidence
- _(evals)_ Keep the stochastic judge out of the verdict a field named `deterministic_pass` reports
- _(observability)_ Plot the error ratio the alert actually tests, instead of one floored at a rate no lab reaches
- _(docs)_ Credit the course rather than the theme in the footer of every published page
- _(ci)_ Stop Dependabot opening pull requests the compatibility holds are designed to reject
- _(ci)_ Build the loopback relay the host gateway needs, so the evidence run reaches its first model call
- _(ci)_ Capture the Ollama version as one line, which `$GITHUB_ENV` rejected as two and which killed the evidence run
- _(gateway)_ Admit the judge's chat-completions shape on the governed model route, which `.env.example` already told readers to use

### 🔒 Gates

- _(tools)_ Close the fence-length hole that disabled a page's remaining prose checks after a nested block
- _(tools)_ Compare the doctor tiers against the arrays the script declares, and report a name it no longer has
- _(tools)_ Read both workflow extensions, walk the corpus that exists, and grade retention per upload step
- _(tools)_ Validate the routes the site serves today, not only the addresses it retired
- _(evals)_ Refuse to let a truncated run report itself as a passing one

### 📚 Documentation

- Replace the coverage-floor rationale with the true one in all six places that stated it
- Record that the site is published from this repository, and that its slugs are now frozen
- Correct the surfaces that cannot complete a guarded write, and name the one that can
- State the platform support tiers and the unverified Kubernetes path where the path is chosen
- Give every Agent Skill a source and an install command

## [0.9.0] - 2026-08-16

The course is now taught in Go. The complete Python course is preserved on the `python` branch and every URL it published still resolves.

### 🚀 Features

- Rewrite the course, the reference agent, and the evaluation harness in Go on ADK Go
- _(evals)_ Add a standalone wire-only evaluator that reaches the agent over REST and A2A
- _(observability)_ Replace MLflow with Grafana Tempo, Loki, and Alertmanager
- _(content)_ Add streaming, incident run, platform operations, scale out, alerting, cost governance, and a Python-to-Go bridge
- _(docs)_ Publish with Hugo and Hextra, with dark mode and no external asset hosts

### 🐛 Bug Fixes

- _(docs)_ Preserve all 76 previously published URLs as aliases, gated by a released-URL ledger

### ♻️ Refactor

- _(ci)_ Pin Pages deployment authority to exactly one job

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

[unreleased]: https://github.com/MLOps-Courses/agentops-open-course/compare/v0.10.0...HEAD
[0.10.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.10.0
[0.9.1]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.9.1
[0.9.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.9.0
[0.7.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.7.0
[0.6.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.6.0
[0.5.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.5.0
[0.3.5]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.3.5
[0.2.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.2.0
[0.1.1]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.1
[0.1.0]: https://github.com/MLOps-Courses/agentops-open-course/releases/tag/v0.1.0
