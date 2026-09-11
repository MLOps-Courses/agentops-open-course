---
name: Docs freshness audit
about: Recurring checklist to re-verify time-sensitive claims (versions, prices, model names, benchmarks) before a release.
title: "docs: freshness audit for <release/date>"
labels: documentation
---

Time-sensitive claims rot silently. Walk this checklist before each release: open the source file, confirm the claim still matches reality (installed version, current price, current model name, re-run benchmark, current foundation status), and check the box or open a fix. Keep this audit open until every box is checked. No workflow reads this checklist: it is the maintainer's own record that the claims were re-verified before a release, not an automated gate. Update this template when a claim moves, is added, or is retired.

## Automated snapshot

The quarterly freshness jobs in `.github/workflows/scan.yml` append a read-only report to this issue. It inventories every `mise.toml` tool, filters stable k3s/Ollama/kagent/mise releases, checks Kubernetes skew and the Ollama asset checksum, resolves kagent charts and arbitrary image digests, checks Wolfi pins, and runs the copied-prose source gate. Every proposal names its upstream authority and required validation tier. It does **not** resolve Go module versions: it only re-reads the `// compatibility hold:` comments, so a newer ADK, a2a-go, or MCP SDK never appears in it and the boxes below are where those are re-checked.

- [ ] Triage every `REVIEW`, `MISMATCH`, `MISSING`, `UNKNOWN`, or `UNAVAILABLE` row in the newest automated comment.
- [ ] Keep upgrades explicit: the reporter must never change a pin, branch, issue state, or pull request.

## Model & provider names

- [ ] `gemini-3.6-flash` is still the current GA model for optional native Gemini calls and its lifecycle remains accurate — `.env.example`, `content/0. Overview/0.5. Provider Options.md`, and `content/2. Agents/2.2. Models.md`.
- [ ] `gemini-3.5-flash` remains the compatibility pin proven with the current agentgateway Vertex tool-result translation, and `infra/scripts/smoke-gke-model.sh` still passes before any GKE model-id change — `content/0. Overview/0.5. Provider Options.md`, `content/6. Platform/6.3. Platform Agents.md`, and the GKE manifests.
- [ ] `qwen3:4b-instruct` is still the default local Ollama model and its weights remain Apache-2.0 licensed — `agents/go/config/config.go`, `content/0. Overview/0.5. Provider Options.md`, `content/6. Platform/6.6. Platform Delivery.md`, and the local manifests.
- [ ] `nomic-embed-text` is still the embedding model — `agents/go/config/config.go`, `content/3. Capabilities/3.4. Memory.md`.

## Prices & cost inputs

- [ ] Recalculate the canonical GKE estimate and its review date from current provider inputs — `content/7. Observability/7.3. Costs.md`.
- [ ] Confirm the current management-fee and free-tier assumptions used by the canonical calculation — `content/7. Observability/7.3. Costs.md`.
- [ ] Confirm the GKE node and disk shape in `infra/gcp` matches the canonical calculation inputs — `content/7. Observability/7.3. Costs.md`.
- [ ] Provider price guidance remains accurate — `content/7. Observability/7.3. Costs.md`, `content/2. Agents/2.2. Models.md`.

## Pinned versions

- [ ] The agentgateway pin and its `202`-on-`DELETE` session-termination quirk still agree with upstream — `mise.toml`, `content/5. Gateway/5.2. MCP Gateway.md`, `content/6. Platform/6.5. Platform Gateway.md`.
- [ ] The kagent chart release and API version still agree with the immutable Helm sources and generated schemas — `infra/helmfile.yaml`, `infra/kagent/schemas`, and Chapter 6.
- [ ] Go, ADK, and OpenTelemetry module pins remain compatible and all three module gates pass — `mise.toml`, `agents/go/go.mod`, `evals/go.mod`, and `tools/go.mod`.
- [ ] Container base-image digests and workflow action pins remain current — `agents/go/Dockerfile` and `.github/workflows/`.
- [ ] The pinned `curlimages/curl` smoke image still resolves for every supported host architecture — `scripts/smoke-host.sh`.
- [ ] Ollama evaluation release asset and SHA-256 still match the pinned version — `.github/workflows/eval.yml`.
- [ ] Docker Buildx remains pinned to the current stable release — `.github/actions/setup-buildx/action.yml`.
- [ ] GitHub Actions SHA pins current (Dependabot) — `.github/workflows/*.yml` and `.github/actions/*/action.yml`.
- [ ] The direct Go module versions the course teaches are still the newest reviewed ones — `google.golang.org/adk/v2`, `github.com/a2aproject/a2a-go/v2`, and `github.com/modelcontextprotocol/go-sdk` in `agents/go/go.mod` and `evals/go.mod`. Dependabot ignores the ADK-owned family and the reporter resolves no module versions, so nothing else proposes these.
- [ ] The vendored Mermaid and FlexSearch bundles are current within the lines they are held on; re-pin with `scripts/vendor-assets.sh <mermaid> <flexsearch>`, whose header carries the recorded reason — `assets/js/vendor/versions.json`.
- [ ] The on-demand `mise x` pins are current — `watchexec` and `k6` in `mise.toml`; the `k6` version also appears four times in `content/7. Observability/7.2. Monitoring.md` and moving it is one coordinated change.
- [ ] The GKE module's google provider constraint and its lock still match a supported upstream release — `infra/gcp/versions.tf` and `infra/gcp/.terraform.lock.hcl`. No ecosystem in `.github/dependabot.yml` watches them.

## Governance & foundation status

`content/8. Community/8.6. AAIF.md` is the most volatile page in the course: it dates donations, names project owners, and prints maturity tiers that the repository cannot pin.

- [ ] AAIF still hosts MCP, agentgateway, and the AGENTS.md convention, and nothing donated since is missing — `content/8. Community/8.6. AAIF.md`.
- [ ] A2A still sits under the Linux Foundation directly rather than a sub-foundation — `content/8. Community/8.6. AAIF.md`.
- [ ] CNCF tiers still correct: Kubernetes, Prometheus, and OpenTelemetry Graduated; kagent still Sandbox — `content/8. Community/8.6. AAIF.md`.
- [ ] Every remaining steward and licence pairing in the map still holds — `content/8. Community/8.6. AAIF.md`.
- [ ] The upstream issue-routing destinations still resolve to the tracker that owns each boundary — `content/8. Community/8.6. AAIF.md`.

## Adjacent-course comparison

`content/0. Overview/0.0. Course.md` states what six other courses do better and dates that reading. A comparison that flatters its author is worthless, and syllabi, access rules, and prices move monthly.

- [ ] Every row still exists, is still reachable at its link, and the strength named for it is still true — `content/0. Overview/0.0. Course.md`.
- [ ] The access note on each row (free, account, API key, cloud account) still matches what the course actually requires today — `content/0. Overview/0.0. Course.md`.
- [ ] The category claim still holds: no newly found course takes one application through the whole build-govern-ship-operate loop with no SaaS, no cloud account, and no fee — `content/0. Overview/0.0. Course.md` and `README.md`.
- [ ] The reading date on the table and in `README.md` was moved to the date of this review, and any row that changed was rewritten rather than silently kept.

## Benchmarks & measured checkpoints

- [ ] The retrieval release checkpoint still reproduces (dataset commit, Ollama version, model manifest/blob, and index provenance) — `content/3. Capabilities/3.4. Memory.md`.
- [ ] `qwen3:4b-instruct` architecture maximum still matches `ollama show`, and the loaded serving window still matches `ollama ps` — `content/3. Capabilities/3.4. Memory.md`.

## Wrap-up

- [ ] Every finding above was corrected or documented accurately before its box was checked.
- [ ] The reviewer and review date are recorded on this issue before the next release is dispatched.
- [ ] This template updated for any claim that moved, was added, or was retired.
