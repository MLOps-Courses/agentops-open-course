package piiwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type detectorFunc func(context.Context, []string) ([][]Span, error)

func (detect detectorFunc) Detect(ctx context.Context, texts []string) ([][]Span, error) {
	return detect(ctx, texts)
}

func newHandler(t *testing.T, detector Detector, timeout time.Duration) *Handler {
	t.Helper()
	handler, err := New(Config{Detector: detector, Timeout: timeout})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return handler
}

func post(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func responseJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body = %q, want JSON: %v", recorder.Body.String(), err)
	}
	return decoded
}

func TestRequestWebhookMasksNamedEntitiesInPlace(t *testing.T) {
	t.Parallel()

	detector := detectorFunc(func(_ context.Context, texts []string) ([][]Span, error) {
		if len(texts) != 2 || texts[0] != "John paged Paris" || texts[1] != "No PII here" {
			t.Fatalf("Detect() texts = %q, want the gateway message contents", texts)
		}
		return [][]Span{
			{
				{Start: 0, End: 4, Entity: EntityPerson},
				{Start: 11, End: 16, Entity: EntityLocation},
			},
			nil,
		}, nil
	})
	handler := newHandler(t, detector, time.Second)

	recorder := post(t, handler, RequestPath, `{
		"body":{"messages":[
			{"role":"user","content":"John paged Paris"},
			{"role":"system","content":"No PII here"}
		]}
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.String()
	for _, want := range []string{"<PERSON> paged <LOCATION>", "No PII here", "named entities masked"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body = %s, want it to contain %q", body, want)
		}
	}
	for _, gone := range []string{"John", "Paris"} {
		if strings.Contains(body, gone) {
			t.Errorf("response body = %s, still contains %q", body, gone)
		}
	}
}

func TestResponseWebhookPreservesTheGatewayChoiceShape(t *testing.T) {
	t.Parallel()

	detector := detectorFunc(func(_ context.Context, _ []string) ([][]Span, error) {
		return [][]Span{{{Start: 12, End: 21, Entity: EntityOrganization}}}, nil
	})
	handler := newHandler(t, detector, time.Second)

	recorder := post(t, handler, ResponsePath, `{
		"body":{"choices":[
			{"message":{"role":"assistant","content":"Escalate to Acme Corp"}}
		]}
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
	}
	for _, want := range []string{`"choices"`, `"role":"assistant"`, "Escalate to <ORGANIZATION>"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("response body = %s, want it to contain %q", recorder.Body.String(), want)
		}
	}
}

func TestModelFailureMasksEveryNonEmptyValue(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
		return nil, errors.New("ollama unavailable")
	}), time.Second)
	recorder := post(t, handler, RequestPath,
		`{"body":{"messages":[{"role":"user","content":"Alice is in Paris"}]}}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want a mask action", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), RedactedMask) {
		t.Errorf("response body = %s, want conservative mask %q", recorder.Body.String(), RedactedMask)
	}
	for _, gone := range []string{"Alice", "Paris", "ollama unavailable"} {
		if strings.Contains(recorder.Body.String(), gone) {
			t.Errorf("response body = %s, leaks %q", recorder.Body.String(), gone)
		}
	}
}

func TestDetectionTimeoutMasksInsteadOfPassingThrough(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, detectorFunc(func(ctx context.Context, _ []string) ([][]Span, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), 20*time.Millisecond)

	started := time.Now()
	recorder := post(t, handler, RequestPath,
		`{"body":{"messages":[{"role":"user","content":"Alice is in Paris"}]}}`)
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("webhook took %v, want the configured deadline", elapsed)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), RedactedMask) {
		t.Errorf("status = %d, body = %q, want a conservative mask action",
			recorder.Code, recorder.Body.String())
	}
}

func TestWebhookInputIsStrictAndBounded(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
		calls.Add(1)
		return nil, nil
	}), time.Second)

	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"body":{"messages":[]},"ignored":true}`},
		{name: "trailing JSON", body: `{"body":{"messages":[]}} {}`},
		{name: "oversized text", body: `{"body":{"messages":[{"role":"user","content":"` + strings.Repeat("x", DefaultMaxTextBytes+1) + `"}]}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := post(t, handler, RequestPath, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, body = %q, want 400", recorder.Code, recorder.Body.String())
			}
			if got := responseJSON(t, recorder); got["error"] != InvalidRequestMessage {
				t.Errorf("response = %v, want opaque error %q", got, InvalidRequestMessage)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("detector calls = %d, want 0 for invalid input", got)
	}
}

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	usable := detectorFunc(func(context.Context, []string) ([][]Span, error) { return nil, nil })
	for name, cfg := range map[string]Config{
		"no detector":               {},
		"negative timeout":          {Detector: usable, Timeout: -time.Second},
		"negative body bound":       {Detector: usable, MaxBodyBytes: -1},
		"negative item bound":       {Detector: usable, MaxItems: -1},
		"negative text bound":       {Detector: usable, MaxTextBytes: -1},
		"negative total text bound": {Detector: usable, MaxTotalTextBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(cfg); !errors.Is(err, ErrIncompleteConfig) {
				t.Errorf("New() error = %v, want %v", err, ErrIncompleteConfig)
			}
		})
	}
}

// TestZeroConfigStillBoundsTheRequest pins what the zero value means. A gateway
// deployment that names only the detector must inherit the published bounds
// rather than "unbounded", because the bounds are what keep a hostile prompt
// from turning one webhook call into an unbounded model call.
func TestZeroConfigStillBoundsTheRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	handler, err := New(Config{Detector: detectorFunc(func(_ context.Context, texts []string) ([][]Span, error) {
		calls.Add(1)
		return make([][]Span, len(texts)), nil
	})})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	oversized := `{"body":{"messages":[{"role":"user","content":"` +
		strings.Repeat("x", DefaultMaxTextBytes+1) + `"}]}}`
	if recorder := post(t, handler, RequestPath, oversized); recorder.Code != http.StatusBadRequest {
		t.Errorf("oversized text status = %d, want 400 from the default text bound", recorder.Code)
	}

	var batch strings.Builder
	batch.WriteString(`{"body":{"messages":[`)
	for index := range DefaultMaxItems + 1 {
		if index > 0 {
			batch.WriteString(",")
		}
		batch.WriteString(`{"role":"user","content":"one"}`)
	}
	batch.WriteString(`]}}`)
	if recorder := post(t, handler, RequestPath, batch.String()); recorder.Code != http.StatusBadRequest {
		t.Errorf("oversized batch status = %d, want 400 from the default item bound", recorder.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("detector calls = %d, want 0 for input the defaults reject", got)
	}
}

// TestWebhookServesOnlyThePinnedPostRoutes keeps the surface exactly as wide as
// agentgateway's contract. Anything else reaching the model would be a private
// endpoint accepting traffic the gateway never sends.
func TestWebhookServesOnlyThePinnedPostRoutes(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
		calls.Add(1)
		return nil, nil
	}), time.Second)

	for name, testCase := range map[string]struct {
		method    string
		path      string
		wantAllow string
		wantCode  int
	}{
		"GET on the request path":     {method: http.MethodGet, path: RequestPath, wantCode: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		"DELETE on the response path": {method: http.MethodDelete, path: ResponsePath, wantCode: http.StatusMethodNotAllowed, wantAllow: http.MethodPost},
		"POST on an unknown path":     {method: http.MethodPost, path: "/healthz", wantCode: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantCode {
				t.Errorf("status = %d, want %d", recorder.Code, testCase.wantCode)
			}
			if got := recorder.Header().Get("Allow"); got != testCase.wantAllow {
				t.Errorf("Allow = %q, want %q", got, testCase.wantAllow)
			}
			if got := responseJSON(t, recorder); got["error"] != InvalidRequestMessage {
				t.Errorf("response = %v, want opaque error %q", got, InvalidRequestMessage)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("detector calls = %d, want 0 for traffic the gateway never sends", got)
	}
}

// TestResponseWebhookInputIsStrictAndBounded is the response-path twin of
// TestWebhookInputIsStrictAndBounded. The two paths decode different shapes, so
// a bound enforced on one proves nothing about the other.
func TestResponseWebhookInputIsStrictAndBounded(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
		calls.Add(1)
		return nil, nil
	}), time.Second)

	choices := func(contents ...string) string {
		var body strings.Builder
		body.WriteString(`{"body":{"choices":[`)
		for index, content := range contents {
			if index > 0 {
				body.WriteString(",")
			}
			body.WriteString(`{"message":{"role":"assistant","content":"` + content + `"}}`)
		}
		body.WriteString(`]}}`)
		return body.String()
	}

	for name, body := range map[string]string{
		"unknown field":        `{"body":{"choices":[]},"ignored":true}`,
		"trailing JSON value":  `{"body":{"choices":[]}} {}`,
		"truncated trailing":   `{"body":{"choices":[]}} {`,
		"empty role":           `{"body":{"choices":[{"message":{"role":"","content":"hello"}}]}}`,
		"oversized role":       `{"body":{"choices":[{"message":{"role":"` + strings.Repeat("r", 33) + `","content":"hi"}}]}}`,
		"oversized message":    choices(strings.Repeat("x", DefaultMaxTextBytes+1)),
		"too many choices":     choices(slices.Repeat([]string{"one"}, DefaultMaxItems+1)...),
		"request text too big": choices(slices.Repeat([]string{strings.Repeat("y", DefaultMaxTextBytes)}, 5)...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			recorder := post(t, handler, ResponsePath, body)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, body = %q, want 400", recorder.Code, recorder.Body.String())
			}
			if got := responseJSON(t, recorder); got["error"] != InvalidRequestMessage {
				t.Errorf("response = %v, want opaque error %q", got, InvalidRequestMessage)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("detector calls = %d, want 0 for invalid input", got)
	}
}

// TestResponseWebhookReturnsNoBodyWhenNothingIsMasked matters because
// agentgateway's action protocol is untagged: a replacement body is what makes
// the gateway rewrite the model response, so sending one that equals the input
// would rewrite every clean answer for nothing.
func TestResponseWebhookReturnsNoBodyWhenNothingIsMasked(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, detectorFunc(func(_ context.Context, texts []string) ([][]Span, error) {
		return make([][]Span, len(texts)), nil
	}), time.Second)
	recorder := post(t, handler, ResponsePath,
		`{"body":{"choices":[{"message":{"role":"assistant","content":"Restarted job 002"}}]}}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"body"`) {
		t.Errorf("response body = %s, want a pass action without a replacement body", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no named entities detected") {
		t.Errorf("response body = %s, want a pass reason", recorder.Body.String())
	}
}

// TestEmptyBatchNeverReachesTheDetector keeps a gateway heartbeat — a webhook
// call with no messages at all — from costing a model call and a timeout.
func TestEmptyBatchNeverReachesTheDetector(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
		calls.Add(1)
		return nil, errors.New("the detector must not be called for an empty batch")
	}), time.Second)

	for path, body := range map[string]string{
		RequestPath:  `{"body":{"messages":[]}}`,
		ResponsePath: `{"body":{"choices":[]}}`,
	} {
		recorder := post(t, handler, path, body)
		if recorder.Code != http.StatusOK {
			t.Errorf("%s status = %d, body = %q, want 200", path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "no named entities detected") {
			t.Errorf("%s response = %s, want a pass action", path, recorder.Body.String())
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("detector calls = %d, want 0 for an empty batch", got)
	}
}

// TestOverlappingSpansMaskOnceAndDeterministically pins the two properties a
// masker built from model output needs: overlapping ranges must never produce
// interleaved or doubled markers, and a model that reports the same bytes under
// two classes must always yield the same text, or an identical request would
// return two different bodies.
func TestOverlappingSpansMaskOnceAndDeterministically(t *testing.T) {
	t.Parallel()

	const text = "Acme Corp paged Acme Corp"
	for name, testCase := range map[string]struct {
		want  string
		spans []Span
	}{
		"a nested span is absorbed by the longest match at the same start": {
			spans: []Span{
				{Entity: EntityOrganization, Start: 0, End: 4},
				{Entity: EntityOrganization, Start: 0, End: 9},
			},
			want: "<ORGANIZATION> paged Acme Corp",
		},
		"two classes over the same bytes mask once, lowest class name first": {
			spans: []Span{
				{Entity: EntityPerson, Start: 0, End: 9},
				{Entity: EntityLocation, Start: 0, End: 9},
			},
			want: "<LOCATION> paged Acme Corp",
		},
		"a chain of staggered spans masks every one of them": {
			spans: []Span{
				{Entity: EntityOrganization, Start: 0, End: 9},
				{Entity: EntityPerson, Start: 5, End: 20},
				{Entity: EntityOrganization, Start: 16, End: 25},
			},
			// The person span covers " paged " too, so the whole text is detected
			// bytes and nothing of it may survive between the markers.
			want: "<ORGANIZATION><PERSON><ORGANIZATION>",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
				return [][]Span{testCase.spans}, nil
			}), time.Second)
			recorder := post(t, handler, RequestPath,
				`{"body":{"messages":[{"role":"user","content":"`+text+`"}]}}`)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.want) {
				t.Errorf("response body = %s, want it to contain %q", recorder.Body.String(), testCase.want)
			}
		})
	}
}

// TestStaggeredOverlapMasksTheUncoveredTail is the regression for the one
// overlap shape that used to leak: a span that starts inside an earlier one but
// ends after it was dropped whole, so its uncovered tail reached the model
// verbatim. This webhook is the control that catches the context-dependent
// names the regex maskers cannot, so a published organization or location tail
// is exactly the failure it was bought to prevent.
func TestStaggeredOverlapMasksTheUncoveredTail(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		text  string
		want  string
		gone  []string
		spans []Span
	}{
		"a person and an organization sharing a surname": {
			text: "Alice Smith Corp",
			spans: []Span{
				{Entity: EntityPerson, Start: 0, End: 11},
				{Entity: EntityOrganization, Start: 6, End: 16},
			},
			want: "<PERSON><ORGANIZATION>",
			gone: []string{"Alice", "Smith", "Corp"},
		},
		"multi-byte runes on both sides of the overlap": {
			// "José" is five bytes and "Álvarez" is eight, so the organization span
			// starts at byte 6 — inside the person span, and on a rune boundary. The
			// remainder must never be sliced there, or the accented rune is cut in
			// half and the whole value is conservatively redacted instead.
			text: "José Álvarez Zenith",
			spans: []Span{
				{Entity: EntityPerson, Start: 0, End: 14},
				{Entity: EntityOrganization, Start: 6, End: 21},
			},
			want: "<PERSON><ORGANIZATION>",
			gone: []string{"José", "Álvarez", "Zenith"},
		},
		"a span nested inside a wider earlier one is still absorbed": {
			// The already-correct half of the masker: this span ends inside what is
			// masked, so it must add nothing rather than a second marker.
			text: "Acme Corp paged Acme Corp",
			spans: []Span{
				{Entity: EntityOrganization, Start: 0, End: 9},
				{Entity: EntityPerson, Start: 5, End: 9},
			},
			want: "<ORGANIZATION> paged Acme Corp",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
				return [][]Span{testCase.spans}, nil
			}), time.Second)
			recorder := post(t, handler, RequestPath,
				`{"body":{"messages":[{"role":"user","content":"`+testCase.text+`"}]}}`)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.want) {
				t.Errorf("response body = %s, want it to contain %q", recorder.Body.String(), testCase.want)
			}
			for _, leaked := range testCase.gone {
				if strings.Contains(recorder.Body.String(), leaked) {
					t.Errorf("response body = %s, still contains the detected %q", recorder.Body.String(), leaked)
				}
			}
		})
	}
}

// TestUnusableSpansMaskTheWholeValue is the fail-closed half of the masker. A
// detector that returns a range this text cannot support is as untrustworthy as
// one that failed outright, so the value is replaced rather than partially cut
// — a partial cut would publish the bytes the model meant to hide.
func TestUnusableSpansMaskTheWholeValue(t *testing.T) {
	t.Parallel()

	// The multi-byte rune is deliberate: "Zoë" ends at byte 4, so a span ending
	// at byte 3 would slice the ë in half and leave invalid UTF-8 behind.
	const text = "Zoë paged Acme"
	for name, spans := range map[string][]Span{
		"a span past the end of the text": {{Entity: EntityPerson, Start: 0, End: len(text) + 1}},
		"a negative start":                {{Entity: EntityPerson, Start: -1, End: 4}},
		"an empty range":                  {{Entity: EntityPerson, Start: 4, End: 4}},
		"an unknown entity class":         {{Entity: Entity("MACHINE"), Start: 0, End: 4}},
		"a cut through a multi-byte rune": {{Entity: EntityPerson, Start: 0, End: 3}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			handler := newHandler(t, detectorFunc(func(context.Context, []string) ([][]Span, error) {
				return [][]Span{spans}, nil
			}), time.Second)
			recorder := post(t, handler, RequestPath,
				`{"body":{"messages":[{"role":"user","content":"`+text+`"}]}}`)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q, want a mask action", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), RedactedMask) {
				t.Errorf("response body = %s, want conservative mask %q", recorder.Body.String(), RedactedMask)
			}
			for _, gone := range []string{"Zoë", "Acme"} {
				if strings.Contains(recorder.Body.String(), gone) {
					t.Errorf("response body = %s, still contains %q", recorder.Body.String(), gone)
				}
			}
		})
	}
}

func TestNoNamedEntitiesReturnsPass(t *testing.T) {
	t.Parallel()

	handler := newHandler(t, detectorFunc(func(_ context.Context, texts []string) ([][]Span, error) {
		return make([][]Span, len(texts)), nil
	}), time.Second)
	recorder := post(t, handler, RequestPath,
		`{"body":{"messages":[{"role":"user","content":"Inspect job 002"}]}}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `"body"`) {
		t.Errorf("response body = %s, want a pass action without a replacement body", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no named entities detected") {
		t.Errorf("response body = %s, want a pass reason", recorder.Body.String())
	}
}
