// Package mcpserver re-exposes the agent's read tools over the Model Context
// Protocol, in their own process.
//
// # Why a second process serves the same tools
//
// MCP lets a tool live outside the agent that calls it, so any MCP client can
// use it: this one server backs the agent directly in Chapter 3.3 and, from
// Chapter 5.2 onwards, sits behind agentgateway with authentication, rate
// limits and guards in front of it. Nothing about the tools changes when that
// happens, which is the lesson — the transport moved, the capability did not.
//
// # The surface is read-only, three times over
//
// A server that could grow a write is not a boundary, so the restriction is
// enforced at three independent points and any one of them alone would hold:
//
//  1. [New] refuses to start unless the tools it is given are exactly the six
//     the client allowlist names. A seventh tool is a startup failure, not a
//     surprise at call time, and a missing one is caught in the same breath.
//  2. The client pins the same six names in its own tool filter
//     (compose.MCPReadToolNames), so even a compromised server that advertised
//     a new tool would never have it offered to the model.
//  3. A tool that requires human confirmation does not execute here. The
//     context an MCP call runs under has no session, no event stream and no
//     human attached, so the confirmation request fails and the call fails with
//     it. See [ErrConfirmationUnavailable]. The two guarded writes stay in the
//     agent process, where a human can actually approve them.
//
// # What the process does not carry
//
// No resilience policy and no governance plugin. Retries, deadlines and the
// circuit breaker belong to the caller — the ADK client supplies its own
// timeout, and a server that retried on the client's behalf would double the
// load on a struggling dependency. Redaction likewise runs in the agent
// process, at the model and tool boundaries. This process reads a database and
// answers.
//
// # State
//
// Read-only, always. The readiness probe reports an unprepared or corrupt
// runtime database as unready and never repairs it: publication and migration
// belong to the A2A startup path, which is the single writer, and an MCP
// replica that migrated state under a running agent would be a data race across
// processes.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
)

// ServerName is the implementation name this server reports at initialize.
//
// It matches the ADK application name the agent runs under (policy.AppName), so
// a gateway route, a Kubernetes service and a trace all attribute work to the
// same string. Clients see it in the initialize response, and the smoke test
// asserts it.
const ServerName = "agentops-agent"

// ErrIncompleteConfig marks a [Config] that cannot produce a working server.
var ErrIncompleteConfig = errors.New("incomplete MCP server configuration")

// ErrConfirmationUnavailable is what a tool requiring human approval gets when
// it is called over MCP.
//
// It is the third of the three read-only enforcements, and the only one that
// still holds if the other two are misconfigured: this process has no session
// to pause, no event stream to publish a confirmation request on, and no human
// listening.
//
// The refusal is a design decision, not a protocol limit. The pinned MCP SDK
// can carry a multi round-trip request, so a server could ask its client to
// elicit an approval — but the approver identity, the session, the invocation
// id, and the audit row that commits in one transaction with the mutation all
// live in the agent process. An approval negotiated over `tools/call` could be
// neither attributed nor replayed, so this server declines to ask rather than
// collecting a confirmation it cannot bind to anything.
var ErrConfirmationUnavailable = errors.New(
	"this tool requires human confirmation, which the MCP server cannot request: " +
		"call it through the agent instead",
)

// Probe reports whether the runtime state this server reads is usable.
//
// It is the seam the data package fills, as a function rather than a store so
// this package cannot accidentally reach a write path:
//
//	Probe: func(ctx context.Context) error {
//		_, err := store.ProbeRuntimeDatabase(ctx)
//		return err
//	}
type Probe func(ctx context.Context) error

// Config is everything the server needs from the rest of the runtime.
type Config struct {
	// Probe backs the readiness route. Required: a replica that reports ready
	// without checking anything is worse than one with no probe at all,
	// because Kubernetes will send it traffic.
	Probe Probe

	// Logger receives the server's own diagnostics. Nil uses slog.Default.
	//
	// On the stdio transport this must not write to standard output: stdout is
	// the protocol stream, and a log line in the middle of it is a framing
	// error. slog's default handler writes to standard error, which is why the
	// default is safe.
	Logger *slog.Logger

	// Tools are the six read tools, in registration order. They are the same
	// tool values the agent binds locally, so the names, descriptions and JSON
	// schemas a client sees over MCP are the ones the model sees in process —
	// identical by construction rather than by convention.
	Tools []tool.Tool

	// Version is reported at initialize. Empty reports "0.0.0".
	Version string

	// Options are the transport and security settings. See [Options].
	Options Options
}

// Server is the MCP server. Build it with [New] and run it with
// [Server.Serve]; the zero value is not usable.
type Server struct {
	mcp     *mcp.Server
	logger  *slog.Logger
	probe   Probe
	names   []string
	options Options
}

// runnableTool is the shape a tool has to have to be re-exposed: ADK's tool
// interface, plus the declaration that carries its JSON schemas and the
// handler that runs it.
//
// It is declared here rather than imported because ADK keeps the equivalent
// unexported. Every tool built by tool/functiontool satisfies it, which is
// every tool this repository writes.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
}

// New builds the server over cfg, refusing anything but the exact read surface.
func New(cfg Config) (*Server, error) {
	if cfg.Probe == nil {
		return nil, fmt.Errorf(
			"%w: Probe is required; a replica that reports ready without checking "+
				"anything will be sent traffic it cannot serve", ErrIncompleteConfig,
		)
	}
	options, err := cfg.Options.resolve()
	if err != nil {
		return nil, err
	}
	if err := checkReadSurface(cfg.Tools); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	version := cfg.Version
	if version == "" {
		version = "0.0.0"
	}

	server := mcp.NewServer(
		&mcp.Implementation{Name: ServerName, Version: version},
		// The SDK's logger is wired to the same destination as ours so a
		// protocol-level failure and an application-level one land in one
		// stream rather than one of them vanishing.
		&mcp.ServerOptions{Logger: logger},
	)
	names := make([]string, 0, len(cfg.Tools))
	for _, exposed := range cfg.Tools {
		runnable, ok := exposed.(runnableTool)
		if !ok {
			return nil, fmt.Errorf(
				"%w: tool %q cannot be re-exposed over MCP; it declares no schema and no handler",
				ErrIncompleteConfig, exposed.Name(),
			)
		}
		if err := register(server, runnable); err != nil {
			return nil, err
		}
		names = append(names, runnable.Name())
	}

	return &Server{
		mcp:     server,
		logger:  logger,
		probe:   cfg.Probe,
		names:   names,
		options: options,
	}, nil
}

// ToolNames returns the tools this server serves, in registration order.
//
// A fresh slice on every call: an allowlist a caller can reorder or truncate in
// place is not an allowlist.
func (s *Server) ToolNames() []string { return slices.Clone(s.names) }

// checkReadSurface holds the server to exactly the tools the client allowlist
// names.
//
// Set equality in both directions, which is the Go port of the Python suite's
// first MCP assertion. One direction alone is not enough: an extra tool here is
// a widened surface the client would never call, and a missing one silently
// disappears the moment AGENT_MCP_URL is set, because the agent then stops
// binding its local equivalent. Both failures are invisible at runtime and both
// are caught here, at startup.
func checkReadSurface(exposed []tool.Tool) error {
	allowed := compose.MCPReadToolNames()
	served := make([]string, 0, len(exposed))
	for _, candidate := range exposed {
		if candidate == nil {
			return fmt.Errorf("%w: Tools contains a nil tool", ErrIncompleteConfig)
		}
		served = append(served, candidate.Name())
	}

	var problems []error
	for _, name := range served {
		if !slices.Contains(allowed, name) {
			problems = append(problems, fmt.Errorf(
				"%w: tool %q is not in the read allowlist (%s); the MCP surface is read-only "+
					"and a server cannot widen it", ErrIncompleteConfig, name, strings.Join(allowed, ", "),
			))
		}
	}
	for _, name := range allowed {
		if !slices.Contains(served, name) {
			problems = append(problems, fmt.Errorf(
				"%w: the read allowlist names %q, which this server does not serve; the client "+
					"would stop offering it to the model entirely", ErrIncompleteConfig, name,
			))
		}
	}
	return errors.Join(problems...)
}

// register adapts one ADK tool onto the MCP protocol.
//
// The low-level Server.AddTool is used rather than the generic mcp.AddTool
// helper because the schemas are values known only at run time — they come from
// the ADK tool's own declaration — and the generic helper infers them from Go
// type parameters instead. Reusing the declaration is the point: it is what
// makes the MCP surface and the in-process surface the same surface.
func register(server *mcp.Server, exposed runnableTool) error {
	declaration := exposed.Declaration()
	if declaration == nil {
		return fmt.Errorf("%w: tool %q declares no schema", ErrIncompleteConfig, exposed.Name())
	}
	input, err := objectSchema(exposed.Name(), "input", declaration.ParametersJsonSchema)
	if err != nil {
		return err
	}
	described := &mcp.Tool{
		Name:        exposed.Name(),
		Description: exposed.Description(),
		InputSchema: input,
	}
	// The output schema is advertised only when it is a well-formed object.
	// AddTool panics on a malformed one, and an unadvertised output schema
	// costs a client nothing — it still receives the same JSON.
	if output, err := objectSchema(exposed.Name(), "output", declaration.ResponseJsonSchema); err == nil {
		described.OutputSchema = output
	}
	server.AddTool(described, handlerFor(exposed))
	return nil
}

// objectSchema checks that a declared schema is the JSON-object shape MCP
// requires, before the SDK panics on it.
func objectSchema(toolName, role string, declared any) (*jsonschema.Schema, error) {
	schema, ok := declared.(*jsonschema.Schema)
	if !ok {
		return nil, fmt.Errorf(
			"%w: tool %q declares a %s schema of type %T, want *jsonschema.Schema",
			ErrIncompleteConfig, toolName, role, declared,
		)
	}
	if schema.Type != "object" {
		return nil, fmt.Errorf(
			"%w: tool %q declares a %s schema of type %q, but MCP requires %q",
			ErrIncompleteConfig, toolName, role, schema.Type, "object",
		)
	}
	return schema, nil
}

// handlerFor turns an ADK tool into an MCP tool handler.
//
// The arguments arrive as raw JSON and are decoded into the map ADK's own
// dispatcher hands a tool, so the tool's schema-driven conversion and its
// refusal messages behave exactly as they do in process.
func handlerFor(exposed runnableTool) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments := map[string]any{}
		if raw := request.Params.Arguments; len(raw) > 0 {
			if err := json.Unmarshal(raw, &arguments); err != nil {
				// A malformed argument object is the client's mistake, and it
				// is reported as a tool error rather than a protocol error so
				// a model on the other end can correct itself.
				return toolFailure(fmt.Errorf("decoding the arguments of %q: %w", exposed.Name(), err)), nil
			}
		}

		result, err := exposed.Run(detachedToolContext{ctx: ctx}, arguments)
		if err != nil {
			return toolFailure(err), nil
		}
		return toolSuccess(exposed.Name(), result)
	}
}

// toolSuccess renders a tool result as both text and structured content.
//
// Both, deliberately. Structured content is what a schema-aware client reads;
// the text block is what every other client — and every model that receives the
// result as a message — actually sees, and a result with no text block reads as
// an empty answer.
func toolSuccess(toolName string, result map[string]any) (*mcp.CallToolResult, error) {
	rendered, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encoding the result of %q: %w", toolName, err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(rendered)}},
		StructuredContent: result,
	}, nil
}

// toolFailure renders a failure as a tool error rather than a protocol error.
//
// The protocol reserves its own errors for "no such tool" and transport faults.
// A tool that failed has to report inside the result, with IsError set, or the
// model never learns that anything went wrong and cannot adapt.
func toolFailure(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		IsError: true,
	}
}
