package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
)

// The custom instrument names. They are a published contract, not labels: the
// course documents each one, the collector's Prometheus exporter renames them
// mechanically (dots to underscores, unit appended, _total for a counter), and
// the shipped alert expressions and dashboards query the renamed form. Renaming
// one here breaks a dashboard in a way nothing fails loudly about, so they are
// declared once, as constants, and asserted by the tests in this package.
const (
	// TokensMetric counts model tokens by direction. Unit "token", so the
	// Prometheus exporter publishes it as agentops_tokens_token_total.
	TokensMetric = "agentops.tokens"
	// InjectionsNeutralizedMetric counts injection markers neutralized in
	// untrusted tool or retrieval output.
	InjectionsNeutralizedMetric = "agentops.guardrails.injections_neutralized"
	// SchemaFailuresMetric counts triage reports that failed schema validation
	// after their one retry — a model-quality signal dashboards watch.
	SchemaFailuresMetric = "agentops.triage_report.schema_failures"
)

// The span attributes token accounting writes alongside the counter.
//
// The counter answers "how many tokens per second is the fleet burning"; these
// answer "what did this conversation cost", which is a per-trace question a
// metric cannot answer without making the session id a label — and a session id
// as a Prometheus label is an unbounded time series.
const (
	SessionInputTokensAttribute  = "agentops.tokens.session.input"
	SessionOutputTokensAttribute = "agentops.tokens.session.output"
	SessionTotalTokensAttribute  = "agentops.tokens.session.total"
	SessionCostAttribute         = "agentops.cost.session.estimate"
)

// The token counter's one dimension. Two values, both known at compile time,
// which is what keeps the series count bounded.
const (
	DirectionAttribute = "direction"
	DirectionInput     = "input"
	DirectionOutput    = "output"
)

// counter builds one Int64Counter lazily against the global meter provider.
//
// Lazily, because the global provider is a no-op until [InstallMeterProvider]
// (or the test) installs a real one, and the API's delegating instrument picks
// the real provider up afterwards on its own. Once, because building the same
// instrument on every call would allocate on every token accounted.
func counter(name, unit, description string) func() metric.Int64Counter {
	return sync.OnceValue(func() metric.Int64Counter {
		built, err := otel.Meter(ScopeName).Int64Counter(
			name, metric.WithUnit(unit), metric.WithDescription(description),
		)
		if err != nil {
			// The OTel API returns a working no-op instrument alongside the
			// error, so the caller keeps running. Losing a metric must never
			// break the rule that produces it — a token budget that stops being
			// enforced because a counter could not be built would turn an
			// observability fault into a spending fault.
			slog.Warn("an agent metric instrument is unavailable",
				"instrument", name, "error", err)
		}
		return built
	})
}

var (
	tokenCounter = counter(TokensMetric, "token",
		"Model tokens consumed by the AgentOps Agent, by direction")
	injectionCounter = counter(InjectionsNeutralizedMetric, "1",
		"Injection markers neutralized in tool/retrieval output")
	schemaFailureCounter = counter(SchemaFailuresMetric, "1",
		"Triage reports that failed schema validation after one retry")
)

// RecordTokenUsage publishes one model response's token accounting.
//
// It satisfies policy.UsageRecorder, which is how it reaches the after-model
// guard: the policy plane owns the rule (accumulate into session state, price
// the totals, refuse when the budget is spent) and this owns the publication.
// Splitting them is what lets the budget be tested without a meter and the
// instrument names be tested without a session.
//
// Both signals are emitted for the same reason they were in the Python track.
// The counter is a rate a dashboard graphs and an alert watches; the span
// attributes are the running totals an engineer reads on the one trace they are
// looking at.
func RecordTokenUsage(ctx context.Context, usage policy.SessionUsage) {
	tokenCounter().Add(ctx, int64(usage.TurnInput),
		metric.WithAttributes(attribute.String(DirectionAttribute, DirectionInput)))
	tokenCounter().Add(ctx, int64(usage.TurnOutput),
		metric.WithAttributes(attribute.String(DirectionAttribute, DirectionOutput)))

	// SpanFromContext returns a non-recording span when there is no active one,
	// and SetAttributes on it is a no-op, so this needs no guard.
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int(SessionInputTokensAttribute, usage.SessionInput),
		attribute.Int(SessionOutputTokensAttribute, usage.SessionOutput),
		attribute.Int(SessionTotalTokensAttribute, usage.SessionInput+usage.SessionOutput),
		attribute.Float64(SessionCostAttribute, usage.CostEstimate),
	)
}

// RecordInjectionsNeutralized publishes how many injection markers one tool
// result carried. It satisfies policy.InjectionRecorder.
func RecordInjectionsNeutralized(ctx context.Context, hits int) {
	if hits <= 0 {
		// The guard already checks this, but a counter that can be incremented
		// by zero produces a series that looks alive while nothing happens.
		return
	}
	injectionCounter().Add(ctx, int64(hits))
}

// RecordSchemaFailure publishes one triage report that failed validation twice.
// It satisfies compose.SchemaFailureRecorder.
func RecordSchemaFailure(ctx context.Context) {
	schemaFailureCounter().Add(ctx, 1)
}

// noopShutdown is what every installer returns when it installed nothing, so a
// caller can always defer the result without a nil check.
func noopShutdown(context.Context) error { return nil }

// InstallMeterProvider builds the OTLP metric pipeline and installs it as the
// process's global meter provider, returning its shutdown function.
//
// ADK v2.3.0 builds a TracerProvider and a LoggerProvider but no MeterProvider
// — setup_otel.go carries the TODO — so without this call every instrument
// above resolves to a no-op and nothing the agent measures about itself ever
// reaches Prometheus, while traces and logs keep flowing. That failure is
// invisible from inside the process, which is exactly why the course ships an
// alert on the token counter going absent.
//
// With no metrics endpoint configured it installs nothing and returns a no-op
// shutdown, matching how ADK treats its own two signals: the account-free local
// path stays silent rather than retrying an export nobody is listening for.
//
// The returned function is never nil, even on error, so `defer shutdown(ctx)`
// is always safe to write before the error is checked.
func InstallMeterProvider(ctx context.Context, res *resource.Resource) (func(context.Context) error, error) {
	if !MetricsConfigured() {
		return noopShutdown, nil
	}
	// Endpoint, headers, protocol and TLS all come from the standard
	// OTEL_EXPORTER_OTLP_* variables, read by the exporter itself. Nothing is
	// configured here that an operator could not also set for the collector.
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return noopShutdown, fmt.Errorf("building the OTLP metric exporter: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(provider)
	return provider.Shutdown, nil
}
