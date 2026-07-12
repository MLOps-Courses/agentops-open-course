# PROMPT.md: Elevating AgentOps Course to a Top-Tier OSS Reference

This document serves as the master instructions and developer prompt for an AI agent to comprehensively improve the [AgentOps Open Course](https://agentops-open-course.fmind.dev/). The goal is to elevate this course to a premier, world-class reference for building, securing, and operating production AI agents using a 100% open-source stack, maximizing Github stars, reader experience, code quality, and infrastructure design.

---

## 1. Global Mission Statement

You are a professional AI software engineer and educator. Your objective is to refine and upgrade the entire repository to guide developers on how to design, evaluate, secure, and deploy production-grade AI agents using Google ADK 2.0, kagent, agentgateway, and MLflow on a cheap GKE Standard cluster.

You have the authority to search, modify, and refactor any file across the repository—including Python code, Kubernetes manifests, Helmfile/Skaffold configs, and Markdown documentation. Do not restrict yourself to a predefined set of files; locate and enhance every area that needs adjustment to satisfy the core imperatives below.

---

## 2. Project Layout Reference

Inspect and modify any part of the project structure to align it with GKE, MLflow, and advanced OSS agent patterns:

```
agentops-open-course/
├── AGENTS.md               # Technical rules, model versions, and formatting conventions
├── README.md               # Human-facing introduction and local setup
├── pyproject.toml          # Global repository configurations
├── mise.toml               # Task definitions (install, serve, build, check, test)
├── mkdocs.yml              # Documentation site structure (Zensical)
├── docs/                   # Course chapters (Overview, Setup, Agents, Capabilities, Quality, Gateway, Platform, Observability, Community)
├── agents/
│   ├── python/             # Reference agent "Ops Copilot" using Google ADK 2.0
│   │   ├── src/agent/      # Core logic (tools, memory, guardrails, delegation)
│   │   ├── tests/          # pytest unit and integration tests
│   │   └── evals/          # MLflow GenAI evaluation and prompt registry
│   └── data/               # Local dataset (runbooks, skills, SQLite DB)
└── infra/
    ├── k8s/                # Kubernetes base manifests (namespace, ingress)
    ├── kagent/             # kagent custom resources (ModelConfig, Agent BYO)
    ├── agentgateway/       # agentgateway Rust-based proxy configurations
    ├── mlflow/             # MLflow tracking server GKE manifests
    ├── gcp/                # GKE cluster and Workload Identity setup scripts
    ├── skaffold.yaml       # Container build (Docker) & deploy pipeline
    └── helmfile.yaml       # Helm releases for kagent-crds and kagent
```

---

## 3. Core Imperatives for the Upgrade

To optimize the practitioner experience and drive GitHub stars, ensure you balance and implement all of the following requirements:

### 1. Elevating the Course Content & Reader Experience

1. **Interactive & FAQ-Driven**: Format all documentation in `docs/` as practical FAQs with clear, actionable headings that address real-world challenges faced by production engineers.
2. **Local-to-Cloud Continuum**: Smoothly guide the reader from local-first development (using k3d, local agentgateway, and SQLite) to cheap cloud-native GKE execution, illustrating how the codebase and tools remain identical.
3. **No Placeholders or Hand-Waving**: Ensure every command, Kubernetes resource definition, and Python snippet is fully runnable and correct.

### 2. Upgrading the Reference Agent Code Quality

1. **Idiomatic Google ADK 2.0**: Refactor `agents/python` to demonstrate state-of-the-art ADK patterns (robust session handlers, structured input/output parsing, dynamic memory, and clean tools definitions).
2. **Defensive Coding & Security**: Ensure the reference agent demonstrates runtime PII redaction, secure input/output validation, and robust exception handling.
3. **High Test Coverage**: Enhance unit and integration tests to ensure that the code is enterprise-grade, reliable, and runs cleanly offline.

### 3. Mastering the Open-Source Infrastructure Stack

1. **kagent (Orchestration)**: Standardize Kubernetes custom resources (`Agent` BYO type, `ModelConfig`, `RemoteMCPServer`) to declare agents as native Kubernetes workloads.
2. **agentgateway (Data Plane Proxy)**: Configure agentgateway to secure, rate-limit, and route all LLM inference calls, MCP tool traffic, and Agent-to-Agent (A2A) message flows.
3. **MLflow (Lifecycle Observability)**: Deploy an in-cluster, self-hosted MLflow server for prompt management, tracing, and LLM-as-judge evaluations, preventing reliance on proprietary cloud SaaS alternatives.

### 4. Cost-Control & Security on Google Cloud (GCP)

1. **GCP Project Context**: Set up all cloud resources to run under the GCP project `agentops-open-course`.
2. **GKE Standard Spot VMs**: Configure the GKE cluster to use a single-node Spot VM pool (e.g., `e2-standard-2` or `e2-medium` node pools) to keep total monthly spend under $20.
3. **Passwordless Workload Identity**: Set up GKE Workload Identity Federation (WIF) to allow pods to securely authenticate with GCP/Vertex AI Gemini models, eliminating static secrets and API keys.

---

## 4. Key Guidelines for Implementation

Use these guidelines to discover and perform changes across the repository:

### 1. Infrastructure (GCP / GKE Manifests)

1. Author scripts or guidelines to provision the cheap Spot VM-based GKE Standard cluster under project `agentops-open-course`.
2. Design and document IAM bindings matching the Kubernetes Service Account to the Google Service Account via Workload Identity.
3. Create the GKE deployment configurations for self-hosted MLflow, utilizing persistent volume claims for SQLite database storage and a Google Cloud Storage (GCS) bucket for logging artifacts.

### 2. Proxy and Routing Policies

1. Configure `agentgateway/config.yaml` to route model requests to Vertex AI Gemini endpoints via Workload Identity authentication.
2. Configure prompt-shielding rules, guardrails, and rate limits within the gateway to showcase secure proxying.

### 3. Documentation (Zensical Site)

1. Update the Zensical chapters in `docs/` to present the updated deployment walkthroughs.
2. Ensure sections covering Chapters 5 (Gateway), 6 (Platform), and 7 (Observability) are rewritten to focus on GKE and MLflow in a cohesive, star-worthy tutorial format.

### 4. Code Maintenance

1. Ensure the Python agent project in `agents/python` is fully aligned with these configurations.
2. Keep the local environment running smoothly by fallback variables or profiles.
3. Verify all changes pass the validation pipeline (`mise run format && mise run check && mise run test`).

---

## 5. Key Files Reference

- [pyproject.toml](file:///home/fmind/externals/agentops-open-course/agents/python/pyproject.toml#L28-L30) — MLflow & pyarrow dependencies.
- [config.py](file:///home/fmind/externals/agentops-open-course/agents/python/src/agent/config.py) — Agent config parameters.
- [agent.yaml](file:///home/fmind/externals/agentops-open-course/infra/kagent/agent.yaml) — kagent Agent resource.
- [modelconfig.yaml](file:///home/fmind/externals/agentops-open-course/infra/kagent/modelconfig.yaml) — kagent ModelConfig resource.
- [helmfile.yaml](file:///home/fmind/externals/agentops-open-course/infra/helmfile.yaml) — kagent Helm installation configuration.
- [skaffold.yaml](file:///home/fmind/externals/agentops-open-course/infra/skaffold.yaml) — GKE deploy lifecycle orchestrator.
