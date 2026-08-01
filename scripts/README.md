# scripts

Automation behind the `mise run` vocabulary. Nothing here is meant to be run by hand as a first resort: almost every entry has a task, and the task is what the course, the git hooks, and CI all call. The table below names the exception.

Two directories, split by what the code does:

- `scripts/` — repository gates and environment setup.
- `infra/scripts/` — runtime orchestration for the host gateway, the local platform, and state.

## Repository gates and setup

| Script                 | Task                                                   | Tier     | What it does                                                                        |
| ---------------------- | ------------------------------------------------------ | -------- | ----------------------------------------------------------------------------------- |
| `check_conventions.py` | `check:docs`, `check:skills`, `check:release-metadata` | base     | Course-page frame, FAQ headings, skill front matter, and version agreement.         |
| `check-licenses.sh`    | `check:licenses`                                       | base     | Repository and Python dependency licences against the reviewed allowlist.           |
| `check-infra.sh`       | `check:infra`                                          | platform | Renders and validates both Kubernetes overlays and the OpenTofu module.             |
| `doctor.sh`            | `doctor`, `doctor:base\|model\|gateway\|platform\|gcp` | each     | Asserts one prerequisite tier is present, naming the install task per missing tool. |
| `freshness_report.py`  | Freshness workflow                                     | release  | Builds the read-only upstream-version and mutable-claim audit report.               |
| `install-helm-diff.sh` | part of `install`                                      | platform | Installs the pinned Helm `diff` plugin after verifying its checksum.                |
| `cluster-start.sh`     | `cluster:start`                                        | platform | Creates the local k3d cluster and its registry, or reconciles an existing one.      |
| `promote.sh`           | `promote`                                              | platform | Eval-gated promotion: gate, render the overlay, print promote/rollback commands.    |
| `release_evidence.py`  | Release workflow                                       | release  | Minimizes and binds qualifying Eval evidence to its exact run attempt.              |
| `release_freshness.py` | Release workflow                                       | release  | Validates GitHub-rendered tasks from a recent freshness audit or a reviewed waiver. |
| `release_reconcile.py` | Release workflow                                       | release  | Proves ownership before deleting a reversible failed-promotion index.               |
| `smoke-host.sh`        | `smoke:host`                                           | gateway  | Proves the host composition against a fake model, then tears it down.               |
| `lib.sh`               | sourced by the others                                  | —        | Strict mode plus shared command and cgroup prerequisite helpers.                    |
| `test-lib.sh`          | `check:shell`                                          | base     | Deterministically checks both cgroup-v1 rejection and cgroup-v2 acceptance.         |

## Runtime orchestration

| Script                             | Task                        | Tier     | What it does                                                                          |
| ---------------------------------- | --------------------------- | -------- | ------------------------------------------------------------------------------------- |
| `infra/scripts/gateway-host.sh`    | `gateway:host*`             | gateway  | Runs the digest-pinned agentgateway container on loopback and its own network.        |
| `infra/scripts/loopback-relay.py`  | used by `gateway-host.sh`   | gateway  | Relay bound to that network's gateway only, so the container can reach host services. |
| `infra/scripts/gateway-tls.sh`     | `gateway:host:auth`         | gateway  | Generates the gitignored demo TLS material for the secured profile.                   |
| `infra/scripts/gateway-jwt.sh`     | `gateway:host:auth`         | gateway  | Generates the gitignored demo JWT keys and tokens.                                    |
| `infra/scripts/backup-state.sh`    | `state:backup`              | platform | Snapshots the agent state PVC.                                                        |
| `infra/scripts/restore-state.sh`   | `state:restore`             | platform | Restores a snapshot into the cluster.                                                 |
| `infra/scripts/backup-drill.sh`    | `state:drill`               | platform | Proves a backup actually restores, end to end.                                        |
| `infra/scripts/check-state.sh`     | called by `check-infra.sh`  | platform | Asserts the shared state claim, `fsGroup`, and read-only mounts.                      |
| `infra/scripts/deploy-gke.sh`      | `gke:deploy`                | GCP      | Verifies the exact context, resolves cloud coordinates, and applies the GKE bundle.   |
| `infra/scripts/render-gke.sh`      | called by GKE checks/deploy | GCP      | Resolves project-neutral GKE placeholders from OpenTofu outputs.                      |
| `infra/scripts/smoke-gke-model.sh` | `gke:smoke`                 | GCP      | Proves exact GKE config, a Vertex tool result, and read-only A2A seed retrieval.      |
| `infra/scripts/secrets.sh`         | run directly (Ch. 6.5)      | platform | SOPS + age encryption for the committed ciphertext.                                   |

## Conventions

1. Shell for orchestration, Python for text. A script that starts a process, publishes a port, or probes `PATH` is shell; a check that parses Markdown, YAML, or a manifest is Python, because that is where a regex quietly gets it wrong.
1. Every script sources `lib.sh` and declares its tools with `require_cmd <tool> <tier>`, so a missing tool says which `mise` command installs it instead of failing with `command not found`.
1. A tier named in `require_cmd` must be a profile `doctor.sh` actually checks that tool in. Otherwise the remedy points at a `doctor:<tier>` run that passes while the script still cannot run.
1. The task name is the public interface. Rename a script freely; renaming a task breaks the course pages that quote it.
1. `check_conventions.py release-metadata` deliberately runs on a bare `python3`: the release workflow has a checkout and nothing else.
