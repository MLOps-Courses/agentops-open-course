# Support

This policy defines stable surfaces, verified platforms, compatibility, upgrade, rollback, and explicit non-goals. The course is pre-1.0: software contracts change deliberately, while course prose can improve between releases.

## What does the course support?

The supported outcome is the account-free OSS path from a clean checkout to:

- Offline course, Go agent, evaluation, repository-tool, security, and infrastructure gates.
- The conversational agent, bounded workflow, and coordinator on Qwen3 through Ollama.
- Typed read tools, repository Agent Skills, MCP, guarded writes, memory, and A2A.
- The host agentgateway path and local k3d platform.
- OpenTelemetry collection into Tempo, Loki, Prometheus, Alertmanager, and Grafana.
- State backup and crash-recoverable restore, deterministic adversarial tests, black-box evaluation, and load testing.

The complete release gate targets Linux x86_64 with cgroup v2. This rewritten checkout has not re-run the k3d runtime campaign; the authoring host uses cgroup v1, and the platform doctor fails before creating a cluster there. Hosted CI uses its declared runner image.

This table is the capacity-planning authority. Values are conservative planning figures, not measured minima or performance guarantees.

<!-- local-platform-capacity: total-ram-gib=14 free-disk-gib=15 -->

| Work tier               | Install or profile                                                                         | Capacity contract                                                                                                                     |
| ----------------------- | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| Read the course         | No install                                                                                 | No measured minimum beyond a browser or Markdown reader.                                                                              |
| Offline engineering     | `install`; `doctor`; `check:core`; `test`                                                  | No measured RAM or disk minimum; the checkout, module caches, and gates are authoritative.                                            |
| Local model             | Offline tier plus `doctor:model`                                                           | No measured host minimum; model disk, memory, and speed are hardware-dependent.                                                       |
| Host gateway            | Model tier plus `doctor:gateway`                                                           | No separate measured minimum; requires a working container engine and capacity for model, agent, gateway, and optional webhook.       |
| Complete local platform | `install:platform`; `doctor:platform`                                                      | Plan for **14 GiB total RAM** and **15 GiB free disk** for model, images, one k3d cluster, and observability together.                |
| Optional GKE laboratory | Reviewed host Google Cloud CLI plus `install:platform`; `doctor:gcp`; reviewed `tofu plan` | Local capacity is not cloud quota; [7.3. Costs](./content/7.%20Observability/7.3.%20Costs.md) owns billable shape and dated estimate. |

macOS, Linux arm64, and WSL2 are best-effort. Tool locks may resolve there, but that does not imply the full container, relay, k3d, and restore journey is release-gated.

The admitted language toolchain is the Go version pinned in root `mise.toml`, `agents/go/go.mod`, `evals/go.mod`, `tools/go.mod`, and the agent Dockerfile. Those pins must agree before a version is supported.

## Which interfaces are stable?

Stable software contracts are:

- The `agent` executable and its documented subcommands.
- The `agent`, `workflow`, and `coordinator` `AGENT_ENTRYPOINT` values.
- Documented environment variables, defaults, validation rules, and network ports in `AGENTS.md`.
- Immutable seed data, writable-state separation, SQLite schema versions, append-only audit behavior, and backup manifest format.
- The documented six read-only MCP tools and guarded in-process write tools.
- A2A message, task, streaming, approval, reconnect, persistence, cancellation, and error behavior.
- The host, local k3d, and optional GKE configuration shapes, Kubernetes resource names, image names, and versioned image tags.
- Evaluation input schemas, the black-box import boundary, stable OpenTelemetry signal names, and sanitized result schemas documented in `evals/README.md`.

Course prose is not frozen. Chapters can be reordered or rewritten, but every push to `main` publishes the site, so a route change must preserve or redirect every previously released URL through `data/released-urls.json`.

Internal Go packages, unexported identifiers, test fixtures, generated HTML, unversioned commits, and upstream implementation details are not public APIs. The repository adapts incompatible upstream changes before changing its documented contract.

## What does evaluation support mean?

`evals` is a standalone Go module that calls the agent through ADK REST or A2A. It must not require or import the agent module.

Offline support covers asset validation, typed event folding, transport equivalence fixtures, deterministic scorers, judge parsing and calibration fixtures, sanitized artifact schemas, and in-memory OpenTelemetry evidence.

Model-backed support requires an explicitly configured model and records one source commit, model identity, evalset digest, transport, pass rate, minimum pass rate, required-case status, and positive usage.

`mise run test` enforces an 80% line-coverage floor per package in `agents/go` and `evals`. It is a quality gate for this repository, not a compatibility or release criterion: a supported version is defined by its behavior, not by its coverage percentage.

Offline validation must never be presented as model-backed proof. A green `mise run check` and `mise run test` say nothing about how the agent answered, whether a judge agreed, or whether a span reached Tempo — only a model-backed `mise run eval` speaks to that, and only about the runtime it ran against.

## How are compatibility and deprecations handled?

Patch releases fix defects without intentionally breaking stable software contracts. Minor releases may add backward-compatible fields, tools, schema versions, or manifests and may restructure course pages.

One supported dependency ceiling remains, recorded as a `// compatibility hold:` comment in `tools/go.mod`: `cdproto` stays at the revision `chromedp` v0.16.0 was built against, and only a real Chrome accessibility acceptance run qualifies a newer one. A hold comment always names three things — the owner that decides the version, the constraint, and the validator — and `check:freshness` renders them as a table.

ADK Go v2.2.0 owned two more until v2.3.0 migrated its own logs to OpenTelemetry 1.45 and log 0.21 and `agents/go` moved with it. The OTel stable 1.44 with log 0.20 ceiling is therefore gone, and so is the pinned `openai-go/v3` and `genai` pair — though ADK still resolves both clients through its own module, so move that pair when ADK moves, not before.

These are upstream compatibility constraints, not general bans on newer clients. A replacement ADK/client family becomes supported only when `cd agents/go && mise run check && mise run test` compiles and passes telemetry, command, and full race tests. Never override a ceiling by upgrading one transitive module alone.

Two runtime pins remain explicit evidence holds:

| Held pin                         | Owner                    | Constraint                                                                                        | Validator                                                                                                      |
| -------------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| k3s `v1.36.2-k3s1`               | Local platform campaign  | A newer node image needs a cgroup-v2 host and cannot be qualified by manifest rendering alone.    | `mise run cluster:start`, `mise run platform:dev`, deployed HITL, trace/log correlation, and restart survival. |
| Ollama workflow archive `0.32.5` | Go evaluation cost owner | A newer server changes the serving identity before this checkout has a reviewed Go cost baseline. | The full Eval workflow, judge calibration, exported OTel evidence, and a reviewed cost re-baseline.            |

`check:freshness` reports both newer candidates as review work rather than claiming they are current. These holds preserve exact inputs; they do not convert the missing runtime evidence into release proof.

While pre-1.0, a breaking software-contract change requires a minor release and documented migration. After 1.0 it requires a major release.

When practical, a deprecated interface remains functional for at least the next minor release. Its replacement and removal target appear in the changelog and at the use site. Security fixes can remove unsafe behavior sooner with a documented migration.

State migrations must be atomic, repeatable, restore-tested, and reject unknown future schema versions. Container version tags are immutable; deploy by digest when exact bytes matter. No floating `latest` deployment contract is supported.

## How do I upgrade?

The target release prepares runtime state at the normal write-owning startup boundary. Seed data is never migrated.

1. Stop every agent, MCP, A2A, gateway, and platform process that can write state.
1. On the current supported release, run the documented state backup and retain the completed snapshot outside the working tree.
1. Record the release tag, deployed image digests, configuration, source commit, and current schema versions.
1. Read every changelog entry through the target release, including migration and rollback notes.
1. Check out the target, then run `mise run install`, `mise run check:core`, and `mise run test`.
1. Start one writable agent or A2A process and perform the shortest incident read before restoring traffic.
1. Run the host or Kubernetes restore drill before accepting the upgrade.

Do not open migrated state with older code unless the target release explicitly declares downgrade compatibility.

## How do I roll back?

Stop every writer, return to the recorded tag or exact image digests, restore its configuration, and restore the pre-upgrade snapshot through the documented state command.

Restore is crash-recoverable, not an instantaneous multi-file rename. Never copy database files around the state boundary or delete unexplained `.restore-*` evidence.

If the candidate changed an evalset or qualification policy, do not reuse results from the previous digest. Release evidence is source-commit and evalset scoped.

## What must a production owner add?

The optional GKE path is a low-cost, production-shaped laboratory, not a production environment.

| Concern                        | Course laboratory                            | Production owner must define and prove                              |
| ------------------------------ | -------------------------------------------- | ------------------------------------------------------------------- |
| Availability                   | One zonal Spot node; single replicas         | Failure domains, disruption budgets, HA stores, and tested failover |
| Public access                  | Port-forwards; no public application edge    | TLS, identity, authorization, abuse controls, and edge threat model |
| Backup and recovery            | Same-environment restore drill               | Off-environment encrypted copies, objectives, and recurring drills  |
| Retention and subject requests | Local retention; no unified subject API      | Schedule plus authenticated discovery, export, and erasure workflow |
| Storage protection             | Cost-first defaults                          | Deletion protection, versioning or soft deletion, and key ownership |
| Network                        | Public control-plane and cloud service paths | Private nodes, controlled egress, DNS policy, and audit             |
| Capacity                       | Fixed bounds and one replica                 | Load-derived scaling, quotas, capacity tests, and failure budgets   |
| Reliability ownership          | Demonstration alerts and local evidence      | SLOs, paging, escalation, runbooks, and accountable owners          |

The course intentionally excludes a model-callable subject-data administration endpoint. A production operator must build that function outside the agent tool surface, authenticated, dry-run-first, and administratively audited.

## What is outside the contract?

- Live cloud deployment, hosted-model calls, cloud cost acceptance, public TLS/auth, HA, disaster recovery, and production operation.
- Automatic online answer scoring or a public human-feedback endpoint.
- Runtime prompt or response content capture by default.
- Full-path release qualification on best-effort operating systems.
- Formal accessibility certification or an exhaustive assistive-technology matrix.
- PDF and ebook publication.
- Coordinated subject discovery, export, or erasure across notes, ADK sessions, A2A tasks, traces, and release artifacts.
- Compatibility for forks, unreviewed patches, mutable tags, or unsupported dependency combinations.
- Availability, uptime, or latency commitments for the published site; `.github/workflows/docs.yml` deploys it to GitHub Pages on a push to `main`, and that hosting is best effort.

## How long is a release supported?

The latest release and `main` receive security and correctness fixes. Older releases do not.

Dependency updates and security triage are best effort from a single maintainer. If safe maintenance stops for six months, the maintainer will announce archival, disable unsupported publication workflows, and seek a successor under [GOVERNANCE.md](./GOVERNANCE.md).

Use [SECURITY.md](./SECURITY.md) for vulnerabilities, [ACCESSIBILITY.md](./ACCESSIBILITY.md) for accessibility barriers, and a public issue for other support requests. Finished derivatives are not support requests: the **Capstone showcase** issue form collects them under the `capstone` label, which is where to look for what other people built.
