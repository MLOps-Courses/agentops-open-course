---
description: Set up a professional local environment and toolchain for building and operating agents.
---

# 1. Setup

## What will you set up in this chapter?

Before you build an agent, create a reproducible local environment. This chapter installs the pinned toolchain, verifies Python and containers, creates the tracked k3d/registry only when needed, and separates model-free tests from local Qwen3 or optional Gemini configuration.

Work through the pages in order:

- **[1.0. System](./1.0. System.md)**: Prerequisites, operating-system setup, shell, and installing mise.
- **[1.1. Python](./1.1. Python.md)**: Install and manage Python with mise and uv.
- **[1.2. Containers](./1.2. Containers.md)**: Docker and Podman basics for packaging agents.
- **[1.3. Kubernetes](./1.3. Kubernetes.md)**: Create and verify the tracked local k3d cluster/registry, with reversible cleanup.
- **[1.4. Providers](./1.4. Providers.md)**: Configure account-free Qwen3 or native Gemini without leaking credentials.
- **[1.5. Workspace](./1.5. Workspace.md)**: Git, editor-neutral development, hooks, and the AGENTS.md standard.
