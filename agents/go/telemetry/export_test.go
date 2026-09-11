package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

// recordingExporter keeps every log record the SDK exports, so a test can read
// the record that actually left the process rather than a formatted rendering
// of it.
type recordingExporter struct {
	records []sdklog.Record
	mu      sync.Mutex
}

func (e *recordingExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		// The SDK reuses its record storage, so a copy is mandatory here.
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error   { return nil }
func (e *recordingExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingExporter) exported() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.records
}

// only returns the single record the exporter received.
func (e *recordingExporter) only(t *testing.T) sdklog.Record {
	t.Helper()

	records := e.exported()
	if len(records) != 1 {
		t.Fatalf("exported %d records, want 1", len(records))
	}
	return records[0]
}

// exportSink wires an [telemetry.ExportHandler] to a readable log pipeline.
func exportSink(t *testing.T, redact telemetry.Redactor) (*telemetry.ExportHandler, *recordingExporter) {
	t.Helper()

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
	)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting the logger provider down: %v", err)
		}
	})
	handler, err := telemetry.NewExportHandler(telemetry.ExportConfig{
		Redact: redact,
		Logger: provider.Logger(telemetry.ScopeName),
	})
	if err != nil {
		t.Fatalf("NewExportHandler() error = %v, want nil", err)
	}
	return handler, exporter
}

// passthrough is the redactor for tests about bounding, where redaction itself
// is not the subject.
func passthrough(_ context.Context, value any) any { return value }

// exportedAttributes flattens an exported record's attributes.
func exportedAttributes(record sdklog.Record) map[string]attribute.Value {
	found := map[string]attribute.Value{}
	record.WalkAttributes(func(attr attribute.KeyValue) bool {
		found[string(attr.Key)] = attr.Value
		return true
	})
	return found
}

// TestExportRequiresARedactor: an unredacted record reaching a collector cannot
// be recalled, so a missing redactor is a startup failure, never a default.
func TestExportRequiresARedactor(t *testing.T) {
	t.Parallel()

	if _, err := telemetry.NewExportHandler(telemetry.ExportConfig{}); err == nil {
		t.Error("NewExportHandler() with no redactor returned no error")
	}
}

// TestExportBoundingRecursesThroughStructuredValues is the port of
// test_export_bounding_recurses_through_structured_values: every string inside
// a structured attribute is capped, and non-strings pass through as themselves
// — a boolean stays a boolean and a number stays a number, because a backend
// that receives them as text can no longer aggregate them.
func TestExportBoundingRecursesThroughStructuredValues(t *testing.T) {
	t.Parallel()

	handler, exporter := exportSink(t, passthrough)
	long := strings.Repeat("x", 3000)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "structured", 0)
	record.AddAttrs(slog.Any("evidence", map[string]any{
		"items":   []any{long, 7},
		"enabled": true,
		"short":   "short",
	}))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	evidence, found := exportedAttributes(exporter.only(t))["evidence"]
	if !found {
		t.Fatal("the structured attribute was not exported")
	}
	members := map[string]attribute.Value{}
	for _, member := range evidence.AsMap() {
		members[string(member.Key)] = member.Value
	}
	items := members["items"].AsSlice()
	if len(items) != 2 {
		t.Fatalf("items has %d members, want 2", len(items))
	}
	capped := items[0].AsString()
	if !strings.HasSuffix(capped, telemetry.TruncatedSuffix) {
		t.Errorf("the long string was not marked truncated: %q", capped[max(0, len(capped)-40):])
	}
	if got := len([]rune(capped)); got != telemetry.MaxExportedChars {
		t.Errorf("the capped string is %d characters, want exactly %d", got, telemetry.MaxExportedChars)
	}
	if got := items[1].AsInt64(); got != 7 {
		t.Errorf("the number was exported as %v, want 7", items[1])
	}
	if !members["enabled"].AsBool() {
		t.Errorf("the boolean was exported as %v, want true", members["enabled"])
	}
	if got := members["short"].AsString(); got != "short" {
		t.Errorf("a short string was rewritten to %q", got)
	}
}

// TestExportFailsClosedWhenRedactionFails is the port of
// test_export_filter_fails_closed_when_local_redaction_fails. If the one piece
// of code that inspects untrusted text breaks, the record loses its body and
// every attribute. Falling back to the original would give the single path that
// matters no protection at all.
func TestExportFailsClosedWhenRedactionFails(t *testing.T) {
	t.Parallel()

	broken := func(context.Context, any) any { panic("recognizer unavailable") }
	handler, exporter := exportSink(t, broken)

	record := slog.NewRecord(time.Now(), slog.LevelError, "raw secret", 0)
	record.AddAttrs(slog.String("credential", "never-export-this"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	exported := exporter.only(t)
	if got := exported.Body().AsString(); got != telemetry.OmittedBody {
		t.Errorf("body = %q, want %q", got, telemetry.OmittedBody)
	}
	if got := exported.AttributesLen(); got != 0 {
		t.Errorf("exported %d attributes after a redaction failure, want 0", got)
	}
	// The record the caller wrote is untouched, so the console beside this
	// handler still shows an engineer what actually happened.
	if got := attributes(record)["credential"]; got != "never-export-this" {
		t.Errorf("the original record was mutated: credential = %q", got)
	}
}

// TestExportRedactsTheMessageAttributesAndWithAttrs is the Go-specific half of
// the policy, and the reason this handler owns its attributes instead of
// wrapping another one: slog keeps With() attributes inside the handler, so a
// filter that only inspected the incoming record would export everything a
// caller attached with logger.With(…) completely untouched.
func TestExportRedactsTheMessageAttributesAndWithAttrs(t *testing.T) {
	t.Parallel()

	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}
	handler, exporter := exportSink(t, governance.RedactPersistedValue)

	derived := handler.
		WithAttrs([]slog.Attr{slog.String("operator", "reach me at jane.doe@acme.example")}).
		WithGroup("call")
	record := slog.NewRecord(time.Now(), slog.LevelWarn,
		"calling upstream with api_key=super-secret-value-123456", 0)
	record.AddAttrs(slog.String("header", "Authorization: Bearer abcdefghijklmnop"))
	if err := derived.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	exported := exporter.only(t)
	body := exported.Body().AsString()
	if strings.Contains(body, "super-secret-value-123456") {
		t.Errorf("the credential survived into the exported body: %q", body)
	}
	if !strings.Contains(body, "api_key="+policy.SecretMask) {
		t.Errorf("body = %q, want it to name the masked credential", body)
	}

	attributes := exportedAttributes(exported)
	// The With() attribute is qualified by no group — it was attached before
	// WithGroup — and it is redacted like everything else.
	operator := attributes["operator"].AsString()
	if strings.Contains(operator, "jane.doe@acme.example") {
		t.Errorf("a With() attribute escaped redaction: %q", operator)
	}
	// The record attribute is qualified by the open group.
	header, found := attributes["call.header"]
	if !found {
		t.Fatalf("the record attribute was not exported under its group: %v", attributes)
	}
	if strings.Contains(header.AsString(), "abcdefghijklmnop") {
		t.Errorf("a bearer token survived into an exported attribute: %q", header.AsString())
	}
}

// TestExportCarriesTheTraceCorrelationAndOnlyTheFailureType is the port of the
// export half of
// test_agent_logs_export_one_safe_trace_correlated_copy_without_duplicate_handlers.
//
// Two properties. The exported record carries the trace and span identifiers,
// which the log SDK takes from the same context the handler was given — that is
// what makes Grafana's trace-to-logs resolve. And a record that logged a
// failure carries the failure's type as its own low-cardinality attribute,
// beside the redacted message, which is what lets a dashboard group failures
// without reading any of their text.
func TestExportCarriesTheTraceCorrelationAndOnlyTheFailureType(t *testing.T) {
	t.Parallel()

	ctx, traceID, spanID := sampledContext(t)
	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}
	handler, exporter := exportSink(t, governance.RedactPersistedValue)

	failure := fmt.Errorf("upstream refused: %w", errors.New("token=never-export-this"))
	record := slog.NewRecord(time.Now(), slog.LevelError, "tool call failed", 0)
	record.AddAttrs(slog.Any("error", failure))
	if err := handler.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	exported := exporter.only(t)
	if got := exported.TraceID().String(); got != traceID {
		t.Errorf("exported trace id = %q, want %q", got, traceID)
	}
	if got := exported.SpanID().String(); got != spanID {
		t.Errorf("exported span id = %q, want %q", got, spanID)
	}
	if got := exported.Severity(); got != log.SeverityError {
		t.Errorf("severity = %v, want %v", got, log.SeverityError)
	}

	attributes := exportedAttributes(exported)
	kind, found := attributes[telemetry.ExceptionTypeKey]
	if !found {
		t.Fatalf("no %s attribute on a record that logged an error: %v", telemetry.ExceptionTypeKey, attributes)
	}
	if got := kind.AsString(); got != fmt.Sprintf("%T", failure) {
		t.Errorf("%s = %q, want %q", telemetry.ExceptionTypeKey, got, fmt.Sprintf("%T", failure))
	}
	if text := attributes["error"].AsString(); strings.Contains(text, "never-export-this") {
		t.Errorf("the credential inside the error survived export: %q", text)
	}
}

// TestConsoleAndExportBothReceiveRedactedRecords pins the durable-sink rule:
// terminals and CI capture persist output too, so neither local nor OTLP logs
// may retain the credential while both keep trace correlation.
func TestConsoleAndExportBothReceiveRedactedRecords(t *testing.T) {
	t.Parallel()

	ctx, traceID, _ := sampledContext(t)
	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if shutdownErr := provider.Shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutting the logger provider down: %v", shutdownErr)
		}
	})
	export, err := telemetry.NewExportHandler(telemetry.ExportConfig{
		Redact: governance.RedactPersistedValue,
		Logger: provider.Logger(telemetry.ScopeName),
	})
	if err != nil {
		t.Fatalf("NewExportHandler() error = %v, want nil", err)
	}
	console := &capture{}
	sanitizedConsole, err := telemetry.NewSanitizingHandler(console, governance.RedactPersistedValue)
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	fanout, err := telemetry.NewMultiHandler(sanitizedConsole, export)
	if err != nil {
		t.Fatalf("NewMultiHandler() error = %v, want nil", err)
	}
	logger := slog.New(telemetry.NewOtelHandler(fanout))

	secret := "password=super-secret-value-123456"
	logger.WarnContext(ctx, secret)

	// The console record is redacted and trace-correlated.
	local := console.only(t)
	if strings.Contains(local.Message, "super-secret-value-123456") {
		t.Errorf("the credential reached the console: %q", local.Message)
	}
	if got := attributes(local)[telemetry.TraceIDKey]; got != traceID {
		t.Errorf("console %s = %q, want %q", telemetry.TraceIDKey, got, traceID)
	}

	// The exported record is redacted, and carries the correlation twice over:
	// as the stamped attributes and as the record's own trace fields.
	exported := exporter.only(t)
	if body := exported.Body().AsString(); strings.Contains(body, "super-secret-value-123456") {
		t.Errorf("the credential reached the collector: %q", body)
	}
	if got := exported.TraceID().String(); got != traceID {
		t.Errorf("exported trace id = %q, want %q", got, traceID)
	}
	if got := exportedAttributes(exported)[telemetry.TraceIDKey].AsString(); got != traceID {
		t.Errorf("exported %s attribute = %q, want %q", telemetry.TraceIDKey, got, traceID)
	}
}

// TestExportNormalizesTheValueTypesTheAgentLogs pins the type mapping the whole
// OTLP path rests on. A number stays a number whatever width the caller used,
// because a backend that receives it as text can no longer aggregate it; and
// anything the redactor cannot walk — a time, a byte body, a struct — becomes
// text first, so nothing reaches the collector having never been inspected.
func TestExportNormalizesTheValueTypesTheAgentLogs(t *testing.T) {
	t.Parallel()

	moment := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	handler, exporter := exportSink(t, passthrough)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "typed", 0)
	record.AddAttrs(
		slog.Any("missing", nil),
		slog.Uint64("attempts", 3),
		slog.Float64("ratio", 0.5),
		slog.Time("observed", moment),
		slog.Duration("elapsed", 1500*time.Millisecond),
		slog.Any("body", []byte("runbook body")),
		slog.Any("services", []string{"shipping", "billing"}),
		slog.Any("labels", map[string]string{"env": "local"}),
		slog.Any("build", struct{ Name string }{Name: "agentops"}),
		// A decoded tool result is the realistic source of raw Go numbers: the
		// values inside a map are whatever the caller put there, and slog widens
		// only top-level attributes, never the contents of one.
		slog.Any("nested", map[string]any{
			"int": 1, "int8": int8(2), "int16": int16(3), "int32": int32(4),
			"uint": uint(5), "uint8": uint8(6), "uint16": uint16(7), "uint32": uint32(8),
			"uint64": uint64(9), "float32": float32(0.25),
			"items": []any{"text", int16(10)},
		}),
	)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	attributes := exportedAttributes(exporter.only(t))
	for key, want := range map[string]attribute.Value{
		"missing":  {},
		"attempts": attribute.Int64Value(3),
		"ratio":    attribute.Float64Value(0.5),
		"observed": attribute.StringValue(moment.Format(time.RFC3339Nano)),
		"elapsed":  attribute.StringValue("1.5s"),
		"body":     attribute.StringValue("runbook body"),
		"services": attribute.SliceValue(attribute.StringValue("shipping"), attribute.StringValue("billing")),
		"labels":   attribute.MapValue(attribute.KeyValue{Key: "env", Value: attribute.StringValue("local")}),
		"build":    attribute.StringValue("{agentops}"),
	} {
		got, found := attributes[key]
		if !found {
			t.Errorf("%s was not exported at all", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v (%v), want %v (%v)", key, got, got.Type(), want, want.Type())
		}
	}

	nested := map[string]attribute.Value{}
	for _, member := range attributes["nested"].AsMap() {
		nested[string(member.Key)] = member.Value
	}
	for key, want := range map[string]attribute.Value{
		"int": attribute.Int64Value(1), "int8": attribute.Int64Value(2), "int16": attribute.Int64Value(3),
		"int32": attribute.Int64Value(4), "uint": attribute.Int64Value(5), "uint8": attribute.Int64Value(6),
		"uint16": attribute.Int64Value(7), "uint32": attribute.Int64Value(8), "uint64": attribute.Int64Value(9),
		"float32": attribute.Float64Value(0.25),
		"items":   attribute.SliceValue(attribute.StringValue("text"), attribute.Int64Value(10)),
	} {
		if got := nested[key]; got != want {
			t.Errorf("nested.%s = %v (%v), want %v (%v)", key, got, got.Type(), want, want.Type())
		}
	}
}

// TestExportFlattensGroupsIntoDottedKeys holds the query contract. Every Loki
// selector and alert expression in this repository addresses an attribute by a
// flat name, so an open group has to become a key prefix rather than a nested
// map. The elisions are part of it: slog drops an empty attribute and an empty
// group, and exporting either would publish a key beginning with "." that
// nothing can query.
func TestExportFlattensGroupsIntoDottedKeys(t *testing.T) {
	t.Parallel()

	handler, exporter := exportSink(t, passthrough)
	derived := handler.WithGroup("tool").WithAttrs([]slog.Attr{
		slog.String("name", "restart_service"),
		// An empty attribute and an empty group reach a handler only through
		// With(): Record.AddAttrs drops empty groups before a handler sees them.
		{},
		slog.Group("nothing"),
	})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "grouped", 0)
	record.AddAttrs(
		slog.Group("call",
			slog.String("target", "warehouse"),
			slog.Group("retry", slog.Int("attempt", 2)),
		),
		slog.Attr{},
		// An anonymous group contributes its members without a level of its own.
		slog.Any("", slog.GroupValue(slog.String("loose", "value"))),
	)
	if err := derived.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	got := slices.Sorted(maps.Keys(exportedAttributes(exporter.only(t))))
	want := []string{"tool.call.retry.attempt", "tool.call.target", "tool.loose", "tool.name"}
	if !slices.Equal(got, want) {
		t.Errorf("exported attribute keys = %v, want %v", got, want)
	}
}

// TestExportHandlerNoOpsFollowSlogsContract keeps two cheap invariants slog
// states outright: an empty attribute list and an empty group name change
// nothing. Returning a derived handler instead would copy the attribute slice on
// every logger.With() call, and a nameless group would prefix every later key
// with a bare dot.
func TestExportHandlerNoOpsFollowSlogsContract(t *testing.T) {
	t.Parallel()

	handler, _ := exportSink(t, passthrough)
	if handler.WithAttrs(nil) != slog.Handler(handler) {
		t.Error("WithAttrs(nil) returned a different handler; it must be a no-op")
	}
	if handler.WithGroup("") != slog.Handler(handler) {
		t.Error(`WithGroup("") returned a different handler; it must be a no-op`)
	}
}

// TestExportSeverityMapsWholeRanges is why severityOf compares ranges instead of
// equalities. slog levels are integers a caller may offset — slog.LevelWarn+1 is
// a legitimate custom level — and mapping by equality would export it as an
// unspecified severity, which is precisely the field a dashboard filters on.
func TestExportSeverityMapsWholeRanges(t *testing.T) {
	t.Parallel()

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting the logger provider down: %v", err)
		}
	})
	handler, err := telemetry.NewExportHandler(telemetry.ExportConfig{
		Redact: passthrough,
		Level:  slog.LevelDebug,
		Logger: provider.Logger(telemetry.ScopeName),
	})
	if err != nil {
		t.Fatalf("NewExportHandler() error = %v, want nil", err)
	}

	cases := []struct {
		level slog.Level
		want  log.Severity
	}{
		{level: slog.LevelDebug - 4, want: log.SeverityDebug},
		{level: slog.LevelDebug, want: log.SeverityDebug},
		{level: slog.LevelInfo, want: log.SeverityInfo},
		{level: slog.LevelInfo + 1, want: log.SeverityInfo},
		{level: slog.LevelWarn, want: log.SeverityWarn},
		{level: slog.LevelWarn + 1, want: log.SeverityWarn},
		{level: slog.LevelError, want: log.SeverityError},
		{level: slog.LevelError + 4, want: log.SeverityError},
	}
	for _, testCase := range cases {
		if handleErr := handler.Handle(
			context.Background(), slog.NewRecord(time.Now(), testCase.level, "level-carrying", 0),
		); handleErr != nil {
			t.Fatalf("Handle(%v) error = %v, want nil", testCase.level, handleErr)
		}
	}

	exported := exporter.exported()
	if len(exported) != len(cases) {
		t.Fatalf("exported %d records, want %d", len(exported), len(cases))
	}
	for index, testCase := range cases {
		if got := exported[index].Severity(); got != testCase.want {
			t.Errorf("%v exported severity %v, want %v", testCase.level, got, testCase.want)
		}
		// The text keeps the caller's own name for the level, which is the only
		// place an offset level survives at all.
		if got := exported[index].SeverityText(); got != testCase.level.String() {
			t.Errorf("%v exported severity text %q, want %q", testCase.level, got, testCase.level.String())
		}
	}
}

// TestExportStringifiesAnUnexpectedRedactorResult is the last step of the
// fail-safe. A redactor may rewrite a value, not just mask inside it, and if it
// returns a shape the normalizer never produces then the export falls back to
// capped text — never an unexamined structure sent to the collector.
func TestExportStringifiesAnUnexpectedRedactorResult(t *testing.T) {
	t.Parallel()

	type unexpected struct{ Note string }
	handler, exporter := exportSink(t, func(_ context.Context, value any) any {
		if text, ok := value.(string); ok && text == "rewrite me" {
			return unexpected{Note: strings.Repeat("z", telemetry.MaxExportedChars+50)}
		}
		return value
	})

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "rewritten", 0)
	record.AddAttrs(slog.String("evidence", "rewrite me"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	evidence := exportedAttributes(exporter.only(t))["evidence"]
	if evidence.Type() != attribute.STRING {
		t.Fatalf("evidence exported as %v, want text", evidence.Type())
	}
	if got := len([]rune(evidence.AsString())); got != telemetry.MaxExportedChars {
		t.Errorf("the stringified value is %d characters, want exactly %d", got, telemetry.MaxExportedChars)
	}
	if !strings.HasSuffix(evidence.AsString(), telemetry.TruncatedSuffix) {
		t.Error("the stringified value was not marked truncated")
	}
}

// TestNewHandlerWithExportFeedsBothLeaves assembles the documented shape —
// correlation outside, fan-out in the middle, independently redacting console
// and OTLP leaves — and proves a single logger call reaches both, redacted.
//
// It installs a global OpenTelemetry logger provider because that is the wiring
// the runtime uses: NewHandler names no Logger, so the export leaf resolves the
// global one ADK's launcher installs. The global is set once per process by
// design, so this test is deliberately not parallel and is the only test here
// that touches it.
func TestNewHandlerWithExportFeedsBothLeaves(t *testing.T) {
	ctx, traceID, _ := sampledContext(t)
	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if shutdownErr := provider.Shutdown(context.Background()); shutdownErr != nil {
			t.Errorf("shutting the logger provider down: %v", shutdownErr)
		}
	})
	global.SetLoggerProvider(provider)

	console := &capture{}
	handler, err := telemetry.NewHandler(telemetry.Config{
		Console: console, Redact: governance.RedactPersistedValue, ExportLogs: true,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v, want nil", err)
	}
	slog.New(handler).WarnContext(ctx, "calling upstream with api_key=super-secret-value-123456")

	local := console.only(t)
	if strings.Contains(local.Message, "super-secret-value-123456") {
		t.Errorf("the credential reached the console: %q", local.Message)
	}
	if got := attributes(local)[telemetry.TraceIDKey]; got != traceID {
		t.Errorf("console %s = %q, want %q", telemetry.TraceIDKey, got, traceID)
	}

	exported := exporter.only(t)
	if body := exported.Body().AsString(); strings.Contains(body, "super-secret-value-123456") {
		t.Errorf("the credential reached the collector: %q", body)
	}
	if got := exported.TraceID().String(); got != traceID {
		t.Errorf("exported trace id = %q, want %q", got, traceID)
	}
}

// TestExportSkipsRecordsBelowTheLevel keeps the redaction pass — the most
// expensive thing on this path — from running for records nothing will read.
func TestExportSkipsRecordsBelowTheLevel(t *testing.T) {
	t.Parallel()

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting the logger provider down: %v", err)
		}
	})
	handler, err := telemetry.NewExportHandler(telemetry.ExportConfig{
		Redact: passthrough,
		Level:  slog.LevelWarn,
		Logger: provider.Logger(telemetry.ScopeName),
	})
	if err != nil {
		t.Fatalf("NewExportHandler() error = %v, want nil", err)
	}

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = true on a warn-level handler")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) = false on a warn-level handler")
	}
}
