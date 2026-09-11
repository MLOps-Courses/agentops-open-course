package kagentinterop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

var referenceIncident = domain.Reference().Incidents.InventoryDown

type fakeKagent struct {
	lastContext  string
	lastTask     string
	invoked      int
	listed       int
	includeAgent bool
}

type scriptedSession struct {
	callErr  error
	closeErr error
	calls    []string
}

func (s *scriptedSession) CallTool(
	_ context.Context,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	s.calls = append(s.calls, params.Name)
	if s.callErr != nil {
		return nil, s.callErr
	}
	switch params.Name {
	case ListAgentsTool:
		return &mcp.CallToolResult{StructuredContent: listAgentsOutput{
			Agents: []Agent{{Ref: IncidentReaderRef}},
		}}, nil
	case InvokeAgentTool:
		return &mcp.CallToolResult{StructuredContent: Result{
			Agent: IncidentReaderRef, Text: "sanitized evidence", ContextID: "context-next",
		}}, nil
	default:
		return &mcp.CallToolResult{IsError: true}, nil
	}
}

func (s *scriptedSession) Close() error { return s.closeErr }

func (f *fakeKagent) endpoint(t *testing.T) string {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "kagent-test", Version: "test"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: ListAgentsTool}, func(
		context.Context,
		*mcp.CallToolRequest,
		struct{},
	) (*mcp.CallToolResult, listAgentsOutput, error) {
		f.listed++
		output := listAgentsOutput{}
		if f.includeAgent {
			output.Agents = []Agent{{Ref: IncidentReaderRef, Description: "read-only incident evidence"}}
		}
		return nil, output, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: InvokeAgentTool}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		input invokeAgentInput,
	) (*mcp.CallToolResult, Result, error) {
		f.invoked++
		f.lastContext = input.ContextID
		f.lastTask = input.Task
		return nil, Result{
			Agent:     input.Agent,
			Text:      referenceIncident + " is caused by the inventory dependency.",
			ContextID: "context-next",
		}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(func() {
		httpServer.CloseClientConnections()
		httpServer.Close()
	})
	return httpServer.URL + "/mcp"
}

func TestInvokeDiscoversTheSpecialistAndPreservesContext(t *testing.T) {
	t.Parallel()

	fake := &fakeKagent{includeAgent: true}
	client, err := Dial(t.Context(), Config{Endpoint: fake.endpoint(t), Timeout: time.Second})
	if err != nil {
		t.Fatalf("Dial() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})

	task := "Investigate " + referenceIncident + "."
	result, err := client.Invoke(t.Context(), task, "context-first")
	if err != nil {
		t.Fatalf("Invoke() error = %v, want nil", err)
	}
	if fake.listed != 1 {
		t.Errorf("list_agents calls = %d, want 1", fake.listed)
	}
	if fake.invoked != 1 {
		t.Errorf("invoke_agent calls = %d, want 1", fake.invoked)
	}
	if fake.lastTask != task {
		t.Errorf("task = %q, want the caller's task", fake.lastTask)
	}
	if fake.lastContext != "context-first" {
		t.Errorf("context_id = %q, want %q", fake.lastContext, "context-first")
	}
	if result.Agent != IncidentReaderRef || result.ContextID != "context-next" {
		t.Errorf("result = %+v, want specialist ref and returned context", result)
	}
}

func TestInvokeRefusesAnUndiscoveredSpecialist(t *testing.T) {
	t.Parallel()

	fake := &fakeKagent{}
	client, err := Dial(t.Context(), Config{Endpoint: fake.endpoint(t), Timeout: time.Second})
	if err != nil {
		t.Fatalf("Dial() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})

	if _, err := client.Invoke(t.Context(), "Investigate "+referenceIncident+".", ""); err == nil {
		t.Fatal("Invoke() error = nil, want the missing specialist to fail closed")
	}
	if fake.invoked != 0 {
		t.Errorf("invoke_agent calls = %d, want 0", fake.invoked)
	}
}

func TestDialAndInvokeRejectUnboundedOrBlankInput(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]Config{
		"missing endpoint":  {},
		"missing timeout":   {Endpoint: "http://127.0.0.1:8083/mcp"},
		"negative timeout":  {Endpoint: "http://127.0.0.1:8083/mcp", Timeout: -time.Second},
		"invalid endpoint":  {Endpoint: "://bad", Timeout: time.Second},
		"endpoint userinfo": {Endpoint: "http://agent:synthetic-secret@127.0.0.1:8083/mcp", Timeout: time.Second},
		"wrong path":        {Endpoint: "http://127.0.0.1:8083/agents", Timeout: time.Second},
		"endpoint query":    {Endpoint: "http://127.0.0.1:8083/mcp?token=value", Timeout: time.Second},
		"relative endpoint": {Endpoint: "/mcp", Timeout: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Dial(t.Context(), config); err == nil {
				t.Fatal("Dial() error = nil, want invalid configuration to fail")
			}
		})
	}

	fake := &fakeKagent{includeAgent: true}
	client, err := Dial(t.Context(), Config{Endpoint: fake.endpoint(t), Timeout: time.Second})
	if err != nil {
		t.Fatalf("Dial() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})

	for name, invocation := range map[string]struct {
		task      string
		contextID string
	}{
		"blank task":        {task: "   "},
		"oversized task":    {task: strings.Repeat("t", maxTaskBytes+1)},
		"invalid task UTF8": {task: string([]byte{0xff})},
		"oversized context": {
			task: "Investigate " + referenceIncident + ".", contextID: strings.Repeat("c", maxContextIDBytes+1),
		},
		"invalid context UTF8": {
			task: "Investigate " + referenceIncident + ".", contextID: string([]byte{0xff}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.Invoke(t.Context(), invocation.task, invocation.contextID); err == nil {
				t.Fatal("Invoke() error = nil, want invalid input to fail")
			}
		})
	}
	if fake.listed != 0 || fake.invoked != 0 {
		t.Errorf("invalid input made remote calls: list=%d invoke=%d", fake.listed, fake.invoked)
	}
}

func TestConfigRejectsEndpointUserinfoWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-secret-do-not-print"
	err := (Config{
		Endpoint: "http://agent:" + secret + "@127.0.0.1:8083/mcp",
		Timeout:  time.Second,
	}).validate()
	if err == nil {
		t.Fatal("Config.validate() error = nil, want credential-bearing endpoint to fail before dialing")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Config.validate() error leaked endpoint userinfo: %v", err)
	}
}

func TestConfigRejectsUndialableEndpointPorts(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"http://127.0.0.1:/mcp",
		"http://127.0.0.1:0/mcp",
		"http://127.0.0.1:65536/mcp",
	} {
		if err := (Config{Endpoint: endpoint, Timeout: time.Second}).validate(); err == nil {
			t.Errorf("Config.validate(%q) error = nil, want invalid port rejection before dialing", endpoint)
		}
	}
}

func TestCallToolOmitsRemoteTransportErrors(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-remote-secret-do-not-print"
	client, err := newClient(&scriptedSession{callErr: errors.New(secret)})
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}
	_, err = client.ListAgents(t.Context())
	if err == nil {
		t.Fatal("ListAgents() error = nil, want the failed remote call to fail closed")
	}
	if !errors.Is(err, ErrInterop) {
		t.Fatalf("ListAgents() error = %v, want ErrInterop", err)
	}
	if !strings.Contains(err.Error(), "call "+ListAgentsTool+" failed") {
		t.Fatalf("ListAgents() error = %v, want the local operation failure class", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ListAgents() error leaked remote MCP text: %v", err)
	}
}

func TestSessionCloseOmitsRemoteErrors(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-close-secret-do-not-print"
	client, err := newClient(&scriptedSession{closeErr: errors.New(secret)})
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}
	err = client.Close()
	if err == nil || !errors.Is(err, ErrInterop) {
		t.Fatalf("Close() error = %v, want ErrInterop", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Close() error leaked remote MCP text: %v", err)
	}
}

func TestDialOmitsRemoteInitializationErrors(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-initialize-secret-do-not-print"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			ID any `json:"id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      input.ID,
			"error":   map[string]any{"code": -32000, "message": secret},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	_, err := Dial(t.Context(), Config{Endpoint: server.URL + "/mcp", Timeout: time.Second})
	if err == nil || !errors.Is(err, ErrInterop) {
		t.Fatalf("Dial() error = %v, want ErrInterop", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Dial() error leaked remote MCP text: %v", err)
	}
}

func TestInvokeRefusesCrossOriginRedirects(t *testing.T) {
	const (
		secret   = "synthetic-cross-origin-task-secret"
		location = "synthetic-cross-origin-location"
	)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var capturedRequests atomic.Int64
			var capturedSecret atomic.Bool
			capture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				capturedRequests.Add(1)
				body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				capturedSecret.Store(strings.Contains(string(body), secret))
				http.Error(writer, "redirect target refuses the request", http.StatusBadGateway)
			}))
			t.Cleanup(capture.Close)

			server := mcp.NewServer(&mcp.Implementation{Name: "kagent-redirect-test", Version: "test"}, nil)
			mcp.AddTool(server, &mcp.Tool{Name: ListAgentsTool}, func(
				context.Context,
				*mcp.CallToolRequest,
				struct{},
			) (*mcp.CallToolResult, listAgentsOutput, error) {
				return nil, listAgentsOutput{Agents: []Agent{{Ref: IncidentReaderRef}}}, nil
			})
			mcp.AddTool(server, &mcp.Tool{Name: InvokeAgentTool}, func(
				context.Context,
				*mcp.CallToolRequest,
				invokeAgentInput,
			) (*mcp.CallToolResult, Result, error) {
				t.Fatal("invoke_agent reached the source server instead of the redirect guard")
				return nil, Result{}, nil
			})
			mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil {
					t.Errorf("read MCP request: %v", err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var envelope struct {
					Method string `json:"method"`
					Params struct {
						Name string `json:"name"`
					} `json:"params"`
				}
				if json.Unmarshal(body, &envelope) == nil && envelope.Method == "tools/call" && envelope.Params.Name == InvokeAgentTool {
					writer.Header().Set("Location", capture.URL+"/"+location)
					writer.WriteHeader(status)
					return
				}
				mcpHandler.ServeHTTP(writer, request)
			}))
			t.Cleanup(func() {
				source.CloseClientConnections()
				source.Close()
			})

			client, err := Dial(t.Context(), Config{Endpoint: source.URL + "/mcp", Timeout: time.Second})
			if err != nil {
				t.Fatalf("Dial() error = %v, want nil", err)
			}
			t.Cleanup(func() {
				if closeErr := client.Close(); closeErr != nil {
					t.Errorf("Close() error = %v, want nil", closeErr)
				}
			})

			_, err = client.Invoke(t.Context(), "Investigate "+secret+".", "context")
			if err == nil || !errors.Is(err, ErrInterop) {
				t.Fatalf("Invoke() error = %v, want ErrInterop", err)
			}
			if got := capturedRequests.Load(); got != 0 || capturedSecret.Load() {
				t.Fatalf("redirect target received %d request(s), want zero cross-origin body replays", got)
			}
			for _, forbidden := range []string{secret, location, capture.URL} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatal("Invoke() error retained redirect-controlled detail")
				}
			}
		})
	}
}

func TestInvokeRecordsOnlyBoundedOperationTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	t.Cleanup(func() {
		if err := meterProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	session := &scriptedSession{}
	client, err := newClientWithProviders(session, tracerProvider, meterProvider)
	if err != nil {
		t.Fatalf("newClientWithProviders() error = %v, want nil", err)
	}
	const privateTask = "Investigate customer@example.com in Paris."
	const privateContext = "customer-session-123"
	if _, err := client.Invoke(t.Context(), privateTask, privateContext); err != nil {
		t.Fatalf("Invoke() error = %v, want nil", err)
	}

	spans := spanRecorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded spans = %d, want list_agents and invoke_agent", len(spans))
	}
	for index, operation := range []string{ListAgentsTool, InvokeAgentTool} {
		span := spans[index]
		if got, want := span.Name(), "agentops.kagent."+operation; got != want {
			t.Errorf("span[%d].Name() = %q, want %q", index, got, want)
		}
		if got := span.InstrumentationScope().Name; got != ScopeName {
			t.Errorf("span[%d] scope = %q, want repository scope %q", index, got, ScopeName)
		}
		attributes := span.Attributes()
		if len(attributes) != 1 || string(attributes[0].Key) != "agentops.kagent.operation" ||
			attributes[0].Value.AsString() != operation {
			t.Errorf("span[%d] attributes = %v, want only the bounded operation", index, attributes)
		}
		if len(span.Events()) != 0 {
			t.Errorf("span[%d] events = %v, want no exception or content events", index, span.Events())
		}
	}

	var gathered metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &gathered); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	metricFound := false
	for _, scope := range gathered.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != CallsMetric {
				continue
			}
			metricFound = true
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data type = %T, want int64 sum", CallsMetric, instrument.Data)
			}
			if len(sum.DataPoints) != 2 {
				t.Errorf("%s data points = %d, want one per operation", CallsMetric, len(sum.DataPoints))
			}
			for _, point := range sum.DataPoints {
				if point.Value != 1 {
					t.Errorf("%s point value = %d, want 1", CallsMetric, point.Value)
				}
				if point.Attributes.Len() != 2 {
					t.Errorf("%s attribute count = %d, want only operation and outcome", CallsMetric, point.Attributes.Len())
				}
			}
		}
	}
	if !metricFound {
		t.Fatalf("no instrument named %q was recorded", CallsMetric)
	}
}
