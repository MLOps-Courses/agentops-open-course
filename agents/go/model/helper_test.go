package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// Every test in this package is offline. A model is only ever reached through
// an httptest server speaking the provider's wire format, which is what lets
// the suite assert *which* provider was selected — the strongest available
// proof — without an account, a key, or a running model server.

// defaults returns the configuration a learner gets from an empty environment.
//
// Reading it through config.LoadFrom instead of assembling a Config by hand is
// deliberate: every test then runs against the shipped account-free defaults,
// so a change to them surfaces here rather than hiding behind a test literal.
func defaults(t *testing.T) config.Config {
	t.Helper()

	cfg, err := config.LoadFrom(map[string]string{})
	if err != nil {
		t.Fatalf("config.LoadFrom(empty environment) error = %v, want nil", err)
	}
	return cfg
}

// recordedRequest is one request a fake provider endpoint received. Field order
// follows go vet's fieldalignment check rather than reading order.
type recordedRequest struct {
	header http.Header
	body   map[string]any
	path   string
}

// stubEndpoint is a fake provider endpoint. It records every request and
// delegates the reply to the test, so a test can assert on the URL, the
// credential and the request body a provider actually produced.
type stubEndpoint struct {
	*httptest.Server

	// received is appended from the server's goroutine and read from the test's,
	// so mu guards it. The two sit apart because field order follows
	// fieldalignment, not the pairing.
	received []recordedRequest
	mu       sync.Mutex
}

// newStubEndpoint starts an endpoint that answers with handle and shuts down
// with the test.
func newStubEndpoint(t *testing.T, handle http.HandlerFunc) *stubEndpoint {
	t.Helper()

	endpoint := &stubEndpoint{}
	endpoint.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint.record(t, r)
		handle(w, r)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

// replyWith starts an endpoint that answers every request with one JSON body.
func replyWith(t *testing.T, body string) *stubEndpoint {
	t.Helper()

	return newStubEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("writing the stub reply: %v", err)
		}
	})
}

// record captures a request from the server's goroutine. Errors are reported
// with Errorf rather than Fatalf, which may only be called from the goroutine
// running the test.
func (e *stubEndpoint) record(t *testing.T, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading the request body: %v", err)
		return
	}
	// Hand the body back so a handler can still read it and answer differently
	// per request — that is how the fallback round trip tells two models apart.
	r.Body = io.NopCloser(bytes.NewReader(raw))

	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decoding the request body %q: %v", raw, err)
			return
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.received = append(e.received, recordedRequest{path: r.URL.Path, header: r.Header.Clone(), body: body})
}

// requests returns everything the endpoint has received so far.
func (e *stubEndpoint) requests() []recordedRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.received)
}

// only returns the single request the endpoint was expected to receive.
func (e *stubEndpoint) only(t *testing.T) recordedRequest {
	t.Helper()

	received := e.requests()
	if len(received) != 1 {
		t.Fatalf("endpoint received %d requests, want exactly 1", len(received))
	}
	return received[0]
}

// mustJSON renders a reply body, failing the test rather than the endpoint if
// the fixture itself is malformed.
func mustJSON(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding the stub reply: %v", err)
	}
	return string(encoded)
}

// prompt is the question every round trip sends. Its content is irrelevant to
// what these tests assert — provider selection, credentials, deadlines and
// chaining — so keeping it fixed keeps the recorded request bodies comparable.
const prompt = "hi"

// request builds the one-turn request the round-trip tests send.
func request() *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role:  string(genai.RoleUser),
			Parts: []*genai.Part{{Text: prompt}},
		}},
	}
}

// drive consumes a model's response sequence and returns the text of every
// response plus the first error.
//
// The sequence is ranged over and only the text is kept. Nothing collects the
// responses themselves: an event stream that is buffered stops streaming, and
// ADK only executes function calls on non-partial responses, so buffering also
// breaks the human-in-the-loop pause. The tests hold the production rule.
func drive(t *testing.T, llm adkmodel.LLM) ([]string, error) {
	t.Helper()

	var spoken []string
	for response, err := range llm.GenerateContent(t.Context(), request(), false) {
		if err != nil {
			return spoken, err
		}
		spoken = append(spoken, responseText(response))
	}
	return spoken, nil
}

// single drives one non-streaming turn and returns the response, failing the
// test unless exactly one was produced.
func single(t *testing.T, llm adkmodel.LLM) *adkmodel.LLMResponse {
	t.Helper()

	var last *adkmodel.LLMResponse
	var failure error
	count := 0
	// Assertions stay outside the loop body: a t.Fatalf inside a range-over-func
	// unwinds through the iterator, which is not a failure mode worth debugging.
	for response, err := range llm.GenerateContent(t.Context(), request(), false) {
		if err != nil {
			failure = err
			break
		}
		last, count = response, count+1
	}
	if failure != nil {
		t.Fatalf("GenerateContent() error = %v, want nil", failure)
	}
	if count != 1 {
		t.Fatalf("GenerateContent() yielded %d responses, want exactly 1", count)
	}
	return last
}

// responseText concatenates the text parts of a response.
func responseText(response *adkmodel.LLMResponse) string {
	if response == nil || response.Content == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range response.Content.Parts {
		if part != nil {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

// errModelDown is the failure a stub model reports. It is a sentinel so the
// fallback tests can prove the wrapping chain keeps the cause reachable through
// errors.Is rather than only matching on a message.
var errModelDown = errors.New("model is unreachable")

// stubLLM answers with fixed text, fails before it answers, or fails after it
// has begun answering — the three cases the fallback rule distinguishes.
type stubLLM struct {
	name       string
	reply      string
	failBefore bool
	failAfter  bool
	calls      int
}

// stubLLM stands in for a provider model wherever the behavior under test is
// the chaining rule rather than the wire format.
var _ adkmodel.LLM = (*stubLLM)(nil)

func (s *stubLLM) Name() string { return s.name }

func (s *stubLLM) GenerateContent(
	_ context.Context, _ *adkmodel.LLMRequest, _ bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		s.calls++
		if s.failBefore {
			yield(nil, fmt.Errorf("%s: %w", s.name, errModelDown))
			return
		}
		if !yield(textResponse(s.reply), nil) {
			return
		}
		if s.failAfter {
			yield(nil, fmt.Errorf("%s dropped the turn: %w", s.name, errModelDown))
		}
	}
}

// textResponse builds the shape a provider model yields for a plain answer.
func textResponse(text string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  string(genai.RoleModel),
			Parts: []*genai.Part{{Text: text}},
		},
	}
}
