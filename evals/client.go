package evals

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultTurnTimeout = 30 * time.Minute
	maxResponseBytes   = 32 << 20
	maxSSEEventBytes   = 4 << 20
)

var (
	errSendRESTRequest     = errors.New("send REST request")
	errSendRESTSSERequest  = errors.New("send REST SSE request")
	errSendA2ARequest      = errors.New("send A2A request")
	errReadA2AStream       = errors.New("read A2A stream response")
	errReadRESTSSEResponse = errors.New("read REST SSE response")
	errDecodeRESTSSEEvent  = errors.New("decode REST SSE event")
	errDecodeRESTResponse  = errors.New("decode REST response")
	errDecodeA2AResponse   = errors.New("decode A2A response")
)

type AgentClient interface {
	CreateSession(context.Context) (string, error)
	Send(context.Context, string, string) (Turn, error)
	Confirm(context.Context, string, Turn, bool, string) (Turn, error)
	Close() error
}

type RESTClient struct {
	baseURL   *url.URL
	http      *http.Client
	appName   string
	userID    string
	streaming bool
}

type RESTClientConfig struct {
	Client    *http.Client
	BaseURL   string
	AppName   string
	UserID    string
	Timeout   time.Duration
	Streaming bool
}

func NewRESTClient(config RESTClientConfig) (*RESTClient, error) {
	baseURL, err := parseBaseURL(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("REST base URL: %w", err)
	}
	if config.AppName == "" {
		config.AppName = "agent"
	}
	if config.UserID == "" {
		config.UserID = "evals"
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTurnTimeout
	}
	return &RESTClient{
		baseURL:   baseURL,
		appName:   config.AppName,
		userID:    config.UserID,
		streaming: config.Streaming,
		http:      clientWithoutRedirects(config.Client, config.Timeout),
	}, nil
}

func (c *RESTClient) CreateSession(ctx context.Context) (string, error) {
	var session struct {
		ID string `json:"id"`
	}
	route := fmt.Sprintf("apps/%s/users/%s/sessions", url.PathEscape(c.appName), url.PathEscape(c.userID))
	if err := c.doJSON(ctx, http.MethodPost, route, map[string]any{}, &session); err != nil {
		return "", fmt.Errorf("create REST session: %w", err)
	}
	if session.ID == "" {
		return "", errors.New("create REST session: response has no id")
	}
	return session.ID, nil
}

func (c *RESTClient) Send(ctx context.Context, sessionID, text string) (Turn, error) {
	return c.run(ctx, sessionID, []Part{{Text: text}})
}

func (c *RESTClient) Confirm(
	ctx context.Context,
	sessionID string,
	turn Turn,
	approved bool,
	rationale string,
) (Turn, error) {
	pending := turn.AwaitingConfirmation
	if pending == nil {
		return Turn{}, errors.New("confirm REST turn: turn is not awaiting confirmation")
	}
	return c.run(ctx, sessionID, []Part{{FunctionResponse: &FunctionResponse{
		ID:   pending.CallID,
		Name: pending.Name,
		Response: map[string]any{
			"confirmed": approved,
			"payload":   map[string]any{"rationale": rationale},
		},
	}}})
}

func (c *RESTClient) Close() error {
	return nil
}

func (c *RESTClient) run(ctx context.Context, sessionID string, parts []Part) (Turn, error) {
	payload := map[string]any{
		"appName":   c.appName,
		"userId":    c.userID,
		"sessionId": sessionID,
		"newMessage": Content{
			Role:  "user",
			Parts: parts,
		},
	}
	if c.streaming {
		events, err := c.runSSE(ctx, payload)
		if err != nil {
			return Turn{}, err
		}
		return FoldEvents(events), nil
	}
	var events []Event
	if err := c.doJSON(ctx, http.MethodPost, "run", payload, &events); err != nil {
		return Turn{}, fmt.Errorf("run REST turn: %w", err)
	}
	return FoldEvents(events), nil
}

func (c *RESTClient) runSSE(ctx context.Context, payload any) ([]Event, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode REST SSE request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve("run_sse"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build REST SSE request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response, err := c.http.Do(request)
	if err != nil {
		// Transport errors can contain URLs, credentials, or provider text.
		return nil, errSendRESTSSERequest
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, responseStatusError(response)
	}

	var events []Event
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maxResponseBytes))
	scanner.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
	eventType := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if eventType == "error" {
				// SSE error data is a provider-controlled response body and may
				// contain echoed prompts, credentials, or private diagnostics.
				return nil, errors.New("REST SSE error event")
			}
			var event Event
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return nil, errDecodeRESTSSEEvent
			}
			events = append(events, event)
		case line == "":
			eventType = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errReadRESTSSEResponse
	}
	return events, nil
}

func (c *RESTClient) doJSON(ctx context.Context, method, route string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.resolve(route), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return errSendRESTRequest
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return responseStatusError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return errDecodeRESTResponse
	}
	return nil
}

func (c *RESTClient) resolve(route string) string {
	resolved := *c.baseURL
	resolved.Path = strings.TrimRight(resolved.Path, "/") + "/" + strings.TrimLeft(route, "/")
	return resolved.String()
}

type A2AClient struct {
	baseURL        *url.URL
	http           *http.Client
	tasks          map[string]string
	identityHeader string
	identity       string
}

type A2AClientConfig struct {
	Client  *http.Client
	BaseURL string
	// IdentityHeader and Identity make this client stand in for the gateway.
	//
	// The agent's A2A boundary grants an authenticated principal only from a header
	// a trusted proxy set, and a guarded write refuses without one. Evaluating "did
	// the agent stop and ask, and did the approval carry a named approver?" therefore
	// needs the harness to be that proxy. Both empty means the client sends no header
	// and the server grants no principal, which is the read-only shape.
	IdentityHeader string
	Identity       string
	Timeout        time.Duration
}

func NewA2AClient(config A2AClientConfig) (*A2AClient, error) {
	baseURL, err := parseBaseURL(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("A2A base URL: %w", err)
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTurnTimeout
	}
	if (config.IdentityHeader == "") != (config.Identity == "") {
		return nil, errors.New("A2A identity needs both a header name and a subject, or neither")
	}
	return &A2AClient{
		baseURL:        baseURL,
		http:           clientWithoutRedirects(config.Client, config.Timeout),
		tasks:          make(map[string]string),
		identityHeader: config.IdentityHeader,
		identity:       config.Identity,
	}, nil
}

func (c *A2AClient) CreateSession(context.Context) (string, error) {
	return randomUUID()
}

func (c *A2AClient) Send(ctx context.Context, sessionID, text string) (Turn, error) {
	return c.send(ctx, sessionID, []a2aPart{{Kind: "text", Text: text}})
}

func (c *A2AClient) Confirm(
	ctx context.Context,
	sessionID string,
	turn Turn,
	approved bool,
	rationale string,
) (Turn, error) {
	pending := turn.AwaitingConfirmation
	if pending == nil {
		return Turn{}, errors.New("confirm A2A turn: turn is not awaiting confirmation")
	}
	return c.send(ctx, sessionID, []a2aPart{{
		Kind: "data",
		Data: map[string]any{
			"id":   pending.CallID,
			"name": pending.Name,
			"response": map[string]any{
				"confirmed": approved,
				"payload":   map[string]any{"rationale": rationale},
			},
		},
		Metadata: map[string]any{"adk_type": "function_response"},
	}})
}

func (c *A2AClient) Close() error {
	return nil
}

func (c *A2AClient) send(ctx context.Context, sessionID string, parts []a2aPart) (Turn, error) {
	messageID, err := randomUUID()
	if err != nil {
		return Turn{}, fmt.Errorf("mint A2A message id: %w", err)
	}
	message := map[string]any{
		"kind":      "message",
		"role":      "user",
		"messageId": messageID,
		"contextId": sessionID,
		"parts":     parts,
	}
	if taskID := c.tasks[sessionID]; taskID != "" {
		message["taskId"] = taskID
	}
	// message/stream rather than message/send, for evidence rather than latency.
	// A2A's unary reply is a stored Task, and storing a task discards the per-event
	// metadata ADK attaches on the way — including `adk_usage_metadata`, which is
	// where every token this run spent is counted. The stream carries one update
	// per ADK event with that metadata intact, which is the same event boundary the
	// REST transport folds, so both transports produce the same typed turn.
	updates, err := c.stream(ctx, "message/stream", map[string]any{"message": message})
	if err != nil {
		return Turn{}, err
	}
	result := foldStreamUpdates(updates)
	// A task id is carried into the next message only while that task can still
	// accept one. An A2A task that reached a terminal state rejects a follow-up
	// with "task in a terminal state" — so remembering a completed id turns every
	// second turn of a conversation into a JSON-RPC error, while forgetting a
	// paused one abandons the confirmation the agent is waiting on.
	if result.Kind == "task" && result.ID != "" && acceptsAnotherMessage(result.Status) {
		c.tasks[sessionID] = result.ID
	} else {
		delete(c.tasks, sessionID)
	}
	events, err := eventsFromA2A(result)
	if err != nil {
		return Turn{}, fmt.Errorf("normalize A2A result: %w", err)
	}
	return FoldEvents(events), nil
}

// acceptsAnotherMessage reports whether an A2A task is still open for input.
//
// The spec's terminal states are completed, canceled, failed, and rejected;
// everything else — submitted, working, input-required, auth-required — can take
// another message on the same task. This lists the open states rather than the
// terminal ones so an unrecognized or missing state falls back to a fresh task,
// which the server always accepts, instead of to an error it always refuses.
//
// Both spellings are matched because the wire carries the short form while the
// server's own diagnostics quote the protobuf enum name.
func acceptsAnotherMessage(status *a2aStatus) bool {
	if status == nil {
		return false
	}
	normalized := strings.ToLower(strings.TrimPrefix(strings.ToUpper(status.State), "TASK_STATE_"))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "submitted", "working", "input-required", "auth-required":
		return true
	default:
		return false
	}
}

type rpcEnvelope struct {
	Error   *rpcError       `json:"error"`
	ID      string          `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
}

type rpcError struct {
	Code int `json:"code"`
}

// a2aStreamEvent is one frame of a message/stream response: the opening task
// snapshot, a status change, an artifact the agent produced, or a bare message.
// The metadata sits on the frame rather than inside the artifact, which is the
// whole reason this transport is streamed — see [A2AClient.send].
type a2aStreamEvent struct {
	Artifact  *a2aArtifact   `json:"artifact,omitempty"`
	Status    *a2aStatus     `json:"status,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Kind      string         `json:"kind"`
	ID        string         `json:"id,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Parts     []a2aPart      `json:"parts,omitempty"`
}

type a2aArtifact struct {
	Parts []a2aPart `json:"parts"`
}

// foldStreamUpdates collapses a stream back into the task shape the unary reply
// would have had, so one normalizer serves both and the tests that pin it keep
// pinning it.
func foldStreamUpdates(updates []a2aStreamEvent) a2aResult {
	result := a2aResult{Kind: "task"}
	for _, update := range updates {
		switch update.Kind {
		case "task":
			// The opening snapshot names the task and repeats the conversation so
			// far. Its stored artifacts belong to earlier turns, so only the identity
			// is taken here; folding them would double-count this turn's evidence and
			// its tokens on the second question of any multi-turn case.
			result.ID, result.ContextID = update.ID, update.ContextID
			if update.Status != nil {
				result.Status = update.Status
			}
		case "status-update":
			result.Status = update.Status
			if len(update.Metadata) > 0 {
				result.Metadata = update.Metadata
			}
		case "artifact-update":
			if update.Artifact == nil {
				continue
			}
			result.Artifacts = append(result.Artifacts, a2aContainer{
				Parts: update.Artifact.Parts, Metadata: update.Metadata,
			})
		case "message":
			result.Artifacts = append(result.Artifacts, a2aContainer{
				Parts: update.Parts, Metadata: update.Metadata,
			})
		}
	}
	return result
}

// stream sends one JSON-RPC request and reads its server-sent event response.
//
// Every frame repeats the request's own id, so each is correlated before its
// payload is trusted, exactly as the unary path did.
func (c *A2AClient) stream(ctx context.Context, method string, params any) ([]a2aStreamEvent, error) {
	id, err := randomUUID()
	if err != nil {
		return nil, fmt.Errorf("mint JSON-RPC id: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	if c.identityHeader != "" {
		request.Header.Set(c.identityHeader, c.identity)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errSendA2ARequest
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, responseStatusError(response)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		// The reader error is as untrusted as the body it was reading.
		return nil, errReadA2AStream
	}

	var updates []a2aStreamEvent
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		update, frameErr := decodeA2AStreamFrame(strings.TrimSpace(strings.TrimPrefix(line, "data:")), method, id)
		if frameErr != nil {
			return nil, frameErr
		}
		updates = append(updates, update)
	}
	if len(updates) > 0 {
		return updates, nil
	}
	// No frames means the stream never started, which is how a JSON-RPC error to a
	// streaming request arrives: one ordinary envelope, no event framing. Decoding
	// the whole body is what turns that into the server's numeric code rather than
	// into "the response was empty".
	update, err := decodeA2AStreamFrame(string(body), method, id)
	if err != nil {
		return nil, err
	}
	return []a2aStreamEvent{update}, nil
}

func decodeA2AStreamFrame(data, method, requestID string) (a2aStreamEvent, error) {
	var envelope rpcEnvelope
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return a2aStreamEvent{}, errDecodeA2AResponse
	}
	// Correlate the frame before trusting either its result or error payload.
	if envelope.JSONRPC != "2.0" {
		return a2aStreamEvent{}, fmt.Errorf("%s response has invalid JSON-RPC version", method)
	}
	if envelope.ID != requestID {
		return a2aStreamEvent{}, fmt.Errorf("%s response has JSON-RPC id mismatch", method)
	}
	if envelope.Error != nil {
		// The remote message and data fields are provider-controlled response
		// bodies. The stable numeric code is sufficient for local diagnosis.
		return a2aStreamEvent{}, fmt.Errorf("%s JSON-RPC error %d", method, envelope.Error.Code)
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return a2aStreamEvent{}, fmt.Errorf("%s response has no result", method)
	}
	var update a2aStreamEvent
	if err := json.Unmarshal(envelope.Result, &update); err != nil {
		return a2aStreamEvent{}, errDecodeA2AResponse
	}
	return update, nil
}

type a2aPart struct {
	Data     map[string]any `json:"data,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
}

type a2aContainer struct {
	Metadata map[string]any `json:"metadata,omitempty"`
	Parts    []a2aPart      `json:"parts"`
}

type a2aStatus struct {
	Message *a2aContainer `json:"message,omitempty"`
	State   string        `json:"state"`
}

type a2aResult struct {
	Status    *a2aStatus     `json:"status,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Kind      string         `json:"kind"`
	ID        string         `json:"id,omitempty"`
	ContextID string         `json:"contextId,omitempty"`
	Parts     []a2aPart      `json:"parts,omitempty"`
	Artifacts []a2aContainer `json:"artifacts,omitempty"`
}

func eventsFromA2A(result a2aResult) ([]Event, error) {
	events := make([]Event, 0, len(result.Artifacts)+1)
	absorb := func(container a2aContainer) error {
		event := Event{Content: &Content{Role: "model"}}
		event.Partial, _ = container.Metadata["adk_partial"].(bool)
		if code, ok := container.Metadata["adk_error_code"].(string); ok {
			event.ErrorCode = code
		}
		if usage, ok := container.Metadata["adk_usage_metadata"].(map[string]any); ok {
			encoded, err := json.Marshal(usage)
			if err != nil {
				return fmt.Errorf("encode A2A usage metadata: %w", err)
			}
			var parsed UsageMetadata
			if err := json.Unmarshal(encoded, &parsed); err != nil {
				return fmt.Errorf("decode A2A usage metadata: %w", err)
			}
			event.UsageMetadata = &parsed
		}
		for _, part := range container.Parts {
			if partial, ok := part.Metadata["adk_partial"].(bool); ok && partial {
				event.Partial = true
			}
			switch {
			case part.Kind == "text":
				event.Content.Parts = append(event.Content.Parts, Part{Text: part.Text})
			case part.Kind == "data" && part.Metadata["adk_type"] == "function_call":
				call, err := decodeA2AFunctionCall(part.Data)
				if err != nil {
					return err
				}
				event.Content.Parts = append(event.Content.Parts, Part{FunctionCall: &call})
			case part.Kind == "data" && part.Metadata["adk_type"] == "function_response":
				response, err := decodeA2AFunctionResponse(part.Data)
				if err != nil {
					return err
				}
				event.Content.Parts = append(event.Content.Parts, Part{FunctionResponse: &response})
			}
		}
		events = append(events, event)
		return nil
	}

	if result.Kind == "message" {
		if err := absorb(a2aContainer{Parts: result.Parts, Metadata: result.Metadata}); err != nil {
			return nil, err
		}
		return events, nil
	}
	for _, artifact := range result.Artifacts {
		if err := absorb(artifact); err != nil {
			return nil, err
		}
	}
	if result.Status != nil && result.Status.Message != nil {
		if err := absorb(*result.Status.Message); err != nil {
			return nil, err
		}
		if strings.EqualFold(result.Status.State, "failed") && len(events) > 0 {
			events[len(events)-1].ErrorMessage = textOf(events[len(events)-1])
		}
	}
	if result.Status != nil && strings.EqualFold(result.Status.State, "failed") && !hasEventError(events) {
		code, _ := result.Metadata["adk_error_code"].(string)
		if code == "" {
			code = "TASK_FAILED"
		}
		events = append(events, Event{ErrorCode: code})
	}
	return events, nil
}

func decodeA2AFunctionCall(data map[string]any) (FunctionCall, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return FunctionCall{}, fmt.Errorf("encode A2A function call: %w", err)
	}
	var call FunctionCall
	if err := json.Unmarshal(encoded, &call); err != nil {
		return FunctionCall{}, fmt.Errorf("decode A2A function call: %w", err)
	}
	if call.Args == nil {
		call.Args = map[string]any{}
	}
	return call, nil
}

func decodeA2AFunctionResponse(data map[string]any) (FunctionResponse, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return FunctionResponse{}, fmt.Errorf("encode A2A function response: %w", err)
	}
	var response FunctionResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return FunctionResponse{}, fmt.Errorf("decode A2A function response: %w", err)
	}
	if response.Response == nil {
		response.Response = map[string]any{}
	}
	return response, nil
}

func hasEventError(events []Event) bool {
	for _, event := range events {
		if event.ErrorCode != "" {
			return true
		}
	}
	return false
}

func textOf(event Event) string {
	if event.Content == nil {
		return ""
	}
	parts := make([]string, 0, len(event.Content.Parts))
	for _, part := range event.Content.Parts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func parseBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		// net/url includes the rejected input in some parse errors. The input can
		// contain userinfo, so keep it out of CI and evaluation diagnostics.
		return nil, errors.New("URL is malformed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("scheme must be http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("host is required")
	}
	if parsed.User != nil {
		return nil, errors.New("credentials must not appear in a URL")
	}
	if !dialableURLPort(parsed) {
		return nil, errors.New("port must be between 1 and 65535")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func responseStatusError(response *http.Response) error {
	// Provider bodies can contain echoed prompts, credentials, endpoints, or
	// private error details. Drain a bounded prefix for connection hygiene but
	// never place it in an error that CI or an evaluation artifact can persist.
	_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
	if err != nil {
		// The reader error is as untrusted as the body; custom transports can put
		// endpoints or credentials in it, so retain only the failure class.
		return fmt.Errorf("HTTP %d; response body unreadable", response.StatusCode)
	}
	return fmt.Errorf("HTTP %d", response.StatusCode)
}

func randomUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
