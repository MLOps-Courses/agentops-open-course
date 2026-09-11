package telemetry_test

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/resilience"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

// reader is the package's single manual metric reader.
//
// One reader, installed once in TestMain, because the OpenTelemetry global is
// set-once for delegation purposes: an instrument built against the global
// provider binds to the first real provider installed and keeps it. Installing
// a provider per test would leave every test after the first measuring nothing,
// and it would pass.
var reader = sdkmetric.NewManualReader()

func TestMain(m *testing.M) {
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	os.Exit(m.Run())
}

// collected reads every metric the package has recorded so far, indexed by
// instrument name.
func collected(t *testing.T) map[string]metricdata.Metrics {
	t.Helper()

	var gathered metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &gathered); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, scope := range gathered.ScopeMetrics {
		for _, metric := range scope.Metrics {
			byName[metric.Name] = metric
		}
	}
	return byName
}

// scopeOf returns the instrumentation scope an instrument was created under.
func scopeOf(t *testing.T, name string) string {
	t.Helper()

	var gathered metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &gathered); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	for _, scope := range gathered.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				return scope.Scope.Name
			}
		}
	}
	t.Fatalf("no instrument named %q was recorded", name)
	return ""
}

// sumOf returns a counter's total across every attribute set, and the totals
// split by the direction attribute when the instrument carries one.
func sumOf(t *testing.T, metric metricdata.Metrics) (int64, map[string]int64) {
	t.Helper()

	if metric.Data == nil {
		// Not yet recorded in this process. Zero is the honest reading, and it
		// keeps a "the counter did not move" assertion meaningful whichever
		// order the tests happen to run in.
		return 0, map[string]int64{}
	}
	sum, ok := metric.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s is %T, want an int64 counter", metric.Name, metric.Data)
	}
	if !sum.IsMonotonic {
		t.Errorf("%s is not monotonic; a counter that can go down is a gauge", metric.Name)
	}
	total := int64(0)
	byDirection := map[string]int64{}
	for _, point := range sum.DataPoints {
		total += point.Value
		direction, found := point.Attributes.Value(telemetry.DirectionAttribute)
		if found {
			byDirection[direction.String()] += point.Value
		}
	}
	return total, byDirection
}

// TestTokenCounterIsTheDocumentedContract pins the instrument Chapter 4.3
// publishes by name, unit and dimension. The unit is load-bearing: the
// collector's Prometheus exporter appends it, so `token` is what makes the
// query name agentops_tokens_token_total rather than agentops_tokens_total.
func TestTokenCounterIsTheDocumentedContract(t *testing.T) {
	telemetry.RecordTokenUsage(context.Background(), policy.SessionUsage{
		TurnInput: 30, TurnOutput: 12, SessionInput: 100, SessionOutput: 40, CostEstimate: 0,
	})

	metric, found := collected(t)[telemetry.TokensMetric]
	if !found {
		t.Fatalf("no instrument named %q was recorded", telemetry.TokensMetric)
	}
	if metric.Unit != "token" {
		t.Errorf("unit = %q, want %q", metric.Unit, "token")
	}
	if metric.Description == "" {
		t.Error("the token counter carries no description; a dashboard shows it verbatim")
	}
	_, byDirection := sumOf(t, metric)
	if got := byDirection[telemetry.DirectionInput]; got != 30 {
		t.Errorf("direction=%s total = %d, want 30", telemetry.DirectionInput, got)
	}
	if got := byDirection[telemetry.DirectionOutput]; got != 12 {
		t.Errorf("direction=%s total = %d, want 12", telemetry.DirectionOutput, got)
	}
}

// TestRecordTokenUsageWritesTheSessionAttributesOntoTheSpan holds the other
// half of the token accounting: the running totals and the cost estimate belong
// on the trace, where the session id already is, rather than on a metric label
// that would make every session a new time series.
func TestRecordTokenUsageWritesTheSessionAttributesOntoTheSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting the tracer provider down: %v", err)
		}
	})

	ctx, span := provider.Tracer("test").Start(context.Background(), "turn")
	telemetry.RecordTokenUsage(ctx, policy.SessionUsage{
		TurnInput: 1, TurnOutput: 2, SessionInput: 700, SessionOutput: 300, CostEstimate: 0.25,
	})
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	attributes := map[string]string{}
	for _, attribute := range ended[0].Attributes() {
		attributes[string(attribute.Key)] = attribute.Value.String()
	}
	for key, want := range map[string]string{
		telemetry.SessionInputTokensAttribute:  "700",
		telemetry.SessionOutputTokensAttribute: "300",
		telemetry.SessionTotalTokensAttribute:  "1000",
		telemetry.SessionCostAttribute:         "0.25",
	} {
		if got := attributes[key]; got != want {
			t.Errorf("span attribute %s = %q, want %q", key, got, want)
		}
	}
}

// TestGuardrailAndReportCountersAreTheDocumentedContract covers the two
// remaining instruments the course names.
func TestGuardrailAndReportCountersAreTheDocumentedContract(t *testing.T) {
	telemetry.RecordInjectionsNeutralized(context.Background(), 3)
	telemetry.RecordSchemaFailure(context.Background())

	byName := collected(t)
	for name, want := range map[string]int64{
		telemetry.InjectionsNeutralizedMetric: 3,
		telemetry.SchemaFailuresMetric:        1,
	} {
		metric, found := byName[name]
		if !found {
			t.Errorf("no instrument named %q was recorded", name)
			continue
		}
		if metric.Unit != "1" {
			t.Errorf("%s unit = %q, want %q", name, metric.Unit, "1")
		}
		if total, _ := sumOf(t, metric); total != want {
			t.Errorf("%s total = %d, want %d", name, total, want)
		}
	}
}

// TestRecordInjectionsIgnoresAZeroCount keeps a series from existing at all
// until something is actually neutralized: a counter that is incremented by
// zero looks alive on a dashboard while nothing has happened.
func TestRecordInjectionsIgnoresAZeroCount(t *testing.T) {
	// One real hit first, so the assertion is about a counter that exists
	// rather than about the order these tests happen to run in.
	telemetry.RecordInjectionsNeutralized(context.Background(), 1)
	before, _ := sumOf(t, collected(t)[telemetry.InjectionsNeutralizedMetric])

	telemetry.RecordInjectionsNeutralized(context.Background(), 0)
	telemetry.RecordInjectionsNeutralized(context.Background(), -4)

	after, _ := sumOf(t, collected(t)[telemetry.InjectionsNeutralizedMetric])
	if before != after {
		t.Errorf("a non-positive hit count moved the counter from %d to %d", before, after)
	}
}

// TestEveryAgentInstrumentSharesOneScope is the claim Chapter 4.3 makes when it
// lists four instruments as "the agent's": they have to be findable together.
// The circuit-breaker counter lives in the resilience package, which imports
// nothing from this module on purpose, so its scope name is a duplicated string
// literal — and a duplicated literal is exactly the kind of thing that drifts.
// This is the test that stops it.
func TestEveryAgentInstrumentSharesOneScope(t *testing.T) {
	breaker, err := resilience.NewBreaker(resilience.BreakerConfig{
		Name: "telemetry-scope-probe", FailureThreshold: 1, ResetTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewBreaker() error = %v, want nil", err)
	}
	permit, admitted := breaker.Allow()
	if !admitted {
		t.Fatal("a fresh breaker refused the first call")
	}
	breaker.RecordFailure(context.Background(), permit)

	telemetry.RecordSchemaFailure(context.Background())

	for _, name := range []string{"agentops.circuit.opened_total", telemetry.SchemaFailuresMetric} {
		if got := scopeOf(t, name); got != telemetry.ScopeName {
			t.Errorf("%s was recorded under scope %q, want %q", name, got, telemetry.ScopeName)
		}
	}
}

// TestRecordersFillThePolicyAndReportSeams is a compile-time proof, not a
// runtime one: the three publishers exist to be assigned to the seams the
// policy plane and the report path declare, and a signature drift there is a
// wiring failure that no runtime test would reach.
func TestRecordersFillThePolicyAndReportSeams(t *testing.T) {
	t.Parallel()

	var (
		usage      policy.UsageRecorder          = telemetry.RecordTokenUsage
		injections policy.InjectionRecorder      = telemetry.RecordInjectionsNeutralized
		schema     compose.SchemaFailureRecorder = telemetry.RecordSchemaFailure
	)
	if usage == nil || injections == nil || schema == nil {
		t.Fatal("the recorders must be assignable to the policy and report seams")
	}
}

// TestInstallMeterProviderInstallsNothingWithoutAnEndpoint keeps the
// account-free path silent, and proves the shutdown function is safe to defer
// before its error is checked.
func TestInstallMeterProviderInstallsNothingWithoutAnEndpoint(t *testing.T) {
	clearOTLPEnvironment(t)

	shutdown, err := telemetry.InstallMeterProvider(context.Background(), telemetry.Resource(testBuildInfo()))
	if err != nil {
		t.Fatalf("InstallMeterProvider() error = %v, want nil", err)
	}
	if shutdown == nil {
		t.Fatal("InstallMeterProvider() returned a nil shutdown; `defer shutdown(ctx)` must always be safe")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() error = %v, want nil", err)
	}
}
