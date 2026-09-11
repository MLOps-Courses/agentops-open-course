package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// TestServerExposesExactlyTheAllowlistedReadTools is the port of the Python
// suite's first MCP assertion, and it is checked in both directions on purpose.
//
// Adding a tool to the server without adding it to the client allowlist means
// it is silently never offered to the model; removing one leaves a stale allow
// entry and the agent stops binding its local equivalent the moment
// AGENT_MCP_URL is set. Set equality catches both, and the infrastructure gate
// asserts the same six names in the three gateway configurations.
func TestServerExposesExactlyTheAllowlistedReadTools(t *testing.T) {
	t.Parallel()

	session := newFixture(t).connect(t)
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	served := make([]string, 0, len(listed.Tools))
	for _, served0 := range listed.Tools {
		served = append(served, served0.Name)
	}
	allowed := compose.MCPReadToolNames()
	slices.Sort(served)
	slices.Sort(allowed)
	if !slices.Equal(served, allowed) {
		t.Errorf("the server serves %v, want exactly the allowlist %v", served, allowed)
	}
}

// TestServerNeverExposesTheGuardedWrites states the point of the whole package
// as an assertion rather than as prose: the two actions that mutate state and
// require human approval have no presence on this surface at all.
func TestServerNeverExposesTheGuardedWrites(t *testing.T) {
	t.Parallel()

	served := newFixture(t).server.ToolNames()
	for _, forbidden := range []string{tools.RestartServiceToolName, tools.ResolveIncidentToolName} {
		if slices.Contains(served, forbidden) {
			t.Errorf("the MCP surface exposes %q; guarded writes stay in the agent process", forbidden)
		}
	}
}

// TestNewRefusesAnythingButTheExactReadSurface holds both failure directions at
// startup, where they are cheap, rather than at call time, where neither is
// visible.
func TestNewRefusesAnythingButTheExactReadSurface(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		build   func(t *testing.T) []adktool.Tool
		wantErr string
	}{
		{
			name: "a seventh tool widens the surface",
			build: func(t *testing.T) []adktool.Tool {
				t.Helper()
				return append(readSurface(t, newStore(t), false), runbookTool(t, "list_secrets", false))
			},
			wantErr: "not in the read allowlist",
		},
		{
			name: "a missing tool silently disappears from the agent",
			build: func(t *testing.T) []adktool.Tool {
				t.Helper()
				return readSurface(t, newStore(t), false)[:5]
			},
			wantErr: "which this server does not serve",
		},
		{
			name: "a nil tool is a wiring mistake, not an empty surface",
			build: func(t *testing.T) []adktool.Tool {
				t.Helper()
				surface := readSurface(t, newStore(t), false)
				surface[0] = nil
				return surface
			},
			wantErr: "contains a nil tool",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := mcpserver.New(mcpserver.Config{
				Probe: func(context.Context) error { return nil },
				Tools: testCase.build(t),
			})
			if err == nil {
				t.Fatal("New() returned no error")
			}
			if !errors.Is(err, mcpserver.ErrIncompleteConfig) {
				t.Errorf("New() error = %v, want it to wrap ErrIncompleteConfig", err)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("New() error = %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}

// TestNewRequiresAProbe: a replica that reports ready without checking anything
// gets sent traffic it cannot serve, which is worse than having no probe.
func TestNewRequiresAProbe(t *testing.T) {
	t.Parallel()

	_, err := mcpserver.New(mcpserver.Config{})
	if !errors.Is(err, mcpserver.ErrIncompleteConfig) {
		t.Errorf("New() error = %v, want it to wrap ErrIncompleteConfig", err)
	}
}

// TestToolCallRoundTripReadsTheDataset is the port of
// test_streamable_http_initialize_and_tool_call_round_trip: the real protocol,
// over the real transport, against the real dataset, in process and without a
// port. It proves the whole chain — initialize, tools/call, argument decoding,
// the ADK tool, SQLite, and the result rendering — rather than any one link.
func TestToolCallRoundTripReadsTheDataset(t *testing.T) {
	t.Parallel()

	session := newFixture(t).connect(t)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      tools.ListIncidentsToolName,
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want nil", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() reported an error: %v", result.Content)
	}

	text := textOf(t, result)
	var listed tools.ListIncidentsResult
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("decoding the tool result %q: %v", text, err)
	}
	if listed.Count == nil || *listed.Count == 0 {
		t.Fatalf("list_incidents returned %d incidents, want the seeded dataset", len(listed.Incidents))
	}
	if listed.Error != "" {
		t.Errorf("list_incidents refused the call: %s", listed.Error)
	}
	// Structured content is what a schema-aware client reads; the text block is
	// what a model sees. Both have to be there.
	if result.StructuredContent == nil {
		t.Error("the result carries no structured content")
	}
}

// TestToolCallCarriesTheSameSchemaTheModelSees is why the server reuses the ADK
// declaration instead of describing the tools a second time: a client's view of
// a tool and the model's view of the same tool cannot drift, because they are
// the same object.
func TestToolCallCarriesTheSameSchemaTheModelSees(t *testing.T) {
	t.Parallel()

	session := newFixture(t).connect(t)
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}

	for _, described := range listed.Tools {
		if described.Description == "" {
			t.Errorf("tool %q carries no description; a client shows it to a model verbatim", described.Name)
		}
		if described.InputSchema == nil {
			t.Errorf("tool %q carries no input schema", described.Name)
			continue
		}
		schema, ok := described.InputSchema.(map[string]any)
		if !ok {
			t.Errorf("tool %q input schema is %T, want a JSON object", described.Name, described.InputSchema)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q input schema type = %v, want \"object\"", described.Name, schema["type"])
		}
	}

	// One schema in detail, so "carries a schema" is not the whole claim: the
	// incident lookup has to describe its one argument to the model.
	for _, described := range listed.Tools {
		if described.Name != tools.GetIncidentToolName {
			continue
		}
		schema, _ := described.InputSchema.(map[string]any)
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s declares no properties: %v", described.Name, schema)
		}
		if _, found := properties["incident_id"]; !found {
			t.Errorf("%s does not declare its incident_id argument: %v", described.Name, properties)
		}
	}
}

// TestARefusalTravelsAsAResultNotAProtocolError preserves the distinction the
// tools package draws: a refusal is advice the model is expected to read and
// act on, so it has to arrive as a result the model can see rather than as a
// transport failure that never reaches it.
func TestARefusalTravelsAsAResultNotAProtocolError(t *testing.T) {
	t.Parallel()

	session := newFixture(t).connect(t)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      tools.GetIncidentToolName,
		Arguments: map[string]any{"incident_id": "not-an-identifier"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want the refusal to arrive as a result", err)
	}

	var lookup tools.GetIncidentResult
	if err := json.Unmarshal([]byte(textOf(t, result)), &lookup); err != nil {
		t.Fatalf("decoding the tool result: %v", err)
	}
	if lookup.Error == "" {
		t.Error("a malformed identifier produced no refusal message for the model")
	}
	if lookup.Incident != nil {
		t.Error("a malformed identifier returned an incident")
	}
}

// TestAConfirmationRequiringToolCannotExecuteOverMCP is the third and last of
// the read-only enforcements, and the only one that still holds when the other
// two are misconfigured.
//
// The tool here is registered under an allowlisted name, so the name check
// cannot be what stops it. It is stopped because this process has no session to
// pause, no event stream to publish an approval request on, and no human
// listening — which is precisely why the two real guarded writes stay in the
// agent process.
func TestAConfirmationRequiringToolCannotExecuteOverMCP(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *fixtureOptions) { o.confirmRunbook = true })
	session := fixture.connect(t)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      compose.GetRunbookToolName,
		Arguments: map[string]any{"query": "anything"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want the refusal to arrive as a result", err)
	}
	if !result.IsError {
		t.Fatal("a tool requiring human confirmation executed over MCP")
	}
	if text := textOf(t, result); !strings.Contains(text, "human confirmation") {
		t.Errorf("the refusal reads %q, want it to say why approval could not be requested", text)
	}
}

// TestServerReportsItsIdentityAtInitialize pins the name a gateway route, a
// Kubernetes service and a trace all attribute work to.
func TestServerReportsItsIdentityAtInitialize(t *testing.T) {
	t.Parallel()

	session := newFixture(t).connect(t)
	if got := session.InitializeResult().ServerInfo.Name; got != mcpserver.ServerName {
		t.Errorf("server name = %q, want %q", got, mcpserver.ServerName)
	}
}

// textOf returns the single text block of a tool result.
func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	for _, block := range result.Content {
		if text, ok := block.(*mcp.TextContent); ok && text.Text != "" {
			return text.Text
		}
	}
	t.Fatalf("the result carries no text content: %v", result.Content)
	return ""
}
