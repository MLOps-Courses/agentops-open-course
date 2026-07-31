---
name: Docs freshness audit
about: Recurring checklist to re-verify time-sensitive claims (versions, prices, model names, benchmarks) before a release.
title: "docs: freshness audit for <release/date>"
labels: documentation
---

Time-sensitive claims rot silently. Walk this checklist before each release: open the source file, confirm the claim still matches reality (installed version, current price, current model name, re-run benchmark, current foundation status), and check the box or open a fix. Update this template when a claim moves, is added, or is retired.

## Automated snapshot

The quarterly `.github/workflows/freshness.yml` workflow appends a read-only report to this issue. It inventories every `mise.toml` tool, filters stable k3s/Ollama/kagent/mise releases, checks Kubernetes skew and the Ollama asset checksum, resolves kagent charts and arbitrary image digests, checks Wolfi pins, and runs the copied-prose source gate. Every proposal names its upstream authority and required validation tier.

- [ ] Triage every `REVIEW`, `MISMATCH`, `MISSING`, or `UNAVAILABLE` row in the newest automated comment.
- [ ] Keep upgrades explicit: the reporter must never change a pin, branch, issue state, or pull request.

## Model & provider names

- [ ] `gemini-3.6-flash` is still the current GA optional Gemini/Vertex model id, with its short-term lifecycle stated honestly — `docs/0. Overview/0.4. Providers.md`, `docs/2. Agents/2.2. Models.md`, `docs/6. Platform/6.3. Platform Agents.md`, and the GKE gateway/agent manifests.
- [ ] `qwen3:4b-instruct` is still the default local Ollama model and its weights remain Apache-2.0 licensed — `agents/python/src/agent/config.py`, `docs/0. Overview/0.4. Providers.md`, `docs/6. Platform/6.6. Platform Delivery.md`, and the local manifests.
- [ ] `nomic-embed-text` is still the embedding model — `agents/python/src/agent/config.py`, `docs/3. Capabilities/3.4. Memory.md`.

## Prices & cost inputs

- [ ] Recalculate the canonical GKE estimate and its review date from current provider inputs — `docs/7. Observability/7.3. Costs.md`.
- [ ] Confirm the current management-fee and free-tier assumptions used by the canonical calculation — `docs/7. Observability/7.3. Costs.md`.
- [ ] Confirm the GKE node and disk shape in `infra/gcp` matches the canonical calculation inputs — `docs/7. Observability/7.3. Costs.md`.
- [ ] Provider price guidance (compute prices at run date, no hard-coded rates) still accurate — `docs/7. Observability/7.3. Costs.md`, `docs/2. Agents/2.2. Models.md`.

## Pinned versions

- [ ] The agentgateway pin and its `202`-on-`DELETE` session-termination quirk still agree with upstream — `mise.toml`, `docs/5. Gateway/5.2. MCP Gateway.md`, `docs/6. Platform/6.5. Platform Gateway.md`.
- [ ] The kagent chart release and API version still agree with the immutable Helm sources and generated schemas — `infra/helmfile.yaml`, `infra/kagent/schemas`, and Chapter 6.
- [ ] Wolfi apk exact pins still resolve from the rolling repository and match both runtime Dockerfiles — `agents/python/Dockerfile`, `infra/mlflow/Dockerfile`, and `docs/6. Platform/6.1. Containers.md`.
- [ ] Container base-image digests, `uv`, and `trivy-action` pins current (Dependabot) — `agents/python/Dockerfile`.
- [ ] The pinned `curlimages/curl` smoke image still resolves for every supported host architecture — `scripts/smoke-host.sh`.
- [ ] Ollama evaluation release asset and SHA-256 still match the pinned version — `.github/workflows/eval.yml`.
- [ ] GitHub Actions SHA pins current (Dependabot) — `.github/workflows/*.yml`.

## Governance & foundation status

`docs/8. Community/8.6. AAIF.md` is the most volatile page in the course: it dates a donation, names project owners, and prints maturity tiers, none of which the repository can pin.

- [ ] AAIF still hosts MCP, agentgateway, and the AGENTS.md convention, and nothing donated since is missing — `docs/8. Community/8.6. AAIF.md`.
- [ ] A2A still sits under the Linux Foundation directly rather than a sub-foundation — `docs/8. Community/8.6. AAIF.md`.
- [ ] CNCF tiers still correct: Kubernetes, Prometheus, and OpenTelemetry Graduated; kagent still Sandbox — `docs/8. Community/8.6. AAIF.md`.
- [ ] MLflow still sits directly under the Linux Foundation, and every remaining steward/licence pairing in the map still holds (Grafana Labs CLA, Ollama, Qwen3, Google ADK) — `docs/8. Community/8.6. AAIF.md`.
- [ ] The upstream issue-routing destinations still resolve to the tracker that owns each boundary — `docs/8. Community/8.6. AAIF.md`.

## Benchmarks & measured checkpoints

- [ ] The retrieval release checkpoint still reproduces (dataset commit, Ollama version, model manifest/blob, and index provenance) — `docs/3. Capabilities/3.4. Memory.md`.
- [ ] `qwen3:4b-instruct` architecture maximum still matches `ollama show`, and the loaded serving window still matches `ollama ps` — `docs/3. Capabilities/3.4. Memory.md`.

## Wrap-up

- [ ] Every unchecked item above has a linked follow-up issue or PR.
- [ ] The release handoff records the reviewer and review date, or links an explicit waiver with an owner and expiry.
- [ ] This template updated for any claim that moved, was added, or was retired.
