package telemetry_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

// capture is a slog.Handler that keeps every record it is given, so a test can
// assert on what a wrapper passed through rather than on formatted text.
type capture struct {
	fail    error
	records []slog.Record
	attrs   []slog.Attr
	groups  []string
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, record slog.Record) error {
	c.records = append(c.records, record)
	return c.fail
}

func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	c.attrs = append(c.attrs, attrs...)
	return c
}

func (c *capture) WithGroup(name string) slog.Handler {
	c.groups = append(c.groups, name)
	return c
}

// only returns the single record the handler captured.
func (c *capture) only(t *testing.T) slog.Record {
	t.Helper()

	if len(c.records) != 1 {
		t.Fatalf("captured %d records, want 1", len(c.records))
	}
	return c.records[0]
}

// attributes flattens a record's attributes into a map for assertions.
func attributes(record slog.Record) map[string]string {
	found := map[string]string{}
	record.Attrs(func(attr slog.Attr) bool {
		found[attr.Key] = attr.Value.String()
		return true
	})
	return found
}

// sampledContext returns a context carrying a valid but non-recording span
// context, which is exactly the shape a propagated traceparent header produces.
func sampledContext(t *testing.T) (context.Context, string, string) {
	t.Helper()

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("parsing the trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("parsing the span id: %v", err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	return trace.ContextWithSpanContext(context.Background(), spanContext),
		traceID.String(), spanID.String()
}

// TestOtelHandlerStampsAPropagatedTraceCorrelation is the load-bearing one: a
// span context arriving in a header is valid but not recording here, and it is
// the case where the log line is this process's only evidence. Gating on
// IsRecording would drop the correlation on every request that came through the
// gateway, which is every request in Chapter 5 onwards.
func TestOtelHandlerStampsAPropagatedTraceCorrelation(t *testing.T) {
	t.Parallel()

	ctx, traceID, spanID := sampledContext(t)
	sink := &capture{}
	handler := telemetry.NewOtelHandler(sink)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "investigating", 0)
	if err := handler.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	stamped := attributes(sink.only(t))
	if got := stamped[telemetry.TraceIDKey]; got != traceID {
		t.Errorf("%s = %q, want %q", telemetry.TraceIDKey, got, traceID)
	}
	if got := stamped[telemetry.SpanIDKey]; got != spanID {
		t.Errorf("%s = %q, want %q", telemetry.SpanIDKey, got, spanID)
	}
}

// TestOtelHandlerStampsNothingWithoutASpan keeps a local run clean: an
// all-zero trace id in every line would be worse than no field at all, because
// a derived field would then link every line to the same nonexistent trace.
func TestOtelHandlerStampsNothingWithoutASpan(t *testing.T) {
	t.Parallel()

	sink := &capture{}
	handler := telemetry.NewOtelHandler(sink)

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "starting", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	stamped := attributes(sink.only(t))
	if _, found := stamped[telemetry.TraceIDKey]; found {
		t.Errorf("stamped %s with no active span: %v", telemetry.TraceIDKey, stamped)
	}
	if _, found := stamped[telemetry.SpanIDKey]; found {
		t.Errorf("stamped %s with no active span: %v", telemetry.SpanIDKey, stamped)
	}
}

// TestOtelHandlerKeepsStampingAfterWithAttrsAndWithGroup is the bug this
// wrapper invites: slog's embedded promotion makes WithAttrs return the *inner*
// handler, so one logger.With() call would silently unwrap the correlation for
// the rest of that logger's life and nothing would fail.
func TestOtelHandlerKeepsStampingAfterWithAttrsAndWithGroup(t *testing.T) {
	t.Parallel()

	ctx, traceID, _ := sampledContext(t)
	sink := &capture{}

	derived := telemetry.NewOtelHandler(sink).
		WithAttrs([]slog.Attr{slog.String("component", "tools")}).
		WithGroup("call")

	record := slog.NewRecord(time.Now(), slog.LevelWarn, "retrying", 0)
	if err := derived.Handle(ctx, record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}
	if got := attributes(sink.only(t))[telemetry.TraceIDKey]; got != traceID {
		t.Errorf("%s = %q after With/WithGroup, want %q", telemetry.TraceIDKey, got, traceID)
	}
}

// TestOtelHandlerIgnoresAnEmptyGroup follows slog's own contract, which says an
// empty group name is a no-op rather than a nameless nesting level.
func TestOtelHandlerIgnoresAnEmptyGroup(t *testing.T) {
	t.Parallel()

	sink := &capture{}
	handler := telemetry.NewOtelHandler(sink)
	if handler.WithGroup("") != slog.Handler(handler) {
		t.Error("WithGroup(\"\") returned a different handler; it must be a no-op")
	}
	if len(sink.groups) != 0 {
		t.Errorf("forwarded an empty group name: %v", sink.groups)
	}
}

// TestMultiHandlerGivesEachDestinationItsOwnRecord is why the fan-out clones:
// slog.Record keeps its attributes in a shared backing array, so a destination
// that appends to its copy would corrupt what the next one reads. The console
// and the exporter must be unable to affect each other.
func TestMultiHandlerGivesEachDestinationItsOwnRecord(t *testing.T) {
	t.Parallel()

	first, second := &capture{}, &capture{}
	fanout, err := telemetry.NewMultiHandler(first, second)
	if err != nil {
		t.Fatalf("NewMultiHandler() error = %v, want nil", err)
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "one message", 0)
	record.AddAttrs(slog.String("incident", "shared"))
	if err := fanout.Handle(context.Background(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	// A destination that mutates its copy must not be visible to the other.
	delivered := first.only(t)
	delivered.AddAttrs(slog.String("added", "by the first destination"))
	if _, leaked := attributes(second.only(t))["added"]; leaked {
		t.Error("one destination's mutation reached the other; the record was not cloned")
	}
}

// TestMultiHandlerReportsEveryFailure keeps one broken destination from hiding
// another, and from stopping the healthy one.
func TestMultiHandlerReportsEveryFailure(t *testing.T) {
	t.Parallel()

	broken := &capture{fail: errors.New("collector unreachable")}
	healthy := &capture{}
	fanout, err := telemetry.NewMultiHandler(broken, healthy)
	if err != nil {
		t.Fatalf("NewMultiHandler() error = %v, want nil", err)
	}

	handleErr := fanout.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "boom", 0))
	if handleErr == nil || !strings.Contains(handleErr.Error(), "collector unreachable") {
		t.Errorf("Handle() error = %v, want it to name the failing destination", handleErr)
	}
	if len(healthy.records) != 1 {
		t.Errorf("the healthy destination received %d records, want 1", len(healthy.records))
	}
}

// TestMultiHandlerRefusesNoDestination catches the wiring mistake that would
// otherwise present as "the process stopped logging".
func TestMultiHandlerRefusesNoDestination(t *testing.T) {
	t.Parallel()

	if _, err := telemetry.NewMultiHandler(); err == nil {
		t.Error("NewMultiHandler() with no destination returned no error")
	}
	if _, err := telemetry.NewMultiHandler(nil, nil); err == nil {
		t.Error("NewMultiHandler(nil, nil) returned no error")
	}
}

// TestNewHandlerRefusesAnIncompleteConfiguration keeps both required seams
// explicit. A missing redactor in particular must not default to a
// pass-through: "we chose not to redact" cannot be the same line of code as "we
// forgot to wire it".
func TestNewHandlerRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := telemetry.NewHandler(telemetry.Config{}); err == nil {
		t.Error("NewHandler() with no console returned no error")
	}
	_, err := telemetry.NewHandler(telemetry.Config{Console: &capture{}})
	if err == nil {
		t.Error("NewHandler() with an unredacted console returned no error")
	}
}

// TestNewHandlerWithoutExportStillCorrelates: with no OTLP log endpoint the
// console is the only sink, and it still gets the identifiers — that is what
// makes a locally collected log file joinable with a locally collected trace.
func TestNewHandlerWithoutExportStillCorrelates(t *testing.T) {
	t.Parallel()

	ctx, traceID, _ := sampledContext(t)
	console := &capture{}
	secret := "password=super-secret-value-123456"
	redact := func(_ context.Context, value any) any {
		if text, ok := value.(string); ok {
			return strings.ReplaceAll(text, "super-secret-value-123456", "<SECRET>")
		}
		return value
	}
	handler, err := telemetry.NewHandler(telemetry.Config{Console: console, Redact: redact})
	if err != nil {
		t.Fatalf("NewHandler() error = %v, want nil", err)
	}
	if err := handler.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, secret, 0)); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}
	if message := console.only(t).Message; strings.Contains(message, "super-secret-value-123456") {
		t.Errorf("console message retained the credential: %q", message)
	}
	if got := attributes(console.only(t))[telemetry.TraceIDKey]; got != traceID {
		t.Errorf("%s = %q, want %q", telemetry.TraceIDKey, got, traceID)
	}
}

func TestSanitizingHandlerCoversLoggerAndRecordAttributes(t *testing.T) {
	t.Parallel()

	console := &capture{}
	const secret = "not-a-real-secret-value"
	redact := func(_ context.Context, value any) any {
		if text, ok := value.(string); ok {
			return strings.ReplaceAll(text, secret, "<SECRET>")
		}
		return value
	}
	handler, err := telemetry.NewSanitizingHandler(console, redact)
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	logger := slog.New(handler).With("bound", "password="+secret)
	logger.ErrorContext(t.Context(), "failed with "+secret, "record", errors.New("token="+secret))

	record := console.only(t)
	if strings.Contains(record.Message, secret) {
		t.Errorf("message retained the synthetic secret: %q", record.Message)
	}
	for key, value := range attributes(record) {
		if strings.Contains(value, secret) {
			t.Errorf("attribute %q retained the synthetic secret: %q", key, value)
		}
	}
}

func TestSanitizingHandlerRedactsAndBoundsKeys(t *testing.T) {
	t.Parallel()

	const secret = "SYNTHETIC_DO_NOT_USE_LOG_KEY_123456"
	redact := func(_ context.Context, value any) any {
		if text, ok := value.(string); ok {
			return strings.ReplaceAll(text, secret, "<SECRET>")
		}
		return value
	}
	console := &capture{}
	handler, err := telemetry.NewSanitizingHandler(console, redact)
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	record := slog.NewRecord(time.Now(), slog.LevelError, "failed", 0)
	record.AddAttrs(
		slog.String("password="+secret, "redacted key"),
		slog.String(strings.Repeat("k", telemetry.MaxExportedChars+100), "bounded key"),
		slog.Any("nested", map[string]any{"token=" + secret: "nested key"}),
	)
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	for key, value := range attributes(console.only(t)) {
		if strings.Contains(key, secret) || strings.Contains(value, secret) {
			t.Fatalf("sanitized attribute retained a sensitive key: %q=%q", key, value)
		}
		if len([]rune(key)) > telemetry.MaxExportedChars {
			t.Fatalf("sanitized attribute key has %d runes, want at most %d", len([]rune(key)), telemetry.MaxExportedChars)
		}
	}
}

func TestSanitizingHandlerFailsClosedWhenRedactionPanics(t *testing.T) {
	t.Parallel()

	console := &capture{}
	handler, err := telemetry.NewSanitizingHandler(console, func(context.Context, any) any {
		panic("synthetic redactor failure")
	})
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	if err := handler.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelError, "raw secret", 0)); err != nil {
		t.Fatalf("Handle() error = %v, want a fail-closed record", err)
	}
	if record := console.only(t); record.Message != telemetry.OmittedBody || record.NumAttrs() != 0 {
		t.Errorf("record = %q with %d attrs, want omitted body and no attrs", record.Message, record.NumAttrs())
	}
}

// leveled is a capture that declines records below its level, which is the shape
// the real fan-out has: the OTLP leaf is bounded by ExportConfig.Level while the
// console keeps whatever level its own handler was built with.
type leveled struct {
	*capture
	min slog.Level
}

func (l leveled) Enabled(_ context.Context, level slog.Level) bool { return level >= l.min }

// failingWriter reports a write error, which is how a full disk or a closed pipe
// reaches the standard-library log path.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// shortWriter accepts a record and then claims it wrote less than it was given,
// which io.Writer treats as a protocol violation rather than a partial success.
type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

// TestMultiHandlerRespectsEachDestinationsLevel is the property that lets the
// two sinks disagree. The console is the log an engineer reads while the
// collector is down, so an exporter that declines debug records must not be able
// to keep them off the terminal, and a level nothing accepts must not cost a
// redaction pass at all.
func TestMultiHandlerRespectsEachDestinationsLevel(t *testing.T) {
	t.Parallel()

	console, export := &capture{}, &capture{}
	fanout, err := telemetry.NewMultiHandler(
		leveled{capture: console, min: slog.LevelDebug},
		leveled{capture: export, min: slog.LevelError},
	)
	if err != nil {
		t.Fatalf("NewMultiHandler() error = %v, want nil", err)
	}

	if !fanout.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = false while a destination still wants it")
	}
	if fanout.Enabled(context.Background(), slog.LevelDebug-1) {
		t.Error("Enabled() = true for a level no destination accepts")
	}
	if handleErr := fanout.Handle(
		context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "routine", 0),
	); handleErr != nil {
		t.Fatalf("Handle() error = %v, want nil", handleErr)
	}
	if len(console.records) != 1 {
		t.Errorf("the console received %d records, want 1", len(console.records))
	}
	if len(export.records) != 0 {
		t.Errorf("the exporter received %d records it declined, want 0", len(export.records))
	}
}

// TestMultiHandlerCarriesWithAttrsAndWithGroupToEveryDestination is the bug a
// fan-out invites: a handler that kept logger.With() context for itself would
// give the console a field the collector never receives, and the two logs would
// stop describing the same event.
func TestMultiHandlerCarriesWithAttrsAndWithGroupToEveryDestination(t *testing.T) {
	t.Parallel()

	first, second := &capture{}, &capture{}
	fanout, err := telemetry.NewMultiHandler(first, second)
	if err != nil {
		t.Fatalf("NewMultiHandler() error = %v, want nil", err)
	}

	if fanout.WithAttrs(nil) != slog.Handler(fanout) {
		t.Error("WithAttrs(nil) returned a different handler; it must be a no-op")
	}
	if fanout.WithGroup("") != slog.Handler(fanout) {
		t.Error(`WithGroup("") returned a different handler; it must be a no-op`)
	}

	bound := []slog.Attr{slog.String("component", "tools")}
	derived := fanout.WithAttrs(bound).WithGroup("call")
	if handleErr := derived.Handle(
		context.Background(), slog.NewRecord(time.Now(), slog.LevelWarn, "retrying", 0),
	); handleErr != nil {
		t.Fatalf("Handle() error = %v, want nil", handleErr)
	}
	for name, sink := range map[string]*capture{"first": first, "second": second} {
		if len(sink.attrs) != 1 || sink.attrs[0].Key != "component" {
			t.Errorf("the %s destination received attrs %v, want the bound component", name, sink.attrs)
		}
		if len(sink.groups) != 1 || sink.groups[0] != "call" {
			t.Errorf("the %s destination received groups %v, want [call]", name, sink.groups)
		}
		if len(sink.records) != 1 {
			t.Errorf("the %s destination received %d records, want 1", name, len(sink.records))
		}
	}
}

// TestWrappersSurfaceTheirDestinationsFailure keeps a broken sink loud. Both
// wrappers sit between slog and something that can fail, and a swallowed error
// here is how "the process stopped logging" becomes invisible.
func TestWrappersSurfaceTheirDestinationsFailure(t *testing.T) {
	t.Parallel()

	// Each wrapper gets its own destination: capture records without a lock, so
	// two parallel subtests sharing one would race instead of proving anything.
	for name, wrap := range map[string]func(*testing.T, slog.Handler) slog.Handler{
		"OtelHandler": func(_ *testing.T, inner slog.Handler) slog.Handler {
			return telemetry.NewOtelHandler(inner)
		},
		"SanitizingHandler": func(t *testing.T, inner slog.Handler) slog.Handler {
			t.Helper()
			handler, err := telemetry.NewSanitizingHandler(inner, func(_ context.Context, value any) any { return value })
			if err != nil {
				t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
			}
			return handler
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler := wrap(t, &capture{fail: errors.New("destination unavailable")})
			handleErr := handler.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelError, "boom", 0))
			if handleErr == nil || !strings.Contains(handleErr.Error(), "destination unavailable") {
				t.Errorf("Handle() error = %v, want it to name the failing destination", handleErr)
			}
		})
	}
}

// TestSanitizingHandlerQualifiesAttributesWithOpenGroups gives the console the
// same flat dotted keys the OTLP path exports, so a field read off a terminal
// line and the same field queried in Loki are spelled identically.
func TestSanitizingHandlerQualifiesAttributesWithOpenGroups(t *testing.T) {
	t.Parallel()

	console := &capture{}
	handler, err := telemetry.NewSanitizingHandler(console, func(_ context.Context, value any) any { return value })
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	if handler.WithAttrs(nil) != slog.Handler(handler) {
		t.Error("WithAttrs(nil) returned a different handler; it must be a no-op")
	}
	if handler.WithGroup("") != slog.Handler(handler) {
		t.Error(`WithGroup("") returned a different handler; it must be a no-op`)
	}

	logger := slog.New(handler).WithGroup("tool").With("name", "restart_service")
	logger.InfoContext(t.Context(), "calling", "target", "warehouse")

	found := attributes(console.only(t))
	for key, want := range map[string]string{
		"tool.name":   "restart_service",
		"tool.target": "warehouse",
	} {
		if got := found[key]; got != want {
			t.Errorf("attribute %s = %q, want %q (all keys: %v)", key, got, want, found)
		}
	}
}

// TestSanitizingHandlerBoundsStringsNestedInStructuredValues mirrors the export
// path's rule on the console: a terminal and a CI log are durable too, so a
// megabyte of model output nested inside a slice must be capped there as well.
func TestSanitizingHandlerBoundsStringsNestedInStructuredValues(t *testing.T) {
	t.Parallel()

	console := &capture{}
	handler, err := telemetry.NewSanitizingHandler(console, func(_ context.Context, value any) any { return value })
	if err != nil {
		t.Fatalf("NewSanitizingHandler() error = %v, want nil", err)
	}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "structured", 0)
	record.AddAttrs(slog.Any("evidence", []any{strings.Repeat("x", telemetry.MaxExportedChars+500), 7}))
	if err := handler.Handle(t.Context(), record); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	var evidence []any
	console.only(t).Attrs(func(attr slog.Attr) bool {
		if attr.Key == "evidence" {
			evidence, _ = attr.Value.Any().([]any)
		}
		return true
	})
	if len(evidence) != 2 {
		t.Fatalf("evidence = %#v, want two members", evidence)
	}
	capped, ok := evidence[0].(string)
	if !ok || len([]rune(capped)) != telemetry.MaxExportedChars {
		t.Errorf("the nested string is %#v, want exactly %d characters", evidence[0], telemetry.MaxExportedChars)
	}
	if !strings.HasSuffix(capped, telemetry.TruncatedSuffix) {
		t.Error("the nested string was not marked truncated")
	}
	// The number keeps its type: a console line a learner greps and a metric a
	// backend aggregates both lose meaning when 7 becomes "7".
	if number, isInt := evidence[1].(int64); !isInt || number != 7 {
		t.Errorf("the nested number is %#v, want int64(7)", evidence[1])
	}
}

// TestSanitizedSinksRefuseAnIncompleteConfiguration keeps both fail-closed seams
// explicit. A nil redactor in particular must never be defaulted to a
// pass-through, because "we chose not to redact" and "we forgot to wire it"
// cannot be the same line of code.
func TestSanitizedSinksRefuseAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	redact := func(_ context.Context, value any) any { return value }
	if _, err := telemetry.NewSanitizingWriter(nil, redact); err == nil {
		t.Error("NewSanitizingWriter() with no destination returned no error")
	}
	if _, err := telemetry.NewSanitizingWriter(&strings.Builder{}, nil); err == nil {
		t.Error("NewSanitizingWriter() with no redactor returned no error")
	}
	if _, err := telemetry.NewSanitizingHandler(nil, redact); err == nil {
		t.Error("NewSanitizingHandler() with no destination returned no error")
	}
	if _, err := telemetry.NewSanitizingHandler(&capture{}, nil); err == nil {
		t.Error("NewSanitizingHandler() with no redactor returned no error")
	}
}

// TestSanitizingWriterReportsWhatItActuallyConsumed pins the io.Writer contract
// this type has to fake. Redaction changes the byte count, so the count returned
// to the standard library must be the caller's own — otherwise log.Print sees a
// short write and retries — while a destination that genuinely wrote less has to
// surface as a failure rather than as silent truncation.
func TestSanitizingWriterReportsWhatItActuallyConsumed(t *testing.T) {
	t.Parallel()

	redact := func(_ context.Context, value any) any {
		text, ok := value.(string)
		if !ok {
			return value
		}
		return strings.ReplaceAll(text, "not-a-real-secret-value", "<SECRET>")
	}

	var output strings.Builder
	writer, err := telemetry.NewSanitizingWriter(&output, redact)
	if err != nil {
		t.Fatalf("NewSanitizingWriter() error = %v, want nil", err)
	}
	// The redacted form is shorter than the original here, which is exactly the
	// case a naive implementation gets wrong.
	original := []byte("token=not-a-real-secret-value\n")
	written, err := writer.Write(original)
	if err != nil || written != len(original) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", written, err, len(original))
	}
	if got := output.Len(); got == len(original) {
		t.Fatalf("the destination received %d bytes; the test no longer exercises a size change", got)
	}

	// An empty write consumes nothing and must not emit a record of its own.
	output.Reset()
	if emptyWritten, emptyErr := writer.Write(nil); emptyWritten != 0 || emptyErr != nil {
		t.Errorf("Write(nil) = (%d, %v), want (0, nil)", emptyWritten, emptyErr)
	}
	if output.Len() != 0 {
		t.Errorf("an empty write produced %q, want nothing", output.String())
	}

	failing, err := telemetry.NewSanitizingWriter(failingWriter{err: errors.New("disk full")}, redact)
	if err != nil {
		t.Fatalf("NewSanitizingWriter() error = %v, want nil", err)
	}
	if _, failErr := failing.Write([]byte("anything\n")); failErr == nil ||
		!strings.Contains(failErr.Error(), "disk full") {
		t.Errorf("Write() error = %v, want it to name the destination failure", failErr)
	}

	short, err := telemetry.NewSanitizingWriter(shortWriter{}, redact)
	if err != nil {
		t.Fatalf("NewSanitizingWriter() error = %v, want nil", err)
	}
	if _, shortErr := short.Write([]byte("anything\n")); !errors.Is(shortErr, io.ErrShortWrite) {
		t.Errorf("Write() error = %v, want %v", shortErr, io.ErrShortWrite)
	}
}

func TestSanitizingWriterProtectsStandardLibraryLogs(t *testing.T) {
	t.Parallel()

	const secret = "not-a-real-secret-value"
	var output strings.Builder
	writer, err := telemetry.NewSanitizingWriter(&output, func(_ context.Context, value any) any {
		text, ok := value.(string)
		if !ok {
			return value
		}
		return strings.ReplaceAll(text, secret, "<SECRET>")
	})
	if err != nil {
		t.Fatalf("NewSanitizingWriter() error = %v, want nil", err)
	}
	original := []byte("provider failed with token=" + secret + "\n")
	written, err := writer.Write(original)
	if err != nil || written != len(original) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", written, err, len(original))
	}
	if strings.Contains(output.String(), secret) {
		t.Errorf("standard log output retained the synthetic secret: %q", output.String())
	}

	output.Reset()
	panicWriter, err := telemetry.NewSanitizingWriter(&output, func(context.Context, any) any {
		panic("synthetic redactor failure")
	})
	if err != nil {
		t.Fatalf("NewSanitizingWriter() error = %v, want nil", err)
	}
	if _, err := panicWriter.Write([]byte("raw secret\n")); err != nil {
		t.Fatalf("Write() error = %v, want a fail-closed record", err)
	}
	if got := output.String(); !strings.Contains(got, telemetry.OmittedBody) || strings.Contains(got, "raw secret") {
		t.Errorf("fail-closed output = %q, want only the omission marker", got)
	}
}
