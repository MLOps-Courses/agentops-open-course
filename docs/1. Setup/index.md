---
description: Prepare a small Python environment and choose model access before building your first agent.
---

# 1. Setup

!!! abstract "In one glance"

    - **You will:** Prepare the laptop environment for Part I.
    - **You need:** Working Python knowledge, a terminal, and internet access for installation.
    - **Time:** about 5 minutes, orientation.

<!-- Section aliases retained for existing bookmarks. -->

<span id="what-is-deliberately-not-part-of-this-chapter"></span>
<span id="what-will-you-set-up-in-this-chapter"></span>
<span id="which-tier-does-each-chapter-actually-require"></span>
<span id="why-are-the-prerequisites-staged-instead-of-installed-up-front"></span>

## Which pages do you need now?

Start with Python runtime installation and model configuration; the workshop introduces everything else when needed.

1. Follow [1.0. System](./1.0.%20System.md) and run `mise run install:learner`.
1. Follow [1.4. Providers](./1.4.%20Providers.md) for a Gemini key, optional Ollama, or offline practice.
1. Continue to [2.1. First Agent](../2.%20Agents/2.1.%20First%20Agent.md).

Python functions, typing, imports, exceptions, virtual environments, and package installation are prerequisites. Prepare with the [Python tutorial](https://docs.python.org/3/tutorial/) if needed. This course teaches agent development, not Python fundamentals.

## Which pages can wait?

Use the other pages as references when the corresponding boundary appears.

- **[1.0. System](./1.0. System.md)** _(hands-on)_: install the small learner runtime; add contributor tools later.
- **[1.1. Python](./1.1. Python.md)** _(reference)_: understand the locked Python project and preparation resources.
- **[1.2. Containers](./1.2. Containers.md)** _(hands-on)_: verify the container engine before Chapter 5.
- **[1.3. Kubernetes](./1.3. Kubernetes.md)** _(reference)_: verify platform prerequisites before Chapter 6.
- **[1.4. Providers](./1.4. Providers.md)** _(hands-on)_: configure Gemini, or choose the explicit Ollama alternative.
- **[1.5. Workspace](./1.5. Workspace.md)** _(hands-on)_: inspect the completed reference and contributor gates after the workshop.

`install:learner` installs one locked Python runtime. It does not install the documentation environment, development tools, MLflow evaluation extras, container images, or Kubernetes tools. Chapter 4 introduces `cd agents/python && mise run install:eval` only when recording evaluations. The first offline exercise checks need no provider key. An interactive Gemini conversation uses a proprietary service and consumes its quota; a free allocation is not guaranteed.

## When does the harder platform part begin?

Part II begins in Chapter 5 and assumes container and Kubernetes foundations.

The gateway first runs on the laptop. Chapter 6 moves it onto local Kubernetes with kagent. Prepare with [Kubernetes Basics](https://kubernetes.io/docs/tutorials/kubernetes-basics/) before that transition if you cannot inspect Deployments, Services, Secrets, PVCs, probes, and NetworkPolicies.

[4.8. Developer Handoff](../4.%20Quality/4.8.%20Developer%20Handoff.md) defines an independent completion point for Part I and the reference checkpoint for platform learners. You can finish the developer course without Kubernetes.

## What proves this chapter worked?

Verify the worked checkpoints before making any model request.

```bash
mise run check:labs
```

**You are done when:**

- The offline worked checkpoints pass in the locked Python runtime.
- You know whether your interactive path uses Gemini quota or optional local inference.
- You know which preparation pages to revisit before Part II.

Continue to [2.1. First Agent](../2.%20Agents/2.1.%20First%20Agent.md) when installation and provider configuration are complete.
