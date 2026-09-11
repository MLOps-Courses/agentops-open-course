// Package telemetry is the agent's observability plane: precisely the parts of
// OpenTelemetry that ADK's launcher does not set up for this process, and
// nothing else.
//
// # What ADK already does, and this package therefore does not
//
// launcher.Config.TelemetryOptions feeds google.golang.org/adk/v2/telemetry,
// which builds a TracerProvider and a LoggerProvider from the standard
// OTEL_EXPORTER_OTLP_* variables and installs both globally. Its spans remain
// non-recording unless [SetContentCaptureDefaults] sees explicit risk
// acceptance. There is no TracerProvider here: a second provider installed
// after ADK's packages load would silently orphan its package-level tracer.
//
// # What ADK does not do, and this package therefore does
//
//  1. Metrics. ADK v2.3.0 constructs no MeterProvider at all — its own TODO
//     says so — so [InstallMeterProvider] builds the OTLP metric pipeline the
//     four custom agentops.* counters need to reach Prometheus. See metrics.go.
//  2. Trace-correlated logs. Go's log/slog knows nothing about OpenTelemetry,
//     so [OtelHandler] stamps the active trace and span identifiers onto every
//     record, and [NewHandler] fans each record out to independently redacted,
//     bounded, fail-closed console and OTLP handlers. [SanitizingWriter] applies
//     the same durable-sink policy to ADK's remaining standard-library logs.
//     See slog.go and export.go.
//     Without the stamping, Grafana's trace-to-logs and Loki's
//     derived fields have no identifier to join on and the correlation the
//     whole observability chapter is built on does not resolve.
//  3. The content-capture defaults and fail-closed ADK trace gate. See
//     [SetContentCaptureDefaults].
//
// # Configuration
//
// Exporters are gated by the standard OTLP environment variables. ADK trace
// recording has the additional explicit risk gate documented above. The
// package reads exporter variables through [ExportConfigured], [LogsConfigured]
// and [MetricsConfigured] rather than holding a package singleton.
package telemetry

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.36.0"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
)

// ScopeName is the OpenTelemetry instrumentation scope every signal this agent
// emits for itself shares.
//
// It is the meter name in Chapter 4.3's documented instrument definitions and
// the logger name on the OTLP log path, so a backend groups the agent's own
// telemetry apart from ADK's (which uses its own gcp.vertex.agent scope). The
// resilience package spells the same string for its circuit-breaker counter
// rather than importing this one, because that package deliberately depends on
// nothing else in the module; the two are pinned together by
// TestScopeNameMatchesTheResilienceMeter.
const ScopeName = "agentops.agent"

// ServiceName is the service.name resource attribute every signal carries.
//
// Tempo, Loki and Prometheus all key their service selectors on it, and the
// Grafana dashboards ship those selectors, so it is a contract rather than a
// label. It matches the ADK application name the policy plugin governs.
const ServiceName = "agentops-agent"

// The content-capture switches and the sampler value that keeps unsafe ADK
// spans non-recording.
//
// Course invariant: telemetry content stays private by default. ADK Go reads
// only the GenAI variable; this repository deliberately owns the ADK-named
// variable as its exact-literal risk gate because the pinned spans always carry
// tool payloads and may carry raw errors.
const (
	EnvADKCaptureMessageContent   = "ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS"
	EnvGenAICaptureMessageContent = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"
	EnvTracesSampler              = "OTEL_TRACES_SAMPLER"
	ContentCaptureDisabled        = "false"
	ContentCaptureEnabled         = "true"
	TraceSamplerDisabled          = "always_off"
)

// The standard OTLP environment variables this package gates on. They are named
// here rather than spelled at each call site because the gating rule — "an
// endpoint for this signal, and the SDK not disabled" — is repeated three times
// and has to stay identical in all three.
const (
	EnvSDKDisabled         = "OTEL_SDK_DISABLED"
	EnvOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	EnvOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// --8<-- [start:content-capture-defaults]
// SetContentCaptureDefaults pins both content-capture switches to "false"
// unless the operator has already chosen a valid value, and disables the
// pinned ADK's unsafe spans unless the operator explicitly accepts their
// content-bearing behavior.
//
// ADK Go v2.3.0 always serializes tool arguments and results onto execute_tool
// spans and records raw error text in exception events. Neither capture switch
// controls those fields, and the ADK provider offers no stable field-filtering
// seam. Therefore false, unset, or malformed
// ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS forces OTEL_TRACES_SAMPLER=always_off,
// overriding any sampler the environment supplied. Malformed values also
// return an error after the boundary is closed. Only the exact literal "true"
// accepts that risk and preserves the operator's sampler choice.
//
// The GenAI switch remains an ordinary default: a truthy value means log-event
// bodies, which the repository sanitizes on both durable log paths, and the
// SPAN_ONLY and SPAN_AND_EVENT values v2.3.0 added reach only spans the gate
// above already holds non-recording. Call this before the launcher builds
// telemetry providers. It is safe and intentional to call it again at the
// provider boundary so direct runtime assembly cannot bypass the fail-closed
// rule.
func SetContentCaptureDefaults() error {
	var problems []error
	pin := func(name, value string) {
		if err := os.Setenv(name, value); err != nil {
			problems = append(problems, fmt.Errorf("pinning %s to %q: %w", name, value, err))
		}
	}

	adkCapture, chosen := os.LookupEnv(EnvADKCaptureMessageContent)
	riskAccepted := chosen && adkCapture == ContentCaptureEnabled
	malformed := chosen && adkCapture != ContentCaptureEnabled && adkCapture != ContentCaptureDisabled
	if !riskAccepted {
		// Normalize malformed values as well as the unset case. A typo in a risk
		// acceptance switch must close the boundary, not silently open it.
		pin(EnvADKCaptureMessageContent, ContentCaptureDisabled)
		pin(EnvTracesSampler, TraceSamplerDisabled)
	}
	if malformed {
		problems = append(problems, fmt.Errorf(
			"%s must be the literal %q or %q",
			EnvADKCaptureMessageContent, ContentCaptureEnabled, ContentCaptureDisabled,
		))
	}

	if _, genAIChosen := os.LookupEnv(EnvGenAICaptureMessageContent); !genAIChosen {
		pin(EnvGenAICaptureMessageContent, ContentCaptureDisabled)
	}
	return errors.Join(problems...)
}

// --8<-- [end:content-capture-defaults]

// sdkDisabled reports the OTEL_SDK_DISABLED kill switch.
//
// The spec defines only "true", but a hand-edited .env reaches this code with
// "1" and "yes" too, and a learner who typed one of those meant to turn export
// off. Accepting the three is deliberate; accepting anything else would make
// "disabled=maybe" a silent on.
func sdkDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSDKDisabled))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// endpointConfigured reports whether any of the named variables holds a value.
func endpointConfigured(names ...string) bool {
	if sdkDisabled() {
		return false
	}
	for _, name := range names {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// ExportConfigured reports whether this process exports any signal at all.
//
// It is the coarse gate: true means at least one of the four endpoints is set,
// so building providers is worth the cost. It does not imply that logs or
// metrics specifically are exported — a traces-only endpoint answers true here
// and false to [LogsConfigured].
func ExportConfigured() bool {
	return endpointConfigured(
		EnvOTLPEndpoint, EnvOTLPTracesEndpoint, EnvOTLPMetricsEndpoint, EnvOTLPLogsEndpoint,
	)
}

// LogsConfigured reports whether log records should be exported over OTLP.
//
// A traces-only endpoint deliberately answers false: the OTLP log path costs a
// redaction pass on every record, and paying it to feed an exporter that has
// nowhere to send is waste. The console handler is unaffected either way.
func LogsConfigured() bool {
	return endpointConfigured(EnvOTLPEndpoint, EnvOTLPLogsEndpoint)
}

// MetricsConfigured reports whether metrics should be exported over OTLP.
func MetricsConfigured() bool {
	return endpointConfigured(EnvOTLPEndpoint, EnvOTLPMetricsEndpoint)
}

// Resource returns the resource every signal this process emits is attributed
// to.
//
// One builder for both providers: it is passed to ADK through
// telemetry.WithResource (for spans and log records) and to
// [InstallMeterProvider] (for metrics), so a metric and the span it was
// recorded on can never disagree about which service produced them.
//
// The resource is schemaless on purpose. ADK merges this value into its own
// resource with resource.Merge, which fails outright when two non-empty schema
// URLs disagree — and ADK's default resource carries the SDK's, which moves
// with every OpenTelemetry release. An empty schema URL merges with anything,
// and the attributes here are repository-owned build/source keys plus stable
// service semantic conventions.
func Resource(build buildinfo.Info) *resource.Resource {
	attributes := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(build.Version),
		attribute.String("agentops.build.mode", string(build.Mode)),
		attribute.String("agentops.source.identity", build.SourceIdentity),
		attribute.String("agentops.source.revision", build.Revision),
		attribute.String("agentops.source.tree_digest", build.TreeDigest),
		attribute.Bool("agentops.source.dirty", build.Dirty),
	}
	if !build.Timestamp.IsZero() {
		attributes = append(attributes, attribute.String("agentops.build.timestamp", build.Timestamp.UTC().Format(time.RFC3339)))
	}
	return resource.NewSchemaless(attributes...)
}
