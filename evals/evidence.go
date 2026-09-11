package evals

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	EvalRunSpan         = "agentops.eval.run"
	EvalCaseSpan        = "agentops.eval.case"
	EvalScoreSpan       = "agentops.eval.score"
	EvalScoreMetric     = "agentops.eval.score"
	EvalCaseMetric      = "agentops.eval.case.passed"
	EvalTokensMetric    = "agentops.eval.tokens"
	EvalCallsMetric     = "agentops.eval.model_calls"
	EvalRunMetric       = "agentops.eval.run.passed"
	evidenceScopeName   = "agentops-evals"
	evidenceHTTPTimeout = 30 * time.Second
)

var (
	errCreateEvaluationTraceExporter    = errors.New("create evaluation trace exporter")
	errCreateEvaluationMetricExporter   = errors.New("create evaluation metric exporter")
	errExportEvaluationTraces           = errors.New("export evaluation traces")
	errExportEvaluationMetrics          = errors.New("export evaluation metrics")
	errShutdownEvaluationTraceExporter  = errors.New("shutdown evaluation trace exporter")
	errShutdownEvaluationMetricExporter = errors.New("shutdown evaluation metric exporter")
	errShutdownEvaluationTelemetry      = errors.New("shutdown evaluation telemetry exporters")
)

type RunEvidence struct {
	RunID     string
	Model     string
	EvalSet   string
	Transport string
	Source    SourceEvidence
}

type CaseEvidence struct {
	CaseID string
	Sample int
}

type CaseOutcome struct {
	Passed bool
	Usage  Usage
}

type RunOutcome struct {
	Passed bool
}

type EndCase func(CaseOutcome)

type EndRun func(RunOutcome)

type EvidenceRecorder interface {
	StartRun(context.Context, RunEvidence) (context.Context, EndRun, error)
	StartCase(context.Context, CaseEvidence) (context.Context, EndCase, error)
	RecordScore(context.Context, Score) error
	Shutdown(context.Context) error
}

type Recorder struct {
	tracer         trace.Tracer
	scoreGauge     metric.Float64Gauge
	caseGauge      metric.Int64Gauge
	tokensGauge    metric.Int64Gauge
	modelCallGauge metric.Int64Gauge
	runGauge       metric.Int64Gauge
	shutdown       func(context.Context) error
}

type evidenceContext struct {
	caseInfo *CaseEvidence
	run      RunEvidence
}

type evidenceContextKey struct{}

type sanitizedTraceExporter struct{ sdktrace.SpanExporter }

func (exporter sanitizedTraceExporter) ExportSpans(
	ctx context.Context,
	spans []sdktrace.ReadOnlySpan,
) error {
	return fixedExporterError(exporter.SpanExporter.ExportSpans(ctx, spans), errExportEvaluationTraces)
}

func (exporter sanitizedTraceExporter) Shutdown(ctx context.Context) error {
	return fixedExporterError(exporter.SpanExporter.Shutdown(ctx), errShutdownEvaluationTraceExporter)
}

type sanitizedMetricExporter struct{ sdkmetric.Exporter }

func (exporter sanitizedMetricExporter) Export(
	ctx context.Context,
	metrics *metricdata.ResourceMetrics,
) error {
	return fixedExporterError(exporter.Exporter.Export(ctx, metrics), errExportEvaluationMetrics)
}

func (exporter sanitizedMetricExporter) ForceFlush(ctx context.Context) error {
	return fixedExporterError(exporter.Exporter.ForceFlush(ctx), errExportEvaluationMetrics)
}

func (exporter sanitizedMetricExporter) Shutdown(ctx context.Context) error {
	return fixedExporterError(exporter.Exporter.Shutdown(ctx), errShutdownEvaluationMetricExporter)
}

func fixedExporterError(err, local error) error {
	if err != nil {
		return local
	}
	return nil
}

func NewRecorder(tracerProvider trace.TracerProvider, meterProvider metric.MeterProvider) (*Recorder, error) {
	if tracerProvider == nil || meterProvider == nil {
		return nil, errors.New("evidence tracer and meter providers are required")
	}
	meter := meterProvider.Meter(evidenceScopeName)
	scoreGauge, err := meter.Float64Gauge(EvalScoreMetric, metric.WithUnit("1"), metric.WithDescription("Evaluation score, one when passing"))
	if err != nil {
		return nil, fmt.Errorf("create score gauge: %w", err)
	}
	caseGauge, err := meter.Int64Gauge(EvalCaseMetric, metric.WithUnit("1"), metric.WithDescription("Evaluation case outcome"))
	if err != nil {
		return nil, fmt.Errorf("create case gauge: %w", err)
	}
	tokensGauge, err := meter.Int64Gauge(EvalTokensMetric, metric.WithUnit("{token}"), metric.WithDescription("Tokens consumed by an evaluation case"))
	if err != nil {
		return nil, fmt.Errorf("create tokens gauge: %w", err)
	}
	modelCallGauge, err := meter.Int64Gauge(EvalCallsMetric, metric.WithUnit("{call}"), metric.WithDescription("Model calls made by an evaluation case"))
	if err != nil {
		return nil, fmt.Errorf("create model-call gauge: %w", err)
	}
	runGauge, err := meter.Int64Gauge(EvalRunMetric, metric.WithUnit("1"), metric.WithDescription("Evaluation run outcome"))
	if err != nil {
		return nil, fmt.Errorf("create run gauge: %w", err)
	}
	return &Recorder{
		tracer:         tracerProvider.Tracer(evidenceScopeName),
		scoreGauge:     scoreGauge,
		caseGauge:      caseGauge,
		tokensGauge:    tokensGauge,
		modelCallGauge: modelCallGauge,
		runGauge:       runGauge,
		shutdown:       func(context.Context) error { return nil },
	}, nil
}

func NewOTLPRecorder(ctx context.Context, endpoint string) (*Recorder, error) {
	traceEndpoint, metricEndpoint, err := otlpSignalEndpoints(endpoint)
	if err != nil {
		return nil, err
	}
	httpClient := clientWithoutRedirects(nil, evidenceHTTPTimeout)
	traceDelegate, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(traceEndpoint),
		otlptracehttp.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, errCreateEvaluationTraceExporter
	}
	traceExporter := sanitizedTraceExporter{SpanExporter: traceDelegate}
	metricDelegate, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(metricEndpoint),
		otlpmetrichttp.WithHTTPClient(httpClient),
	)
	if err != nil {
		_ = traceExporter.Shutdown(ctx)
		return nil, errCreateEvaluationMetricExporter
	}
	metricExporter := sanitizedMetricExporter{Exporter: metricDelegate}
	evalResource := resource.NewSchemaless(attribute.String("service.name", "agentops-evals"))
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(evalResource),
		sdktrace.WithBatcher(traceExporter),
	)
	metricReader := sdkmetric.NewPeriodicReader(metricExporter)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(evalResource),
		sdkmetric.WithReader(metricReader),
	)
	recorder, err := NewRecorder(tracerProvider, meterProvider)
	if err != nil {
		_ = errors.Join(tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
		return nil, err
	}
	recorder.shutdown = func(shutdownCtx context.Context) error {
		if errors.Join(tracerProvider.Shutdown(shutdownCtx), meterProvider.Shutdown(shutdownCtx)) != nil {
			return errShutdownEvaluationTelemetry
		}
		return nil
	}
	return recorder, nil
}

func otlpSignalEndpoints(endpoint string) (string, string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || !dialableURLPort(parsed) ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("EVAL_OTEL_EXPORTER_OTLP_ENDPOINT must be an http(s) base URL without credentials, path, query, or fragment")
	}
	// OTel 1.45 stopped appending signal paths to WithEndpointURL. Build the
	// standard OTLP/HTTP paths here so the eval-only base URL remains stable.
	traceEndpoint, err := url.JoinPath(endpoint, "v1/traces")
	if err != nil {
		return "", "", fmt.Errorf("build evaluation trace endpoint: %w", err)
	}
	metricEndpoint, err := url.JoinPath(endpoint, "v1/metrics")
	if err != nil {
		return "", "", fmt.Errorf("build evaluation metric endpoint: %w", err)
	}
	return traceEndpoint, metricEndpoint, nil
}

func dialableURLPort(parsed *url.URL) bool {
	portText := parsed.Port()
	if portText == "" {
		// URL.Port cannot distinguish no port from an explicit empty port.
		return !strings.HasSuffix(parsed.Host, ":")
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535
}

func NewNoopRecorder() (*Recorder, error) {
	return NewRecorder(otel.GetTracerProvider(), otel.GetMeterProvider())
}

func (r *Recorder) StartRun(ctx context.Context, run RunEvidence) (context.Context, EndRun, error) {
	if run.RunID == "" || run.Model == "" || run.EvalSet == "" || run.Transport == "" {
		return nil, nil, errors.New("run evidence needs run id, source, model, evalset, and transport")
	}
	if err := run.Source.Validate(); err != nil {
		return nil, nil, err
	}
	attributes := runAttributes(run)
	spanCtx, span := r.tracer.Start(ctx, EvalRunSpan, trace.WithAttributes(attributes...))
	spanCtx = context.WithValue(spanCtx, evidenceContextKey{}, evidenceContext{run: run})
	end := func(outcome RunOutcome) {
		passed := int64(0)
		if outcome.Passed {
			passed = 1
			span.SetStatus(codes.Ok, "evaluation run passed")
		} else {
			span.SetStatus(codes.Error, "evaluation run failed")
		}
		r.runGauge.Record(spanCtx, passed, metric.WithAttributes(attributes...))
		span.End()
	}
	return spanCtx, end, nil
}

func (r *Recorder) StartCase(ctx context.Context, evalCase CaseEvidence) (context.Context, EndCase, error) {
	parent, ok := ctx.Value(evidenceContextKey{}).(evidenceContext)
	if !ok || parent.run.RunID == "" {
		return nil, nil, errors.New("case evidence requires an active evaluation run")
	}
	if evalCase.CaseID == "" || evalCase.Sample < 1 {
		return nil, nil, errors.New("case evidence needs a case id and positive sample")
	}
	attributes := append(runAttributes(parent.run), caseAttributes(evalCase)...)
	spanCtx, span := r.tracer.Start(ctx, EvalCaseSpan, trace.WithAttributes(attributes...))
	parent.caseInfo = &evalCase
	spanCtx = context.WithValue(spanCtx, evidenceContextKey{}, parent)
	end := func(outcome CaseOutcome) {
		passed := int64(0)
		if outcome.Passed {
			passed = 1
			span.SetStatus(codes.Ok, "evaluation case passed")
		} else {
			span.SetStatus(codes.Error, "evaluation case failed")
		}
		options := metric.WithAttributes(attributes...)
		r.caseGauge.Record(spanCtx, passed, options)
		r.tokensGauge.Record(spanCtx, outcome.Usage.TotalTokens, options)
		r.modelCallGauge.Record(spanCtx, outcome.Usage.ModelCalls, options)
		span.End()
	}
	return spanCtx, end, nil
}

func (r *Recorder) RecordScore(ctx context.Context, score Score) error {
	evidence, ok := ctx.Value(evidenceContextKey{}).(evidenceContext)
	if !ok || evidence.caseInfo == nil {
		return errors.New("score evidence requires an active evaluation case")
	}
	if err := validateScoreValue(score.Name, score.Value); err != nil {
		return fmt.Errorf("score evidence: %w", err)
	}
	attributes := append(runAttributes(evidence.run), caseAttributes(*evidence.caseInfo)...)
	attributes = append(attributes,
		attribute.String("agentops.eval.score_name", score.Name),
		attribute.Bool("agentops.eval.passed", score.Passed),
	)
	_, span := r.tracer.Start(ctx, EvalScoreSpan, trace.WithAttributes(attributes...))
	if score.Passed {
		span.SetStatus(codes.Ok, "evaluation score passed")
	} else {
		span.SetStatus(codes.Error, "evaluation score failed")
	}
	r.scoreGauge.Record(ctx, score.Value, metric.WithAttributes(attributes...))
	span.End()
	return nil
}

func (r *Recorder) Shutdown(ctx context.Context) error {
	return r.shutdown(ctx)
}

func runAttributes(run RunEvidence) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("eval.run.id", run.RunID),
		attribute.String("agentops.source.revision", run.Source.Revision),
		attribute.Bool("agentops.source.dirty", run.Source.Dirty),
		attribute.String("gen_ai.request.model", run.Model),
		attribute.String("agentops.eval.evalset", run.EvalSet),
		attribute.String("agentops.eval.transport", run.Transport),
	}
}

func caseAttributes(evalCase CaseEvidence) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("agentops.eval.case_id", evalCase.CaseID),
		attribute.Int("agentops.eval.sample", evalCase.Sample),
	}
}
