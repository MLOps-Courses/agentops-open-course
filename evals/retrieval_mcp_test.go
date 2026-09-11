package evals

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPRetrievalClientCallsTheReadOnlySearchTool(t *testing.T) {
	t.Parallel()

	session := &fakeRetrievalToolSession{result: &mcp.CallToolResult{
		StructuredContent: map[string]any{
			"count":     float64(2),
			"retrieval": "semantic",
			"runbooks": []any{
				map[string]any{"slug": "high-latency", "content": "private runbook body"},
				map[string]any{"slug": "service-down", "content": "another private body"},
			},
		},
	}}
	client := &MCPRetrievalClient{session: session}

	result, err := client.SearchRunbooks(t.Context(), "private incident query", 3)
	if err != nil {
		t.Fatalf("SearchRunbooks() error = %v", err)
	}
	if session.params.Name != "search_runbooks" {
		t.Fatalf("tool name = %q, want search_runbooks", session.params.Name)
	}
	arguments, ok := session.params.Arguments.(map[string]any)
	if !ok {
		t.Fatalf("arguments type = %T, want map[string]any", session.params.Arguments)
	}
	if arguments["query"] != "private incident query" || arguments["limit"] != 3 {
		t.Fatalf("arguments = %#v", arguments)
	}
	if result.Mode != RetrievalSemantic || !slices.Equal(result.Slugs, []string{"high-latency", "service-down"}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestMCPRetrievalClientDoesNotEchoToolErrors(t *testing.T) {
	t.Parallel()

	for name, session := range map[string]*fakeRetrievalToolSession{
		"protocol": {err: errors.New("provider leaked a private query")},
		"tool": {
			result: &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "private tool payload"}},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &MCPRetrievalClient{session: session}
			_, err := client.SearchRunbooks(t.Context(), "private incident query", 3)
			if err == nil {
				t.Fatal("SearchRunbooks() error = nil")
			}
			for _, private := range []string{"private incident query", "provider leaked", "private tool payload"} {
				if strings.Contains(err.Error(), private) {
					t.Errorf("error leaks %q: %v", private, err)
				}
			}
		})
	}
}

func TestMCPRetrievalClientCloseOmitsRemoteErrorsAndIsIdempotent(t *testing.T) {
	t.Parallel()

	const marker = "SYNTHETIC_MCP_CLOSE_DETAIL_DO_NOT_LOG"
	var closeCalls atomic.Int64
	client := &MCPRetrievalClient{close: func() error {
		closeCalls.Add(1)
		return errors.New(marker)
	}}

	first := client.Close()
	second := client.Close()
	for attempt, err := range []error{first, second} {
		if err == nil {
			t.Fatalf("Close() attempt %d error = nil, want the local close failure class", attempt+1)
		}
		if got, want := err.Error(), "close local retrieval MCP session"; got != want {
			t.Fatalf("Close() attempt %d error = %q, want %q", attempt+1, got, want)
		}
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("Close() attempt %d retained remote transport detail", attempt+1)
		}
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("close callback calls = %d, want one idempotent attempt", got)
	}
	if !errors.Is(second, first) {
		t.Fatal("second Close() did not return the stored first result")
	}
}

func TestMCPRetrievalClientRefusesCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var capturedRequests atomic.Int64
			capture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				capturedRequests.Add(1)
				http.Error(writer, "redirect target refuses the request", http.StatusBadGateway)
			}))
			t.Cleanup(capture.Close)

			location := capture.URL + "/" + redirectLocationMarker
			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", location)
				writer.WriteHeader(status)
			}))
			t.Cleanup(source.Close)

			_, err := connectMCPRetrievalClient(t.Context(), source.URL+mcpRetrievalPath, time.Second)
			if err == nil {
				t.Fatal("connectMCPRetrievalClient() error = nil, want the redirect to fail closed")
			}
			if got := capturedRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d request(s), want zero cross-origin body replays", got)
			}
			for _, forbidden := range []string{redirectLocationMarker, capture.URL} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatal("retrieval connection error retained redirect-controlled detail")
				}
			}
		})
	}
}

func TestMCPRetrievalClientRejectsMalformedStructuredResults(t *testing.T) {
	t.Parallel()

	tests := map[string]any{
		"missing": nil,
		"count": map[string]any{
			"count": float64(2), "retrieval": "keyword",
			"runbooks": []any{map[string]any{"slug": "high-latency", "content": "body"}},
		},
		"mode": map[string]any{
			"count": float64(1), "retrieval": "hybrid",
			"runbooks": []any{map[string]any{"slug": "high-latency", "content": "body"}},
		},
	}
	for name, structured := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &MCPRetrievalClient{session: &fakeRetrievalToolSession{
				result: &mcp.CallToolResult{StructuredContent: structured},
			}}
			if _, err := client.SearchRunbooks(t.Context(), "query", 3); err == nil {
				t.Fatal("SearchRunbooks() error = nil")
			}
		})
	}
}

func TestRetrievalRuntimeEnvironmentOwnsIsolationAndMode(t *testing.T) {
	t.Parallel()

	values, err := retrievalRuntimeEnvironment(map[string]string{
		"MCP_HOST":                   "0.0.0.0",
		"MCP_ALLOWED_HOSTS":          "*",
		"AGENT_SEMANTIC_RETRIEVAL":   "false",
		"AGENT_EMBEDDING_MODEL":      "wrong",
		"AGENT_WRITES_DISABLED":      "false",
		"EVAL_UNRELATED_PASSTHROUGH": "kept",
	}, RetrievalSemantic, "embed-fixture", 43210)
	if err != nil {
		t.Fatalf("retrievalRuntimeEnvironment() error = %v", err)
	}
	want := map[string]string{
		"MCP_TRANSPORT":              "streamable-http",
		"MCP_HOST":                   "127.0.0.1",
		"MCP_PORT":                   "43210",
		"MCP_ALLOWED_HOSTS":          "127.0.0.1:43210",
		"AGENT_SEMANTIC_RETRIEVAL":   "true",
		"AGENT_EMBEDDING_MODEL":      "embed-fixture",
		"AGENT_WRITES_DISABLED":      "true",
		"EVAL_UNRELATED_PASSTHROUGH": "kept",
	}
	for key, expected := range want {
		if got := values[key]; got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}

	keyword, err := retrievalRuntimeEnvironment(nil, RetrievalKeyword, "embed-fixture", 43211)
	if err != nil {
		t.Fatalf("retrievalRuntimeEnvironment(keyword) error = %v", err)
	}
	if keyword["AGENT_SEMANTIC_RETRIEVAL"] != "false" {
		t.Errorf("keyword semantic switch = %q, want false", keyword["AGENT_SEMANTIC_RETRIEVAL"])
	}
}

func TestNewMCPRetrievalRuntimeFactoryValidatesConfigurationWithoutStartingAProcess(t *testing.T) {
	t.Parallel()

	valid := MCPRetrievalRuntimeFactoryConfig{
		Source: testSourceEvidence(), Binary: "agent", DataDir: "data", EmbeddingModel: "embed", Timeout: time.Second,
	}
	if factory, err := NewMCPRetrievalRuntimeFactory(valid); err != nil || factory == nil {
		t.Fatalf("NewMCPRetrievalRuntimeFactory(valid) = %v, %v", factory, err)
	}

	tests := map[string]MCPRetrievalRuntimeFactoryConfig{
		"binary":    func() MCPRetrievalRuntimeFactoryConfig { value := valid; value.Binary = ""; return value }(),
		"data":      func() MCPRetrievalRuntimeFactoryConfig { value := valid; value.DataDir = ""; return value }(),
		"embedding": func() MCPRetrievalRuntimeFactoryConfig { value := valid; value.EmbeddingModel = ""; return value }(),
		"timeout":   func() MCPRetrievalRuntimeFactoryConfig { value := valid; value.Timeout = -1; return value }(),
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewMCPRetrievalRuntimeFactory(config); err == nil {
				t.Fatal("NewMCPRetrievalRuntimeFactory() error = nil")
			}
		})
	}
}

func TestMCPRetrievalRuntimeRejectsAStaleBinaryBeforeStartup(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	binary := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(runtimeVersionJSON(t, stale)) + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	factory, err := NewMCPRetrievalRuntimeFactory(MCPRetrievalRuntimeFactoryConfig{
		Source: source, Binary: binary, DataDir: t.TempDir(), EmbeddingModel: "embed",
	})
	if err != nil {
		t.Fatalf("NewMCPRetrievalRuntimeFactory() error = %v", err)
	}

	if _, err := factory(t.Context(), RetrievalKeyword); err == nil ||
		!strings.Contains(err.Error(), "tree_digest") {
		t.Fatalf("stale retrieval binary error = %v", err)
	}
}

type fakeRetrievalToolSession struct {
	result *mcp.CallToolResult
	err    error
	params mcp.CallToolParams
}

func (s *fakeRetrievalToolSession) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.params = *params
	return s.result, s.err
}
