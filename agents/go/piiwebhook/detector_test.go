package piiwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

type recordingLLM struct {
	err      error
	request  *adkmodel.LLMRequest
	response string
}

func (m *recordingLLM) Name() string { return "recording" }

func (m *recordingLLM) GenerateContent(
	_ context.Context, request *adkmodel.LLMRequest, stream bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if stream {
			yield(nil, errors.New("unexpected streaming request"))
			return
		}
		m.request = request
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		yield(&adkmodel.LLMResponse{Content: genai.NewContentFromText(m.response, genai.RoleModel)}, nil)
	}
}

func TestModelDetectorUsesStrictStructuredOutputAndExactText(t *testing.T) {
	t.Parallel()

	model := &recordingLLM{response: `{
		"items":[
			{"index":0,"entities":[
				{"type":"PERSON","text":"Zoë"},
				{"type":"LOCATION","text":"Paris"}
			]},
			{"index":1,"entities":[
				{"type":"ORGANIZATION","text":"Acme"}
			]}
		]
	}`}
	detector, err := NewModelDetector(model)
	if err != nil {
		t.Fatalf("NewModelDetector() error = %v, want nil", err)
	}

	texts := []string{"Zoë left Paris; Zoë returned", "Acme paged Acme"}
	got, err := detector.Detect(t.Context(), texts)
	if err != nil {
		t.Fatalf("Detect() error = %v, want nil", err)
	}
	want := [][]Span{
		{
			{Entity: EntityPerson, Start: 0, End: 4},
			{Entity: EntityLocation, Start: 10, End: 15},
			{Entity: EntityPerson, Start: 17, End: 21},
		},
		{
			{Entity: EntityOrganization, Start: 0, End: 4},
			{Entity: EntityOrganization, Start: 11, End: 15},
		},
	}
	if !slices.EqualFunc(got, want, func(left, right []Span) bool { return slices.Equal(left, right) }) {
		t.Errorf("Detect() spans = %#v, want %#v", got, want)
	}

	request := model.request
	if request == nil || len(request.Contents) != 1 || request.Config == nil {
		t.Fatalf("model request = %#v, want one bounded structured request", request)
	}
	if request.Config.ResponseMIMEType != "application/json" || request.Config.ResponseSchema == nil {
		t.Errorf("response format = (%q, %#v), want application/json plus schema",
			request.Config.ResponseMIMEType, request.Config.ResponseSchema)
	}
	if request.Config.Temperature == nil || *request.Config.Temperature != 0 {
		t.Errorf("temperature = %v, want explicit zero", request.Config.Temperature)
	}
	if request.Config.MaxOutputTokens <= 0 || request.Config.MaxOutputTokens > 2048 {
		t.Errorf("max output tokens = %d, want a positive bounded value", request.Config.MaxOutputTokens)
	}
	if request.Config.SystemInstruction == nil || !strings.Contains(
		request.Config.SystemInstruction.Parts[0].Text, "exact substrings",
	) {
		t.Errorf("system instruction = %#v, want exact-substring constraint", request.Config.SystemInstruction)
	}
	if content := request.Contents[0].Parts[0].Text; !strings.Contains(content, `"index":0`) ||
		!strings.Contains(content, `"text":"Zoë left Paris; Zoë returned"`) {
		t.Errorf("model input = %q, want indexed JSON text", content)
	}
}

func TestModelDetectorRejectsUntrustedOutput(t *testing.T) {
	t.Parallel()

	for name, output := range map[string]string{
		"malformed JSON":      `{"items":[`,
		"unknown field":       `{"items":[{"index":0,"entities":[],"extra":true}]}`,
		"missing index":       `{"items":[]}`,
		"duplicate index":     `{"items":[{"index":0,"entities":[]},{"index":0,"entities":[]}]}`,
		"unknown entity":      `{"items":[{"index":0,"entities":[{"type":"MACHINE","text":"job-001"}]}]}`,
		"hallucinated text":   `{"items":[{"index":0,"entities":[{"type":"PERSON","text":"Alice"}]}]}`,
		"placeholder entity":  `{"items":[{"index":0,"entities":[{"type":"PERSON","text":"<REDACTED>"}]}]}`,
		"trailing JSON value": `{"items":[{"index":0,"entities":[]}]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detector, err := NewModelDetector(&recordingLLM{response: output})
			if err != nil {
				t.Fatalf("NewModelDetector() error = %v, want nil", err)
			}
			if _, err := detector.Detect(t.Context(), []string{"No name here"}); err == nil {
				t.Fatal("Detect() error = nil, want strict output rejection")
			}
		})
	}
}

// scriptedLLM replays an exact sequence of model results, which is how the
// single-response rule below can be tested at all: the parser's contract is
// about how many responses arrive and what they carry, and recordingLLM always
// yields exactly one good one.
type scriptedLLM struct {
	err       error
	responses []*adkmodel.LLMResponse
}

func (m *scriptedLLM) Name() string { return "scripted" }

func (m *scriptedLLM) GenerateContent(
	_ context.Context, _ *adkmodel.LLMRequest, _ bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		for _, response := range m.responses {
			if !yield(response, nil) {
				return
			}
		}
	}
}

func TestNewModelDetectorRequiresAModel(t *testing.T) {
	t.Parallel()

	if _, err := NewModelDetector(nil); !errors.Is(err, ErrIncompleteConfig) {
		t.Errorf("NewModelDetector(nil) error = %v, want %v", err, ErrIncompleteConfig)
	}
}

// TestOpenAICompatibleDetectorRefusesAnIncompleteConfiguration keeps the
// endpoint explicit. The BaseURL cases are the security-carrying ones: this
// client must address the local model directly, and a URL carrying credentials,
// a query or a fragment is a sign it was pointed somewhere else — including at
// agentgateway, which would make the webhook call itself.
func TestOpenAICompatibleDetectorRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	const reachable = "http://127.0.0.1:11434/v1"
	for name, cfg := range map[string]OpenAIConfig{
		"no base URL":              {APIKey: "local-marker", Model: "qwen-test"},
		"base URL with no host":    {BaseURL: "/v1", APIKey: "local-marker", Model: "qwen-test"},
		"non-HTTP scheme":          {BaseURL: "ftp://127.0.0.1/v1", APIKey: "local-marker", Model: "qwen-test"},
		"base URL with a password": {BaseURL: "http://user:pass@127.0.0.1/v1", APIKey: "local-marker", Model: "qwen-test"},
		"base URL with a query":    {BaseURL: "http://127.0.0.1/v1?key=leak", APIKey: "local-marker", Model: "qwen-test"},
		"base URL with a fragment": {BaseURL: "http://127.0.0.1/v1#top", APIKey: "local-marker", Model: "qwen-test"},
		"no API key":               {BaseURL: reachable, Model: "qwen-test"},
		"blank API key":            {BaseURL: reachable, APIKey: "   ", Model: "qwen-test"},
		"no model":                 {BaseURL: reachable, APIKey: "local-marker"},
		"negative timeout":         {BaseURL: reachable, APIKey: "local-marker", Model: "qwen-test", Timeout: -time.Second},
		"negative response bound":  {BaseURL: reachable, APIKey: "local-marker", Model: "qwen-test", MaxResponseBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewOpenAICompatibleDetector(t.Context(), cfg); !errors.Is(err, ErrIncompleteConfig) {
				t.Errorf("NewOpenAICompatibleDetector() error = %v, want %v", err, ErrIncompleteConfig)
			}
		})
	}
}

// TestInjectedModelDetectorIsReadyWithoutACatalogCall keeps readiness honest for
// the deterministic path: a detector built over an injected model has no
// endpoint to poll, so a probe that reported "not ready" would take a healthy
// webhook out of service.
func TestInjectedModelDetectorIsReadyWithoutACatalogCall(t *testing.T) {
	t.Parallel()

	detector, err := NewModelDetector(&recordingLLM{response: `{"items":[]}`})
	if err != nil {
		t.Fatalf("NewModelDetector() error = %v, want nil", err)
	}
	if err := detector.Ready(t.Context()); err != nil {
		t.Errorf("Ready() error = %v, want nil for an injected model", err)
	}
}

// TestReadinessRefusesAnUnusableModelCatalog is what keeps the gateway from
// routing to a webhook whose model is missing: readiness has to fail before the
// first masked request, not on it, because a failing detector masks everything
// and every answer downstream turns into <REDACTED>.
func TestReadinessRefusesAnUnusableModelCatalog(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		catalog   string
		status    int
		wantReady bool
	}{
		"the configured model is listed":    {status: http.StatusOK, catalog: `{"data":[{"id":"qwen-test"}]}`, wantReady: true},
		"the catalog is unavailable":        {status: http.StatusInternalServerError, catalog: `{}`},
		"the catalog is not JSON":           {status: http.StatusOK, catalog: `not json`},
		"the configured model is absent":    {status: http.StatusOK, catalog: `{"data":[{"id":"some-other-model"}]}`},
		"the catalog lists no model at all": {status: http.StatusOK, catalog: `{"data":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/models" {
					t.Errorf("readiness path = %q, want /v1/models", request.URL.Path)
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(testCase.status)
				_, _ = io.WriteString(writer, testCase.catalog)
			}))
			defer server.Close()

			detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
				BaseURL: server.URL + "/v1", APIKey: "local-marker", Model: "qwen-test", Timeout: time.Second,
			})
			if err != nil {
				t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
			}
			if readyErr := detector.Ready(t.Context()); (readyErr == nil) != testCase.wantReady {
				t.Errorf("Ready() error = %v, want ready = %v", readyErr, testCase.wantReady)
			}
		})
	}
}

// TestReadinessAndDetectionFailWhenTheModelIsUnreachable covers the outage every
// local deployment eventually has: Ollama is not running. Both entry points must
// return an error so the handler falls back to masking rather than passing text
// through unexamined.
func TestReadinessAndDetectionFailWhenTheModelIsUnreachable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close() // Nothing listens on this port now, which is the point.

	detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
		BaseURL: address + "/v1", APIKey: "local-marker", Model: "qwen-test", Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
	}
	if readyErr := detector.Ready(t.Context()); readyErr == nil {
		t.Error("Ready() error = nil, want a failure for an unreachable model")
	}
	if _, detectErr := detector.Detect(t.Context(), []string{"Alice is private"}); detectErr == nil {
		t.Error("Detect() error = nil, want a failure for an unreachable model")
	}
}

// TestDetectionInputIsBoundedBeforeTheModelIsCalled proves the bound is a
// pre-flight check rather than a post-hoc one: an oversized batch must cost
// nothing, because the whole point of these limits is that a hostile prompt
// cannot turn one webhook call into an unbounded local inference.
func TestDetectionInputIsBoundedBeforeTheModelIsCalled(t *testing.T) {
	t.Parallel()

	for name, texts := range map[string][]string{
		"too many items":       slices.Repeat([]string{"one"}, DefaultMaxItems+1),
		"one oversized item":   {strings.Repeat("x", DefaultMaxTextBytes+1)},
		"too much text in all": slices.Repeat([]string{strings.Repeat("y", DefaultMaxTextBytes)}, 5),
		"invalid UTF-8 text":   {"lone continuation byte \x80"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			model := &recordingLLM{response: `{"items":[]}`}
			detector, err := NewModelDetector(model)
			if err != nil {
				t.Fatalf("NewModelDetector() error = %v, want nil", err)
			}
			if _, err := detector.Detect(t.Context(), texts); err == nil {
				t.Fatal("Detect() error = nil, want the input bound to refuse")
			}
			if model.request != nil {
				t.Error("the model was called for input the bounds reject")
			}
		})
	}
}

// TestDetectionOfAnEmptyBatchSkipsTheModel keeps an empty gateway call free.
func TestDetectionOfAnEmptyBatchSkipsTheModel(t *testing.T) {
	t.Parallel()

	model := &recordingLLM{response: `{"items":[]}`}
	detector, err := NewModelDetector(model)
	if err != nil {
		t.Fatalf("NewModelDetector() error = %v, want nil", err)
	}
	spans, err := detector.Detect(t.Context(), nil)
	if err != nil {
		t.Fatalf("Detect() error = %v, want nil", err)
	}
	if len(spans) != 0 {
		t.Errorf("Detect() = %#v, want an empty result", spans)
	}
	if model.request != nil {
		t.Error("the model was called for an empty batch")
	}
}

// TestOnlyOneCompletePlainTextResponseIsTrusted pins the shape the parser
// accepts. Everything else — a stream, a partial chunk, a tool call, an empty
// or oversized body — is a response this guardrail cannot verify, and an
// unverifiable response must be an error so the handler masks conservatively.
func TestOnlyOneCompletePlainTextResponseIsTrusted(t *testing.T) {
	t.Parallel()

	good := func() *adkmodel.LLMResponse {
		return &adkmodel.LLMResponse{
			Content: genai.NewContentFromText(`{"items":[{"index":0,"entities":[]}]}`, genai.RoleModel),
		}
	}
	for name, model := range map[string]*scriptedLLM{
		"no response at all": {},
		"two responses":      {responses: []*adkmodel.LLMResponse{good(), good()}},
		"a nil response":     {responses: []*adkmodel.LLMResponse{nil}},
		"a partial chunk": {responses: []*adkmodel.LLMResponse{{
			Partial: true, Content: genai.NewContentFromText(`{"items":[{"index":0,"entities":[]}]}`, genai.RoleModel),
		}}},
		"no content": {responses: []*adkmodel.LLMResponse{{}}},
		"two parts": {responses: []*adkmodel.LLMResponse{{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: `{"items":[]}`}, {Text: "and more"}},
		}}}},
		"a tool call instead of text": {responses: []*adkmodel.LLMResponse{{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "exfiltrate"}}},
		}}}},
		"a thought part": {responses: []*adkmodel.LLMResponse{{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: `{"items":[{"index":0,"entities":[]}]}`, Thought: true}},
		}}}},
		"an empty body": {responses: []*adkmodel.LLMResponse{{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}},
		}}},
		"a body past the response bound": {responses: []*adkmodel.LLMResponse{{
			Content: genai.NewContentFromText(strings.Repeat("x", int(DefaultMaxModelResponseBytes)+1), genai.RoleModel),
		}}},
		"a transport failure": {err: errors.New("ollama unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detector, err := NewModelDetector(model)
			if err != nil {
				t.Fatalf("NewModelDetector() error = %v, want nil", err)
			}
			if _, err := detector.Detect(t.Context(), []string{"No name here"}); err == nil {
				t.Fatal("Detect() error = nil, want the unverifiable response to be refused")
			}
		})
	}
}

// TestModelDetectorRejectsRepeatedAndOverlongEntityLists closes the two ways a
// model can inflate one item's work: repeating an entity, which would report the
// same bytes twice, and returning more entities than the schema allows.
func TestModelDetectorRejectsRepeatedAndOverlongEntityLists(t *testing.T) {
	t.Parallel()

	entity := `{"type":"PERSON","text":"Alice"}`
	for name, output := range map[string]string{
		"a repeated entity": `{"items":[{"index":0,"entities":[` + entity + `,` + entity + `]}]}`,
		"too many entities": `{"items":[{"index":0,"entities":[` +
			strings.Join(slices.Repeat([]string{entity}, maxEntitiesPerItem+1), ",") + `]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detector, err := NewModelDetector(&recordingLLM{response: output})
			if err != nil {
				t.Fatalf("NewModelDetector() error = %v, want nil", err)
			}
			if _, err := detector.Detect(t.Context(), []string{"Alice paged Alice"}); err == nil {
				t.Fatal("Detect() error = nil, want strict output rejection")
			}
		})
	}
}

// TestSpansAreOrderedForTheMasker: the masker consumes spans in order and keeps
// the first of any overlapping pair, so this ordering is what decides whether
// "Acme Corp" is masked whole or reduced to "<ORGANIZATION> Corp". Ties on both
// offsets fall back to the class name so an identical response never produces
// two different bodies.
func TestSpansAreOrderedForTheMasker(t *testing.T) {
	t.Parallel()

	detector, err := NewModelDetector(&recordingLLM{response: `{
		"items":[{"index":0,"entities":[
			{"type":"ORGANIZATION","text":"Acme"},
			{"type":"ORGANIZATION","text":"Acme Corp"},
			{"type":"PERSON","text":"Acme Corp"},
			{"type":"LOCATION","text":"Corp"}
		]}]
	}`})
	if err != nil {
		t.Fatalf("NewModelDetector() error = %v, want nil", err)
	}
	got, err := detector.Detect(t.Context(), []string{"Acme Corp"})
	if err != nil {
		t.Fatalf("Detect() error = %v, want nil", err)
	}
	want := []Span{
		{Entity: EntityOrganization, Start: 0, End: 4},
		{Entity: EntityOrganization, Start: 0, End: 9},
		{Entity: EntityPerson, Start: 0, End: 9},
		{Entity: EntityLocation, Start: 5, End: 9},
	}
	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("Detect() spans = %#v, want %#v", got, want)
	}
}

func TestOpenAICompatibleDetectorUsesResponsesStructuredOutput(t *testing.T) {
	t.Parallel()

	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer local-marker" {
			t.Errorf("Authorization = %q, want local marker", authorization)
		}
		if request.URL.Path == "/v1/models" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"object":"list","data":[{"id":"qwen-test","object":"model"}]}`)
			return
		}
		if request.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q, want /v1/responses", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
			"id":"resp_ner","model":"qwen-test",
			"output":[{"type":"message","content":[{"type":"output_text","text":"{\"items\":[{\"index\":0,\"entities\":[{\"type\":\"PERSON\",\"text\":\"Alice\"}]}]}"}]}],
			"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}
		}`)
	}))
	defer server.Close()

	detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
		BaseURL: server.URL + "/v1",
		APIKey:  "local-marker",
		Model:   "qwen-test",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
	}
	if readyErr := detector.Ready(t.Context()); readyErr != nil {
		t.Fatalf("Ready() error = %v, want the configured model in /v1/models", readyErr)
	}
	got, err := detector.Detect(t.Context(), []string{"Alice responded"})
	if err != nil {
		t.Fatalf("Detect() error = %v, want nil", err)
	}
	if len(got) != 1 || !slices.Equal(got[0], []Span{{Entity: EntityPerson, Start: 0, End: 5}}) {
		t.Errorf("Detect() = %#v, want Alice PERSON span", got)
	}

	format, _ := requestBody["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true {
		t.Errorf("structured format = %#v, want strict json_schema", format)
	}
	if requestBody["model"] != "qwen-test" {
		t.Errorf("model = %#v, want qwen-test", requestBody["model"])
	}
}

func TestOpenAICompatibleDetectorBoundsTheResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, strings.Repeat("x", 2048))
	}))
	defer server.Close()

	detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
		BaseURL:          server.URL + "/v1",
		APIKey:           "local-marker",
		Model:            "qwen-test",
		Timeout:          time.Second,
		MaxResponseBytes: 256,
	})
	if err != nil {
		t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
	}
	if _, err := detector.Detect(t.Context(), []string{"Alice responded"}); err == nil {
		t.Fatal("Detect() error = nil, want oversized response rejection")
	}
}

// TestOpenAICompatibleDetectorBoundsAStreamedResponse closes the gap the
// Content-Length check alone leaves. A model that streams its answer announces
// no length up front, so the only thing standing between the webhook and an
// unbounded read is the bound applied while reading. The accepted case is the
// other half of the same contract: a cap that also rejects the largest legal
// answer is a cap nobody can configure.
func TestOpenAICompatibleDetectorBoundsAStreamedResponse(t *testing.T) {
	t.Parallel()

	body := `{"id":"resp_ner","model":"qwen-test","output":[{"type":"message","content":` +
		`[{"type":"output_text","text":"{\"items\":[{\"index\":0,\"entities\":[]}]}"}]}],` +
		`"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},` +
		`"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`

	for name, testCase := range map[string]struct {
		bound   int64
		wantErr bool
	}{
		"a streamed body exactly at the bound is accepted":   {bound: int64(len(body))},
		"a streamed body one byte over the bound is refused": {bound: int64(len(body)) - 1, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				flusher, streamable := writer.(http.Flusher)
				if !streamable {
					t.Error("the test server cannot stream; the read bound would not be exercised")
					return
				}
				// Flushing mid-body is what forces chunked transfer encoding, and
				// chunked means no Content-Length for the transport check to catch.
				_, _ = io.WriteString(writer, body[:16])
				flusher.Flush()
				_, _ = io.WriteString(writer, body[16:])
			}))
			defer server.Close()

			// Timeout is deliberately left unset: the published default has to be
			// usable on its own, since that is how the runtime builds this client.
			detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
				BaseURL: server.URL + "/v1", APIKey: "local-marker", Model: "qwen-test",
				MaxResponseBytes: testCase.bound,
			})
			if err != nil {
				t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
			}
			got, err := detector.Detect(t.Context(), []string{"No name here"})
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Detect() error = %v, want an error = %v", err, testCase.wantErr)
			}
			if !testCase.wantErr && len(got) != 1 {
				t.Errorf("Detect() = %#v, want one result for one input text", got)
			}
		})
	}
}

func TestOpenAICompatibleDetectorRefusesCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var captured atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				captured.Add(1)
				http.Error(writer, "redirect target", http.StatusBadGateway)
			}))
			t.Cleanup(target.Close)
			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", target.URL+"/v1/responses")
				writer.WriteHeader(status)
			}))
			t.Cleanup(source.Close)

			detector, err := NewOpenAICompatibleDetector(t.Context(), OpenAIConfig{
				BaseURL: source.URL + "/v1",
				APIKey:  "redirect-sensitive-pii-token",
				Model:   "qwen-test",
				Timeout: time.Second,
			})
			if err != nil {
				t.Fatalf("NewOpenAICompatibleDetector() error = %v, want nil", err)
			}
			if _, err := detector.Detect(t.Context(), []string{"Alice is private"}); err == nil {
				t.Fatal("Detect() error = nil, want redirect refusal")
			}
			if got := captured.Load(); got != 0 {
				t.Fatalf("redirect target received %d request(s), want zero credential or PII replays", got)
			}
		})
	}
}
