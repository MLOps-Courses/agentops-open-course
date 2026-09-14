---
description: Build agents in Python on your laptop, then operate them on an open-source Kubernetes platform.
---

# AgentOps Open Course

!!! abstract "In one glance"

    - **You will:** Build an agent, evaluate its behavior, and operate the same application on Kubernetes.
    - **You need:** Working Python knowledge for Part I; container and Kubernetes knowledge for Part II.
    - **Time:** about 10 minutes, orientation.

<!-- Section aliases retained for existing bookmarks. -->

<span id="how-do-you-begin"></span>
<span id="how-does-the-course-progress"></span>
<span id="what-are-the-six-commands-that-reach-the-first-agent-turn"></span>
<span id="what-does-open-source-mean-here"></span>
<span id="what-is-the-system-you-will-inspect-and-extend"></span>
<span id="what-will-you-be-able-to-do"></span>
<span id="which-chapter-do-you-need"></span>

## What will you build?

Build a Python incident assistant, then learn how a platform team operates it.

**Part I — Agent development** uses Google ADK and a Gemini API key on your laptop. You build through small exercises, inspect failures, and compare your work with tested solutions. No GPU, Docker, or Kubernetes is needed.

**Part II — Platform engineering** moves the reference agent behind agentgateway and onto Kubernetes with kagent. This part is deliberately more demanding: you already know containers, Kubernetes resources, networking, and `kubectl`.

The written course is CC BY 4.0 and the code is MIT. The application and platform software are open source. Gemini is a hosted proprietary model service; an optional Ollama/open-weight model path is available.

## How should you start?

Start with the small Python workshop, then consult the reference pages when you need more depth.

1. Check the prerequisites in [0.0. Course](./0.%20Overview/0.0.%20Course.md).
1. Install the Python runtime with [1.0. System](./1.%20Setup/1.0.%20System.md).
1. Configure your Gemini API key with [1.4. Providers](./1.%20Setup/1.4.%20Providers.md).
1. Build your first agent in [2.1. First Agent](./2.%20Agents/2.1.%20First%20Agent.md).
1. Carry your code through [2.6. Workshop](./2.%20Agents/2.6.%20Workshop.md).

<!-- quickstart: unverified-preview -->

The first interactive run is an **unverified preview** of model behavior. Offline exercise checks prove Python behavior; successful conversation and model evaluation are separate evidence.

## What are the two learning parts?

Each part has a useful completion point of its own.

| Part                   | Chapters                           | What you produce                                                                                                   |
| ---------------------- | ---------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| Agent development      | 1–4, with Chapter 0 as orientation | Python tools, state, approvals, a bounded workflow, evaluation cases, and an API/A2A handoff                       |
| Platform engineering   | 5–7                                | The same application behind agentgateway, registered with kagent, deployed on Kubernetes, observed and recoverable |
| Capstone and community | 8                                  | A learner-owned domain and documented evidence; optional OSS maintenance and release work                          |

Part I requires Python functions, imports, virtual environments, package installation, and basic debugging. Use the [Python tutorial](https://docs.python.org/3/tutorial/) if you need preparation.

Part II assumes Kubernetes fundamentals. Prepare with [Kubernetes Basics](https://kubernetes.io/docs/tutorials/kubernetes-basics/) before starting [5. Gateway](./5.%20Gateway/index.md). Cloud deployment is an optional extension; local Kubernetes is the main platform lab.

## How much does it cost to follow?

Reading, copying, and modifying the course is free.

Gemini removes the local model hardware requirement. Free-tier access depends on model, project, region, and current quotas; paid-tier calls can be billed. Check the current [Gemini pricing](https://ai.google.dev/gemini-api/docs/pricing) and [rate limits](https://ai.google.dev/gemini-api/docs/rate-limits).

Every workshop check works without a model or API key. Ollama is an optional alternative for suitable hardware, and recorded examples support study when a provider is unavailable. Neither a fake nor a recording proves live model quality.

## How should you use the reference?

The reference shows how the small workshop patterns fit into a larger, tested application.

Use `agents/python/labs/solutions` to compare one exercise at a time. Use `agents/python/src/agent` for the completed reference, including persistent sessions, approval audit, privacy policy, MCP, A2A, and recovery. Platform learners can begin with that reference after passing the developer handoff checks.

Every course page declares its outcome, prerequisites, and estimated reading or exercise time. Reference pages are lookups, not required line-by-line reading. Exercise time varies; the estimates are planning aids, not measured learner completion times.

## What proves this page worked?

Choose Part I or Part II and check its prerequisites before installing tools.

**You are done when:**

- You know which part you are taking and what you will produce.
- You understand the difference between free course material, open-source software, and hosted model access.
- You know where to find the starting exercise and its worked solution.

Continue to [0.0. Course](./0.%20Overview/0.0.%20Course.md) to choose your route.
