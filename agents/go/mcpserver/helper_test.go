package mcpserver_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// repositoryDataset is the committed dataset, relative to this package. Every
// fixture copies it: the committed database is byte-reproducible and gated by a
// rebuild comparison, so a test that wrote to it would break the repository
// rather than only itself.
const repositoryDataset = "../../data"

// errProbeRefused stands in for an unusable runtime database.
var errProbeRefused = errors.New("runtime database is not initialized")

// fixture is one isolated dataset, the six tools over it, and the server.
type fixture struct {
	server *mcpserver.Server
	store  *data.Store
	// probes counts readiness probe calls, so a test can prove the probe is
	// what answered rather than a cached status.
	probes int
}

// fixtureOptions tunes a fixture.
type fixtureOptions struct {
	// probe replaces the readiness probe.
	probe mcpserver.Probe
	// tools replaces the six-tool surface entirely, for the tests that are
	// about what New accepts.
	tools []adktool.Tool
	// options replaces the transport settings.
	options mcpserver.Options
	// confirmRunbook builds the get_runbook stand-in with RequireConfirmation,
	// which is how the read-only enforcement is proven without relying on the
	// name allowlist that would normally have rejected such a tool.
	confirmRunbook bool
}

// newFixture copies the dataset, builds the six tools and the server over it.
func newFixture(t *testing.T, configure ...func(*fixtureOptions)) *fixture {
	t.Helper()

	var opts fixtureOptions
	for _, apply := range configure {
		apply(&opts)
	}
	if opts.options.Transport == "" {
		// The fixture serves the streamable HTTP transport unless a test says
		// otherwise, because that is the transport the gateway and the
		// Kubernetes deployment use — and the only one with an MCP endpoint to
		// exercise. The stdio default itself is asserted separately, against a
		// server built without this fixture.
		opts.options.Transport = mcpserver.TransportStreamableHTTP
	}

	store := newStore(t)
	fixture := &fixture{store: store}
	probe := opts.probe
	if probe == nil {
		probe = func(ctx context.Context) error {
			fixture.probes++
			_, err := store.ProbeRuntimeDatabase(ctx)
			return err
		}
	}

	exposed := opts.tools
	if exposed == nil {
		exposed = readSurface(t, store, opts.confirmRunbook)
	}

	server, err := mcpserver.New(mcpserver.Config{
		Probe:   probe,
		Tools:   exposed,
		Version: "0.0.0-test",
		Options: opts.options,
	})
	if err != nil {
		t.Fatalf("mcpserver.New() error = %v, want nil", err)
	}
	fixture.server = server
	return fixture
}

// newStore copies the committed dataset into a temporary directory.
//
// The state directory deliberately does not exist. A read falls back to the
// committed seed and the readiness probe reports unready — which is the exact
// separation this server has to preserve.
func newStore(t *testing.T) *data.Store {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.CopyFS(dataDir, os.DirFS(repositoryDataset)); err != nil {
		t.Fatalf("copy the committed dataset: %v", err)
	}
	return data.New(data.Config{DataDir: dataDir, StateDir: filepath.Join(root, "state")})
}

// readSurface builds the six read tools in the order the allowlist names them.
//
// The four incident and log tools are the real ones the agent binds locally,
// built over the same store, which is what makes a round trip through this
// server a real read rather than a mock. The two runbook tools are stand-ins
// with the allowlisted names and honest schemas: the memory package that owns
// them is not part of this module yet, and substituting them here is what lets
// the six-name contract be asserted today rather than deferred.
func readSurface(t *testing.T, store *data.Store, confirmRunbook bool) []adktool.Tool {
	t.Helper()

	built, err := tools.New(tools.Config{
		Store: store,
		Guard: func(ctx context.Context, _ string, call func(context.Context) error) error {
			// No retries, no deadline, no breaker: the MCP process carries no
			// resilience policy, and the caller supplies its own timeout.
			return call(ctx)
		},
		Redact: func(text string) string { return text },
	})
	if err != nil {
		t.Fatalf("tools.New() error = %v, want nil", err)
	}
	return []adktool.Tool{
		built.ListIncidents(),
		built.GetIncident(),
		built.GetServiceStatus(),
		built.SearchServiceLogs(),
		runbookTool(t, compose.GetRunbookToolName, confirmRunbook),
		runbookTool(t, compose.SearchRunbooksToolName, false),
	}
}

// runbookArgs and runbookResult are the stand-in runbook tool's boundary types.
type runbookArgs struct {
	Query string `json:"query" jsonschema:"the runbook slug or symptom to look up"`
}

type runbookResult struct {
	Runbook string `json:"runbook"`
}

// runbookTool builds one stand-in runbook tool.
func runbookTool(t *testing.T, name string, requireConfirmation bool) adktool.Tool {
	t.Helper()

	built, err := functiontool.New(
		functiontool.Config{
			Name:                name,
			Description:         "Stand-in for the runbook knowledge tool the memory package owns.",
			RequireConfirmation: requireConfirmation,
		},
		func(_ agent.Context, args runbookArgs) (runbookResult, error) {
			return runbookResult{Runbook: args.Query}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q) error = %v, want nil", name, err)
	}
	return built
}

// connect starts an HTTP test server over the fixture's handler and returns an
// initialized MCP client session against it.
//
// The real protocol over the real transport, in process: initialize, the
// capability exchange and the JSON-RPC framing all happen exactly as they would
// against a bound port, without one.
func (f *fixture) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()

	httpServer := httptest.NewServer(f.server.Handler())
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "offline-contract-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: httpServer.URL + mcpserver.StreamablePath,
	}, nil)
	if err != nil {
		t.Fatalf("connecting to the MCP server: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("closing the MCP session: %v", err)
		}
	})
	return session
}
