package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/httpguard"
)

// The two Ollama endpoints this package speaks. /api/embed produces the
// vectors; /api/tags is where the immutable artifact digest that binds them to
// a model build comes from.
const (
	embedPath = "/api/embed"
	tagsPath  = "/api/tags"
)

// digestTimeout caps the model-metadata request. It is deliberately much
// shorter than the embedding timeout: a cold model can take minutes to load,
// but listing tags is a local lookup, and the digest is optional anyway — a
// slow /api/tags must not turn into a slow tool call.
const digestTimeout = 10 * time.Second

// maxResponseBytes bounds what a compromised or confused endpoint can make this
// process allocate. A 100-chunk corpus at 768 dimensions is roughly 2 MB of
// JSON, so this leaves three orders of magnitude of headroom.
const maxResponseBytes = 64 << 20

// EmbeddingUnavailableError reports that the embeddings endpoint — or a vector
// generation compatible with the current provenance — cannot serve this search.
//
// It is the one failure search_runbooks recovers from, by falling back to the
// deterministic keyword scorer. Every other failure propagates, because a
// fallback that hides a broken corpus or a broken state directory would make
// the degradation invisible.
type EmbeddingUnavailableError struct {
	// Err is the underlying cause, when there is one. It comes first only
	// because go vet's fieldalignment check wants the pointer-dense field there.
	Err error
	// Reason is the operator-facing sentence, and says what to do next.
	Reason string
}

func (e *EmbeddingUnavailableError) Error() string { return e.Reason }

func (e *EmbeddingUnavailableError) Unwrap() error { return e.Err }

// embeddingUnavailable builds the error above. Every refusal in this file and
// in retrieval.go goes through it, so there is one place the class is defined.
func embeddingUnavailable(reason string, cause error) error {
	return &EmbeddingUnavailableError{Reason: reason, Err: cause}
}

// embeddingsClient talks to the local Ollama (or any endpoint that speaks the
// same two routes).
type embeddingsClient struct {
	http    *http.Client
	url     string
	model   string
	timeout time.Duration
}

// newEmbeddingsClient builds the client, defaulting the HTTP client so a caller
// only has to supply one when it wants to intercept the transport.
func newEmbeddingsClient(client *http.Client, url, model string, timeout time.Duration) *embeddingsClient {
	// No client-level timeout: the per-request deadline below is a context, so
	// it composes with the tool deadline the guard installs instead of racing it.
	// Clone caller-owned clients before installing the no-redirect boundary.
	client = httpguard.NoRedirects(client)
	return &embeddingsClient{
		http:    client,
		url:     strings.TrimSuffix(url, "/"),
		model:   model,
		timeout: timeout,
	}
}

// unavailable is the actionable failure every transport-level problem reports.
//
// It names the endpoint, the model, and the two ways out — pull the model, or
// turn the feature off — because "embeddings unavailable" on its own sends a
// learner to search the source.
func (c *embeddingsClient) unavailable(cause error) error {
	return embeddingUnavailable(fmt.Sprintf(
		"Embeddings unavailable at %s with model %q; start Ollama and `ollama pull %s`, "+
			"or unset AGENT_SEMANTIC_RETRIEVAL.",
		c.url, c.model, c.model,
	), cause)
}

// Embed returns one vector per text.
//
// Every response is validated before it is trusted: the batch size, the shape
// of each vector, and every value in it. A vector store is durable state, and a
// NaN or a string that reaches it makes the whole generation unusable in a way
// that only shows up at query time.
func (c *embeddingsClient) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	payload, err := json.Marshal(map[string]any{"model": c.model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("encode the embedding request: %w", err)
	}
	body, err := c.call(ctx, http.MethodPost, embedPath, payload, c.timeout)
	if err != nil {
		return nil, err
	}
	raw, err := field(body, "embeddings")
	if err != nil {
		return nil, c.unavailable(err)
	}
	list, ok := raw.([]any)
	if !ok || len(list) != len(texts) {
		return nil, embeddingUnavailable("Embeddings endpoint returned a mismatched batch.", nil)
	}
	vectors := make([][]float64, 0, len(list))
	for _, item := range list {
		vector, err := parseVector(item)
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, vector)
	}
	return vectors, nil
}

// parseVector validates one embedding vector.
func parseVector(item any) ([]float64, error) {
	values, ok := item.([]any)
	if !ok || len(values) == 0 {
		return nil, embeddingUnavailable("Embeddings endpoint returned an empty or invalid vector.", nil)
	}
	vector := make([]float64, 0, len(values))
	for _, value := range values {
		// Anything that is not a JSON number decodes to another Go type — a bool, a
		// string, a nested array — and is rejected here rather than silently
		// coerced. JSON has no infinity literal, so a magnitude SQLite could not
		// store shows up as a decode failure instead, one layer up.
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, embeddingUnavailable("Embeddings endpoint returned a non-finite or non-numeric vector.", nil)
		}
		vector = append(vector, number)
	}
	return vector, nil
}

// ModelDigest returns Ollama's immutable digest for the configured model, or ""
// when the endpoint does not expose one.
//
// The digest is optional on purpose: an endpoint that is not Ollama still
// serves vectors, and refusing to work without provenance metadata would make
// the feature unusable behind a proxy. "" is a real provenance value — it means
// "unresolved" — and a generation built while unresolved is invalidated as soon
// as a digest does resolve, because the two values differ.
func (c *embeddingsClient) ModelDigest(ctx context.Context) string {
	if c.model == "" {
		return ""
	}
	timeout := digestTimeout
	if c.timeout > 0 {
		timeout = min(c.timeout, digestTimeout)
	}
	body, err := c.call(ctx, http.MethodGet, tagsPath, nil, timeout)
	if err != nil {
		return ""
	}
	raw, err := field(body, "models")
	if err != nil {
		return ""
	}
	list, ok := raw.([]any)
	if !ok {
		return ""
	}
	// Ollama reports an untagged model as "name:latest", so a configured name
	// without a tag has to match both spellings. A name that already carries a
	// tag matches exactly — asking for :latest and getting :v2 is a different
	// artifact, and pretending otherwise is what provenance exists to prevent.
	aliases := map[string]bool{c.model: true}
	if !strings.Contains(c.model, ":") {
		aliases[c.model+":latest"] = true
	}
	for _, item := range list {
		model, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := model["name"].(string)
		if !aliases[name] {
			continue
		}
		// The first matching entry decides, including when it carries no usable
		// digest: a second entry for the same name would be the same artifact.
		digest, ok := model["digest"].(string)
		if !ok {
			return ""
		}
		return digest
	}
	return ""
}

// call issues one request and returns its whole body, bounded.
//
// The body is read here rather than streamed back because the request deadline
// is a context: returning an unread body would leave the caller racing the
// cancel this function has to schedule.
//
// The deadline composes with whatever deadline the caller already installed —
// the shorter of the two wins, which is what a tool timeout wrapping an
// embedding timeout should do.
func (c *embeddingsClient) call(
	ctx context.Context, method, path string, payload []byte, timeout time.Duration,
) (body []byte, err error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.url+path, reader)
	if err != nil {
		return nil, c.unavailable(fmt.Errorf("build the %s request: %w", path, err))
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, c.unavailable(fmt.Errorf("call %s: %w", path, err))
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && err == nil {
			err = c.unavailable(fmt.Errorf("close the %s response: %w", path, closeErr))
			body = nil
		}
	}()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// The body is drained so the connection can be reused, and discarded: an
		// error page is not something to hand back to a model.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return nil, c.unavailable(fmt.Errorf("call %s: unexpected status %s", path, response.Status))
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, c.unavailable(fmt.Errorf("read the %s response: %w", path, err))
	}
	return body, nil
}

// field decodes one top-level key of a JSON object.
//
// The two-stage decode distinguishes "the key is missing" — a protocol error
// that reports the actionable endpoint message — from "the key holds the wrong
// thing", which reports the specific shape complaint. Collapsing them would
// tell a learner to pull a model when the model is already pulled.
func field(body []byte, name string) (any, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode the response: %w", err)
	}
	raw, ok := envelope[name]
	if !ok {
		return nil, fmt.Errorf("the response carries no %q field", name)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode the %q field: %w", name, err)
	}
	return decoded, nil
}
