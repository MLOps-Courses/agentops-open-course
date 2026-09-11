// Package piiwebhook implements agentgateway's request and response guardrail
// webhook contract for model-backed named-entity masking.
//
// The gateway is defense in depth. The agent's deterministic in-process
// redactor remains the always-on floor; this package adds PERSON, LOCATION and
// ORGANIZATION recognition only when a gateway calls these private endpoints.
package piiwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// RequestPath and ResponsePath are agentgateway 1.4.1's fixed webhook
	// paths. Keeping them here makes the handler and A2A route wiring share one
	// authority.
	RequestPath  = "/request"
	ResponsePath = "/response"

	// RedactedMask replaces a whole non-empty value when the configured model
	// cannot certify it. A reachable webhook must never turn a model outage into
	// pass-through.
	RedactedMask = "<REDACTED>"

	InvalidRequestMessage = "invalid guardrail request"

	DefaultMaxBodyBytes      int64 = 64 * 1024
	DefaultMaxItems                = 32
	DefaultMaxTextBytes            = 8 * 1024
	DefaultMaxTotalTextBytes       = 32 * 1024
	DefaultDetectionTimeout        = 8 * time.Second
)

var ErrIncompleteConfig = errors.New("incomplete PII webhook configuration")

// Entity is one named-entity class the optional model layer may report.
type Entity string

const (
	EntityPerson       Entity = "PERSON"
	EntityLocation     Entity = "LOCATION"
	EntityOrganization Entity = "ORGANIZATION"
)

// Span is one half-open UTF-8 byte range returned by a Detector.
type Span struct {
	Entity Entity
	Start  int
	End    int
}

// Detector finds named-entity spans in each supplied text. The outer result
// must have exactly one element per input text.
type Detector interface {
	Detect(ctx context.Context, texts []string) ([][]Span, error)
}

// Config bounds the webhook's parser and model call.
type Config struct {
	Detector Detector

	MaxBodyBytes      int64
	Timeout           time.Duration
	MaxItems          int
	MaxTextBytes      int
	MaxTotalTextBytes int
}

// Handler serves the two agentgateway webhook paths.
type Handler struct {
	detector Detector

	maxBodyBytes      int64
	timeout           time.Duration
	maxItems          int
	maxTextBytes      int
	maxTotalTextBytes int
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type promptMessages struct {
	Messages []message `json:"messages"`
}

type responseChoice struct {
	Message message `json:"message"`
}

type responseChoices struct {
	Choices []responseChoice `json:"choices"`
}

type promptRequest struct {
	Body promptMessages `json:"body"`
}

type responseRequest struct {
	Body responseChoices `json:"body"`
}

type action struct {
	Body   any    `json:"body,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type webhookResponse struct {
	Action action `json:"action"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// New validates cfg and returns a bounded webhook handler.
func New(cfg Config) (*Handler, error) {
	if cfg.Detector == nil {
		return nil, fmt.Errorf("%w: Detector is required", ErrIncompleteConfig)
	}
	if cfg.Timeout < 0 || cfg.MaxBodyBytes < 0 || cfg.MaxItems < 0 ||
		cfg.MaxTextBytes < 0 || cfg.MaxTotalTextBytes < 0 {
		return nil, fmt.Errorf("%w: bounds cannot be negative", ErrIncompleteConfig)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultDetectionTimeout
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.MaxItems == 0 {
		cfg.MaxItems = DefaultMaxItems
	}
	if cfg.MaxTextBytes == 0 {
		cfg.MaxTextBytes = DefaultMaxTextBytes
	}
	if cfg.MaxTotalTextBytes == 0 {
		cfg.MaxTotalTextBytes = DefaultMaxTotalTextBytes
	}
	return &Handler{
		detector:          cfg.Detector,
		maxBodyBytes:      cfg.MaxBodyBytes,
		timeout:           cfg.Timeout,
		maxItems:          cfg.MaxItems,
		maxTextBytes:      cfg.MaxTextBytes,
		maxTotalTextBytes: cfg.MaxTotalTextBytes,
	}, nil
}

// ServeHTTP validates the pinned agentgateway wire shape before calling the
// model. It never includes parser or model errors in a response because either
// may contain the original sensitive text.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		h.writeJSON(writer, http.StatusMethodNotAllowed, errorResponse{Error: InvalidRequestMessage})
		return
	}
	switch request.URL.Path {
	case RequestPath:
		h.handleRequest(writer, request)
	case ResponsePath:
		h.handleResponse(writer, request)
	default:
		h.writeJSON(writer, http.StatusNotFound, errorResponse{Error: InvalidRequestMessage})
	}
}

func (h *Handler) handleRequest(writer http.ResponseWriter, request *http.Request) {
	var payload promptRequest
	if err := h.decode(writer, request, &payload); err != nil || h.validateMessages(payload.Body.Messages) != nil {
		h.writeJSON(writer, http.StatusBadRequest, errorResponse{Error: InvalidRequestMessage})
		return
	}

	texts := make([]string, len(payload.Body.Messages))
	for index := range payload.Body.Messages {
		texts[index] = payload.Body.Messages[index].Content
	}
	masked, changed := h.detect(request.Context(), texts)
	for index := range payload.Body.Messages {
		payload.Body.Messages[index].Content = masked[index]
	}
	if !changed {
		h.writeJSON(writer, http.StatusOK, webhookResponse{Action: action{Reason: "no named entities detected"}})
		return
	}
	h.writeJSON(writer, http.StatusOK, webhookResponse{Action: action{
		Body: payload.Body, Reason: "named entities masked",
	}})
}

func (h *Handler) handleResponse(writer http.ResponseWriter, request *http.Request) {
	var payload responseRequest
	if err := h.decode(writer, request, &payload); err != nil {
		h.writeJSON(writer, http.StatusBadRequest, errorResponse{Error: InvalidRequestMessage})
		return
	}
	messages := make([]message, len(payload.Body.Choices))
	for index := range payload.Body.Choices {
		messages[index] = payload.Body.Choices[index].Message
	}
	if err := h.validateMessages(messages); err != nil {
		h.writeJSON(writer, http.StatusBadRequest, errorResponse{Error: InvalidRequestMessage})
		return
	}

	texts := make([]string, len(messages))
	for index := range messages {
		texts[index] = messages[index].Content
	}
	masked, changed := h.detect(request.Context(), texts)
	for index := range payload.Body.Choices {
		payload.Body.Choices[index].Message.Content = masked[index]
	}
	if !changed {
		h.writeJSON(writer, http.StatusOK, webhookResponse{Action: action{Reason: "no named entities detected"}})
		return
	}
	h.writeJSON(writer, http.StatusOK, webhookResponse{Action: action{
		Body: payload.Body, Reason: "named entities masked",
	}})
}

func (h *Handler) decode(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxBodyBytes)
	defer func() { _ = request.Body.Close() }()
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func (h *Handler) validateMessages(messages []message) error {
	if len(messages) > h.maxItems {
		return errors.New("too many messages")
	}
	total := 0
	for _, item := range messages {
		if item.Role == "" || len(item.Role) > 32 || !utf8.ValidString(item.Role) || !utf8.ValidString(item.Content) {
			return errors.New("invalid message")
		}
		if len(item.Content) > h.maxTextBytes {
			return errors.New("message too large")
		}
		total += len(item.Content)
		if total > h.maxTotalTextBytes {
			return errors.New("request text too large")
		}
	}
	return nil
}

// detect returns either validated masking results or a conservative mask of
// every non-empty input. The boolean reports whether a replacement body is
// required by agentgateway's untagged action protocol.
func (h *Handler) detect(ctx context.Context, texts []string) ([]string, bool) {
	if len(texts) == 0 {
		return []string{}, false
	}
	detectCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	spans, err := h.detector.Detect(detectCtx, texts)
	if err != nil || len(spans) != len(texts) {
		return maskAll(texts), hasNonEmpty(texts)
	}

	masked := slices.Clone(texts)
	changed := false
	for index := range texts {
		redacted, err := maskSpans(texts[index], spans[index])
		if err != nil {
			return maskAll(texts), hasNonEmpty(texts)
		}
		masked[index] = redacted
		changed = changed || redacted != texts[index]
	}
	return masked, changed
}

func maskSpans(text string, spans []Span) (string, error) {
	ordered := slices.Clone(spans)
	slices.SortFunc(ordered, func(left, right Span) int {
		if left.Start != right.Start {
			return left.Start - right.Start
		}
		if left.End != right.End {
			return right.End - left.End
		}
		return strings.Compare(string(left.Entity), string(right.Entity))
	})

	var output strings.Builder
	written := 0
	for _, found := range ordered {
		if !found.Entity.valid() || found.Start < 0 || found.End <= found.Start || found.End > len(text) ||
			!utf8.ValidString(text[found.Start:found.End]) {
			return "", errors.New("invalid detector span")
		}
		if found.End <= written {
			// This span lies entirely inside what is already masked, so it has
			// nothing left to hide. The descending-End tie-break puts the widest
			// span at a given start first, which is what makes a nested span reach
			// this branch rather than the partial-overlap one below.
			continue
		}
		if found.Start > written {
			output.WriteString(text[written:found.Start])
		}
		// A span that starts inside an earlier one but ends after it is only
		// partially covered, and its uncovered tail is still text the detector
		// called an entity. That tail is replaced with this span's own marker
		// instead of being written out, so two markers end up adjacent — which is
		// the honest rendering, because skipping the span outright published the
		// tail verbatim and handed the model the bytes the detector meant to hide.
		// Nothing is sliced at the overlap either, so a multi-byte rune straddling
		// it cannot be cut in half.
		output.WriteByte('<')
		output.WriteString(string(found.Entity))
		output.WriteByte('>')
		written = found.End
	}
	output.WriteString(text[written:])
	return output.String(), nil
}

func (e Entity) valid() bool {
	return e == EntityPerson || e == EntityLocation || e == EntityOrganization
}

func maskAll(texts []string) []string {
	masked := make([]string, len(texts))
	for index, text := range texts {
		if text != "" {
			masked[index] = RedactedMask
		}
	}
	return masked
}

func hasNonEmpty(texts []string) bool {
	return slices.ContainsFunc(texts, func(text string) bool { return text != "" })
}

func (h *Handler) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		// The status line is already on the wire, so a log record is the only
		// remaining action — and without one a truncated body is invisible to
		// both the gateway and the operator. slog.Default is the redacting
		// handler the runtime installs, so the error class is safe to record.
		slog.Default().Error("PII webhook response encoding failed", "error_type", fmt.Sprintf("%T", err))
	}
}
