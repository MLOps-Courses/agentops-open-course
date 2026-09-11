// Package compose wires the runnable ADK compositions: the conversational
// agent, the least-privilege coordinator, the bounded triage workflow, and the
// structured triage report.
//
// # One tool surface, four bindings
//
// Every composition draws from the same [Tools] value and binds a different
// subset of it. That is not tidiness — it is the mechanism behind two course
// guarantees:
//
//   - Least privilege (Chapter 3.7). The diagnosis specialist holds only reads,
//     so a prompt injection in a log line cannot make it mutate state: it has no
//     write tool to call. The remediation specialist holds only the two guarded
//     writes, so it cannot read raw logs at all. Neither is a prompt rule the
//     model may ignore; it is the absence of a capability.
//   - Bounded planning (Chapter 3.5). Only the investigate stage of the workflow
//     holds list_incidents, so no later stage can reopen discovery and drift to
//     a different incident than the one the plan named.
//
// # Reads and writes have different authority
//
// The conversational entrypoint alone switches its six read and runbook tools
// from direct local calls to MCP when [Config.MCP] is set (AGENT_MCP_URL). The
// workflow and coordinator specialists always bind local tools. The two guarded
// writes stay in process and confirmed on every path, because a remote server
// must never be able to originate a mutation.
//
// # No policy is wired here, on purpose
//
// Budget, compaction, PII redaction, write validation, output hardening and
// error handling are attached once for the whole application by the policy
// plugin, so every agent — including sub-agents and workflow nodes — is
// governed without repeating a line of it. Every llmagent.Config this package
// produces leaves all six callback slots empty, and
// TestNoCompositionWiresItsOwnPolicyCallbacks asserts that absence: a stage that
// re-attached its own callbacks would make the app-level plugin run twice,
// double-counting tokens and double-redacting.
//
// # Configuration
//
// The package does not import the configuration loader for anything but the
// entrypoint enum: [Config] is a value the runtime builds, exactly as the tools
// and policy packages are wired.
package compose

import (
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// The names every composition registers under. They are constants because the
// evalsets, the A2A agent card, the course prose and the delegation
// instructions all key on them: the coordinator's instruction literally tells
// the model to transfer to "diagnosis_agent", so a rename that missed one would
// leave a coordinator pointing at an agent that no longer exists.
const (
	// AgentName is the default conversational agent.
	AgentName = "agentops_agent"
	// CoordinatorName triages and delegates; it holds no write tool itself.
	CoordinatorName = "coordinator_agent"
	// DiagnosisName is the read-only specialist.
	DiagnosisName = "diagnosis_agent"
	// RemediationName is the write-only specialist.
	RemediationName = "remediation_agent"
	// WorkflowName is the bounded read-only triage graph.
	WorkflowName = "triage_workflow"
	// ReportAgentName produces the schema-validated triage report.
	ReportAgentName = "triage_report_agent"
)

// Tool names this module does not own elsewhere.
//
// The four incident reads and the two guarded writes are named by the tools
// package and read from there, so they are spelled once. These four are not:
// the memory package builds the tools, but the names are already load-bearing
// here, in the root instruction and in the MCP allowlist below, so this package
// has to be able to say them.
const (
	// GetRunbookToolName fetches one runbook by slug.
	GetRunbookToolName = "get_runbook"
	// SearchRunbooksToolName finds runbook guidance by symptom.
	SearchRunbooksToolName = "search_runbooks"
	// RecallIncidentContextToolName reads the caller's durable incident notes.
	RecallIncidentContextToolName = "recall_incident_context"
	// SaveIncidentNoteToolName writes one durable incident note.
	SaveIncidentNoteToolName = "save_incident_note"
)

// ErrIncompleteConfig marks a [Config] that cannot produce a working
// composition.
//
// It is a sentinel so a caller can tell "this composition is wired wrong" —
// which is a startup bug — apart from a runtime failure.
var ErrIncompleteConfig = errors.New("incomplete composition configuration")

// Tools is the function-tool surface the compositions draw from.
//
// Each tool is named individually rather than handed over as one slice because
// every composition binds a different subset, and a slice cannot express "the
// evidence-review stage may re-read the runbook but may not list incidents".
// The grouping helpers below reproduce the Python track's ALL_TOOLS,
// KNOWLEDGE_TOOLS and ACTION_TOOLS in their registration order.
type Tools struct {
	// The four idempotent incident reads, built by the tools package.
	ListIncidents     tool.Tool
	GetIncident       tool.Tool
	GetServiceStatus  tool.Tool
	SearchServiceLogs tool.Tool

	// The two runbook-knowledge reads, built by the memory package.
	GetRunbook     tool.Tool
	SearchRunbooks tool.Tool

	// The two guarded writes, built by the tools package. Both pause for human
	// confirmation before their handler runs, and no composition may substitute
	// a look-alike: they are carried by identity, never by name.
	RestartService  tool.Tool
	ResolveIncident tool.Tool
}

// ReadTools returns the four idempotent incident reads in registration order.
// It is the Go counterpart of Python's ALL_TOOLS.
//
// The slice is fresh on every call: a caller that appends to it, or reorders
// it, must not be able to change what the next caller sees.
func (t Tools) ReadTools() []tool.Tool {
	return []tool.Tool{t.ListIncidents, t.GetIncident, t.GetServiceStatus, t.SearchServiceLogs}
}

// KnowledgeTools returns the two runbook reads in registration order. It is the
// Go counterpart of Python's KNOWLEDGE_TOOLS.
func (t Tools) KnowledgeTools() []tool.Tool {
	return []tool.Tool{t.GetRunbook, t.SearchRunbooks}
}

// ActionTools returns the two guarded writes in registration order. It is the
// Go counterpart of Python's ACTION_TOOLS.
func (t Tools) ActionTools() []tool.Tool {
	return []tool.Tool{t.RestartService, t.ResolveIncident}
}

// Config is everything the compositions need from the rest of the runtime.
//
// Field order follows what the compiler wants (govet's fieldalignment check),
// not what a reader would group.
type Config struct {
	// Model is the LLM every agent in every composition shares. One instance,
	// not one per agent: the model package validates credentials once and hands
	// back a client with a single endpoint, deadline, retry budget and
	// connection pool. Building one per agent would multiply all four.
	Model model.LLM

	// GenerateContentConfig carries sampling settings (AGENT_MODEL_TEMPERATURE).
	// Nil keeps each provider's default, which is what the model package returns
	// when the temperature is unset.
	GenerateContentConfig *genai.GenerateContentConfig

	// Skills is the least-privilege skill toolset (Chapter 3.2). Build it with
	// [NewSkills]; it is a toolset rather than a tool list because it also
	// injects the skills catalog into the system instruction.
	Skills tool.Toolset

	// MCP is the governed MCP route (Chapter 5.2). Non-nil replaces the six
	// local read and runbook tools on the conversational entrypoint *only* —
	// see the package doc. Build it with [NewMCPToolset]; nil keeps every read
	// in process.
	MCP tool.Toolset

	// Tools is the function-tool surface. Every field is required: a nil tool
	// would produce an agent whose own instruction names a capability it does
	// not have.
	Tools Tools

	// Instruction overrides the committed root instruction. Empty uses
	// [Instruction], which is what every runtime path does.
	Instruction string

	// Entrypoint selects which composition [Compose.RootAgent] builds.
	Entrypoint config.Entrypoint

	// Memory holds the long-term memory tools (Chapter 3.4), in the order the
	// agent registers them: recall_incident_context then save_incident_note.
	// They stay local even when the read tools are served over MCP, because the
	// store is per user and per application.
	Memory []tool.Tool
}

// Compose builds the compositions over a validated configuration. The zero
// value is not usable; construct one with [New].
type Compose struct {
	model                 model.LLM
	generateContentConfig *genai.GenerateContentConfig
	skills                tool.Toolset
	mcp                   tool.Toolset

	tools Tools

	instruction string
	entrypoint  config.Entrypoint

	memory []tool.Tool
}

// New validates cfg and returns the composer.
//
// Validation is fail-fast and accumulating: a wiring with three holes reports
// three lines, because a composition is assembled once at startup and every
// hole is a capability the agent's own instruction claims it has.
func New(cfg Config) (*Compose, error) {
	var problems []error

	if cfg.Model == nil {
		problems = append(problems, fmt.Errorf("%w: Model is required", ErrIncompleteConfig))
	}
	if !cfg.Entrypoint.Valid() {
		problems = append(problems, fmt.Errorf(
			"%w: Entrypoint %q is not one of %s",
			ErrIncompleteConfig, cfg.Entrypoint, config.Entrypoints(),
		))
	}
	if cfg.Skills == nil {
		problems = append(problems, fmt.Errorf(
			"%w: Skills is required; without it the model is never told a procedure exists",
			ErrIncompleteConfig,
		))
	}
	problems = append(problems, missingToolProblems(cfg)...)
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	instruction := cfg.Instruction
	if instruction == "" {
		instruction = Instruction()
	}
	return &Compose{
		model:                 cfg.Model,
		generateContentConfig: cfg.GenerateContentConfig,
		skills:                cfg.Skills,
		mcp:                   cfg.MCP,
		memory:                append([]tool.Tool(nil), cfg.Memory...),
		tools:                 cfg.Tools,
		instruction:           instruction,
		entrypoint:            cfg.Entrypoint,
	}, nil
}

// missingToolProblems reports every tool the compositions need and cfg does not
// supply, naming each one the way the model would call it.
func missingToolProblems(cfg Config) []error {
	required := []struct {
		value tool.Tool
		name  string
	}{
		{cfg.Tools.ListIncidents, tools.ListIncidentsToolName},
		{cfg.Tools.GetIncident, tools.GetIncidentToolName},
		{cfg.Tools.GetServiceStatus, tools.GetServiceStatusToolName},
		{cfg.Tools.SearchServiceLogs, tools.SearchServiceLogsToolName},
		{cfg.Tools.GetRunbook, GetRunbookToolName},
		{cfg.Tools.SearchRunbooks, SearchRunbooksToolName},
		{cfg.Tools.RestartService, tools.RestartServiceToolName},
		{cfg.Tools.ResolveIncident, tools.ResolveIncidentToolName},
	}
	problems := make([]error, 0, len(required)+1)
	for _, entry := range required {
		if entry.value == nil {
			problems = append(problems, fmt.Errorf(
				"%w: Tools.%s is required", ErrIncompleteConfig, entry.name,
			))
		}
	}
	if len(cfg.Memory) == 0 {
		problems = append(problems, fmt.Errorf(
			"%w: Memory is required; the root instruction tells the model to call %s and %s",
			ErrIncompleteConfig, RecallIncidentContextToolName, SaveIncidentNoteToolName,
		))
	}
	for index, memoryTool := range cfg.Memory {
		if memoryTool == nil {
			problems = append(problems, fmt.Errorf(
				"%w: Memory[%d] is nil", ErrIncompleteConfig, index,
			))
		}
	}
	return problems
}

// Entrypoint reports which composition [Compose.RootAgent] builds.
func (c *Compose) Entrypoint() config.Entrypoint { return c.entrypoint }

// Instruction returns the instruction the conversational root agent carries,
// after any override.
func (c *Compose) Instruction() string { return c.instruction }

// rootBuilders names the three compositions an entrypoint selects between.
//
// It exists so the selection rule and the selection contents are separately
// expressible — and separately testable — without putting a test seam on
// [Compose]. The Python track got the same property from importing each
// sub-module inside its branch: the composition that was not selected is never
// constructed, which matters because constructing one costs a skill catalog
// read and an MCP connection.
type rootBuilders struct {
	agent       func() (agent.Agent, error)
	workflow    func() (agent.Agent, error)
	coordinator func() (agent.Agent, error)
}

// RootAgent builds only the composition the entrypoint selects.
func (c *Compose) RootAgent() (agent.Agent, error) {
	return selectRoot(c.entrypoint, rootBuilders{
		agent:       c.ConversationalAgent,
		workflow:    c.TriageWorkflow,
		coordinator: c.CoordinatorAgent,
	})
}

// selectRoot calls exactly one builder.
func selectRoot(entrypoint config.Entrypoint, builders rootBuilders) (agent.Agent, error) {
	switch entrypoint {
	case config.EntrypointWorkflow:
		return builders.workflow()
	case config.EntrypointCoordinator:
		return builders.coordinator()
	case config.EntrypointAgent:
		return builders.agent()
	default:
		// Unreachable after New's validation, and stated anyway: a future
		// entrypoint added to the enum must fail loudly here rather than
		// silently serving the conversational agent.
		return nil, fmt.Errorf(
			"%w: no composition is registered for entrypoint %q",
			ErrIncompleteConfig, entrypoint,
		)
	}
}

// newAgent builds one LLM agent, naming it in any failure.
func newAgent(cfg llmagent.Config) (agent.Agent, error) {
	built, err := llmagent.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("building the %s agent: %w", cfg.Name, err)
	}
	return built, nil
}

// baseConfig is the part of every llmagent.Config that does not vary: the
// shared model and its sampling settings, and — deliberately — nothing else.
//
// Every callback slot is left zero. See the package doc: policy is attached
// once at the application boundary, never per agent.
func (c *Compose) baseConfig(name, description, instruction string) llmagent.Config {
	return llmagent.Config{
		Name:                  name,
		Description:           description,
		Instruction:           instruction,
		Model:                 c.model,
		GenerateContentConfig: c.generateContentConfig,
	}
}
