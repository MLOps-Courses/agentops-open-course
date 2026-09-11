package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// Redactor removes personal data and credentials from a value that is about to
// reach a durable local or remote log sink.
//
// It is the seam the policy package fills: (*policy.Policy).RedactPersistedValue
// satisfies it directly. The persisted policy, not the boundary one, because a
// log record shipped to a collector is durable in exactly the way an audit row
// is — once it is in Loki it cannot be unsent.
//
// It takes the record's context because policy callbacks share the request
// cancellation boundary even though deterministic redaction needs no network.
type Redactor func(ctx context.Context, value any) any

// The export bounds. Every exported string is capped, and a capped string is
// exactly MaxExportedChars long including the suffix, so a backend's own limits
// are never the thing that decides what a record says.
const (
	// MaxExportedChars caps every string on the OTLP path. A model response or
	// a SQL error can be megabytes; a log pipeline that accepts them drops
	// batches under load, which loses the records that mattered.
	MaxExportedChars = 2048
	// TruncatedSuffix marks a string the cap shortened, so a reader can tell a
	// truncated value from a value that was genuinely that long.
	TruncatedSuffix = "... [truncated]"
	// OmittedBody replaces the whole record body when redaction itself fails.
	OmittedBody = "[log body omitted: local redaction unavailable]"
	// ExceptionTypeKey carries the failure's Go type on a record that logged an
	// error. It is the OpenTelemetry semantic convention key.
	ExceptionTypeKey = "exception.type"
)

// ExportConfig configures the OTLP log path.
type ExportConfig struct {
	// Redact is required. A nil redactor is refused rather than defaulted to a
	// pass-through: "we forgot to wire redaction" and "we chose not to redact"
	// must not be the same line of code.
	Redact Redactor

	// Level bounds what is exported. Nil means slog.LevelInfo.
	Level slog.Leveler

	// Logger overrides the destination. Nil uses the global OpenTelemetry
	// logger provider, which is what ADK's launcher installs — and which is a
	// no-op until it does, so records emitted early are dropped rather than
	// buffered. Tests set this to a provider they can read back.
	Logger log.Logger
}

// ExportHandler is the slog handler on the OTLP path: it redacts, bounds and
// fails closed before a record leaves the process.
//
// It owns its accumulated attributes rather than delegating to an inner
// handler, and that is a security property rather than a style choice. slog
// stores the attributes from With() inside the handler, not on the record, so a
// handler that only filtered what arrives in Handle would export everything a
// caller attached with logger.With("password", …) untouched.
//
// Four rules, each of which the Python track's export filter also enforced:
//
//  1. Every string — the message, every attribute, every string nested inside a
//     map or slice — goes through the redactor and is then capped.
//  2. A record that logged an error carries only the error's type under
//     [ExceptionTypeKey] in addition to the redacted text; Go errors carry no
//     stack, so there is no traceback body to strip.
//  3. If redaction fails, the body becomes [OmittedBody] and every attribute is
//     dropped. The original is never exported as a fallback.
//  4. Nothing here mutates the record; the sibling console handler applies its
//     own fail-closed sanitization to an independent record clone.
type ExportHandler struct {
	logger log.Logger
	redact Redactor
	level  slog.Leveler

	// attrs are the accumulated With() attributes, already qualified by the
	// groups that were open when they were added.
	attrs []exportAttr
	// groups is the currently open group path, applied to record attributes.
	groups []string
}

// exportAttr is one flattened attribute: a fully qualified key and the value as
// it was written, before redaction.
type exportAttr struct {
	value any
	key   string
}

// NewExportHandler builds the OTLP log handler.
func NewExportHandler(cfg ExportConfig) (*ExportHandler, error) {
	if cfg.Redact == nil {
		return nil, errors.New(
			"telemetry: ExportConfig.Redact is required; an unredacted record " +
				"reaching the collector cannot be recalled",
		)
	}
	logger := cfg.Logger
	if logger == nil {
		// The global provider's logger is a delegate: it is a no-op until a
		// provider is installed and then updates itself in place, so resolving
		// it now — before ADK's launcher has run — is correct.
		logger = global.Logger(ScopeName)
	}
	level := cfg.Level
	if level == nil {
		level = slog.LevelInfo
	}
	return &ExportHandler{logger: logger, redact: cfg.Redact, level: level}, nil
}

// Enabled reports whether a record at this level is worth building.
//
// It asks the OpenTelemetry logger too, so with no provider installed the whole
// redaction pass is skipped rather than performed and thrown away.
func (h *ExportHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level < h.level.Level() {
		return false
	}
	return h.logger.Enabled(ctx, log.EnabledParameters{Severity: severityOf(level)})
}

// Handle redacts, bounds and emits one record.
func (h *ExportHandler) Handle(ctx context.Context, record slog.Record) error {
	emitted := log.Record{}
	emitted.SetTimestamp(record.Time)
	emitted.SetObservedTimestamp(time.Now())
	emitted.SetSeverity(severityOf(record.Level))
	emitted.SetSeverityText(record.Level.String())

	// Collected before redaction, because redaction replaces an error value
	// with its text and the dynamic type is gone by then.
	collected := h.collect(record)
	exceptionType := firstErrorType(collected)

	body, attributes := h.safeValues(ctx, record.Message, collected)
	emitted.SetBody(body)
	if exceptionType != "" {
		attributes = append(attributes, attribute.String(ExceptionTypeKey, exceptionType))
	}
	emitted.AddAttributes(attributes...)

	h.logger.Emit(ctx, emitted)
	return nil
}

// WithAttrs returns a handler carrying attrs, qualified by the open groups.
func (h *ExportHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	derived := h.clone()
	for _, attr := range attrs {
		derived.attrs = appendFlattened(derived.attrs, groupPrefix(h.groups), attr)
	}
	return derived
}

// WithGroup returns a handler that qualifies later attributes with name.
func (h *ExportHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	derived := h.clone()
	derived.groups = append(derived.groups, name)
	return derived
}

// clone copies the handler's accumulated state.
//
// slices.Clone rather than a shared backing array: two loggers derived from the
// same parent must not be able to overwrite each other's attributes, which is
// what append onto a shared array does.
func (h *ExportHandler) clone() *ExportHandler {
	return &ExportHandler{
		logger: h.logger,
		redact: h.redact,
		level:  h.level,
		attrs:  slices.Clone(h.attrs),
		groups: slices.Clone(h.groups),
	}
}

// collect flattens the handler's attributes and the record's into one list.
func (h *ExportHandler) collect(record slog.Record) []exportAttr {
	collected := slices.Clone(h.attrs)
	prefix := groupPrefix(h.groups)
	record.Attrs(func(attr slog.Attr) bool {
		collected = appendFlattened(collected, prefix, attr)
		return true
	})
	return collected
}

// safeValues applies the redaction policy, failing closed.
//
// The recover is the whole point of the split: the redactor is the only code
// here that inspects untrusted text, and if it ever panics — a bad pattern, a
// nil map, a future recognizer — the record must lose its body rather than
// export it unredacted. Returning the original on failure would make the one
// path that matters the one path with no protection.
func (h *ExportHandler) safeValues(
	ctx context.Context, message string, collected []exportAttr,
) (body attribute.Value, attributes []attribute.KeyValue) {
	defer func() {
		// Nothing is logged from here: this handler is on the logging path, and
		// a diagnostic emitted during a failed export would re-enter it.
		if recovered := recover(); recovered != nil {
			body, attributes = attribute.StringValue(OmittedBody), nil
		}
	}()

	body = boundedLogValue(safeValue(ctx, h.redact, message))
	attributes = make([]attribute.KeyValue, 0, len(collected))
	for _, attr := range collected {
		attributes = append(attributes, attribute.KeyValue{
			Key:   attribute.Key(safeKey(ctx, h.redact, attr.key)),
			Value: boundedLogValue(safeValue(ctx, h.redact, attr.value)),
		})
	}
	return body, attributes
}

// safeValue recursively redacts values and map keys before either durable sink
// sees them. Attribute keys are data too: a dynamic key can contain the same
// credential or personal-data shapes as a value.
func safeValue(ctx context.Context, redact Redactor, value any) any {
	switch typed := normalize(value).(type) {
	case map[string]any:
		safe := make(map[string]any, len(typed))
		for key, item := range typed {
			safe[safeKey(ctx, redact, key)] = safeValue(ctx, redact, item)
		}
		return safe
	case []any:
		safe := make([]any, len(typed))
		for index, item := range typed {
			safe[index] = safeValue(ctx, redact, item)
		}
		return safe
	default:
		return redact(ctx, typed)
	}
}

func safeKey(ctx context.Context, redact Redactor, key string) string {
	return bounded(fmt.Sprint(redact(ctx, normalize(key))))
}

// groupPrefix joins the open group path into a key prefix.
//
// Dotted keys rather than nested maps: Loki, Prometheus and every alert
// expression in this repository address an attribute by a flat name, and a
// nested map would have to be flattened again at query time by whoever reads
// it. The record's own structure is preserved for the values that are genuinely
// structured — a map attribute still exports as a map.
func groupPrefix(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	return strings.Join(groups, ".") + "."
}

// appendFlattened adds one attribute, expanding groups into dotted keys.
func appendFlattened(dst []exportAttr, prefix string, attr slog.Attr) []exportAttr {
	// Resolve first: a slog.LogValuer computes its value lazily, and an
	// unresolved one would be exported as an opaque type name.
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		// slog's contract elides an empty attribute rather than exporting "".
		return dst
	}
	if attr.Value.Kind() == slog.KindGroup {
		members := attr.Value.Group()
		if len(members) == 0 {
			// An empty group is elided too, name included.
			return dst
		}
		nested := prefix
		if attr.Key != "" {
			nested = prefix + attr.Key + "."
		}
		for _, member := range members {
			dst = appendFlattened(dst, nested, member)
		}
		return dst
	}
	return append(dst, exportAttr{key: prefix + attr.Key, value: attr.Value.Any()})
}

// firstErrorType returns the Go type of the first error-valued attribute.
//
// The type, not the message: it is the low-cardinality half of a failure, the
// half a dashboard can group by, and it is what the OpenTelemetry convention
// asks for. The message travels as the attribute's own redacted value.
func firstErrorType(collected []exportAttr) string {
	for _, attr := range collected {
		if failure, ok := attr.value.(error); ok {
			return fmt.Sprintf("%T", failure)
		}
	}
	return ""
}

// normalize rewrites a value into the shapes the redactor understands.
//
// The redactor walks strings, maps and slices and returns anything else
// unchanged — so a value of some other type would reach the exporter having
// never been inspected, and be stringified there. Stringifying *first* closes
// that gap: whatever a caller attached, the redactor sees text it can mask.
// Numbers and booleans stay themselves, because they carry no free text and a
// number exported as a string is a metric a backend can no longer aggregate.
func normalize(value any) any {
	switch typed := value.(type) {
	case nil, string, bool, int64, float64:
		return value
	case int:
		return int64(typed)
	case int8:
		return int64(typed)
	case int16:
		return int64(typed)
	case int32:
		return int64(typed)
	case uint:
		return int64(typed) //nolint:gosec // a token or byte count never approaches the int64 ceiling.
	case uint8:
		return int64(typed)
	case uint16:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		return int64(typed) //nolint:gosec // same bound as uint above.
	case float32:
		return float64(typed)
	case error:
		return typed.Error()
	case time.Time:
		// RFC 3339 rather than Go's default layout: it is what every log
		// backend parses, and it sorts lexicographically.
		return typed.Format(time.RFC3339Nano)
	case fmt.Stringer:
		return typed.String()
	case []byte:
		// Bytes are text far more often than not in this agent — a JSON tool
		// result, a runbook body — so they are redacted as text.
		return string(typed)
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalize(item)
		}
		return normalized
	case map[string]string:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = item
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, item := range typed {
			normalized[index] = normalize(item)
		}
		return normalized
	case []string:
		normalized := make([]any, len(typed))
		for index, item := range typed {
			normalized[index] = item
		}
		return normalized
	default:
		return fmt.Sprint(value)
	}
}

// boundedLogValue caps every string in a redacted value and converts it to the
// OpenTelemetry log model.
//
// Capping happens after redaction, never before: truncating first could cut a
// credential in half and leave the surviving half looking like ordinary text
// that no recognizer matches.
func boundedLogValue(value any) attribute.Value {
	switch typed := value.(type) {
	case nil:
		return attribute.Value{}
	case string:
		return attribute.StringValue(bounded(typed))
	case bool:
		return attribute.BoolValue(typed)
	case int64:
		return attribute.Int64Value(typed)
	case float64:
		return attribute.Float64Value(typed)
	case map[string]any:
		members := make([]attribute.KeyValue, 0, len(typed))
		for key, item := range typed {
			members = append(members, attribute.KeyValue{Key: attribute.Key(key), Value: boundedLogValue(item)})
		}
		// Sorted, because Go randomizes map iteration and two identical records
		// would otherwise export their attributes in a different order.
		slices.SortFunc(members, func(a, b attribute.KeyValue) int {
			if a.Key < b.Key {
				return -1
			}
			if a.Key > b.Key {
				return 1
			}
			return 0
		})
		return attribute.MapValue(members...)
	case []any:
		members := make([]attribute.Value, 0, len(typed))
		for _, item := range typed {
			members = append(members, boundedLogValue(item))
		}
		return attribute.SliceValue(members...)
	default:
		// Reachable only when a redactor returns a shape normalize did not
		// produce. Stringifying is the conservative answer: the value is
		// exported as text, capped, and never as an unexamined structure.
		return attribute.StringValue(bounded(fmt.Sprint(value)))
	}
}

// bounded caps one string at [MaxExportedChars] characters, suffix included.
//
// Characters, not bytes, and the cut lands on a rune boundary: a byte slice
// through a multi-byte rune produces invalid UTF-8, which some OTLP collectors
// reject — losing the whole batch rather than one field.
func bounded(text string) string {
	if utf8.RuneCountInString(text) <= MaxExportedChars {
		return text
	}
	keep := MaxExportedChars - utf8.RuneCountInString(TruncatedSuffix)
	for index := range text {
		if keep == 0 {
			return text[:index] + TruncatedSuffix
		}
		keep--
	}
	// Unreachable: the rune count above already proved there are more than
	// `keep` runes. Returning the suffix alone keeps the cap honest anyway.
	return TruncatedSuffix
}

// severityOf maps an slog level onto the OpenTelemetry severity scale.
//
// The comparisons are ranges rather than equalities because slog levels are
// integers a caller can offset — slog.LevelWarn+1 is a legitimate level, and it
// is a warning, not an unknown.
func severityOf(level slog.Level) log.Severity {
	switch {
	case level < slog.LevelInfo:
		return log.SeverityDebug
	case level < slog.LevelWarn:
		return log.SeverityInfo
	case level < slog.LevelError:
		return log.SeverityWarn
	default:
		return log.SeverityError
	}
}
