# Support

This policy says which surfaces are stable, where the complete path is verified, and how to upgrade or roll back. The course is pre-1.0: the software contracts below are versioned and change deliberately, while the course prose is still being improved release by release.

## What does the course support?

The course has a Python developer part and a more demanding Kubernetes platform engineering part. Python knowledge is assumed in Part I; container and Kubernetes knowledge is assumed in Part II. Preparation links live in Chapter 0.

The default model path is hosted Gemini with conditional free-tier access. The software stack is open source; the Gemini service is proprietary. Ollama/Qwen3 is an optional local alternative, and offline workshop checks require no model. The supported outcomes from a clean checkout are:

- the offline course, agent, security, and infrastructure gates;
- the conversational agent, bounded workflow, and coordinator with configured Gemini or explicitly selected Qwen3/Ollama;
- the six read tools, repository Agent Skills, MCP, guarded writes, memory, and A2A;
- the host agentgateway path and the local k3d platform;
- self-hosted MLflow, OpenTelemetry, Prometheus, Grafana, and Loki;
- state backup and restore, deterministic adversarial tests, model evaluation, and load testing.

The complete release gate is verified on Linux x86_64 with cgroup v2. CI uses Ubuntu 24.04; the maintainer gate also runs on Debian 12. Kubernetes removed cgroup v1 support in 1.35, and `mise run doctor:platform` checks the required cgroup v2 hierarchy before creating the pinned cluster.

This table is the course's single capacity-planning authority. **Total RAM** is installed physical memory; **available RAM** is what the operating system can allocate now; **free disk** is unused filesystem capacity. Values are binary GiB, not decimal GB. “Not measured” means the course has no honest minimum yet: pass the named doctor and gate instead of treating a guess as a requirement.

<!-- local-platform-capacity: total-ram-gib=14 free-disk-gib=15 -->

| Work tier               | Install/profile                                   | Capacity contract                                                                                                                                                                                                                                   |
| ----------------------- | ------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Read the course         | No install                                        | No measured minimum beyond a browser or Markdown reader.                                                                                                                                                                                            |
| Offline engineering     | `install`; `doctor`; `check:core`; `test`         | No measured RAM or disk minimum. The two locked Python environments and repository checkout must fit; the gates are authoritative.                                                                                                                  |
| Local model             | Offline tier plus `doctor:model`                  | No measured host minimum. The Qwen3 download and runtime consume additional disk and available RAM; model speed is hardware-dependent.                                                                                                              |
| Host gateway            | Contributor tier plus `doctor:gateway`            | No separate measured minimum. A working container engine and enough available RAM for the agent and gateway are required; optional Ollama adds its own model footprint.                                                                             |
| Complete local platform | `install:platform`; `doctor:platform`             | Conservative planning value: **14 GiB total RAM** and **15 GiB free disk** for the optional Ollama profile, images, one k3d cluster, and observability running at once. The Gemini profile has no local model footprint; its minimum is unmeasured. |
| Optional GKE laboratory | `install:gcp`; `doctor:gcp`; reviewed `tofu plan` | Local capacity is not the cloud quota. The canonical billable resource shape and dated estimate live only in [7.3. Costs](./docs/7.%20Observability/7.3.%20Costs.md).                                                                               |

The local-platform numbers are conservative planning values, not measured minima or performance guarantees. `doctor:*` can verify tools, services, credentials, cgroup mode, and some free-disk boundaries; portable measurement of “enough available RAM” is not reliable across supported systems, so it does not pretend to certify that property.

macOS, Linux arm64, and WSL2 remain best-effort. Their lock entries keep tool installation reproducible, but they are not release-gated through the full container, loopback-relay, and k3d journey. Closed [issue #111](https://github.com/MLOps-Courses/agentops-open-course/issues/111) records the evidence that would be needed before expanding that matrix. Report platform-specific defects, but do not infer full-platform support from a successful `mise run install`.

The admitted interpreter is CPython 3.13.x only: every Python project declares `>=3.13,<3.14`, ty checks 3.13, and CI plus both runtime images use that line. Python 3.14 is explicitly unsupported until the locked ADK/GenAI dependency chain imports warning-free and the full agent boundary suite passes on it. A newer interpreter existing upstream is not a support claim.

## Which interfaces are stable?

Stability is split by payload, because a course page and a database schema fail differently.

**Software contracts** are versioned and change only as described below:

- the `agent` Python distribution, its package-level `root_agent`, and the `agent`, `workflow`, and `coordinator` `AGENT_ENTRYPOINT` values;
- documented environment variables, defaults, validation rules, and the network ports listed in `AGENTS.md`;
- immutable seed data, writable-state separation, SQLite schema versions, append-only audit behavior, and backup snapshot format;
- the documented six read-only MCP tools, the guarded in-process write tools, and the A2A task, streaming, approval, reconnect, persistence, cancellation, and error behavior;
- the host, local k3d, and optional GKE configuration shapes; Kubernetes resource names; image names; and versioned GHCR tags.

**Course prose** gets exactly one guarantee: published course URLs are never left to 404. Chapters may be reordered, pages renamed, split, merged, or rewritten in any release, and a moved page keeps a redirect from its previous URL. Pedagogy improves faster than schemas do, and freezing page names would only protect the wrong thing.

Internal Python modules, test helpers, generated HTML, unversioned Git commits, and upstream implementation details are not public APIs. Upstream MCP, A2A, ADK, kagent, and agentgateway contracts remain pinned dependencies; this repository adapts incompatible upstream changes before changing its own documented surface.

## How are compatibility and deprecations handled?

Patch releases fix defects without intentionally breaking a stable software contract. Minor releases may add backward-compatible configuration, tools, schema fields, or manifests, and may restructure course pages. While the project is pre-1.0, a breaking change to a software contract requires a minor release and a documented migration; after 1.0 it will require a major release.

When practical, a deprecated interface remains functional for at least the next minor release. Its replacement and removal target appear in the changelog and at the use site. Security fixes may remove an unsafe interface sooner; the release notes then explain the risk and migration. Migrations must be atomic, repeatable, restore-tested, and reject unknown future schema versions.

Versioned container tags are immutable. Deploy by digest when exact bytes matter; the project does not publish a floating `latest` deployment contract.

## How do I upgrade between supported releases?

The target release prepares runtime state at the normal writable startup boundary. Seed data is never migrated. Version-specific schema or configuration changes belong in that target's changelog entry, so this procedure remains valid after the next release.

1. Stop the agent, MCP, A2A, gateway, and platform processes that can write state.
1. On the currently running supported release, run `mise run state:backup` and keep the completed snapshot outside the working tree.
1. Record that release's Git tag, deployed image digests, and configuration.
1. Read every changelog entry between the recorded release and the target release, including its migration and rollback notes.
1. Check out the target release, then run `mise run install` and `mise run check:core`.
1. Start one writable agent or A2A process and run the shortest incident read before restoring normal traffic.
1. Run `mise run state:drill` or the Chapter 6 Kubernetes restore drill before treating the upgrade as accepted.

To roll back, stop writers, return to the recorded release tag or image digests, restore its configuration, and restore the pre-upgrade snapshot with `mise run state:restore -- <snapshot>`. Do not open a migrated database with older code unless the target release notes explicitly declare that downgrade safe.

## What must a production owner add?

The optional GKE path is a low-cost, production-shaped laboratory, not a production environment. This table is the explicit delta; none of these controls is implied by a green lab.

| Concern                        | Course laboratory                         | Production owner must define and prove                                  |
| ------------------------------ | ----------------------------------------- | ----------------------------------------------------------------------- |
| Availability                   | One zonal Spot node; single replicas      | Regional/multi-zone failure domains, disruption budgets, and HA stores  |
| Public access                  | Port-forwards; no public application edge | TLS, identity, authorization, abuse controls, and edge threat model     |
| Backup and disaster recovery   | Same-cluster PVC snapshot drill           | Off-cluster encrypted copies, restore objectives, and recurring drills  |
| Retention and subject requests | Local retention; no unified subject API   | Retention schedule plus authenticated discovery/export/erasure workflow |
| Storage protection             | Cost-first defaults                       | Deletion protection, bucket versioning/soft deletion, and key ownership |
| Network                        | Public control-plane/cloud service paths  | Private nodes, controlled egress/NAT/proxy, DNS policy, and audit       |
| Capacity                       | Fixed resource bounds and one replica     | Load-derived autoscaling, quotas, capacity tests, and failure budgets   |
| Reliability ownership          | Demonstration alerts and local evidence   | SLOs, paging routes, escalation, runbooks, and accountable owners       |

The course deliberately excludes a model-callable or public subject-data administration endpoint. A production operator who promises coordinated subject discovery, export, or erasure must build it outside the agent tool surface, authenticated and dry-run-first, while recording legal retention exceptions and an administrative audit trail.

## What is outside the contract?

- Live GCP deployment, Vertex calls, cloud cost acceptance, public TLS/auth, high availability, disaster recovery, and production operations.
- Completing the capstone in a maintainer-chosen domain. The capstone is intentionally the learner's adaptation and is scored by its published rubric; independent learner validation remains external evidence.
- Full-path release qualification on macOS, Linux arm64, or WSL2.
- Formal WCAG conformance certification or an exhaustive assistive-technology matrix.
- PDF and ebook publication. Repository Markdown is the accessible offline fallback; another format requires independent learner and accessibility evidence first.
- Coordinated subject discovery, export, or erasure across long-term notes, ADK sessions, A2A tasks, and MLflow.
- Compatibility for forks, local patches, mutable upstream tags, or unsupported dependency combinations.

## How long is a release supported?

The latest release and `main` receive security and correctness fixes. Older releases do not. This file owns that window: `SECURITY.md` and `GOVERNANCE.md` link here rather than restating it.

Dependency updates and security triage are best effort from a single maintainer — typically within a week, faster for an exploitable finding with a published fix. The maintainer targets small patch releases as fixes accumulate and a reviewed minor release when a capability is ready. If the project cannot be maintained safely for six months, the maintainer will announce archival, disable unsupported publication workflows, and seek a successor under `GOVERNANCE.md`.

Use [SECURITY.md](./SECURITY.md) for vulnerabilities, [ACCESSIBILITY.md](./ACCESSIBILITY.md) for accessibility barriers, and a public issue for other support requests.

## Learner and reference boundaries

`install:learner` installs the locked runtime for cumulative exercises; contributor and maintainer gates use their own larger tiers. `learning/` holds learner-owned files, is ignored by Git, and is never removed by the runtime reset task. Learners should back it up or version it in their own repository.

The small workshop demonstrates patterns. The full reference owns persistent A2A state, audit, privacy, and recovery; the platform manifests deploy that reference. The LangGraph comparison is an optional, read-only A2A exercise and is not a second supported production implementation.

`local-gemini` is the main learner platform profile. The original `local` profile remains the Ollama/fake path used by account-free platform CI. Static validation covers both; a successful fake-backed run does not qualify Gemini model behavior. A2A responses expose trace identifiers only when telemetry creates a valid trace context; absence means there is no response trace to score.

## Migrating to the Python developer and platform course

This unreleased redesign changes the default provider from local Ollama to native Gemini. Existing Ollama users should keep `AGENT_MODEL_PROVIDER=openai-compatible`, `AGENT_MODEL=qwen3:4b-instruct`, `OPENAI_BASE_URL=http://127.0.0.1:11434/v1`, and the non-secret `OPENAI_API_KEY=local-ollama` explicit in their configuration.

Use `gateway:host:ollama` and `platform:dev:ollama` for the existing local-model profiles. The default host and platform tasks now select Gemini and need its separately configured credential. The Python state schema and seed are unchanged; learner files under `learning/` are independent of runtime state. Do not infer a qualified live Gemini deployment from the offline migration checks.

## Current unreleased qualification limit

The development, evaluation, and framework-comparison dependency profiles include `rouge-score`, because the locked ADK evaluator imports it even when the configured metric is tool trajectory. It brings `nltk==3.10.3`, which the package audit rejects for `PYSEC-2026-3740`. The [upstream advisory](https://github.com/nltk/nltk/security/advisories/GHSA-8mgp-746c-j5xp) lists no patched version as checked on 2026-09-12.

The runtime-only learner/agent and MLflow server profiles exclude NLTK and pass the package advisory audit. The full `mise run check` remains blocked until the dependency can be removed without breaking evaluation or an upstream fix is qualified. No advisory exception or scorer bypass is applied.
