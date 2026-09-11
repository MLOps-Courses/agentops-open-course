package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestEvidenceRecorderRejectsNonFiniteScores(t *testing.T) {
	t.Parallel()
	recorder, err := NewNoopRecorder()
	if err != nil {
		t.Fatal(err)
	}
	ctx, endRun, err := recorder.StartRun(context.Background(), RunEvidence{
		RunID: "run-1", Source: testSourceEvidence(),
		Model: "model", EvalSet: "set", Transport: "rest",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, endCase, err := recorder.StartCase(ctx, CaseEvidence{CaseID: "case-1", Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		endCase(CaseOutcome{})
		endRun(RunOutcome{})
	})

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		err := recorder.RecordScore(ctx, Score{Name: "trajectory", Value: value, Passed: true})
		if err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("RecordScore(value=%v) error = %v", value, err)
		}
	}
}

func TestEvidenceRecorderRefusesOrphanEvidence(t *testing.T) {
	t.Parallel()

	if _, err := NewRecorder(nil, nil); err == nil ||
		!strings.Contains(err.Error(), "tracer and meter providers are required") {
		t.Fatalf("NewRecorder(nil, nil) error = %v, want a missing-provider refusal", err)
	}
	recorder, err := NewNoopRecorder()
	if err != nil {
		t.Fatal(err)
	}

	// Evidence is only usable if every span and sample can be traced back to the
	// run and case it belongs to, so an unanchored or unidentified emission fails
	// instead of landing in the collector as something nobody can correlate.
	valid := RunEvidence{
		RunID: "run-1", Model: "model", EvalSet: "set", Transport: "rest", Source: testSourceEvidence(),
	}
	for name, mutate := range map[string]func(*RunEvidence){
		"no run id":    func(run *RunEvidence) { run.RunID = "" },
		"no model":     func(run *RunEvidence) { run.Model = "" },
		"no evalset":   func(run *RunEvidence) { run.EvalSet = "" },
		"no transport": func(run *RunEvidence) { run.Transport = "" },
		"no source":    func(run *RunEvidence) { run.Source = SourceEvidence{Revision: "abc"} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := valid
			mutate(&run)
			if _, _, startErr := recorder.StartRun(context.Background(), run); startErr == nil {
				t.Fatalf("StartRun(%+v) error = nil, want an incomplete-identity refusal", run)
			}
		})
	}

	if _, _, caseErr := recorder.StartCase(context.Background(), CaseEvidence{CaseID: "case-1", Sample: 1}); caseErr == nil ||
		!strings.Contains(caseErr.Error(), "active evaluation run") {
		t.Fatalf("StartCase(no run) error = %v, want an unanchored-case refusal", caseErr)
	}
	ctx, endRun, err := recorder.StartRun(context.Background(), valid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { endRun(RunOutcome{}) })
	for name, evidence := range map[string]CaseEvidence{
		"no case id": {Sample: 1},
		"no sample":  {CaseID: "case-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, caseErr := recorder.StartCase(ctx, evidence); caseErr == nil ||
				!strings.Contains(caseErr.Error(), "case id and positive sample") {
				t.Fatalf("StartCase(%+v) error = %v, want an unidentified-case refusal", evidence, caseErr)
			}
		})
	}
	if err := recorder.RecordScore(ctx, NewBinaryScore("trajectory", true, "reason")); err == nil ||
		!strings.Contains(err.Error(), "active evaluation case") {
		t.Fatalf("RecordScore(no case) error = %v, want an unanchored-score refusal", err)
	}
}

func TestEvidenceRecorderEmitsRunCaseScoreTraceAndMetrics(t *testing.T) {
	t.Parallel()
	spanExporter := tracetest.NewInMemoryExporter()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExporter))
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	recorder, err := NewRecorder(tracerProvider, meterProvider)
	if err != nil {
		t.Fatal(err)
	}
	ctx, endRun, err := recorder.StartRun(context.Background(), RunEvidence{
		RunID: "run-1", Source: testSourceEvidence(),
		Model: "model", EvalSet: "set", Transport: "rest",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, endCase, err := recorder.StartCase(ctx, CaseEvidence{CaseID: "case-1", Sample: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordScore(ctx, NewBinaryScore("trajectory", true, "secret rationale")); err != nil {
		t.Fatal(err)
	}
	endCase(CaseOutcome{Passed: true, Usage: Usage{TotalTokens: 10, ModelCalls: 2}})
	endRun(RunOutcome{Passed: true})

	spans := spanExporter.GetSpans()
	names := make([]string, 0, len(spans))
	traceIDs := make(map[string]struct{})
	for _, span := range spans {
		names = append(names, span.Name)
		traceIDs[span.SpanContext.TraceID().String()] = struct{}{}
		for _, field := range span.Attributes {
			if strings.Contains(field.Value.String(), "secret rationale") {
				t.Fatalf("span leaked score reason: %+v", field)
			}
		}
	}
	slices.Sort(names)
	wantNames := []string{EvalCaseSpan, EvalRunSpan, EvalScoreSpan}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) || len(traceIDs) != 1 {
		t.Fatalf("span names/trace ids = %v/%v", names, traceIDs)
	}

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	metricNames := make([]string, 0)
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			metricNames = append(metricNames, metric.Name)
		}
	}
	for _, want := range []string{EvalScoreMetric, EvalCaseMetric, EvalTokensMetric, EvalCallsMetric, EvalRunMetric} {
		if !slices.Contains(metricNames, want) {
			t.Fatalf("metrics %v missing %q", metricNames, want)
		}
	}
}

func TestExpectedSignalExitDoesNotHideOrdinaryFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal semantics are unavailable on Windows")
	}
	failed := exec.Command("sh", "-c", "exit 1")
	if err := failed.Run(); err == nil || expectedSignalExit(err, os.Interrupt) {
		t.Fatalf("ordinary failure classified as the expected interrupt: %v", err)
	}
	interrupted := exec.Command("sh", "-c", "kill -INT $$")
	if err := interrupted.Run(); err == nil || !expectedSignalExit(err, os.Interrupt) {
		t.Fatalf("interrupt exit was not classified as expected: %v", err)
	}
}

func TestImportGraphCannotCrossIntoAgentModule(t *testing.T) {
	if err := ValidateImportBoundary(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
}

func TestOTLPSignalEndpoints(t *testing.T) {
	t.Parallel()
	traces, metrics, err := otlpSignalEndpoints("http://collector.example:4318")
	if err != nil {
		t.Fatal(err)
	}
	if traces != "http://collector.example:4318/v1/traces" || metrics != "http://collector.example:4318/v1/metrics" {
		t.Fatalf("signal endpoints = %q, %q", traces, metrics)
	}
	for _, endpoint := range []string{
		"collector.example:4318",
		"http://user:secret@collector.example:4318",
		"http://collector.example:4318/custom",
		"http://collector.example:4318?tenant=one",
		"http://collector.example:4318#fragment",
		"http://collector.example:",
		"http://collector.example:0",
		"http://collector.example:65536",
	} {
		if _, _, endpointErr := otlpSignalEndpoints(endpoint); endpointErr == nil {
			t.Fatalf("otlpSignalEndpoints(%q) succeeded", endpoint)
		}
	}
}

func TestOTLPRecorderRefusesRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var capturedRequests atomic.Int64
			capture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				capturedRequests.Add(1)
				writer.Header().Set("Content-Type", "application/x-protobuf")
				writer.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(capture.Close)

			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Location", capture.URL+request.URL.Path)
				writer.WriteHeader(status)
			}))
			t.Cleanup(source.Close)

			recorder, err := NewOTLPRecorder(t.Context(), source.URL)
			if err != nil {
				t.Fatalf("NewOTLPRecorder() error = %v, want nil", err)
			}
			emitOTLPTestEvidence(t, recorder)
			shutdownErr := shutdownOTLPTestRecorder(t, recorder)
			if got := capturedRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d evidence request(s), want zero", got)
			}
			if shutdownErr == nil {
				t.Fatal("Shutdown() error = nil, want redirected exports to fail closed")
			}
		})
	}
}

func TestOTLPRecorderShutdownOmitsCollectorBodies(t *testing.T) {
	t.Parallel()

	const marker = "SYNTHETIC_COLLECTOR_BODY_DO_NOT_LOG"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, marker, http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	recorder, err := NewOTLPRecorder(t.Context(), server.URL)
	if err != nil {
		t.Fatalf("NewOTLPRecorder() error = %v, want nil", err)
	}
	emitOTLPTestEvidence(t, recorder)
	shutdownErr := shutdownOTLPTestRecorder(t, recorder)
	if shutdownErr == nil {
		t.Fatal("Shutdown() error = nil, want collector rejection")
	}
	if got, want := shutdownErr.Error(), "shutdown evaluation telemetry exporters"; got != want {
		t.Fatalf("Shutdown() error = %q, want fixed local class %q", got, want)
	}
	if strings.Contains(shutdownErr.Error(), marker) {
		t.Fatal("Shutdown() error retained a collector response body")
	}
}

func emitOTLPTestEvidence(t *testing.T, recorder *Recorder) {
	t.Helper()
	_, endRun, err := recorder.StartRun(t.Context(), RunEvidence{
		RunID: "otlp-security-test", Source: testSourceEvidence(),
		Model: "model", EvalSet: "set", Transport: "rest",
	})
	if err != nil {
		t.Fatalf("StartRun() error = %v, want nil", err)
	}
	endRun(RunOutcome{Passed: true})
}

func shutdownOTLPTestRecorder(t *testing.T, recorder *Recorder) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return recorder.Shutdown(ctx)
}

func TestProcessUtilitiesKeepHarnessIdentityStateAndTelemetry(t *testing.T) {
	t.Parallel()
	directory, cleanup, err := NewIsolatedState()
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(directory+"/probe", []byte("ok"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	environment := processEnvironment(AgentProcessConfig{
		Entrypoint: "agent", DataDir: "data", StateDir: directory, Port: 1234,
		Environment: map[string]string{
			"AGENT_ENTRYPOINT":  "coordinator",
			"AGENT_STATE_DIR":   "shared-state",
			"AGENT_DATA_DIR":    "other-data",
			"AGENT_A2A_PORT":    "9999",
			"OTEL_SDK_DISABLED": "false",
			"EVAL_MARKER":       "ok",
		},
	})
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "OTEL_SDK_DISABLED=true") || strings.Contains(joined, "OTEL_SDK_DISABLED=false") || !strings.Contains(joined, "EVAL_MARKER=ok") {
		t.Fatalf("environment override missing: %s", joined)
	}
	for key, want := range map[string]string{
		"AGENT_ENTRYPOINT": "agent",
		"AGENT_STATE_DIR":  directory,
		"AGENT_DATA_DIR":   "data",
		"AGENT_A2A_PORT":   "1234",
	} {
		matches := 0
		for _, entry := range environment {
			name, value, found := strings.Cut(entry, "=")
			if found && name == key {
				matches++
				if value != want {
					t.Errorf("%s = %q, want harness value %q", key, value, want)
				}
			}
		}
		if matches != 1 {
			t.Errorf("%s appears %d times, want exactly once", key, matches)
		}
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	if _, statErr := os.Stat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("state directory still exists: %v", statErr)
	}
	port, err := FreePort()
	if err != nil || port < 1 {
		t.Fatalf("FreePort = %d, %v", port, err)
	}
}

// TestRunArtifactCarriesNoContent is the sanitization boundary as a test.
//
// The run artifact is the one evaluation output that gets published, so it may
// carry the checkout, the model, the evalset, per-case scores, usage, and timing
// — and nothing a model or a tool wrote. Schema 5 added `duration_ms` and
// `case_consistency`, and the reason both were safe to add is that both are
// numbers; this test is what keeps a future field honest, by driving a run whose
// every content-bearing surface holds a distinct marker and then searching the
// serialized artifact for all of them.
func TestRunArtifactCarriesNoContent(t *testing.T) {
	t.Parallel()

	recorder, err := NewNoopRecorder()
	if err != nil {
		t.Fatal(err)
	}
	markers := map[string]string{
		"question":      "MARKER_QUESTION_alice.smith@example.com",
		"answer":        "MARKER_ANSWER_the-model-said-this",
		"reference":     "MARKER_REFERENCE_expected-text",
		"tool result":   "MARKER_TOOL_RESULT_row-from-the-database",
		"tool argument": "MARKER_TOOL_ARGUMENT_INC-002",
		"rationale":     "MARKER_RATIONALE_approving-because",
	}
	approve := true
	evalset := EvalSet{
		ID: "set", Name: "Set", Digest: strings.Repeat("a", 64),
		Cases: []EvalCase{{
			ID: "case-one",
			Conversation: []Invocation{
				{
					UserContent:   EvalContent{Parts: []EvalPart{{Text: markers["question"]}}},
					FinalResponse: EvalContent{Parts: []EvalPart{{Text: markers["reference"]}}},
					DeterministicChecks: DeterministicChecks{
						RequiredConfirmation: &ExpectedToolCall{
							Name: "restart_service", Args: map[string]any{"name": "inventory"},
						},
					},
					IntermediateData: IntermediateData{ToolUses: []ExpectedToolCall{{
						Name: "restart_service", Args: map[string]any{"name": "inventory"},
					}}},
				},
				{
					Confirmation:  &ConfirmationInput{Approve: &approve, Rationale: markers["rationale"]},
					UserContent:   EvalContent{Parts: []EvalPart{{Text: markers["question"]}}},
					FinalResponse: EvalContent{Parts: []EvalPart{{Text: markers["reference"]}}},
				},
			},
		}},
	}
	pending := Turn{
		Text: markers["answer"],
		ToolCalls: []ToolCall{
			{Name: "restart_service", Args: map[string]any{"name": "inventory", "note": markers["tool argument"]}, CallID: "call-write"},
			{Name: ConfirmationTool, Args: map[string]any{
				"originalFunctionCall": map[string]any{
					"name": "restart_service", "args": map[string]any{"name": "inventory"}, "id": "call-write",
				},
			}},
		},
		ToolResponses: []ToolResponse{{
			Name: "restart_service", CallID: "call-write",
			Response: map[string]any{"status": PendingConfirmationStatus, "detail": markers["tool result"]},
		}},
		Usage: Usage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18, ModelCalls: 1},
	}
	pending.AwaitingConfirmation = &pending.ToolCalls[1]

	artifact, err := Run(t.Context(), RunnerConfig{
		EvalSet: evalset, RunID: "run", Source: testSourceEvidence(),
		Model: ModelEvidence{Provider: "provider", Name: "model"}, Transport: "a2a",
		Repeat: 2, MinimumPassRate: 0, Recorder: recorder,
		ClientFactory: func(context.Context, EvalCase, int) (AgentClient, func() error, error) {
			return &markerClient{pending: pending, answered: Turn{Text: markers["answer"]}}, func() error { return nil }, nil
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(artifact.Summary.CaseConsistency) != 1 || artifact.Summary.CaseConsistency[0].Samples != 2 {
		t.Fatalf("case consistency = %+v, want one case over two samples", artifact.Summary.CaseConsistency)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for surface, marker := range markers {
		if bytes.Contains(encoded, []byte(marker)) {
			t.Fatalf("the run artifact leaked the %s: %s", surface, encoded)
		}
	}
}

// markerClient answers the proposal turn with a pending guarded write and the
// confirmation turn with a plain answer, which is the two-turn shape schema 5
// had to stay content-free across.
type markerClient struct {
	pending  Turn
	answered Turn
}

func (c *markerClient) CreateSession(context.Context) (string, error) { return "session", nil }

func (c *markerClient) Send(context.Context, string, string) (Turn, error) {
	return c.pending, nil
}

func (c *markerClient) Confirm(
	_ context.Context, _ string, turn Turn, _ bool, _ string,
) (Turn, error) {
	if turn.AwaitingConfirmation == nil {
		return Turn{}, errors.New("confirm called on a turn with nothing pending")
	}
	return c.answered, nil
}

func (c *markerClient) Close() error { return nil }
