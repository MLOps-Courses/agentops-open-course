package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// A cold local embedding model may legitimately take the agent's default
	// two-minute embedding timeout. The harness stays bounded but must finish
	// after the boundary it is measuring rather than canceling it first.
	defaultRetrievalRequestTimeout = 5 * time.Minute
	mcpRetrievalToolName           = "search_runbooks"
	mcpRetrievalPath               = "/mcp"
	mcpLivenessPath                = "/livez"
)

type retrievalToolSession interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// MCPRetrievalClient calls only search_runbooks on the public read-only MCP
// surface. The response body is reduced to ranked slugs before it reaches the
// scorer; runbook content is never retained in evaluation evidence.
type MCPRetrievalClient struct {
	session  retrievalToolSession
	close    func() error
	closeErr error
	closeOne sync.Once
}

func (c *MCPRetrievalClient) SearchRunbooks(
	ctx context.Context,
	query string,
	limit int,
) (RetrievalSearchResult, error) {
	if c == nil || c.session == nil {
		return RetrievalSearchResult{}, errors.New("MCP retrieval client has no session")
	}
	if strings.TrimSpace(query) == "" || limit < 1 || limit > retrievalLimit {
		return RetrievalSearchResult{}, errors.New("search_runbooks needs a query and a limit from 1 to 3")
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{
		Name: mcpRetrievalToolName,
		Arguments: map[string]any{
			"query": query,
			"limit": limit,
		},
	})
	if err != nil {
		// Protocol/provider errors can contain the query or endpoint. The caller
		// needs the failed boundary, not an unsafe payload copied into CI output.
		return RetrievalSearchResult{}, errors.New("call search_runbooks over MCP")
	}
	if result == nil {
		return RetrievalSearchResult{}, errors.New("search_runbooks returned no MCP result")
	}
	if result.IsError {
		return RetrievalSearchResult{}, errors.New("search_runbooks returned a tool error")
	}
	return decodeMCPRetrievalResult(result.StructuredContent)
}

func (c *MCPRetrievalClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOne.Do(func() {
		if c.close != nil {
			if err := c.close(); err != nil {
				// SDK shutdown errors can contain the endpoint or provider response.
				c.closeErr = errors.New("close local retrieval MCP session")
			}
		}
	})
	return c.closeErr
}

type mcpRetrievalOutput struct {
	Count     *int          `json:"count"`
	Retrieval RetrievalMode `json:"retrieval"`
	// Content is accepted because it is part of the public tool result, but
	// RawMessage prevents a second string copy and it is discarded here.
	Runbooks []struct {
		Slug    string          `json:"slug"`
		Content json.RawMessage `json:"content"`
	} `json:"runbooks"`
}

func decodeMCPRetrievalResult(structured any) (RetrievalSearchResult, error) {
	if structured == nil {
		return RetrievalSearchResult{}, errors.New("search_runbooks returned no structured result")
	}
	encoded, err := json.Marshal(structured)
	if err != nil {
		return RetrievalSearchResult{}, errors.New("encode the structured search_runbooks result")
	}
	var output mcpRetrievalOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		return RetrievalSearchResult{}, errors.New("decode the structured search_runbooks result")
	}
	if output.Count == nil || *output.Count != len(output.Runbooks) {
		return RetrievalSearchResult{}, errors.New("search_runbooks count does not match its ranked results")
	}
	if output.Retrieval != RetrievalKeyword && output.Retrieval != RetrievalSemantic {
		return RetrievalSearchResult{}, errors.New("search_runbooks returned an unknown retrieval mode")
	}
	slugs := make([]string, 0, len(output.Runbooks))
	for _, runbook := range output.Runbooks {
		slugs = append(slugs, runbook.Slug)
	}
	return RetrievalSearchResult{Mode: output.Retrieval, Slugs: slugs}, nil
}

type MCPRetrievalRuntimeFactoryConfig struct {
	Output          io.Writer
	Environment     map[string]string
	Binary          string
	DataDir         string
	EmbeddingModel  string
	Source          SourceEvidence
	Timeout         time.Duration
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

// MCPRetrievalEvaluationConfig keeps runtime and evidence identity together so
// a CLI cannot label one embedding model while the child process runs another.
type MCPRetrievalEvaluationConfig struct {
	Source               SourceEvidence
	EmbeddingModelDigest string
	Runtime              MCPRetrievalRuntimeFactoryConfig
}

func RunMCPRetrievalEvaluation(
	ctx context.Context,
	config MCPRetrievalEvaluationConfig,
) (RetrievalArtifact, error) {
	config.Runtime.Source = config.Source
	factory, err := NewMCPRetrievalRuntimeFactory(config.Runtime)
	if err != nil {
		return RetrievalArtifact{}, err
	}
	return RunRetrievalEvaluation(ctx, RetrievalRunConfig{
		Factory:              factory,
		DataDir:              config.Runtime.DataDir,
		Source:               config.Source,
		EmbeddingModel:       config.Runtime.EmbeddingModel,
		EmbeddingModelDigest: config.EmbeddingModelDigest,
	})
}

func NewMCPRetrievalRuntimeFactory(
	config MCPRetrievalRuntimeFactoryConfig,
) (RetrievalRuntimeFactory, error) {
	var problems []error
	if strings.TrimSpace(config.Binary) == "" {
		problems = append(problems, errors.New("MCP retrieval runtime needs an agent binary"))
	}
	if strings.TrimSpace(config.DataDir) == "" {
		problems = append(problems, errors.New("MCP retrieval runtime needs an immutable data directory"))
	}
	if !boundedIdentity(config.EmbeddingModel) {
		problems = append(problems, errors.New("MCP retrieval runtime needs a non-empty, trimmed, single-line embedding model"))
	}
	if err := config.Source.Validate(); err != nil {
		problems = append(problems, fmt.Errorf("MCP retrieval runtime source: %w", err))
	}
	if config.Timeout < 0 || config.StartupTimeout < 0 || config.ShutdownTimeout < 0 {
		problems = append(problems, errors.New("MCP retrieval runtime timeouts must not be negative"))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	if config.Timeout == 0 {
		config.Timeout = defaultRetrievalRequestTimeout
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = DefaultStartupTimeout
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = DefaultShutdownTimeout
	}
	config.Environment = cloneStringMap(config.Environment)
	return func(ctx context.Context, mode RetrievalMode) (RetrievalRuntime, error) {
		return startMCPRetrievalRuntime(ctx, config, mode)
	}, nil
}

func startMCPRetrievalRuntime(
	ctx context.Context,
	config MCPRetrievalRuntimeFactoryConfig,
	mode RetrievalMode,
) (RetrievalRuntime, error) {
	stateDir, removeState, err := NewIsolatedState()
	if err != nil {
		return RetrievalRuntime{}, err
	}
	fail := func(err error) (RetrievalRuntime, error) {
		return RetrievalRuntime{}, errors.Join(err, removeState())
	}
	binary, err := snapshotRuntimeBinary(config.Binary, stateDir)
	if err != nil {
		return fail(err)
	}
	if identityErr := verifyRuntimeIdentity(ctx, binary, config.Source, executeRuntimeCommand); identityErr != nil {
		return fail(identityErr)
	}
	port, err := FreePort()
	if err != nil {
		return fail(err)
	}
	environment, err := retrievalRuntimeEnvironment(config.Environment, mode, config.EmbeddingModel, port)
	if err != nil {
		return fail(err)
	}
	process, err := startMCPRetrievalProcess(ctx, mcpRetrievalProcessConfig{
		Output: config.Output, Environment: environment, Binary: binary,
		DataDir: config.DataDir, StateDir: stateDir, Port: port,
		StartupTimeout: config.StartupTimeout, ShutdownTimeout: config.ShutdownTimeout,
	})
	if err != nil {
		return fail(err)
	}
	client, err := connectMCPRetrievalClient(ctx, process.BaseURL()+mcpRetrievalPath, config.Timeout)
	if err != nil {
		return RetrievalRuntime{}, errors.Join(err, process.Close(), removeState())
	}
	return RetrievalRuntime{
		ID: filepath.Base(stateDir), Client: client,
		Close: func() error {
			return errors.Join(client.Close(), process.Close(), removeState())
		},
	}, nil
}

func retrievalRuntimeEnvironment(
	base map[string]string,
	mode RetrievalMode,
	embeddingModel string,
	port int,
) (map[string]string, error) {
	if mode != RetrievalKeyword && mode != RetrievalSemantic {
		return nil, fmt.Errorf("unsupported retrieval mode %q", mode)
	}
	if !boundedIdentity(embeddingModel) || port < 1 || port > 65535 {
		return nil, errors.New("retrieval runtime needs an embedding model and a valid port")
	}
	values := cloneStringMap(base)
	values["MCP_TRANSPORT"] = "streamable-http"
	values["MCP_HOST"] = "127.0.0.1"
	values["MCP_PORT"] = strconv.Itoa(port)
	values["MCP_ALLOWED_HOSTS"] = "127.0.0.1:" + strconv.Itoa(port)
	values["AGENT_SEMANTIC_RETRIEVAL"] = strconv.FormatBool(mode == RetrievalSemantic)
	values["AGENT_EMBEDDING_MODEL"] = embeddingModel
	// The MCP surface registers reads only. The process-level switch makes the
	// evaluation fail closed if that public contract ever widens by mistake.
	values["AGENT_WRITES_DISABLED"] = "true"
	return values, nil
}

type mcpRetrievalProcessConfig struct {
	Output          io.Writer
	Environment     map[string]string
	Binary          string
	DataDir         string
	StateDir        string
	Port            int
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

func startMCPRetrievalProcess(
	ctx context.Context,
	config mcpRetrievalProcessConfig,
) (*AgentProcess, error) {
	command := exec.Command(config.Binary, "mcp")
	command.Env = processEnvironment(AgentProcessConfig{
		Environment: config.Environment,
		Binary:      config.Binary,
		Entrypoint:  "agent",
		DataDir:     config.DataDir,
		StateDir:    config.StateDir,
		Port:        config.Port,
	})
	command.Stdout = config.Output
	command.Stderr = config.Output
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start retrieval MCP process: %w", err)
	}
	process := &AgentProcess{
		command: command, done: make(chan error, 1),
		baseURL:         "http://127.0.0.1:" + strconv.Itoa(config.Port),
		shutdownTimeout: config.ShutdownTimeout,
	}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	// Read tools deliberately fall back to immutable seed data while readiness
	// requires the A2A writer to have published runtime state. This evaluation
	// must not create that writable state, so liveness proves process startup and
	// the MCP calls themselves prove the read boundary.
	if err := process.waitReady(ctx, mcpLivenessPath, config.StartupTimeout); err != nil {
		return nil, errors.Join(err, process.Close())
	}
	return process, nil
}

func connectMCPRetrievalClient(
	ctx context.Context,
	endpoint string,
	timeout time.Duration,
) (*MCPRetrievalClient, error) {
	if err := validateLocalMCPEndpoint(endpoint); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("MCP retrieval client timeout must be positive")
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: clientWithoutRedirects(&http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: nil,
			},
		}, timeout),
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(
		&mcp.Implementation{Name: "agentops-retrieval-eval", Version: "1.0.0"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}},
	)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, errors.New("connect to the local retrieval MCP runtime")
	}
	return &MCPRetrievalClient{session: session, close: session.Close}, nil
}

func validateLocalMCPEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" ||
		parsed.Path != mcpRetrievalPath || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("retrieval MCP endpoint must be a loopback HTTP /mcp URL without credentials, query, or fragment")
	}
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source)+7)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
