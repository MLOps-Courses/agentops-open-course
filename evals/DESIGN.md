# Evaluation design evidence

This note preserves the verified design facts from the abandoned Python scaffold that preceded the Go harness. The implementation was discarded; these facts remain the compatibility contract.

## Verified wire contract

- ADK Go 2.3.0 exposes the runtime routes `POST /apps/{app}/users/{user}/sessions`, `POST /run`, `POST /run_sse`, and `GET /list-apps`. Its eval routes are unimplemented.
- The REST event shape carries `content.parts`, `usageMetadata`, `partial`, `errorCode`, and `errorMessage`.
- The deployed A2A surface is JSON-RPC, and this harness reads it over `message/stream` rather than `message/send`. The unary reply is a stored Task, and storing a task discards the per-event metadata ADK attaches on the way — including `adk_usage_metadata`, which is where every token a run spent is counted. The stream carries one update per ADK event with that metadata intact, on the same event boundary the REST transport folds. Text, function calls, and function responses arrive in task artifacts or status messages. ADK metadata uses `adk_` prefixes, including `adk_type`, `adk_usage_metadata`, `adk_partial`, and `adk_error_code`.
- `adk_request_confirmation` is the confirmation wrapper emitted for a guarded tool. A confirmation reply is a function response with the original call id and a `{confirmed, payload: {rationale}}` response.

These facts were checked against the local source of `google.golang.org/adk/v2@v2.3.0`, notably `server/adkrest/internal/{models,routers}` and `server/adka2a/v2/{metadata,parts}.go`. They are covered by fixed-wire HTTP tests in this module so an upstream shape change fails offline.

## Folding contract

Both transports normalize captured events and call the same `FoldEvents` function. A scorer sees one `Turn` type containing final text, ordered tool calls, tool evidence, usage, provider failure, and an optional pending confirmation. Streaming partials are retained as raw evidence but excluded from text, tool, and token totals because the final event repeats their content.

Transport equivalence is a load-bearing test: equivalent REST and A2A captures must fold to an equivalent `Turn`. This is the mechanical reason one scorer can grade either deployment surface.

## Scoring contract

- Trajectory matching is binary, required-subset, and in order. Extra calls and optional actual arguments are allowed. An expected empty string accepts an omitted argument because the tool's default supplies it; a non-empty actual value does not satisfy that explicit empty-string expectation.
- Cost is recorded, not scored: per-case `total_tokens` and `model_calls` land in the artifact, and the next run of the same evalset and model warns when the total moves more than 25%.
- Groundedness recognizes incident ids, severities, dataset service names, and dataset runbook slugs. Every entity in the answer must appear in that turn's question or tool-response evidence.
- Structured-report scoring validates the strict report object rather than accepting prose or a second JSON value.
- The gateway judge returns strict JSON `{passed, rationale}`. Its agreement with the balanced 12-case calibration set is measured and printed; reading that number is a human step, not an automated floor.
- The deterministic score names are `trajectory`, `confirmation`, `authority`, `refusal`, and `safety`, plus `groundedness` and `schema` when those are enabled. Approval and write authority have their own names, `confirmation` and `authority`; the injection and PII cases both land on `safety`, because the committed checks record a forbidden tool and a forbidden output and nothing that separates one intent from the other. The confirmation scorer accepts only ADK's exact confirmation-required placeholder plus its pending wrapper; a completed write response fails.
- A conversation that answers a confirmation the agent never proposed is scored, not crashed: the case stops there with a failed `confirmation` score and the run continues. A provider error code is the opposite call — it comes from the gateway or the runtime rather than from the agent's reasoning, so it stops the run rather than being recorded as a model regression.

## Evidence boundary

An evaluation run is its own OpenTelemetry trace. Cases are child spans and scores are metrics. The run carries the checkout revision, its dirty state, the model, and the evalset. The exporter is configured only through `EVAL_OTEL_EXPORTER_OTLP_ENDPOINT`; it never inherits the agent's OTLP destination.

Offline validation proves the parsers, scorers, process/client behavior, import boundary, assets, and in-memory OTel evidence. Real token usage, a real model verdict, and visibility in a live Tempo/Grafana stack require an explicitly run model-backed evaluation and are not implied by the offline suite.
