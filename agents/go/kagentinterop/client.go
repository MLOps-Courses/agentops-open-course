// Package kagentinterop invokes the optional declarative incident specialist
// through kagent's agent-as-MCP surface.
package kagentinterop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/httpguard"
)

const (
	// IncidentReaderRef is deliberately fixed: this comparison client cannot
	// turn kagent's whole agent inventory into an ambient delegation surface.
	IncidentReaderRef = "agentops/incident-reader"
	ListAgentsTool    = "list_agents"
	InvokeAgentTool   = "invoke_agent"

	ScopeName   = "agentops.kagent.interop"
	CallsMetric = "agentops.kagent.client.calls"

	maxTaskBytes      = 4096
	maxContextIDBytes = 256
)

// ErrInterop marks a refused configuration, malformed response, or failed MCP
// exchange at the optional kagent boundary.
var ErrInterop = errors.New("kagent interoperability")

// Config bounds the Streamable HTTP connection to kagent's MCP endpoint.
type Config struct {
	Endpoint string
	Timeout  time.Duration
}

// Agent is the sanitized inventory shape returned by list_agents.
type Agent struct {
	Ref         string `json:"ref"`
	Description string `json:"description,omitempty"`
}

type listAgentsOutput struct {
	Agents []Agent `json:"agents"`
}

type invokeAgentInput struct {
	Agent     string `json:"agent"`
	Task      string `json:"task"`
	ContextID string `json:"context_id,omitempty"`
}

// Result is the structured invoke_agent response. ContextID is returned to the
// caller so a later process can continue the same kagent/A2A conversation.
type Result struct {
	Agent     string `json:"agent"`
	Text      string `json:"text"`
	ContextID string `json:"context_id,omitempty"`
}

type toolSession interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
}

// Client is a least-privilege client for the one optional incident specialist.
type Client struct {
	session toolSession
	tracer  trace.Tracer
	calls   metric.Int64Counter
}

// Dial connects to a bounded Streamable HTTP endpoint and initializes the
// instrumentation used by ListAgents and Invoke.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:   cfg.Endpoint,
		HTTPClient: httpguard.NoRedirects(&http.Client{Timeout: cfg.Timeout}),
	}
	session, err := mcp.NewClient(&mcp.Implementation{
		Name:    "agentops-kagent-interop",
		Version: "1",
	}, nil).Connect(ctx, transport, nil)
	if err != nil {
		// Initialization failures can be JSON-RPC errors with server-controlled text.
		return nil, fmt.Errorf("%w: connect to the kagent MCP endpoint failed", ErrInterop)
	}
	client, err := newClient(session)
	if err != nil {
		if closeErr := session.Close(); closeErr != nil {
			err = errors.Join(err, errors.New("close the rejected MCP session failed"))
		}
		return nil, err
	}
	return client, nil
}

func (cfg Config) validate() error {
	if cfg.Timeout <= 0 {
		return fmt.Errorf("%w: Timeout must be positive", ErrInterop)
	}
	parsed, err := url.ParseRequestURI(cfg.Endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		// Userinfo can contain credentials and may be copied into transport errors.
		// Refuse it before dialing, while diagnostics still omit the input value.
		return fmt.Errorf("%w: Endpoint must be an absolute HTTP(S) URL", ErrInterop)
	}
	if parsed.Path != "/mcp" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: Endpoint must identify the kagent /mcp path without a query or fragment", ErrInterop)
	}
	portText := parsed.Port()
	port, portErr := strconv.Atoi(portText)
	if strings.HasSuffix(parsed.Host, ":") || (portText != "" && (portErr != nil || port < 1 || port > 65535)) {
		return fmt.Errorf("%w: Endpoint port must be between 1 and 65535", ErrInterop)
	}
	return nil
}

func newClient(session toolSession) (*Client, error) {
	return newClientWithProviders(session, otel.GetTracerProvider(), otel.GetMeterProvider())
}

func newClientWithProviders(
	session toolSession,
	tracerProvider trace.TracerProvider,
	meterProvider metric.MeterProvider,
) (*Client, error) {
	calls, err := meterProvider.Meter(ScopeName).Int64Counter(
		CallsMetric,
		metric.WithUnit("1"),
		metric.WithDescription("Calls to kagent's agent-as-MCP tools by operation and outcome"),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create the call counter: %w", ErrInterop, err)
	}
	return &Client{session: session, tracer: tracerProvider.Tracer(ScopeName), calls: calls}, nil
}

// Close releases the MCP client session.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	if err := c.session.Close(); err != nil {
		// Session shutdown errors are remote/transport-controlled diagnostics.
		return fmt.Errorf("%w: close the MCP session failed", ErrInterop)
	}
	return nil
}

// ListAgents returns kagent's accepted and deployment-ready inventory.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var output listAgentsOutput
	if err := callTool(ctx, c, ListAgentsTool, struct{}{}, &output); err != nil {
		return nil, err
	}
	return output.Agents, nil
}

// Invoke discovers the optional specialist and invokes only that exact agent.
// An opaque contextID is forwarded and the returned replacement is preserved.
func (c *Client) Invoke(ctx context.Context, task, contextID string) (Result, error) {
	if err := validateInvocation(task, contextID); err != nil {
		return Result{}, err
	}
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return Result{}, err
	}
	if !slices.ContainsFunc(agents, func(agent Agent) bool { return agent.Ref == IncidentReaderRef }) {
		return Result{}, fmt.Errorf("%w: specialist %q is not accepted and deployment-ready", ErrInterop, IncidentReaderRef)
	}

	var result Result
	err = callTool(ctx, c, InvokeAgentTool, invokeAgentInput{
		Agent: IncidentReaderRef, Task: task, ContextID: contextID,
	}, &result)
	if err != nil {
		return Result{}, err
	}
	if result.Agent != IncidentReaderRef {
		// The remote identity is untrusted response content and can carry secrets.
		return Result{}, fmt.Errorf("%w: invoke_agent returned an unexpected agent identity", ErrInterop)
	}
	return result, nil
}

func validateInvocation(task, contextID string) error {
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("%w: task must not be blank", ErrInterop)
	}
	if !utf8.ValidString(task) || len(task) > maxTaskBytes {
		return fmt.Errorf("%w: task must be valid UTF-8 and at most %d bytes", ErrInterop, maxTaskBytes)
	}
	if !utf8.ValidString(contextID) || len(contextID) > maxContextIDBytes {
		return fmt.Errorf("%w: context_id must be valid UTF-8 and at most %d bytes", ErrInterop, maxContextIDBytes)
	}
	return nil
}

func callTool[T any](
	ctx context.Context,
	client *Client,
	operation string,
	arguments any,
	output *T,
) (err error) {
	ctx, span := client.tracer.Start(ctx, "agentops.kagent."+operation,
		trace.WithAttributes(attribute.String("agentops.kagent.operation", operation)))
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "error"
			span.SetStatus(codes.Error, "kagent MCP call failed")
		} else {
			span.SetStatus(codes.Ok, "")
		}
		client.calls.Add(ctx, 1, metric.WithAttributes(
			attribute.String("operation", operation),
			attribute.String("outcome", outcome),
		))
		span.End()
	}()

	result, err := client.session.CallTool(ctx, &mcp.CallToolParams{Name: operation, Arguments: arguments})
	if err != nil {
		// Transport and JSON-RPC errors can contain server-controlled provider or
		// prompt text. Preserve only the local operation class at this stderr boundary.
		return fmt.Errorf("%w: call %s failed", ErrInterop, operation)
	}
	if result == nil || result.IsError {
		// Tool content can include provider errors or model text. Keep it out of
		// both the returned error and telemetry at this control-plane boundary.
		return fmt.Errorf("%w: %s returned an error", ErrInterop, operation)
	}
	if result.StructuredContent == nil {
		return fmt.Errorf("%w: %s returned no structured content", ErrInterop, operation)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("%w: encode %s structured content: %w", ErrInterop, operation, err)
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		return fmt.Errorf("%w: decode %s structured content: %w", ErrInterop, operation, err)
	}
	return nil
}
