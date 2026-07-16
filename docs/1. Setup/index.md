---
description: Set up a professional local environment and toolchain for building and operating agents.
---

# 1. Setup

## What will you set up in this chapter?

Before you run the reference agent, create a reproducible local environment without requiring every system service, model, cluster, or cloud account at once. `mise install` materializes the pinned CLI set; staged doctors check only the external prerequisites used by your current chapter:

| Tier     | Command                    | What it validates                                                   |
| -------- | -------------------------- | ------------------------------------------------------------------- |
| Base     | `mise run doctor`          | Docs/Python entry prerequisites, not the complete infra check stack |
| Model    | `mise run doctor:model`    | Local Ollama plus `qwen3:4b` for Chapters 2-4                       |
| Gateway  | `mise run doctor:gateway`  | Docker prerequisites for the Chapter 5 host wrapper                 |
| Platform | `mise run doctor:platform` | Containers, k3d, kagent, and Skaffold for Chapter 6                 |
| GCP      | `mise run doctor:gcp`      | Optional ADC, Vertex, and GKE prerequisites                         |

The base learning path uses `mise run check:core`. It excludes infrastructure execution so a learner does not need Docker before Chapter 5. The complete `mise run check` maintainer gate adds both Kubernetes overlays, Compose configuration, Skaffold, Helm, and OpenTofu validation.

Use the pages by checkpoint rather than treating every service as an up-front prerequisite:

- **Base setup now:** [1.0. System](./1.0. System.md), [1.1. Python](./1.1. Python.md), [1.4. Providers](./1.4. Providers.md), and [1.5. Workspace](./1.5. Workspace.md).
- **Before Chapter 5:** [1.2. Containers](./1.2. Containers.md) validates the Docker runtime used by the host gateway wrapper.
- **Before Chapter 6:** [1.3. Kubernetes](./1.3. Kubernetes.md) validates platform tools without creating the cluster early.
