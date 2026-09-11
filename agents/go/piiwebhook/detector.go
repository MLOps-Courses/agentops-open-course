package piiwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3/option"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/httpguard"
)

const (
	DefaultMaxModelResponseBytes int64 = 256 * 1024
	maxEntitiesPerItem                 = 128
	maxEntityTextBytes                 = 512
	maxNEROutputTokens           int32 = 2048
)

const nerInstruction = `You are a named-entity recognizer for a private PII guardrail.
Return only PERSON, LOCATION, and ORGANIZATION entities as exact substrings copied with the original case from each indexed input.
PERSON means a natural person's name. LOCATION means a geographic place. ORGANIZATION means a legal entity, institution, or brand.
Do not classify incident identifiers, service names, hostnames, commands, paths, code, or angle-bracket placeholders.
Return one output item for every input index, including items with no entities. Do not infer, normalize, translate, or paraphrase.`

var errModelOutput = errors.New("invalid named-entity model output")

// OpenAIConfig describes the direct local OpenAI-compatible connection used by
// named-entity detection. BaseURL must point at Ollama, not agentgateway, or the
// webhook would recursively call itself.
type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration

	MaxResponseBytes int64
}

// ModelDetector converts a strict model response into byte spans that Handler
// can validate and mask.
type ModelDetector struct {
	model     adkmodel.LLM
	readiness func(context.Context) error
}

type modelInput struct {
	Items []modelInputItem `json:"items"`
}

type modelInputItem struct {
	Text  string `json:"text"`
	Index int    `json:"index"`
}

type modelOutput struct {
	Items []modelOutputItem `json:"items"`
}

type modelOutputItem struct {
	Entities []modelOutputEntity `json:"entities"`
	Index    int                 `json:"index"`
}

type modelOutputEntity struct {
	Type Entity `json:"type"`
	Text string `json:"text"`
}

// NewModelDetector accepts an ADK model so deterministic fakes exercise the
// same request and parser used by the local Ollama adapter.
func NewModelDetector(model adkmodel.LLM) (*ModelDetector, error) {
	if model == nil {
		return nil, fmt.Errorf("%w: model is required", ErrIncompleteConfig)
	}
	return &ModelDetector{model: model}, nil
}

// NewOpenAICompatibleDetector builds a no-retry ADK Responses API client with
// explicit endpoint, credential marker, timeout, and response-size bounds.
func NewOpenAICompatibleDetector(ctx context.Context, cfg OpenAIConfig) (*ModelDetector, error) {
	baseURL, err := validateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("%w: APIKey is required", ErrIncompleteConfig)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("%w: Model is required", ErrIncompleteConfig)
	}
	if cfg.Timeout < 0 || cfg.MaxResponseBytes < 0 {
		return nil, fmt.Errorf("%w: bounds cannot be negative", ErrIncompleteConfig)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultDetectionTimeout
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = DefaultMaxModelResponseBytes
	}

	transport := &boundedTransport{
		base:             http.DefaultTransport,
		maxResponseBytes: cfg.MaxResponseBytes,
	}
	httpClient := httpguard.NoRedirects(&http.Client{Transport: transport, Timeout: cfg.Timeout})
	client := &openaimodel.ClientConfig{
		APIKey:     strings.TrimSpace(cfg.APIKey),
		BaseURL:    baseURL,
		HTTPClient: httpClient,
		Options:    []option.RequestOption{option.WithMaxRetries(0)},
	}
	llm, err := openaimodel.NewModel(ctx, strings.TrimSpace(cfg.Model), client)
	if err != nil {
		return nil, fmt.Errorf("build named-entity model: %w", err)
	}
	detector, err := NewModelDetector(llm)
	if err != nil {
		return nil, err
	}
	detector.readiness = modelReadiness(httpClient, baseURL, strings.TrimSpace(cfg.APIKey), strings.TrimSpace(cfg.Model))
	return detector, nil
}

// Ready checks the model catalog without running inference. Detectors built
// over an injected model have no external dependency and are ready once built.
func (d *ModelDetector) Ready(ctx context.Context) error {
	if d.readiness == nil {
		return nil
	}
	return d.readiness(ctx)
}

func modelReadiness(client *http.Client, baseURL, apiKey, model string) func(context.Context) error {
	type catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	return func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
		if err != nil {
			return fmt.Errorf("build named-entity readiness request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("query named-entity model catalog: %w", err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("named-entity model catalog returned HTTP %d", response.StatusCode)
		}
		var available catalog
		if err := json.NewDecoder(response.Body).Decode(&available); err != nil {
			return fmt.Errorf("decode named-entity model catalog: %w", err)
		}
		if !slices.ContainsFunc(available.Data, func(candidate struct {
			ID string `json:"id"`
		},
		) bool {
			return candidate.ID == model
		}) {
			return errors.New("configured named-entity model is absent from the model catalog")
		}
		return nil
	}
}

func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: BaseURL must be an http(s) origin or path", ErrIncompleteConfig)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Detect makes one non-streaming structured-output call for the whole bounded
// batch. Every external field is revalidated before it becomes a trusted span.
func (d *ModelDetector) Detect(ctx context.Context, texts []string) ([][]Span, error) {
	if err := validateDetectionInput(texts); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return [][]Span{}, nil
	}

	input := modelInput{Items: make([]modelInputItem, len(texts))}
	for index, text := range texts {
		input.Items[index] = modelInputItem{Index: index, Text: text}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode named-entity request: %w", err)
	}

	temperature := float32(0)
	request := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(string(payload), genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(nerInstruction, genai.RoleUser),
			Temperature:       &temperature,
			MaxOutputTokens:   maxNEROutputTokens,
			ResponseMIMEType:  "application/json",
			ResponseSchema:    nerResponseSchema(),
		},
	}
	response, err := oneModelText(d.model.GenerateContent(ctx, request, false))
	if err != nil {
		return nil, err
	}
	return parseModelOutput(response, texts)
}

func validateDetectionInput(texts []string) error {
	if len(texts) > DefaultMaxItems {
		return errors.New("named-entity input: too many items")
	}
	total := 0
	for _, text := range texts {
		if !utf8.ValidString(text) || len(text) > DefaultMaxTextBytes {
			return errors.New("named-entity input: invalid text")
		}
		total += len(text)
		if total > DefaultMaxTotalTextBytes {
			return errors.New("named-entity input: too much text")
		}
	}
	return nil
}

func oneModelText(sequence iter.Seq2[*adkmodel.LLMResponse, error]) (string, error) {
	var output string
	responses := 0
	for response, err := range sequence {
		if err != nil {
			return "", fmt.Errorf("named-entity model call: %w", err)
		}
		responses++
		if responses > 1 || response == nil || response.Partial || response.Content == nil ||
			len(response.Content.Parts) != 1 || !plainTextPart(response.Content.Parts[0]) {
			return "", errModelOutput
		}
		output = response.Content.Parts[0].Text
		if output == "" || len(output) > int(DefaultMaxModelResponseBytes) || !utf8.ValidString(output) {
			return "", errModelOutput
		}
	}
	if responses != 1 {
		return "", errModelOutput
	}
	return output, nil
}

func plainTextPart(part *genai.Part) bool {
	return part != nil && part.Text != "" && part.MediaResolution == nil &&
		part.CodeExecutionResult == nil && part.ExecutableCode == nil && part.FileData == nil &&
		part.FunctionCall == nil && part.FunctionResponse == nil && part.InlineData == nil &&
		!part.Thought && len(part.ThoughtSignature) == 0 && part.VideoMetadata == nil &&
		part.ToolCall == nil && part.ToolResponse == nil && len(part.PartMetadata) == 0
}

func parseModelOutput(raw string, texts []string) ([][]Span, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var output modelOutput
	if err := decoder.Decode(&output); err != nil {
		return nil, errModelOutput
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errModelOutput
	}
	if len(output.Items) != len(texts) {
		return nil, errModelOutput
	}

	result := make([][]Span, len(texts))
	seenIndexes := make([]bool, len(texts))
	for _, item := range output.Items {
		if item.Index < 0 || item.Index >= len(texts) || seenIndexes[item.Index] ||
			len(item.Entities) > maxEntitiesPerItem {
			return nil, errModelOutput
		}
		seenIndexes[item.Index] = true
		seenEntities := make(map[modelOutputEntity]struct{}, len(item.Entities))
		for _, entity := range item.Entities {
			if !entity.Type.valid() || entity.Text == "" || len(entity.Text) > maxEntityTextBytes ||
				!utf8.ValidString(entity.Text) || strings.ContainsAny(entity.Text, "<>") {
				return nil, errModelOutput
			}
			if _, duplicate := seenEntities[entity]; duplicate {
				return nil, errModelOutput
			}
			seenEntities[entity] = struct{}{}
			spans := exactSpans(texts[item.Index], entity)
			if len(spans) == 0 {
				return nil, errModelOutput
			}
			result[item.Index] = append(result[item.Index], spans...)
		}
		slices.SortFunc(result[item.Index], func(left, right Span) int {
			if left.Start != right.Start {
				return left.Start - right.Start
			}
			if left.End != right.End {
				return left.End - right.End
			}
			return strings.Compare(string(left.Entity), string(right.Entity))
		})
	}
	return result, nil
}

func exactSpans(text string, entity modelOutputEntity) []Span {
	var spans []Span
	for searchFrom := 0; searchFrom <= len(text)-len(entity.Text); {
		offset := strings.Index(text[searchFrom:], entity.Text)
		if offset < 0 {
			break
		}
		start := searchFrom + offset
		end := start + len(entity.Text)
		spans = append(spans, Span{Entity: entity.Type, Start: start, End: end})
		searchFrom = end
	}
	return spans
}

func nerResponseSchema() *genai.Schema {
	maxItems := int64(DefaultMaxItems)
	maxEntities := int64(maxEntitiesPerItem)
	maxText := int64(maxEntityTextBytes)
	return &genai.Schema{
		Title:    "named_entity_detection",
		Type:     genai.TypeObject,
		Required: []string{"items"},
		Properties: map[string]*genai.Schema{
			"items": {
				Type:     genai.TypeArray,
				MaxItems: &maxItems,
				Items: &genai.Schema{
					Type:     genai.TypeObject,
					Required: []string{"index", "entities"},
					Properties: map[string]*genai.Schema{
						"index": {Type: genai.TypeInteger},
						"entities": {
							Type:     genai.TypeArray,
							MaxItems: &maxEntities,
							Items: &genai.Schema{
								Type:     genai.TypeObject,
								Required: []string{"type", "text"},
								Properties: map[string]*genai.Schema{
									"type": {
										Type: genai.TypeString,
										Enum: []string{string(EntityPerson), string(EntityLocation), string(EntityOrganization)},
									},
									"text": {Type: genai.TypeString, MaxLength: &maxText},
								},
							},
						},
					},
				},
			},
		},
	}
}

type boundedTransport struct {
	base             http.RoundTripper
	maxResponseBytes int64
}

func (t *boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > t.maxResponseBytes {
		_ = response.Body.Close()
		return nil, fmt.Errorf("named-entity model response exceeds %d bytes", t.maxResponseBytes)
	}
	response.Body = &boundedReadCloser{
		ReadCloser: response.Body,
		remaining:  t.maxResponseBytes,
	}
	return response, nil
}

type boundedReadCloser struct {
	io.ReadCloser
	remaining int64
}

func (r *boundedReadCloser) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		var extra [1]byte
		count, err := r.ReadCloser.Read(extra[:])
		if count > 0 {
			return 0, errors.New("named-entity model response exceeded configured bound")
		}
		return 0, err
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	count, err := r.ReadCloser.Read(buffer)
	r.remaining -= int64(count)
	return count, err
}
