// Package tools exposes the AgentOps Agent's function tools: four idempotent
// reads over the committed dataset, and two guarded mock writes that pause for
// human approval and record attributable evidence.
//
// Three properties define the package, and each one is a course guarantee
// rather than an implementation detail:
//
//   - Reads are wrapped, writes are not. Every read runs through a [Guard] — a
//     deadline, bounded retries, a circuit breaker — because a read is
//     idempotent. The two writes deliberately never do: an automatic retry can
//     cross an unknown commit boundary.
//   - A write pauses first. Both actions carry RequireConfirmation, so ADK
//     stops before the handler runs and asks a human. The approval must carry a
//     rationale; the handler re-checks confirmation, identity and rationale
//     itself, so it fails closed even when reached some other way.
//   - A write and its evidence commit together. The mutation and the audit row
//     land in one transaction (see the data package), and replaying the same
//     invocation returns the original audit row without mutating anything a
//     second time.
//
// A refusal — a malformed identifier, an unknown service, a missing rationale —
// is a *result value* carrying an "error" key, never a Go error. That
// distinction is load-bearing: ADK turns a returned error into an opaque
// failure the policy plane classifies, while a refusal is advice the model is
// expected to read and act on. Genuine failures (the database is unreachable,
// the audit insert was rejected) are Go errors and stay Go errors.
package tools

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// The names the model calls, and the names everything else keys on: the
// evalsets, the MCP allowlist, the guardrail's action set, and the course
// prose. They are constants so no other package has to spell them, and so
// renaming one is a single, visible edit rather than a search.
const (
	ListIncidentsToolName     = "list_incidents"
	GetIncidentToolName       = "get_incident"
	GetServiceStatusToolName  = "get_service_status"
	SearchServiceLogsToolName = "search_service_logs"
	RestartServiceToolName    = "restart_service"
	ResolveIncidentToolName   = "resolve_incident"
)

// auditActor is what the audit log records as the actor of an agent-initiated
// write. The human who approved it is recorded separately, in approved_by.
const auditActor = "agentops-agent"

// ErrIncompleteConfig marks a [Config] that cannot produce working tools.
//
// It is a sentinel so a caller can tell "this composition is wired wrong" —
// which is a startup bug — apart from a runtime failure.
var ErrIncompleteConfig = errors.New("incomplete tools configuration")

// Store is the dataset access these tools need.
//
// It is declared here rather than reused from the data package because the
// consumer is the one that knows which operations it depends on: a reader sees
// the whole surface the tools can reach, and a test can substitute a store that
// loses a race the real one cannot be made to lose on demand. [data.Store]
// satisfies it.
type Store interface {
	ListIncidents(ctx context.Context, filter data.IncidentFilter) ([]domain.Incident, error)
	GetIncident(ctx context.Context, id domain.IncidentID) (*domain.Incident, error)
	ListServices(ctx context.Context) ([]domain.Service, error)
	GetService(ctx context.Context, name domain.Slug) (*domain.Service, error)
	ReadServiceLogs(service string) ([]string, bool, error)
	LogsDir() string
	RestartServiceWithAudit(
		ctx context.Context, name domain.Slug, identity data.AuditIdentity,
	) (*domain.AuditEntry, error)
	ResolveIncidentWithAudit(
		ctx context.Context, id domain.IncidentID, identity data.AuditIdentity,
	) (*domain.AuditEntry, error)
}

// Guard runs one idempotent read tool call.
//
// It is the seam the resilience package fills: the deadline, the bounded
// retries and the circuit breaker all live behind it, exactly as Python's
// with_resilience decorator wrapped each read function. call must therefore be
// safe to run more than once — which is precisely why the two guarded writes
// below never go through a Guard.
//
// The tool name is passed through because the circuit breaker is per tool: one
// failing dependency must not shed load from the others.
type Guard func(ctx context.Context, toolName string, call func(context.Context) error) error

// Redactor removes PII and credentials from text that is about to be persisted.
//
// It is the seam the policy package fills. The approval rationale travels
// through it before it reaches the append-only audit trail, where it can never
// be edited or deleted again.
type Redactor func(text string) string

// Config is everything the tools need from the rest of the runtime.
//
// Guard and Redact are required rather than optional because a nil one would be
// a silent downgrade of a Chapter 4.5 guarantee — an unguarded read or an
// unredacted audit row — and silence is exactly the wrong failure mode for a
// security control. A composition has to state its choice.
type Config struct {
	// Store reads the dataset and performs the two audited mutations.
	Store Store
	// Guard wraps every read. See [Guard].
	Guard Guard
	// Redact cleans an approval rationale before it is persisted. See [Redactor].
	Redact Redactor
	// WritesDisabled is the AGENT_WRITES_DISABLED kill-switch, read once at
	// startup exactly as the configuration documents it: roll the setting out and
	// restart to freeze mutations while reads keep working.
	WritesDisabled bool
}

// Tools is the agent's function-tool surface.
//
// The six tools are built once, during [New], because schema inference can fail
// and a tool that cannot describe itself must not reach a model. The zero value
// is not usable.
type Tools struct {
	store  Store
	guard  Guard
	redact Redactor

	listIncidents     tool.Tool
	getIncident       tool.Tool
	getServiceStatus  tool.Tool
	searchServiceLogs tool.Tool
	restartService    tool.Tool
	resolveIncident   tool.Tool

	writesDisabled bool
}

// New builds the six tools over cfg.
func New(cfg Config) (*Tools, error) {
	var problems []error
	if cfg.Store == nil {
		problems = append(problems, fmt.Errorf("%w: Store is required", ErrIncompleteConfig))
	}
	if cfg.Guard == nil {
		problems = append(problems, fmt.Errorf(
			"%w: Guard is required; an unguarded read has no deadline, no retries and no circuit breaker",
			ErrIncompleteConfig,
		))
	}
	if cfg.Redact == nil {
		problems = append(problems, fmt.Errorf(
			"%w: Redact is required; an unredacted rationale would reach the append-only audit trail",
			ErrIncompleteConfig,
		))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	agentTools := &Tools{
		store:          cfg.Store,
		guard:          cfg.Guard,
		redact:         cfg.Redact,
		writesDisabled: cfg.WritesDisabled,
	}

	// Every example identifier the model is shown comes from the vocabulary, not
	// from a literal here. A learner who pivots the course onto another domain
	// edits domain/vocabulary.go and these descriptions follow; the ratchet in
	// domain/portability_test.go is what keeps it that way.
	vocabulary := domain.Reference()

	// A table rather than six `if err != nil` blocks: the six constructions
	// differ only in their arguments, and collecting them here gives them one
	// error path and puts the registration order in one readable place.
	build := []struct {
		target      *tool.Tool
		constructor func() (tool.Tool, error)
	}{
		{&agentTools.listIncidents, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        ListIncidentsToolName,
				Description: listIncidentsDescription,
			}, map[string]string{
				"status": "Optional filter — one of " + incidentStatusList() +
					". Leave empty to list incidents of every status.",
				"service": "Optional service name to filter by (e.g. " +
					vocabulary.Services.Checkout + "). Leave empty for all.",
			}, agentTools.runListIncidents)
		}},
		{&agentTools.getIncident, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        GetIncidentToolName,
				Description: getIncidentDescription,
			}, map[string]string{
				"incident_id": "The incident identifier, e.g. " + vocabulary.Incidents.CheckoutLatency + ".",
			}, agentTools.runGetIncident)
		}},
		{&agentTools.getServiceStatus, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        GetServiceStatusToolName,
				Description: getServiceStatusDescription,
			}, map[string]string{
				"name": "The service name, e.g. " + vocabulary.Services.Checkout +
					" or " + vocabulary.Services.Inventory + ".",
			}, agentTools.runGetServiceStatus)
		}},
		{&agentTools.searchServiceLogs, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:        SearchServiceLogsToolName,
				Description: searchServiceLogsDescription,
			}, map[string]string{
				"service": "Service slug, e.g. " + vocabulary.Services.Checkout +
					" or " + vocabulary.Services.Inventory + ".",
				"query": "Optional case-insensitive text that each returned line must contain. " +
					"Leave empty for the first diagnostic read; filter only after reviewing the unfiltered sample.",
				"limit": fmt.Sprintf("Maximum lines to return, from %d to %d.",
					minLogSearchLimit, maxLogSearchLimit),
			}, agentTools.runSearchServiceLogs)
		}},
		// --8<-- [start:guarded-tool-entry]
		{&agentTools.restartService, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:                RestartServiceToolName,
				Description:         restartServiceDescription,
				RequireConfirmation: true,
			}, map[string]string{
				"name": "The service to restart, e.g. " + vocabulary.Services.Inventory + ".",
			}, agentTools.runRestartService)
		}},
		// --8<-- [end:guarded-tool-entry]
		{&agentTools.resolveIncident, func() (tool.Tool, error) {
			return newTool(functiontool.Config{
				Name:                ResolveIncidentToolName,
				Description:         resolveIncidentDescription,
				RequireConfirmation: true,
			}, map[string]string{
				"incident_id": "The incident to resolve, e.g. " + vocabulary.Incidents.InventoryDown + ".",
			}, agentTools.runResolveIncident)
		}},
	}
	for _, entry := range build {
		built, err := entry.constructor()
		if err != nil {
			return nil, err
		}
		*entry.target = built
	}
	return agentTools, nil
}

// The four read tools, individually. Naming each one lets a specialist agent
// expose only the reads its contract needs, which is what Chapter 3.7's
// least-privilege delegation is built on.
func (t *Tools) ListIncidents() tool.Tool     { return t.listIncidents }
func (t *Tools) GetIncident() tool.Tool       { return t.getIncident }
func (t *Tools) GetServiceStatus() tool.Tool  { return t.getServiceStatus }
func (t *Tools) SearchServiceLogs() tool.Tool { return t.searchServiceLogs }

// The two guarded writes, individually.
func (t *Tools) RestartService() tool.Tool  { return t.restartService }
func (t *Tools) ResolveIncident() tool.Tool { return t.resolveIncident }

// ReadTools returns the four idempotent reads, in the order the default agent
// registers them. It is the Go counterpart of Python's ALL_TOOLS.
//
// The slice is fresh on every call: a caller that appends to it, or reorders
// it, must not be able to change what the next caller sees.
func (t *Tools) ReadTools() []tool.Tool {
	return []tool.Tool{t.listIncidents, t.getIncident, t.getServiceStatus, t.searchServiceLogs}
}

// ActionTools returns the two guarded writes, in registration order. It is the
// Go counterpart of Python's ACTION_TOOLS.
func (t *Tools) ActionTools() []tool.Tool {
	return []tool.Tool{t.restartService, t.resolveIncident}
}

// newTool builds one function tool with an explicit, fully documented schema.
//
// The schema is inferred from the argument and result types and then edited,
// rather than left to inference alone, for two reasons the recon spike pinned
// down: ADK reads descriptions only from struct tags, and a description that
// must name a domain identifier cannot be a tag because a tag has to be a
// literal; and the inferrer models an optional field as the union
// ["null", T], which some providers reject outright.
// --8<-- [start:new-tool]
func newTool[TArgs, TResults any](
	cfg functiontool.Config, descriptions map[string]string, handler functiontool.Func[TArgs, TResults],
) (tool.Tool, error) {
	input, err := jsonschema.For[TArgs](nil)
	if err != nil {
		return nil, fmt.Errorf("infer the %s input schema: %w", cfg.Name, err)
	}
	output, err := jsonschema.For[TResults](nil)
	if err != nil {
		return nil, fmt.Errorf("infer the %s output schema: %w", cfg.Name, err)
	}
	if err = describeProperties(input, descriptions); err != nil {
		return nil, fmt.Errorf("document the %s arguments: %w", cfg.Name, err)
	}
	collapseNullable(input)
	collapseNullable(output)
	cfg.InputSchema, cfg.OutputSchema = input, output

	built, err := functiontool.New(cfg, handler)
	if err != nil {
		return nil, fmt.Errorf("build the %s tool: %w", cfg.Name, err)
	}
	return built, nil
}

// --8<-- [end:new-tool]

// describeProperties writes the model-facing description of every argument, and
// refuses a schema where one is missing.
//
// That refusal is the point: an undocumented argument is a silent capability
// loss — the model still sees the field and has no idea what to put in it — and
// this is the earliest place it can be caught. It is a construction-time
// invariant rather than only a test because the descriptions are assembled at
// runtime from the vocabulary.
func describeProperties(schema *jsonschema.Schema, descriptions map[string]string) error {
	for name, description := range descriptions {
		property, ok := schema.Properties[name]
		if !ok {
			return fmt.Errorf("%w: there is no argument named %q to document", ErrIncompleteConfig, name)
		}
		property.Description = description
	}
	// Sorted rather than ranged: Go randomizes map iteration, and an error that
	// names a different argument on every run is not reproducible.
	for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
		if schema.Properties[name].Description == "" {
			return fmt.Errorf("%w: argument %q carries no description for the model", ErrIncompleteConfig, name)
		}
	}
	return nil
}

// collapseNullable rewrites {"type": ["null", T]} back to {"type": T} for every
// optional property, recursively.
//
// The inferrer produces that union for every Go pointer and slice, because both
// can be nil, and some model providers reject a union type outright. An
// *optional* property never travels as null — an argument the model omits is
// absent, and a result field that does not apply is dropped by `omitzero` — so
// for those the union describes a value that cannot occur and is safe to lose.
//
// A *required* property is left alone, and that restraint is load-bearing: an
// incident's resolved_at is required and genuinely null while the incident is
// open, and flattening its type would make every open incident fail validation
// on the way out.
func collapseNullable(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	for name, property := range schema.Properties {
		if !required[name] && len(property.Types) == 2 && slices.Contains(property.Types, "null") {
			for _, kind := range property.Types {
				if kind != "null" {
					// Type and Types are mutually exclusive in this schema type, so
					// the slice has to be cleared as the scalar is set.
					property.Type, property.Types = kind, nil
				}
			}
		}
		collapseNullable(property)
	}
	collapseNullable(schema.Items)
	// Map-typed fields land in AdditionalProperties rather than Properties, so the
	// recursion is incomplete without this line. No struct here declares one today;
	// this completes the walk rather than fixing an observed break.
	collapseNullable(schema.AdditionalProperties)
}
