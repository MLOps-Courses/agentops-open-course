---
name: Docs freshness audit
about: Recurring checklist to re-verify time-sensitive claims (versions, prices, model names, benchmarks) before a release.
title: "docs: freshness audit for <release/date>"
labels: documentation
---

Time-sensitive claims rot silently. Walk this checklist before each release: open the source file, confirm the claim still matches reality (installed version, current price, current model name, re-run benchmark), and check the box or open a fix. Update this template when a claim moves, is added, or is retired.

## Model & provider names

- [ ] `gemini-3.5-flash` is still the correct optional Gemini/Vertex model id — `docs/0. Overview/0.4. Providers.md`, `docs/2. Agents/2.2. Models.md`, `docs/6. Platform/6.3. Platform Agents.md`, and the GKE gateway/agent manifests.
- [ ] `qwen3:4b-instruct` is still the default local Ollama model and its weights remain Apache-2.0 licensed — `agents/python/src/agent/config.py`, `docs/0. Overview/0.4. Providers.md`, `docs/6. Platform/6.6. Platform Delivery.md`, and the local manifests.
- [ ] `nomic-embed-text` still the embedding model — `agents/python/src/agent/config.py`, `docs/3. Capabilities/3.4. Memory.md`.

## Prices & cost targets

- [ ] GKE lab "under USD 20 per month" target still plausible — `docs/6. Platform/6.6. Platform Delivery.md`.
- [ ] GKE management fee "USD 0.10 per cluster-hour" and free-tier credit assumption still current — `docs/7. Observability/7.3. Costs.md`.
- [ ] `e2-standard-2` Spot node + 30 GiB disk shape still the cheapest sensible choice — `docs/6. Platform/6.6. Platform Delivery.md`, `docs/7. Observability/7.3. Costs.md`.
- [ ] Provider price guidance (compute prices at run date, no hard-coded rates) still accurate — `docs/7. Observability/7.3. Costs.md`, `docs/2. Agents/2.2. Models.md`.

## Pinned versions

- [ ] agentgateway `v1.3.1` and its `202`-on-`DELETE` session-termination quirk — `docs/5. Gateway/5.2. MCP Gateway.md`, `docs/6. Platform/6.5. Platform Gateway.md`.
- [ ] kagent charts `0.9.11` and API `kagent.dev/v1alpha2` — `docs/6. Platform/6.2. Platform Install.md`, `docs/6. Platform/6.3. Platform Agents.md`, `infra/helmfile.yaml`.
- [ ] Wolfi apk exact pins (`python-3.13=...`, `libstdc++=...`) still resolve; refresh if dropped from the rolling repo — `agents/python/Dockerfile`, `docs/6. Platform/6.1. Containers.md`.
- [ ] Container base-image digests, `uv`, and `trivy-action` pins current (Renovate) — `agents/python/Dockerfile`.
- [ ] The pinned `curlimages/curl` smoke image still resolves for every supported host architecture — `scripts/smoke-host.sh`.
- [ ] Ollama evaluation release asset and SHA-256 still match the pinned version — `.github/workflows/eval.yml`.
- [ ] GitHub Actions SHA pins current (Renovate) — `.github/workflows/*.yml`.

## Benchmarks & measured checkpoints

- [ ] Retrieval benchmark checkpoint still reproduces (dataset commit, Ollama version, embed blob) — `docs/3. Capabilities/3.4. Memory.md` ("Measured checkpoint").
- [ ] `qwen3:4b-instruct` serving-window / context-length note still matches `ollama show` output — `docs/3. Capabilities/3.4. Memory.md`.

## Wrap-up

- [ ] Every unchecked item above has a linked follow-up issue or PR.
- [ ] This template updated for any claim that moved, was added, or was retired.
