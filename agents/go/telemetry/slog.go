package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

// The keys the trace correlation is published under.
//
// Snake case, not the dotted OpenTelemetry attribute names, because these are
// read by Loki's derived fields and by a human scanning a console line — and
// Grafana's shipped Loki-to-Tempo link is configured against exactly these two.
// The OTLP log path does not need them: the log SDK stamps the trace and span
// identifiers onto the exported record itself, from the same context.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

// OtelHandler stamps the active span's trace and span identifiers onto every
// record before passing it to the handler it wraps.
//
// This is the join key for the whole observability chapter. Tempo's
// trace-to-logs jumps from a span to the log lines of that trace, and Loki's
// derived fields jump back from a log line to the trace, and neither direction
// resolves unless the identifiers are on the line. Without this type the OTLP
// spans and the log stream are two unrelated piles of data.
type OtelHandler struct {
	slog.Handler
}

// NewOtelHandler wraps inner so every record it receives carries the active
// trace correlation.
func NewOtelHandler(inner slog.Handler) *OtelHandler {
	return &OtelHandler{Handler: inner}
}

// Handle adds the trace correlation, when there is one, and delegates.
func (h *OtelHandler) Handle(ctx context.Context, record slog.Record) error {
	// IsValid, not IsRecording. A span context extracted from an inbound
	// traceparent header is valid but not recording in this process, and those
	// are precisely the requests where correlation earns its keep: the trace
	// exists, it was sampled by the caller, and this process's log line is the
	// only evidence of what happened here. Gating on IsRecording — as the
	// generic reference handler does — would drop the correlation on every
	// propagated request.
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		record.AddAttrs(
			slog.String(TraceIDKey, spanContext.TraceID().String()),
			slog.String(SpanIDKey, spanContext.SpanID().String()),
		)
	}
	if err := h.Handler.Handle(ctx, record); err != nil {
		return fmt.Errorf("handling a trace-correlated log record: %w", err)
	}
	return nil
}

// WithAttrs returns a handler that keeps stamping the correlation.
//
// Overriding this and WithGroup is not optional: the embedded handler's own
// implementations return the *inner* handler, so a single logger.With() call
// would silently unwrap the correlation for the rest of that logger's life.
func (h *OtelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &OtelHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup returns a handler that keeps stamping the correlation.
func (h *OtelHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		// slog's contract: an empty group name is a no-op, and forwarding it
		// would nest every later attribute under a nameless group.
		return h
	}
	return &OtelHandler{Handler: h.Handler.WithGroup(name)}
}

// MultiHandler fans one record out to several handlers.
//
// It is what lets the console and OTLP paths enforce their independent
// redacting, bounding handlers. The two sinks must not share mutable state, so
// every handler gets its own clone of the record: slog.Record keeps its
// attributes in a shared backing array, and a handler that appends to its copy
// would otherwise corrupt what the next one reads.
type MultiHandler struct {
	handlers []slog.Handler
}

// SanitizingHandler redacts and bounds a record before a durable local handler
// receives it. It owns attributes added through logger.With so those values
// cannot bypass the Handle-time policy.
type SanitizingHandler struct {
	handler slog.Handler
	redact  Redactor
	attrs   []exportAttr
	groups  []string
}

// SanitizingWriter protects packages that still use the standard library log
// package, whose global logger writes bytes instead of slog records.
type SanitizingWriter struct {
	destination io.Writer
	redact      Redactor
	guard       sync.Mutex
}

// NewSanitizingWriter wraps a byte-oriented log destination with the same
// fail-closed redaction and bound used for structured console records.
func NewSanitizingWriter(destination io.Writer, redact Redactor) (*SanitizingWriter, error) {
	if destination == nil {
		return nil, errors.New("telemetry: a sanitized writer needs a destination")
	}
	if redact == nil {
		return nil, errors.New("telemetry: a sanitized writer needs a redactor")
	}
	return &SanitizingWriter{destination: destination, redact: redact}, nil
}

func (w *SanitizingWriter) Write(original []byte) (int, error) {
	if len(original) == 0 {
		return 0, nil
	}
	w.guard.Lock()
	defer w.guard.Unlock()

	sanitized := w.safeString(string(original))
	written, err := io.WriteString(w.destination, sanitized)
	if err != nil {
		return 0, fmt.Errorf("writing a sanitized standard log record: %w", err)
	}
	if written != len(sanitized) {
		return 0, io.ErrShortWrite
	}
	// io.Writer reports consumption of the caller's bytes, not the differently
	// sized redacted representation written to the destination.
	return len(original), nil
}

func (w *SanitizingWriter) safeString(original string) (sanitized string) {
	sanitized = OmittedBody + "\n"
	defer func() {
		if recover() != nil {
			sanitized = OmittedBody + "\n"
		}
	}()
	return bounded(fmt.Sprint(safeValue(context.Background(), w.redact, original)))
}

// NewSanitizingHandler wraps a local destination with fail-closed redaction.
func NewSanitizingHandler(handler slog.Handler, redact Redactor) (*SanitizingHandler, error) {
	if handler == nil {
		return nil, errors.New("telemetry: a sanitized handler needs a destination")
	}
	if redact == nil {
		return nil, errors.New("telemetry: a sanitized handler needs a redactor")
	}
	return &SanitizingHandler{handler: handler, redact: redact}, nil
}

func (h *SanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *SanitizingHandler) Handle(ctx context.Context, record slog.Record) error {
	sanitized := h.safeRecord(ctx, record)
	if err := h.handler.Handle(ctx, sanitized); err != nil {
		return fmt.Errorf("handling a sanitized log record: %w", err)
	}
	return nil
}

func (h *SanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	derived := h.clone()
	for _, attr := range attrs {
		derived.attrs = appendFlattened(derived.attrs, groupPrefix(h.groups), attr)
	}
	return derived
}

func (h *SanitizingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	derived := h.clone()
	derived.groups = append(derived.groups, name)
	return derived
}

func (h *SanitizingHandler) clone() *SanitizingHandler {
	return &SanitizingHandler{
		handler: h.handler,
		redact:  h.redact,
		attrs:   slices.Clone(h.attrs),
		groups:  slices.Clone(h.groups),
	}
}

func (h *SanitizingHandler) safeRecord(ctx context.Context, record slog.Record) (sanitized slog.Record) {
	sanitized = slog.NewRecord(record.Time, record.Level, OmittedBody, record.PC)
	defer func() {
		if recover() != nil {
			sanitized = slog.NewRecord(record.Time, record.Level, OmittedBody, record.PC)
		}
	}()

	message := bounded(fmt.Sprint(safeValue(ctx, h.redact, record.Message)))
	sanitized = slog.NewRecord(record.Time, record.Level, message, record.PC)
	collected := slices.Clone(h.attrs)
	prefix := groupPrefix(h.groups)
	record.Attrs(func(attr slog.Attr) bool {
		collected = appendFlattened(collected, prefix, attr)
		return true
	})
	for _, attr := range collected {
		sanitized.AddAttrs(slog.Any(
			safeKey(ctx, h.redact, attr.key),
			boundedAny(safeValue(ctx, h.redact, attr.value)),
		))
	}
	return sanitized
}

func boundedAny(value any) any {
	switch typed := value.(type) {
	case string:
		return bounded(typed)
	case map[string]any:
		boundedMap := make(map[string]any, len(typed))
		for key, item := range typed {
			boundedMap[key] = boundedAny(item)
		}
		return boundedMap
	case []any:
		boundedSlice := make([]any, len(typed))
		for index, item := range typed {
			boundedSlice[index] = boundedAny(item)
		}
		return boundedSlice
	default:
		return value
	}
}

// NewMultiHandler fans records out to every non-nil handler given.
//
// It refuses an empty set rather than returning a handler that silently
// discards everything: "the process logs nowhere" is a startup bug, and it is
// far cheaper to find here than in an incident.
func NewMultiHandler(handlers ...slog.Handler) (*MultiHandler, error) {
	kept := make([]slog.Handler, 0, len(handlers))
	for _, handler := range handlers {
		if handler != nil {
			kept = append(kept, handler)
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("a multi handler needs at least one destination")
	}
	return &MultiHandler{handlers: kept}, nil
}

// Enabled reports whether any destination wants this level.
func (h *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every destination that wants it.
//
// A failing destination does not stop the others and does not hide its failure:
// the errors are joined, so one broken exporter cannot make the console lose a
// line, and a broken console cannot make the export silently stop.
func (h *MultiHandler) Handle(ctx context.Context, record slog.Record) error {
	var problems []error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// WithAttrs returns a handler whose destinations all carry attrs.
func (h *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	derived := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		derived = append(derived, handler.WithAttrs(attrs))
	}
	return &MultiHandler{handlers: derived}
}

// WithGroup returns a handler whose destinations all carry the group.
func (h *MultiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	derived := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		derived = append(derived, handler.WithGroup(name))
	}
	return &MultiHandler{handlers: derived}
}

// Config is everything the logging plane needs from the rest of the runtime.
type Config struct {
	// Console receives every record after fail-closed redaction. Required: a
	// process with no local log is undiagnosable when the collector is the
	// thing that is broken.
	Console slog.Handler

	// Redact removes personal data and credentials from every durable log sink.
	// Required whether or not OTLP export is configured. See [Redactor].
	Redact Redactor

	// Level bounds what reaches the OTLP path. Nil means slog.LevelInfo. The
	// console keeps whatever level its own handler was built with.
	Level slog.Leveler

	// ExportLogs turns the OTLP log path on. Wire it from [LogsConfigured]
	// rather than reading the environment here, so a caller decides once and a
	// test can decide differently without touching the process environment.
	ExportLogs bool
}

// NewHandler assembles the process's slog handler.
//
// The shape is: correlation on the outside, fan-out in the middle, and
// independently redacting console and exporter leaves.
//
//	OtelHandler → MultiHandler ─┬→ console (redacted, bounded, fail-closed)
//	                            └→ export  (redacted, bounded, fail-closed)
//
// Correlation is outermost so both leaves see the identifiers. Local terminals,
// container stdout and CI capture are durable in practice, so neither leaf may
// receive raw credentials, PII, provider bodies or endpoint values.
//
// Install the result with slog.SetDefault.
func NewHandler(cfg Config) (slog.Handler, error) {
	if cfg.Console == nil {
		return nil, errors.New("telemetry: Config.Console is required")
	}
	console, err := NewSanitizingHandler(cfg.Console, cfg.Redact)
	if err != nil {
		return nil, err
	}
	if !cfg.ExportLogs {
		// No OTLP log endpoint: the console alone, still trace-correlated,
		// because `adk web`'s own trace view and a local Tempo both benefit and
		// the stamping costs one context lookup.
		return NewOtelHandler(console), nil
	}
	export, err := NewExportHandler(ExportConfig{Redact: cfg.Redact, Level: cfg.Level})
	if err != nil {
		return nil, err
	}
	fanout, err := NewMultiHandler(console, export)
	if err != nil {
		return nil, err
	}
	return NewOtelHandler(fanout), nil
}
